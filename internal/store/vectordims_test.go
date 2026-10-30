package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// A store's first embedding wider than any HNSW index can cover (4000) is
// refused with a clear error when the ingest binds the width.
func TestFirstEmbeddingWiderThanIndexableIsRefused(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r, doc := newEmbeddingStore(t, s, "wide")

	err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(4097)}}, "")
	if !errors.Is(err, store.ErrVectorDimsUnsupported) || !strings.Contains(err.Error(), "4000") ||
		!strings.Contains(err.Error(), "v0.9") {
		t.Fatalf("ingest of 4097 dims = %v, want ErrVectorDimsUnsupported mentioning 4000 and v0.9", err)
	}
	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dimensions != 0 || got.ChunkCount != 0 {
		t.Errorf("refused ingest left dimensions=%d chunks=%d, want 0/0", got.Dimensions, got.ChunkCount)
	}
	// Nothing was created for the refused width.
	var exists bool
	if err := s.DB().QueryRow(ctx, "SELECT to_regclass('chunk_embeddings_4097') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Errorf("a refused width left a table behind (exists=%v, err=%v)", exists, err)
	}
	// 4000 is the last width that binds.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(store.MaxHalfvecIndexedDims)}}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.Dimensions != store.MaxHalfvecIndexedDims {
		t.Errorf("dimensions = %d, want %d", got.Dimensions, store.MaxHalfvecIndexedDims)
	}
}

// A store bound to 5000 dimensions before the limit existed keeps working,
// is reported at startup and shows up in ragmux_rag_index_missing.
func TestExistingWideStoreIsAuditedAtStartup(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	r, doc := newEmbeddingStore(t, s, "legacy-wide")
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 5000 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	// It still ingests (no HNSW index on that table) and still searches.
	if err := s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: vec(5000)}}, ""); err != nil {
		t.Fatalf("ingest into the existing 5000 store: %v", err)
	}
	if hits, err := s.SearchTopK(ctx, r.ID, vec(5000), 1); err != nil || len(hits) != 1 {
		t.Fatalf("search on the existing 5000 store: %v %d hits", err, len(hits))
	}
	if info, err := s.VectorIndex(ctx, 5000); err != nil || info.State != store.VectorIndexNotIndexable {
		t.Errorf("VectorIndex(5000) = %+v, %v, want not_indexable", info, err)
	}
	if _, err := s.StartVectorReindex(ctx, 5000); !errors.Is(err, store.ErrVectorNotIndexable) {
		t.Errorf("reindex of 5000 = %v, want ErrVectorNotIndexable", err)
	}

	var logs syncBuffer
	s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "v0.9") || !strings.Contains(out, "dimensions=5000") {
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
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 5000 WHERE id = $1", r.ID); err != nil {
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

// A store saved before the language existed (fts_config 'turkish', language
// 'simple' after migration 0027) is left as it is, parses its queries with
// 'simple' and is warned about at startup with the language to set.
func TestFTSConfigTurkishIsWarnedAtStartup(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "tr", EmbeddingConnectionID: conn.ID, ChunkSize: 200, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	// What an installation upgraded from 0.5 holds: the old column set, the
	// new one at its migration default.
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET fts_config = 'turkish' WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	// An unrelated save (the dashboard sends the derived fts_config back) must
	// not erase the leftover the warning is read from.
	r, err = s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Name = "renamed"
	if _, err := s.UpdateRAGStore(ctx, r); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "fts_config=turkish") || !strings.Contains(out, "language to 'tr'") {
		t.Errorf("startup log has no fts_config warning:\n%s", out)
	}
	got, err := s2.ListRAGStores(ctx)
	if err != nil || len(got) != 1 || got[0].Language != "simple" || got[0].FTSConfig != "simple" || got[0].FTSStatus != store.FTSNotNeeded {
		t.Errorf("the store must start on 'simple', untouched: %v %+v", err, got)
	}
}

// --- halfvec: 2001..4000 dimensions (ADR-026) ---

func requireHalfvec(t *testing.T, s *store.Store) {
	t.Helper()
	if !s.Caps().SupportsHalfvec() {
		t.Skipf("pgvector %q has no halfvec", s.Caps().PgvectorVersion)
	}
}

