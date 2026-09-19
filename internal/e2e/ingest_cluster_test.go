package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// countingEmbedder counts the embedding passes it was asked for. Every
// document in these tests splits into a single chunk, so one pass is one
// document: the counter is what proves a document was not embedded twice
// by two replicas.
type countingEmbedder struct{ calls atomic.Int64 }

func (c *countingEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	c.calls.Add(1)
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// ingestFixture opens two stores on one schema - two replicas over one
// database - and returns a RAG store to upload into.
func ingestFixture(t *testing.T) (a, b *store.Store, ragStoreID int64) {
	t.Helper()
	ctx := context.Background()
	cfg := testdb.Config(t)
	a = testdb.OpenWith(t, cfg)
	b = testdb.OpenWith(t, cfg)
	conn, err := a.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai",
		BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := a.CreateRAGStore(ctx, &store.RAGStore{Name: "s", EmbeddingConnectionID: conn.ID, ChunkSize: 400})
	if err != nil {
		t.Fatal(err)
	}
	return a, b, rs.ID
}

func seedDocuments(t *testing.T, st *store.Store, ragStoreID int64, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("# Document %d\n\nA single paragraph about fruit number %d.", i, i)
		d, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: ragStoreID,
			Filename: fmt.Sprintf("doc-%d.md", i), SizeBytes: int64(len(text))}, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	return ids
}

// TestTwoIngestersProcessEachDocumentOnce is the ingestion half of running
// more than one replica: both poll the same queue, and the claim with
// SKIP LOCKED is what stops them from doing the same document twice.
func TestTwoIngestersProcessEachDocumentOnce(t *testing.T) {
	ctx := context.Background()
	a, b, ragStoreID := ingestFixture(t)
	emb := &countingEmbedder{}
	factory := func(*store.ModelConnection) (provider.Embedder, error) { return emb, nil }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	settings := rag.Settings{PollInterval: 20 * time.Millisecond, Lease: 30 * time.Second}

	ingA := rag.NewIngester(ctx, a, factory, 2, log, settings)
	defer ingA.Stop()
	ingB := rag.NewIngester(ctx, b, factory, 2, log, settings)
	defer ingB.Stop()
	if ingA.Owner() == ingB.Owner() {
		t.Fatalf("two ingesters share the owner %q; their claims would be indistinguishable", ingA.Owner())
	}

	const n = 10
	ids := seedDocuments(t, a, ragStoreID, n)
	ingA.Kick()
	ingB.Kick()

	deadline := time.Now().Add(30 * time.Second)
	for {
		ready := 0
		for _, id := range ids {
			d, err := a.GetDocument(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			switch d.Status {
			case store.DocReady:
				ready++
			case store.DocFailed:
				t.Fatalf("document %d failed: %s", id, d.Error)
			}
		}
		if ready == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d documents became ready", ready, n)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := emb.calls.Load(); got != n {
		t.Errorf("embedding passes = %d, want %d: a document was ingested more than once", got, n)
	}
	for _, id := range ids {
		d, err := a.GetDocument(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// A finished ingestion gives the attempt back, so a healthy
		// document never accumulates towards INGEST_MAX_ATTEMPTS.
		if d.Attempts > 1 {
			t.Errorf("document %d took %d attempts", id, d.Attempts)
		}
		if d.ChunkCount != 1 {
			t.Errorf("document %d has %d chunks; the test assumes one per document", id, d.ChunkCount)
		}
	}
}

// TestExpiredLeaseIsReclaimed covers the crash case: a replica claims a
// document and then stops heartbeating (it was killed, or lost the
// database). Nothing releases the claim, so the lease has to.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	a, b, ragStoreID := ingestFixture(t)
	id := seedDocuments(t, a, ragStoreID, 1)[0]

	claimed, err := a.ClaimDocument(ctx, "replica-a", time.Second, 5)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != id || claimed.Status != store.DocProcessing || claimed.Attempts != 1 {
		t.Fatalf("first claim: %+v", claimed)
	}
	// While the lease is live the document is invisible to everyone else.
	if _, err := b.ClaimDocument(ctx, "replica-b", time.Minute, 5); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a live lease must not be claimable: %v", err)
	}
	// replica-a never heartbeats.
	time.Sleep(1200 * time.Millisecond)

	again, err := b.ClaimDocument(ctx, "replica-b", time.Minute, 5)
	if err != nil {
		t.Fatalf("claim after the lease expired: %v", err)
	}
	if again.ID != id || again.Attempts != 2 {
		t.Errorf("reclaim: id=%d attempts=%d, want id=%d attempts=2", again.ID, again.Attempts, id)
	}
	// And the replica that lost it is told so the next time it renews.
	if err := a.ExtendDocumentLease(ctx, id, "replica-a", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old owner's heartbeat should report the lost lease, got %v", err)
	}
}

// TestReleaseDocumentsRequeuesOnCleanStop keeps the single-instance
// experience: a restart picks its work up again straight away instead of
// waiting out a two-minute lease.
func TestReleaseDocumentsRequeuesOnCleanStop(t *testing.T) {
	ctx := context.Background()
	a, b, ragStoreID := ingestFixture(t)
	id := seedDocuments(t, a, ragStoreID, 1)[0]
	if _, err := a.ClaimDocument(ctx, "replica-a", time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	n, err := a.ReleaseDocuments(ctx, "replica-a")
	if err != nil || n != 1 {
		t.Fatalf("release: n=%d err=%v", n, err)
	}
	got, err := b.ClaimDocument(ctx, "replica-b", time.Minute, 5)
	if err != nil {
		t.Fatalf("claim right after a clean release: %v", err)
	}
	// The cancelled attempt was given back, so this is attempt one again.
	if got.ID != id || got.Attempts != 1 {
		t.Errorf("after release: id=%d attempts=%d", got.ID, got.Attempts)
	}
}

// TestAttemptCapStopsACrashLoop: a document that keeps taking its replica
// down must stop being handed around the cluster.
func TestAttemptCapStopsACrashLoop(t *testing.T) {
	ctx := context.Background()
	a, _, ragStoreID := ingestFixture(t)
	id := seedDocuments(t, a, ragStoreID, 1)[0]
	const maxAttempts = 3
	for i := 1; i <= maxAttempts; i++ {
		d, err := a.ClaimDocument(ctx, fmt.Sprintf("replica-%d", i), time.Millisecond, maxAttempts)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if d.Attempts != i {
			t.Errorf("claim %d recorded attempts=%d", i, d.Attempts)
		}
		time.Sleep(5 * time.Millisecond) // the lease expires; nobody heartbeated
	}
	if _, err := a.ClaimDocument(ctx, "replica-4", time.Minute, maxAttempts); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a document past the attempt cap must not be claimable: %v", err)
	}
	// The janitor is what gives such a row its final status.
	n, err := a.FailExhaustedDocuments(ctx, maxAttempts)
	if err != nil || n != 1 {
		t.Fatalf("fail exhausted: n=%d err=%v", n, err)
	}
	d, err := a.GetDocument(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.DocFailed || d.Error == "" {
		t.Errorf("exhausted document: status=%s error=%q", d.Status, d.Error)
	}
}
