package store_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// syncBuffer is a log sink safe for the pool's background goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func vec(dims int) []float32 {
	v := make([]float32, dims)
	v[0] = 1
	return v
}

func newEmbeddingStore(t *testing.T, s *store.Store, name string) (*store.RAGStore, *store.Document) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: name + "-emb", ProviderType: "openai", ModelName: "text-embedding-3-large"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID, ChunkSize: 200, ChunkOverlap: 10, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	return r, doc
}

// A store's first embedding wider than pgvector can index is refused with
// a clear error when the ingest binds the width.
func TestFirstEmbeddingWiderThanIndexableIsRefused(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, doc := newEmbeddingStore(t, s, "wide")

	err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(3072)}}, "")
	if !errors.Is(err, store.ErrVectorDimsUnsupported) || !strings.Contains(err.Error(), "v0.6") {
		t.Fatalf("ingest of 3072 dims = %v, want ErrVectorDimsUnsupported mentioning v0.6", err)
	}
	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dimensions != 0 || got.ChunkCount != 0 {
		t.Errorf("refused ingest left dimensions=%d chunks=%d, want 0/0", got.Dimensions, got.ChunkCount)
	}
	// The width limit is the only thing refused: 2000 still binds.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(store.MaxIndexedVectorDims)}}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.Dimensions != store.MaxIndexedVectorDims {
		t.Errorf("dimensions = %d, want %d", got.Dimensions, store.MaxIndexedVectorDims)
	}
}