// basis is the unit vector along axis i.
func basis(dims, i int) []float32 {
	v := make([]float32, dims)
	v[i] = 1
	return v
}

// ingestBasis binds a store to dims dimensions by ingesting n chunks, chunk i
// being the unit vector along axis i.
func ingestBasis(t *testing.T, s *store.Store, r *store.RAGStore, doc *store.Document, dims, n int) {
	t.Helper()
	chunks := make([]*store.Chunk, n)
	for i := range chunks {
		chunks[i] = &store.Chunk{Index: i, Content: fmt.Sprintf("chunk %d", i), Embedding: basis(dims, i)}
	}
	if err := s.ReplaceDocumentChunksFrom(context.Background(), doc, r.EmbeddingConnectionID, chunks, ""); err != nil {
		t.Fatal(err)
	}
}

// indexlessWidth builds what a store bound to a 2001..4000 model looks like
// after an upgrade from before halfvec: a populated table without its halfvec
// index.
func indexlessWidth(t *testing.T, s *store.Store, name string, dims int) (*store.RAGStore, *store.Document) {
	t.Helper()
	r, doc := newEmbeddingStore(t, s, name)
	ingestBasis(t, s, r, doc, dims, 5)
	if _, err := s.DB().Exec(context.Background(), fmt.Sprintf("DROP INDEX chunk_embeddings_%d_hnsw_half", dims)); err != nil {
		t.Fatal(err)
	}
	return r, doc
}

