package store_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// brokenBackend is a lexical backend that answers Prepare and then fails its
// query, which is the shape of every way a BM25 index can go missing under a
// running process.
//
// The case it stands for is the one docs/configuration.md walks an operator
// into: changing PG_SEARCH_TOKENIZER means "DROP INDEX IF EXISTS
// idx_chunks_bm25;", with no restart asked for. Every replica that had
// already built the index keeps answering Prepare out of its own memory and
// then sends @@@ at a table that no longer carries a BM25 index.
type brokenBackend struct {
	prepareErr error
	// prepared and queried count how far each search got; invalidated
	// records that the failed query reached Invalidate.
	prepared    atomic.Int64
	queried     atomic.Int64
	invalidated atomic.Int64
	// realPrepare runs the production ensureBM25Index, so the test exercises
	// the actual short-circuit rather than a stand-in for it.
	realPrepare bool
}

func (b *brokenBackend) Name() string { return store.BackendPgSearch }

// Available is unconditionally true: this server has no pg_search, and the
// point of the test is the path taken *after* a backend was accepted.
func (b *brokenBackend) Available(store.Capabilities) bool { return true }

func (b *brokenBackend) Prepare(ctx context.Context, s *store.Store) error {
	b.prepared.Add(1)
	if b.prepareErr != nil {
		return b.prepareErr
	}
	if b.realPrepare {
		return s.EnsureBM25Index(ctx)
	}
	return nil
}

func (b *brokenBackend) Invalidate(s *store.Store) {
	b.invalidated.Add(1)
	s.SetBM25Ready(false)
}

func (b *brokenBackend) HybridQuery(store.SearchParams) (string, []any) {
	b.queried.Add(1)
	// Stands for pg_search's "`chunks` does not contain a `USING bm25`
	// index": a statement the server refuses to plan.
	return "SELECT 1 FROM chunks_without_a_bm25_index", nil
}

// seedSearchable creates a rag store configured for pg_search with three
// embedded chunks, so a search has something to return once it degrades.
func seedSearchable(t *testing.T, s *store.Store, name string) *store.RAGStore {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{
		Name: "emb-" + name, ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID,
		ChunkSize: 100, ChunkOverlap: 10, TopK: 3, SearchBackend: store.BackendPgSearch})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := []*store.Chunk{
		{Index: 0, Content: "apples grow on trees", Embedding: []float32{1, 0, 0, 0}},
		{Index: 1, Content: "bananas are yellow", Embedding: []float32{0.7, 0.7, 0, 0}},
		{Index: 2, Content: "the zyxquux protocol is obscure", Embedding: []float32{0, 0, 0, 1}},
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	r, err = s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDroppedLexicalIndexFallsBackInsteadOfFailing is the regression this
// phase exists for. Before it, Prepare returned nil from a cached flag, the
// hybrid query failed on the server and the error travelled all the way out
// as a 502 -- on every hybrid search, on every replica, until each was
// restarted. PRD behaviour rule 8 says a retrieval failure must degrade, and
// this is the one path where it did not.
func TestDroppedLexicalIndexFallsBackInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	h := &countingHandler{}
	s, err := store.Open(ctx, testdb.Config(t), slog.New(h))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := seedSearchable(t, s, "dropped")

	b := &brokenBackend{realPrepare: true}
	defer store.SwapSearchBackend(store.BackendPgSearch, b)()
	// The index this process believes it built; the database no longer has
	// it, which is exactly what a DROP INDEX elsewhere leaves behind.
	s.SetBM25Ready(true)

	q := []float32{1, 0, 0, 0}
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 3})
	if err != nil {
		t.Fatalf("a dropped lexical index must not fail the search: %v", err)
	}
	if used != store.BackendPgvector {
		t.Errorf("backend = %q, want pgvector", used)
	}
	if len(hits) == 0 {
		t.Error("the fallback must still retrieve, not just avoid the error")
	}
	if b.queried.Load() != 1 {
		t.Errorf("the lexical backend was queried %d times, want 1", b.queried.Load())
	}
	if b.invalidated.Load() != 1 {
		t.Errorf("Invalidate ran %d times, want 1", b.invalidated.Load())
	}
	if s.BM25Ready() {
		t.Error("a failed query must clear the cached index flag so the next Prepare rebuilds")
	}
	if n := h.count("falling back to pgvector"); n != 1 {
		t.Errorf("fallback warnings = %d, want 1 (messages: %v)", n, h.msgs)
	}

	// The next search re-Prepares rather than trusting the flag. Here the
	// rebuild fails too (no pg_search on this server), which must still
	// degrade rather than fail.
	hits, used, err = s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 3})
	if err != nil || used != store.BackendPgvector || len(hits) == 0 {
		t.Fatalf("second search: backend=%q err=%v hits=%d", used, err, len(hits))
	}
	if b.prepared.Load() != 2 {
		t.Errorf("Prepare ran %d times, want 2: the flag must not survive the failure", b.prepared.Load())
	}
}

// TestUnpreparableLexicalIndexFallsBack covers the other half of the same
// rule: a backend whose index cannot be built at all.
func TestUnpreparableLexicalIndexFallsBack(t *testing.T) {
	ctx := context.Background()
	h := &countingHandler{}
	s, err := store.Open(ctx, testdb.Config(t), slog.New(h))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := seedSearchable(t, s, "unprepared")

	b := &brokenBackend{prepareErr: errors.New("another replica is still building idx_chunks_bm25")}
	defer store.SwapSearchBackend(store.BackendPgSearch, b)()

	hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", []float32{1, 0, 0, 0},
		store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 3})
	if err != nil {
		t.Fatalf("an unpreparable index must not fail the search: %v", err)
	}
	if used != store.BackendPgvector || len(hits) == 0 {
		t.Errorf("backend = %q with %d hits, want pgvector with hits", used, len(hits))
	}
	// Prepare failed, so the broken query must never have been built.
	if b.queried.Load() != 0 {
		t.Errorf("the lexical query ran %d times after Prepare failed", b.queried.Load())
	}
	if n := h.count("could not be prepared"); n != 1 {
		t.Errorf("prepare warnings = %d, want 1 (messages: %v)", n, h.msgs)
	}
}

// TestVectorModeIgnoresABrokenLexicalBackend: vector-only retrieval never
// touches a lexical index, so a broken one must not cost it a Prepare, a
// retry or a downgrade of the reported backend.
func TestVectorModeIgnoresABrokenLexicalBackend(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r := seedSearchable(t, s, "vectoronly")

	b := &brokenBackend{}
	defer store.SwapSearchBackend(store.BackendPgSearch, b)()

	hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", []float32{1, 0, 0, 0},
		store.SearchOptions{Mode: store.SearchVector, Backend: r.SearchBackend, Candidates: 3})
	if err != nil || len(hits) == 0 {
		t.Fatalf("vector search: %v (%d hits)", err, len(hits))
	}
	if used != store.BackendPgSearch {
		t.Errorf("backend = %q; vector mode reports the configured backend unchanged", used)
	}
	if b.prepared.Load() != 0 || b.queried.Load() != 0 {
		t.Errorf("vector mode touched the lexical backend: prepared=%d queried=%d",
			b.prepared.Load(), b.queried.Load())
	}
}
