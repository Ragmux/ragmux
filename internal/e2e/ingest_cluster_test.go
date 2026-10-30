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

func (c *countingEmbedder) Embed(_ context.Context, in []string) ([][]float32, *provider.Usage, error) {
	c.calls.Add(1)
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil, nil
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

// A worker whose lease expired must stop writing to the row. Another replica
// owns it now, and the old worker's chunks would replace what the new owner
// wrote and mark the document ready with content a job behind.
func TestWritesAreRefusedAfterTheLeaseIsTakenOver(t *testing.T) {
	ctx := context.Background()
	a, b, ragStoreID := ingestFixture(t)
	id := seedDocuments(t, a, ragStoreID, 1)[0]

	claimed, err := a.ClaimDocument(ctx, "replica-a", 300*time.Millisecond, 5)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := b.ClaimDocument(ctx, "replica-b", time.Minute, 5); err != nil {
		t.Fatalf("the expired lease should be claimable: %v", err)
	}

	if err := a.SetDocumentProgress(ctx, id, 50, nil, "replica-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("progress write by the old owner: %v, want ErrNotFound", err)
	}
	err = a.ReplaceDocumentChunks(ctx, claimed, []*store.Chunk{
		{Index: 0, Content: "stale", Embedding: []float32{1, 0, 0}}}, "replica-a")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("chunk write by the old owner: %v, want ErrNotFound", err)
	}
	doc, err := b.GetDocument(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status == store.DocReady {
		t.Error("the old owner marked a document it no longer owns ready")
	}
	// The replica that does own it is unaffected.
	if err := b.SetDocumentProgress(ctx, id, 50, nil, "replica-b"); err != nil {
		t.Errorf("progress write by the current owner: %v", err)
	}
}

// TestDuplicateUploadReusesEmbeddingsEndToEnd drives the content dedup through
// the upload endpoint. Stores embed the file name by default (contextual
// chunks), so a duplicate is the same bytes under the same name: it is
// embedded once, says so in the response, keeps the metadata of its own upload
// and searches like its twin. A renamed copy and the same file in another
// store are embedded.
func TestDuplicateUploadReusesEmbeddingsEndToEnd(t *testing.T) {
	backend := mockUpstream(t)
	defer backend.Close()
	var embeds atomic.Int64
	up := countingEmbeddings(t, backend, &embeds)
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	newStore := func(name string) int64 {
		rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": name, "embedding_connection_id": connID,
			"chunk_size": 200, "chunk_overlap": 0, "top_k": 5}, "")
		return int64(rs["id"].(float64))
	}
	storeA, storeB := newStore("a"), newStore("b")
	const text = "Apples are red and crunchy."
	// upload sends one file and waits for it; wantDup is the expected
	// deduplicated marker and embedCalls the expected number of embedding
	// calls the upload itself cost.
	upload := func(storeID int64, name, meta string, wantDup bool, embedCalls int64) map[string]any {
		t.Helper()
		before := embeds.Load()
		out := e.uploadWithMetadata(storeID, name, text, meta)
		if out["_status"] != float64(202) || out["deduplicated"] != wantDup {
			t.Fatalf("upload %s: want deduplicated=%v, got %v", name, wantDup, out)
		}
		e.waitReady(int64(out["id"].(float64)))
		if got := embeds.Load() - before; got != embedCalls {
			t.Errorf("upload %s cost %d embedding call(s), want %d", name, got, embedCalls)
		}
		return out
	}

	first := upload(storeA, "apple.md", `{"lang":"en"}`, false, 1)
	firstID := int64(first["id"].(float64))
	// Same bytes, other name: the store embeds the name, so this is new text.
	upload(storeA, "apple-copy.md", "", false, 1)
	// Same bytes, same name: reused, with the metadata of this upload.
	dup := upload(storeA, "apple.md", `{"lang":"tr"}`, true, 0)
	dupID := int64(dup["id"].(float64))
	if dup["duplicate_of"] != float64(firstID) {
		t.Errorf("duplicate_of = %v, want %d", dup["duplicate_of"], firstID)
	}
	if m, _ := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", dupID), nil, "")["metadata"].(map[string]any); m["lang"] != "tr" {
		t.Errorf("the duplicate lost the metadata of its own upload: %v", m)
	}

	search := func(body map[string]any) []map[string]any {
		t.Helper()
		body["query"], body["top_k"] = "apple", 10
		out := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeA), body, "")
		if out["_status"] != float64(200) {
			t.Fatalf("search: %v", out)
		}
		var hits []map[string]any
		for _, h := range out["hits"].([]any) {
			hits = append(hits, h.(map[string]any))
		}
		return hits
	}
	byDoc := map[float64]map[string]any{}
	hits := search(map[string]any{})
	for _, h := range hits {
		byDoc[h["document_id"].(float64)] = h
	}
	if len(hits) != 3 || byDoc[float64(firstID)] == nil || byDoc[float64(dupID)] == nil {
		t.Fatalf("search returned %d hits, want one per document: %v", len(hits), hits)
	}
	if a, b := byDoc[float64(firstID)], byDoc[float64(dupID)]; a["distance"] != b["distance"] || a["content"] != b["content"] {
		t.Errorf("twins score differently: %v vs %v", a, b)
	}
	// The new document answers the metadata filter of its own upload.
	if only := search(map[string]any{"filter": map[string]any{"lang": "tr"}}); len(only) != 1 || only[0]["document_id"] != float64(dupID) {
		t.Errorf("filtering on the duplicate's own metadata: %v", only)
	}

	// Another store never shares embeddings.
	upload(storeB, "apple.md", "", false, 1)

	// The duplicate owns its chunks: deleting the document it borrowed vectors
	// from leaves it searchable with the same score.
	wantDistance := byDoc[float64(dupID)]["distance"]
	if del := e.call("DELETE", fmt.Sprintf("/admin/api/documents/%d", firstID), nil, ""); del["_status"] != float64(200) && del["_status"] != float64(204) {
		t.Fatalf("delete the source document: %v", del)
	}
	var found bool
	for _, h := range search(map[string]any{}) {
		if h["document_id"] == float64(dupID) {
			found = h["distance"] == wantDistance
		}
	}
	if !found {
		t.Errorf("the duplicate is gone or scores differently after its source was deleted")
	}
}