// waitVectorIndex polls the catalog until the width reports want.
func waitVectorIndex(t *testing.T, s *store.Store, dims int, want string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		info, err := s.VectorIndex(context.Background(), dims)
		if err != nil {
			t.Fatal(err)
		}
		if info.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("vector index of %d dimensions is %q, want %q", dims, info.State, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// forceHNSW is what it takes to make the planner pick the HNSW index on a
// table of a handful of rows: the plain store index and the bitmap and
// sequential scans out of the way (the transaction is rolled back).
func forceHNSW(dims int) []string {
	return []string{fmt.Sprintf("DROP INDEX chunk_embeddings_%d_store", dims),
		"SET LOCAL enable_seqscan = off", "SET LOCAL enable_bitmapscan = off"}
}

// shortPlan clips the plan's lines: a literal 3072-number query vector sits in
// them.
func shortPlan(plan string) string {
	lines := strings.Split(plan, "\n")
	for i, l := range lines {
		if len(l) > 140 {
			lines[i] = l[:140] + "..."
		}
	}
	return strings.Join(lines, "\n")
}

func indexDef(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	var def string
	err := s.DB().QueryRow(context.Background(),
		"SELECT COALESCE((SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1), '')",
		name).Scan(&def)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

// A 3072-dimension store is born with a halfvec HNSW index on the same
// vector(3072) table, the planner uses it, and the distances that come back
// are those of the stored full-precision vectors, not of their fp16 copies.
func TestHalfvecStoreIsIndexedAndKeepsFullPrecisionDistance(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	requireHalfvec(t, s)
	r, doc := newEmbeddingStore(t, s, "wide3072")
	ingestBasis(t, s, r, doc, 3072, 5)

	def := indexDef(t, s, "chunk_embeddings_3072_hnsw_half")
	for _, want := range []string{"USING hnsw", "(embedding)::halfvec(3072)", "halfvec_cosine_ops"} {
		if !strings.Contains(def, want) {
			t.Errorf("halfvec index definition lacks %q: %q", want, def)
		}
	}
	if def := indexDef(t, s, "chunk_embeddings_3072_hnsw"); def != "" {
		t.Errorf("a vector index cannot exist at 3072 dimensions: %q", def)
	}
	var colType string
	if err := s.DB().QueryRow(ctx, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = to_regclass('chunk_embeddings_3072') AND attname = 'embedding'`).Scan(&colType); err != nil || colType != "vector(3072)" {
		t.Errorf("embedding column = %q, %v, want vector(3072): the storage must not change", colType, err)
	}
	if info, err := s.VectorIndex(ctx, 3072); err != nil || info.State != store.VectorIndexHalfvec {
		t.Errorf("VectorIndex(3072) = %+v, %v, want hnsw_halfvec", info, err)
	}

	// 0.3333 is not an fp16 number, so a distance taken from half precision
	// would be off in the fifth decimal.
	q := basis(3072, 0)
	q[1] = 0.3333
	opts := store.SearchOptions{Mode: store.SearchVector, Candidates: 3}
	hits, plan, err := s.SearchUnderSettings(ctx, r.ID, "", q, opts, forceHNSW(3072)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "Index Scan using chunk_embeddings_3072_hnsw_half") {
		t.Fatalf("EXPLAIN does not show a scan of the halfvec HNSW index:\n%s", shortPlan(plan))
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	for i, axis := range []int{0, 1} {
		want := cosineDistance(q, basis(3072, axis))
		if hits[i].Content != fmt.Sprintf("chunk %d", axis) || math.Abs(hits[i].Distance-want) > 1e-6 {
			t.Errorf("hit %d = %q at %.8f, want chunk %d at %.8f", i, hits[i].Content, hits[i].Distance, axis, want)
		}
	}

	// The same through the public paths: vector mode, hybrid mode and the
	// max_distance cut, which is taken on the exact distance too.
	for _, mode := range []string{store.SearchVector, store.SearchHybrid} {
		got, err := s.Search(ctx, r.ID, "chunk", q, store.SearchOptions{Mode: mode, Candidates: 3})
		if err != nil || len(got) == 0 || got[0].Content != "chunk 0" ||
			math.Abs(got[0].Distance-cosineDistance(q, basis(3072, 0))) > 1e-6 {
			t.Errorf("%s search = %+v, %v", mode, got, err)
		}
	}
	cut, err := s.Search(ctx, r.ID, "", q, store.SearchOptions{Mode: store.SearchVector, Candidates: 5, MaxDistance: 0.2})
	if err != nil || len(cut) != 1 || cut[0].Content != "chunk 0" {
		t.Errorf("max_distance 0.2 = %+v, %v, want only chunk 0", cut, err)
	}

	// A table a second store shares is not indexed twice.
	r2, doc2 := newEmbeddingStore(t, s, "wide3072-b")
	ingestBasis(t, s, r2, doc2, 3072, 2)
	var n int
	if err := s.DB().QueryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema()
		AND tablename = 'chunk_embeddings_3072'`).Scan(&n); err != nil || n != 3 {
		t.Errorf("indexes on chunk_embeddings_3072 = %d (%v), want 3 (pkey, store, halfvec)", n, err)
	}
}

// The ParadeDB backend builds its vector leg from the same statement.
func TestHalfvecHybridSearchOnPgSearch(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	requireHalfvec(t, s)
	testdb.RequirePgSearch(t, s)
	r, doc := newEmbeddingStore(t, s, "wide-bm25")
	ingestBasis(t, s, r, doc, 2500, 4)
	q := basis(2500, 2)
	q[3] = 0.3333
	hits, backend, err := s.SearchWithBackend(ctx, r.ID, "chunk", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: 4})
	if err != nil || backend != store.BackendPgSearch || len(hits) == 0 || hits[0].Content != "chunk 2" ||
		math.Abs(hits[0].Distance-cosineDistance(q, basis(2500, 2))) > 1e-6 {
		t.Fatalf("pg_search hybrid on a halfvec width = %+v via %s, %v", hits, backend, err)
	}
}

// The upgrade path: an index-less 2500-wide store keeps answering (exactly,
// in halfvec order), is counted and warned about at startup, and the reindex
// action gives it the index without changing a result or a distance.
func TestReindexBuildsTheHalfvecIndexAndKeepsResults(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	requireHalfvec(t, s)
	r, _ := indexlessWidth(t, s, "legacy2500", 2500)

	q := basis(2500, 0)
	q[1] = 0.3333
	q[2] = 0.1
	opts := store.SearchOptions{Mode: store.SearchVector, Candidates: 5}
	before, err := s.Search(ctx, r.ID, "", q, opts)
	if err != nil || len(before) != 5 {
		t.Fatalf("search without the index = %v, %d hits", err, len(before))
	}

	var logs syncBuffer
	s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "dimensions=2500") ||
		!strings.Contains(out, "reindex") {
		t.Errorf("startup log does not point at the reindex action:\n%s", out)
	}
	if n := s2.RAGIndexMissing(); n != 1 {
		t.Fatalf("RAGIndexMissing = %d, want 1", n)
	}
	reg := metrics.New(metrics.Options{})
	obs.New(reg).RegisterStore(s2)
	if text := reg.Text(); !strings.Contains(text, "\nragmux_rag_index_missing 1\n") {
		t.Errorf("gauge not exported as 1:\n%s", text)
	}
	if info, err := s2.VectorIndex(ctx, 2500); err != nil || info.State != store.VectorIndexMissing {
		t.Fatalf("VectorIndex(2500) = %+v, %v, want missing", info, err)
	}
	annotated, _ := s2.GetRAGStore(ctx, r.ID)
	if err := s2.AnnotateVectorIndex(ctx, annotated); err != nil || annotated.VectorIndex != store.VectorIndexMissing {
		t.Errorf("annotated store = %q, %v, want missing", annotated.VectorIndex, err)
	}

	info, err := s2.StartVectorReindex(ctx, 2500)
	if err != nil || info.State != store.VectorIndexBuilding {
		t.Fatalf("StartVectorReindex = %+v, %v, want building", info, err)
	}
	waitVectorIndex(t, s2, 2500, store.VectorIndexHalfvec)
	// The gauge is recounted when the build returns, a moment after the
	// index turns valid.
	deadline := time.Now().Add(10 * time.Second)
	for s2.RAGIndexMissing() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s2.RAGIndexMissing(); n != 0 {
		t.Errorf("RAGIndexMissing after the build = %d, want 0", n)
	}
	if text := reg.Text(); !strings.Contains(text, "\nragmux_rag_index_missing 0\n") {
		t.Errorf("gauge not exported as 0 after the build:\n%s", text)
	}
	var valid bool
	if err := s2.DB().QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('chunk_embeddings_2500_hnsw_half')`).
		Scan(&valid); err != nil || !valid {
		t.Errorf("catalog indisvalid = %v, %v", valid, err)
	}
	if def := indexDef(t, s2, "chunk_embeddings_2500_hnsw_half"); !strings.Contains(def, "(embedding)::halfvec(2500)") ||
		!strings.Contains(def, "halfvec_cosine_ops") {
		t.Errorf("index definition = %q", def)
	}

	after, err := s2.Search(ctx, r.ID, "", q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("results changed with the index:\nbefore %+v\nafter  %+v", before, after)
	}
	forced, plan, err := s2.SearchUnderSettings(ctx, r.ID, "", q, opts, forceHNSW(2500)...)
	if err != nil || !strings.Contains(plan, "Index Scan using chunk_embeddings_2500_hnsw_half") {
		t.Fatalf("index scan not used (%v):\n%s", err, shortPlan(plan))
	}
	if !reflect.DeepEqual(before, forced) {
		t.Errorf("results through the index differ:\nbefore %+v\nforced %+v", before, forced)
	}

	if _, err := s2.StartVectorReindex(ctx, 2500); !errors.Is(err, store.ErrVectorIndexPresent) {
		t.Errorf("second reindex = %v, want ErrVectorIndexPresent", err)
	}
	if _, err := s2.StartVectorReindex(ctx, 1536); !errors.Is(err, store.ErrVectorIndexPresent) {
		t.Errorf("reindex of a width that needs none = %v, want ErrVectorIndexPresent", err)
	}
	if _, err := s2.StartVectorReindex(ctx, 4097); !errors.Is(err, store.ErrVectorNotIndexable) {
		t.Errorf("reindex of 4097 = %v, want ErrVectorNotIndexable", err)
	}
	affected, err := s2.StoresWithDimensions(ctx, 2500)
	if err != nil || len(affected) != 1 || affected[0].ID != r.ID {
		t.Errorf("StoresWithDimensions = %+v, %v", affected, err)
	}
}

// A new store is not put on an exact scan: while a width has a table but no
// valid halfvec index, binding to it is refused, and works once the width is
// reindexed. The store that is already there keeps ingesting.
func TestBindingToAnIndexlessWidthIsRefusedUntilReindexed(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	requireHalfvec(t, s)
	a, docA := indexlessWidth(t, s, "legacy", 2500)
	b, docB := newEmbeddingStore(t, s, "newcomer")

	err := s.CheckBindable(ctx, 2500)
	if !errors.Is(err, store.ErrVectorIndexMissing) || errors.Is(err, store.ErrVectorDimsUnsupported) ||
		!strings.Contains(err.Error(), "reindex") {
		t.Fatalf("CheckBindable(2500) = %v, want ErrVectorIndexMissing telling to reindex", err)
	}
	if err := s.CheckBindable(ctx, 2600); err != nil {
		t.Errorf("a width with no table must stay bindable: %v", err)
	}
	err = s.ReplaceDocumentChunksFrom(ctx, docB, b.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "x", Embedding: basis(2500, 0)}}, "")
	if !errors.Is(err, store.ErrVectorIndexMissing) {
		t.Fatalf("first ingest of the newcomer = %v, want ErrVectorIndexMissing", err)
	}
	if got, _ := s.GetRAGStore(ctx, b.ID); got.Dimensions != 0 || got.ChunkCount != 0 {
		t.Errorf("refused ingest left dimensions=%d chunks=%d", got.Dimensions, got.ChunkCount)
	}
	// The bound store is not affected.
	ingestBasis(t, s, a, docA, 2500, 3)
	if def := indexDef(t, s, "chunk_embeddings_2500_hnsw_half"); def != "" {
		t.Errorf("an ingest must not build the index behind the operator's back: %q", def)
	}

	if _, err := s.StartVectorReindex(ctx, 2500); err != nil {
		t.Fatal(err)
	}
	waitVectorIndex(t, s, 2500, store.VectorIndexHalfvec)
	if err := s.CheckBindable(ctx, 2500); err != nil {
		t.Errorf("CheckBindable after the reindex: %v", err)
	}
	ingestBasis(t, s, b, docB, 2500, 2)
	if got, _ := s.GetRAGStore(ctx, b.ID); got.Dimensions != 2500 || got.ChunkCount != 2 {
		t.Errorf("newcomer after the reindex: dimensions=%d chunks=%d, want 2500/2", got.Dimensions, got.ChunkCount)
	}
}

// With no store left on the width and nothing in its table, there is nobody
// to run the reindex from and nothing to wait for: the next binding indexes
// the empty table in place.
func TestEmptyUnreferencedIndexlessTableIsIndexedOnBinding(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s0 := testdb.OpenWith(t, cfg)
	requireHalfvec(t, s0)
	a, _ := indexlessWidth(t, s0, "legacy", 2500)
	if err := s0.DeleteRAGStore(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// A restart: the first process remembers having made the table.
	s := testdb.OpenWith(t, cfg)
	b, docB := newEmbeddingStore(t, s, "newcomer")
	if err := s.CheckBindable(ctx, 2500); err != nil {
		t.Fatalf("CheckBindable on an empty, unreferenced table: %v", err)
	}
	ingestBasis(t, s, b, docB, 2500, 2)
	if info, err := s.VectorIndex(ctx, 2500); err != nil || info.State != store.VectorIndexHalfvec {
		t.Errorf("VectorIndex(2500) = %+v, %v, want hnsw_halfvec", info, err)
	}
}

// A build that was cut short leaves an invalid index. It is not mistaken for
// a running build, binding to the width is refused, and the next reindex
// drops it and builds a valid one.
func TestInterruptedReindexIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	requireHalfvec(t, s)
	r, _ := indexlessWidth(t, s, "legacy", 2500)
	if _, err := s.DB().Exec(ctx, "CREATE INDEX chunk_embeddings_2500_hnsw_half ON chunk_embeddings_2500 "+
		"USING hnsw ((embedding::halfvec(2500)) halfvec_cosine_ops)"); err != nil {
		t.Fatal(err)
	}
	// What a cancelled CREATE INDEX CONCURRENTLY leaves behind.
	if _, err := s.DB().Exec(ctx, "UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass('chunk_embeddings_2500_hnsw_half')"); err != nil {
		t.Skipf("cannot invalidate an index without superuser rights: %v", err)
	}
	oid := func() (id uint32, valid bool) {
		t.Helper()
		if err := s.DB().QueryRow(ctx, `SELECT indexrelid::oid::text::bigint, indisvalid FROM pg_index
			WHERE indexrelid = to_regclass('chunk_embeddings_2500_hnsw_half')`).Scan(&id, &valid); err != nil {
			t.Fatal(err)
		}
		return id, valid
	}
	oldOID, valid := oid()
	if valid {
		t.Fatal("fixture index is valid")
	}
	if info, err := s.VectorIndex(ctx, 2500); err != nil || info.State != store.VectorIndexMissing {
		t.Fatalf("VectorIndex with an abandoned index = %+v, %v, want missing", info, err)
	}
	if err := s.CheckBindable(ctx, 2500); !errors.Is(err, store.ErrVectorIndexMissing) || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("CheckBindable = %v, want ErrVectorIndexMissing (interrupted)", err)
	}
	// Searches stay right on an invalid index (the planner ignores it).
	if hits, err := s.SearchTopK(ctx, r.ID, basis(2500, 1), 2); err != nil || len(hits) != 2 || hits[0].Content != "chunk 1" {
		t.Errorf("search with an invalid index = %+v, %v", hits, err)
	}

	if _, err := s.StartVectorReindex(ctx, 2500); err != nil {
		t.Fatal(err)
	}
	waitVectorIndex(t, s, 2500, store.VectorIndexHalfvec)
	newOID, valid := oid()
	if !valid || newOID == oldOID {
		t.Errorf("after the reindex: oid %d -> %d, valid=%v; want a freshly built valid index", oldOID, newOID, valid)
	}
	if err := s.CheckBindable(ctx, 2500); err != nil {
		t.Errorf("CheckBindable after the cleanup: %v", err)
	}
}

// A build someone else is running is reported as such and is not started a
// second time.
func TestRunningReindexIsReportedAndNotRestarted(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	requireHalfvec(t, s)
	r, _ := indexlessWidth(t, s, "legacy", 2500)

	// Another replica's build holds the width's lock.
	holder, err := s.DB().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", store.LockVecReindex, int32(2500)); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_, _ = holder.Exec(ctx, "SELECT pg_advisory_unlock($1, $2)", store.LockVecReindex, int32(2500))
			holder.Release()
		}
	}
	defer release()

	info, err := s.VectorIndex(ctx, 2500)
	if err != nil || info.State != store.VectorIndexBuilding {
		t.Fatalf("VectorIndex during a build = %+v, %v, want building", info, err)
	}
	got, _ := s.GetRAGStore(ctx, r.ID)
	if err := s.AnnotateVectorIndex(ctx, got); err != nil || got.VectorIndex != store.VectorIndexBuilding ||
		got.VectorIndexProgress == nil {
		t.Errorf("annotated store = %q progress=%v, %v", got.VectorIndex, got.VectorIndexProgress, err)
	}
	if err := s.CheckBindable(ctx, 2500); !errors.Is(err, store.ErrVectorIndexMissing) || !strings.Contains(err.Error(), "being built") {
		t.Errorf("CheckBindable during a build = %v", err)
	}
	if info, err := s.StartVectorReindex(ctx, 2500); err != nil || info.State != store.VectorIndexBuilding {
		t.Fatalf("StartVectorReindex during a build = %+v, %v, want building", info, err)
	}
	time.Sleep(300 * time.Millisecond)
	if def := indexDef(t, s, "chunk_embeddings_2500_hnsw_half"); def != "" {
		t.Errorf("a second build was started: %q", def)
	}
	release()
	if info, err := s.VectorIndex(ctx, 2500); err != nil || info.State != store.VectorIndexMissing {
		t.Errorf("VectorIndex after the lock is gone = %+v, %v, want missing", info, err)
	}
}

// Before pgvector 0.7.0 there is no halfvec: the ceiling stays at 2000, the
// message says why, nothing is reindexable and searches keep the vector order.
func TestOldPgvectorKeepsTheLimitAt2000(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	defer s.SetCaps(store.Capabilities{PgvectorVersion: "0.6.2"})()
	r, doc := newEmbeddingStore(t, s, "old")

	err := s.CheckBindable(ctx, 3072)
	if !errors.Is(err, store.ErrVectorDimsUnsupported) || !strings.Contains(err.Error(), "0.7.0") ||
		!strings.Contains(err.Error(), "0.6.2") {
		t.Fatalf("CheckBindable(3072) on pgvector 0.6.2 = %v", err)
	}
	if err := s.CheckBindable(ctx, 2000); err != nil {
		t.Errorf("CheckBindable(2000): %v", err)
	}
	err = s.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID,
		[]*store.Chunk{{Index: 0, Content: "a", Embedding: basis(3072, 0)}}, "")
	if !errors.Is(err, store.ErrVectorDimsUnsupported) {
		t.Fatalf("ingest of 3072 on pgvector 0.6.2 = %v", err)
	}
	if info, err := s.VectorIndex(ctx, 3072); err != nil || info.State != store.VectorIndexNotIndexable {
		t.Errorf("VectorIndex(3072) = %+v, %v, want not_indexable", info, err)
	}
	if _, err := s.StartVectorReindex(ctx, 3072); !errors.Is(err, store.ErrVectorNotIndexable) || !strings.Contains(err.Error(), "0.7.0") {
		t.Errorf("reindex on pgvector 0.6.2 = %v", err)
	}

	// A store bound to 3072 before still searches, in the plain vector order.
	if _, err := s.DB().Exec(ctx, "UPDATE rag_stores SET dimensions = 3072 WHERE id = $1", r.ID); err != nil {
		t.Fatal(err)
	}
	ingestBasis(t, s, r, doc, 3072, 3)
	if hits, err := s.SearchTopK(ctx, r.ID, basis(3072, 2), 1); err != nil || len(hits) != 1 || hits[0].Content != "chunk 2" {
		t.Errorf("search on pgvector 0.6.2 = %+v, %v", hits, err)
	}
}

// --- concurrent reindex, ingest and reindex (CREATE INDEX CONCURRENTLY) ---

// holdSnapshot opens a repeatable-read transaction that has read the table, so
// a CREATE INDEX CONCURRENTLY on it builds and then waits "for old snapshots"
// until the transaction ends. It makes the build as long as the test needs,
// with no dependence on how fast the machine builds an index. The returned
// function ends the transaction (idempotent).
func holdSnapshot(t *testing.T, s *store.Store, table string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

// waitBuildBlocked waits until the build of the width's halfvec index has made
// the (invalid) index and sits in its last wait. From there on a statement
// that waits for the build, and that the build waits for in turn, deadlocks.
func waitBuildBlocked(t *testing.T, s *store.Store, dims int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := s.DB().QueryRow(context.Background(), `SELECT COUNT(*) FROM pg_stat_progress_create_index
			WHERE index_relid = to_regclass($1) AND phase LIKE 'waiting for old snapshots%'`,
			fmt.Sprintf("chunk_embeddings_%d_hnsw_half", dims)).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the build never reached its wait for old snapshots")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// within runs f and fails the test if it does not return in time (a statement
// stuck behind the build is the failure these tests look for).
func within(t *testing.T, what string, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

func indexIsValid(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	var valid bool
	if err := s.DB().QueryRow(context.Background(),
		"SELECT COALESCE((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)), false)", name).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	return valid
}

// A second reindex call -- a double click, another replica -- while the build
// runs gets the build's state, touches nothing of the table, and the build
// survives to be valid.
func TestReindexWhileReindexingReturnsTheRunningBuild(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	requireHalfvec(t, s)
	indexlessWidth(t, s, "legacy", 2500)
	// Another replica: its own store, none of this one's memory.
	other := testdb.OpenWith(t, cfg)

	release := holdSnapshot(t, s, "chunk_embeddings_2500")
	if info, err := s.StartVectorReindex(ctx, 2500); err != nil || info.State != store.VectorIndexBuilding {
		t.Fatalf("first StartVectorReindex = %+v, %v", info, err)
	}
	waitBuildBlocked(t, s, 2500)

	for name, st := range map[string]*store.Store{"same replica": s, "other replica": other} {
		within(t, "the second reindex ("+name+")", 15*time.Second, func() {
			info, err := st.StartVectorReindex(ctx, 2500)
			if err != nil || info.State != store.VectorIndexBuilding {
				t.Errorf("second StartVectorReindex (%s) = %+v, %v, want the running build", name, info, err)
			}
		})
	}
	release()
	waitVectorIndex(t, s, 2500, store.VectorIndexHalfvec)
	if !indexIsValid(t, s, "chunk_embeddings_2500_hnsw_half") {
		t.Error("the build did not survive the second call")
	}
}

// An ingest into a store of the width, on this replica or another, while the
// index is being built neither waits for the build nor kills it.
func TestIngestWhileReindexingDoesNotKillTheBuild(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	requireHalfvec(t, s)
	r, doc := indexlessWidth(t, s, "legacy", 2500)
	// A restarted process: it has not cached the table.
	other := testdb.OpenWith(t, cfg)

	release := holdSnapshot(t, s, "chunk_embeddings_2500")
	if _, err := s.StartVectorReindex(ctx, 2500); err != nil {
		t.Fatal(err)
	}
	waitBuildBlocked(t, s, 2500)

	for name, st := range map[string]*store.Store{"same replica": s, "other replica": other} {
		within(t, "the ingest ("+name+")", 15*time.Second, func() {
			chunks := []*store.Chunk{{Index: 0, Content: name, Embedding: basis(2500, 7)}}
			if err := st.ReplaceDocumentChunksFrom(ctx, doc, r.EmbeddingConnectionID, chunks, ""); err != nil {
				t.Errorf("ingest (%s) during the build: %v", name, err)
			}
		})
	}
	// Binding a new store is still turned away while the index is not there.
	if err := other.CheckBindable(ctx, 2500); !errors.Is(err, store.ErrVectorIndexMissing) || !strings.Contains(err.Error(), "being built") {
		t.Errorf("CheckBindable during the build = %v", err)
	}
	release()
	waitVectorIndex(t, s, 2500, store.VectorIndexHalfvec)
	if !indexIsValid(t, s, "chunk_embeddings_2500_hnsw_half") {
		t.Error("the build did not survive the ingest")
	}
	if got, _ := s.GetRAGStore(ctx, r.ID); got.ChunkCount != 1 {
		t.Errorf("chunks after the ingest = %d, want 1", got.ChunkCount)
	}
}

// ragmux_rag_index_missing follows a reindex another replica ran, within the
// age a count may reach.
func TestIndexMissingFollowsAnotherReplicasReindex(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	builder := testdb.OpenWith(t, cfg)
	requireHalfvec(t, builder)
	indexlessWidth(t, builder, "legacy", 2500)
	watcher := testdb.OpenWith(t, cfg) // counts 1 at startup
	if n := watcher.RAGIndexMissing(); n != 1 {
		t.Fatalf("RAGIndexMissing at startup = %d, want 1", n)
	}
	if _, err := builder.StartVectorReindex(ctx, 2500); err != nil {
		t.Fatal(err)
	}
	waitVectorIndex(t, builder, 2500, store.VectorIndexHalfvec)
	// Within its age the count is the one it has.
	if n := watcher.RAGIndexMissing(); n != 1 {
		t.Errorf("RAGIndexMissing within the count's age = %d, want 1 (no recount per read)", n)
	}
	defer watcher.SetIndexMissingTTL(time.Nanosecond)()
	if n := watcher.RAGIndexMissing(); n != 0 {
		t.Errorf("RAGIndexMissing after another replica's reindex = %d, want 0", n)
	}
}

// A store with no width has no index to build.
func TestReindexOfAnUnboundStoreIsRefused(t *testing.T) {
	s := testdb.Open(t)
	if _, err := s.StartVectorReindex(context.Background(), 0); !errors.Is(err, store.ErrVectorNotBound) {
		t.Errorf("StartVectorReindex(0) = %v, want ErrVectorNotBound", err)
	}
}