// A store bound to 3072 dimensions before the limit existed keeps working,
// is reported at startup and shows up in ragmux_rag_index_missing.
func TestExistingWideStoreIsAuditedAtStartup(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	r, doc := newEmbeddingStore(t, s, "legacy-wide")
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 3072 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	// It still ingests (no HNSW index on that table) and still searches.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(3072)}}, ""); err != nil {
		t.Fatalf("ingest into the existing 3072 store: %v", err)
	}
	if hits, err := s.SearchTopK(ctx, r.ID, vec(3072), 1); err != nil || len(hits) != 1 {
		t.Fatalf("search on the existing 3072 store: %v %d hits", err, len(hits))
	}

	var logs syncBuffer
	s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "halfvec") || !strings.Contains(out, "dimensions=3072") {
		t.Errorf("startup log has no index-missing warning:\n%s", out)
	}
	if n := s2.RAGIndexMissing(); n != 1 {
		t.Errorf("RAGIndexMissing = %d, want 1", n)
	}
	reg := metrics.New(metrics.Options{})
	obs.New(reg).RegisterStore(s2)
	if text := reg.Text(); !strings.Contains(text, "\nragmux_rag_index_missing 1\n") {
		t.Errorf("gauge not exported as 1:\n%s", text)
	}

	// Deleting the store takes it out of the gauge, and a second delete
	// path (or a failed startup audit) cannot push it below zero.
	if err := s2.DeleteRAGStore(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if n := s2.RAGIndexMissing(); n != 0 {
		t.Errorf("after delete RAGIndexMissing = %d, want 0", n)
	}
	if text := reg.Text(); !strings.Contains(text, "\nragmux_rag_index_missing 0\n") {
		t.Errorf("gauge not exported as 0 after delete:\n%s", text)
	}
	if err := s.DeleteRAGStore(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if n := s2.RAGIndexMissing(); n != 0 {
		t.Errorf("RAGIndexMissing = %d after a repeated delete, want 0", n)
	}
}

// A wide store moved to another embedding connection leaves the gauge, and
// the gauge stays at zero even when the startup audit never counted it.
func TestIndexMissingRecountsOnConnectionChange(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, _ := newEmbeddingStore(t, s, "rebind")
	// Made wide after Open, so the startup audit did not count it (as if it
	// had failed): a decrement would take the gauge to -1.
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 3072 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "small", ProviderType: "openai", ModelName: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	r.EmbeddingConnectionID = other.ID
	if _, err := s.UpdateRAGStore(ctx, r); err != nil {
		t.Fatal(err)
	}
	if n := s.RAGIndexMissing(); n != 0 {
		t.Errorf("RAGIndexMissing = %d, want 0", n)
	}
}

// Changing the embedding connection resets the dimension, and an ingest
// that embedded with the old connection stops instead of writing.
func TestConnectionChangeResetsDimensionsAndStopsStaleIngest(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, doc := newEmbeddingStore(t, s, "switch")
	oldConn := r.EmbeddingConnectionID
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 4 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	newConn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-2", ProviderType: "openai", ModelName: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	r.EmbeddingConnectionID = newConn.ID
	upd, err := s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Dimensions != 0 || upd.EmbeddingConnectionID != newConn.ID {
		t.Fatalf("after connection change: dimensions=%d connection=%d, want 0/%d", upd.Dimensions, upd.EmbeddingConnectionID, newConn.ID)
	}

	// The ingest that started before the switch carries the old width.
	err = s.ReplaceDocumentChunksFrom(ctx, doc, oldConn, []*store.Chunk{{Index: 0, Content: "a", Embedding: vec(4)}}, "")
	if !errors.Is(err, store.ErrEmbeddingConnectionChanged) {
		t.Fatalf("stale ingest = %v, want ErrEmbeddingConnectionChanged", err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.Dimensions != 0 || got.ChunkCount != 0 {
		t.Errorf("stale ingest wrote: dimensions=%d chunks=%d", got.Dimensions, got.ChunkCount)
	}
	// An ingest with the new connection binds the new width.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, newConn.ID, []*store.Chunk{{Index: 0, Content: "a", Embedding: vec(8)}}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.Dimensions != 8 || got.ChunkCount != 1 {
		t.Errorf("after new ingest: dimensions=%d chunks=%d, want 8/1", got.Dimensions, got.ChunkCount)
	}
	// Saving other settings leaves the bound width alone.
	cur, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.TopK = 4
	again, err := s.UpdateRAGStore(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	if again.Dimensions != 8 || again.TopK != 4 {
		t.Errorf("update without connection change: dimensions=%d top_k=%d, want 8/4", again.Dimensions, again.TopK)
	}
}

// The real race: the ingest passes its unlocked pre-check while the
// connection change is still uncommitted, then waits on the row lock and
// must see the change once it commits. Without the locked re-check (or
// without the lock) the stale vectors would be written.
func TestStaleIngestRacingConnectionChangeIsStoppedUnderLock(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, doc := newEmbeddingStore(t, s, "race")
	oldConn := r.EmbeddingConnectionID
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 4 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	newConn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-2", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// Create the vector table up front so the ingest's only wait is the row lock.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, oldConn, []*store.Chunk{{Index: 0, Content: "a", Embedding: vec(4)}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(ctx, "DELETE FROM chunks WHERE rag_store_id = $1", r.ID); err != nil {
		t.Fatal(err)
	}

	// The switch, as UpdateRAGStore writes it, held open in its own transaction.
	tx, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE rag_stores SET embedding_connection_id = $1, dimensions = 0 WHERE id = $2",
		newConn.ID, r.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.ReplaceDocumentChunksFrom(ctx, doc, oldConn, []*store.Chunk{{Index: 0, Content: "a", Embedding: vec(4)}}, "")
	}()
	// Wait until the ingest is blocked behind the switch's row lock, i.e. it
	// has already passed the unlocked pre-check against the old binding.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.DB().QueryRow(ctx, "SELECT COUNT(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))",
			pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("ingest finished without waiting for the row lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("ingest never blocked on the store row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrEmbeddingConnectionChanged) {
		t.Fatalf("racing stale ingest = %v, want ErrEmbeddingConnectionChanged", err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.Dimensions != 0 || got.ChunkCount != 0 || got.EmbeddingConnectionID != newConn.ID {
		t.Errorf("after the race: dimensions=%d chunks=%d connection=%d, want 0/0/%d",
			got.Dimensions, got.ChunkCount, got.EmbeddingConnectionID, newConn.ID)
	}
}

// fts_config other than 'simple' is accepted as before, with a warning.
func TestFTSConfigTurkishIsWarnedAtStartup(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "tr", EmbeddingConnectionID: conn.ID, ChunkSize: 200, TopK: 3, FTSConfig: "turkish"}); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "fts_config=turkish") || !strings.Contains(out, "v0.6-9") {
		t.Errorf("startup log has no fts_config warning:\n%s", out)
	}
	got, err := s2.ListRAGStores(ctx)
	if err != nil || len(got) != 1 || got[0].FTSConfig != "turkish" {
		t.Errorf("the store must be left as configured: %v %+v", err, got)
	}
}
