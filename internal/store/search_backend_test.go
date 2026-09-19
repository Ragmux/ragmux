package store_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// countingHandler records the warn-level messages a store logs.
type countingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.msgs = append(h.msgs, r.Message)
		h.mu.Unlock()
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

// seedStore creates a rag store with three chunks and returns it.
func seedStore(t *testing.T, ctx context.Context, s *store.Store, name, backend string) *store.RAGStore {
	t.Helper()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb-" + name, ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: name, EmbeddingConnectionID: conn.ID,
		ChunkSize: 100, ChunkOverlap: 10, TopK: 3, SearchBackend: backend})
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

// TestUnavailableBackendFallsBackToPgvector is the case a restored dump
// produces: the row says pg_search, the server has never heard of it. The
// search must answer with hits from the pgvector path, report the backend it
// really used, and say so in the log exactly once per store per process.
func TestUnavailableBackendFallsBackToPgvector(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	h := &countingHandler{}
	s, err := store.Open(ctx, cfg, slog.New(h))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Caps().PgSearch {
		t.Skip("this server carries pg_search; the fallback path needs one without it")
	}

	r := seedStore(t, ctx, s, "restored", store.BackendPgSearch)
	if r.SearchBackend != store.BackendPgSearch {
		t.Fatalf("store did not keep its configured backend: %+v", r)
	}
	q := []float32{1, 0, 0, 0}
	for i := range 3 {
		hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
			store.SearchOptions{Mode: store.SearchHybrid, Backend: r.SearchBackend, Candidates: 3})
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		if used != store.BackendPgvector {
			t.Errorf("search %d used backend %q, want pgvector", i, used)
		}
		if len(hits) == 0 {
			t.Errorf("search %d returned no hits; the fallback must still retrieve", i)
		}
		for _, hit := range hits {
			if hit.LexScore != 0 {
				t.Errorf("pgvector must not expose a lexical score: %+v", hit)
			}
		}
	}
	// One warning per store per process, not one per query.
	if n := h.count("falling back to pgvector"); n != 1 {
		t.Errorf("fallback warnings = %d, want exactly 1 (messages: %v)", n, h.msgs)
	}
	// A store that is valid here must not be warned about at all.
	ok := seedStore(t, ctx, s, "healthy", store.BackendPgvector)
	if _, used, err := s.SearchWithBackend(ctx, ok.ID, "apples", q,
		store.SearchOptions{Mode: store.SearchHybrid, Candidates: 3}); err != nil || used != store.BackendPgvector {
		t.Errorf("pgvector store: %q %v", used, err)
	}
	if n := h.count("falling back to pgvector"); n != 1 {
		t.Errorf("a healthy store must not warn: %v", h.msgs)
	}
}

func TestSearchBackendAvailabilityReporting(t *testing.T) {
	s := testdb.Open(t)
	if !s.SearchBackendAvailable(store.BackendPgvector) || s.SearchBackendReason(store.BackendPgvector) != "" {
		t.Error("pgvector must always be available")
	}
	if s.SearchBackendReason("lucene") == "" {
		t.Error("an unknown backend needs a reason")
	}
	if s.Caps().PgSearch {
		if s.SearchBackendReason(store.BackendPgSearch) != "" {
			t.Errorf("pg_search is available but reports %q", s.SearchBackendReason(store.BackendPgSearch))
		}
	} else if !strings.Contains(s.SearchBackendReason(store.BackendPgSearch), "pg_search") {
		t.Errorf("reason should name the extension: %q", s.SearchBackendReason(store.BackendPgSearch))
	}
}

// TestPgSearchHybridSearch is the real thing, and runs only where ParadeDB
// is. It also proves the claim the whole feature rests on: switching the
// backend needs no reprocessing, so the same chunks answer under both.
func TestPgSearchHybridSearch(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	testdb.RequirePgSearch(t, s)

	r := seedStore(t, ctx, s, "restored", store.BackendPgSearch)
	q := []float32{1, 0, 0, 0}
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: 2})
	if err != nil {
		t.Fatalf("pg_search hybrid: %v", err)
	}
	if used != store.BackendPgSearch {
		t.Fatalf("backend = %q, want pg_search", used)
	}
	var lexical *store.SearchHit
	for i := range hits {
		if hits[i].Index == 2 {
			lexical = &hits[i]
		}
	}
	if lexical == nil {
		t.Fatalf("the rare term should surface through BM25: %+v", hits)
	}
	if lexical.FTSRank == 0 || lexical.LexScore <= 0 {
		t.Errorf("BM25 hit should carry a rank and a raw score: %+v", lexical)
	}

	// A blank query must not error; the lexical leg is simply empty.
	if _, _, err := s.SearchWithBackend(ctx, r.ID, "   ", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: 3}); err != nil {
		t.Errorf("blank query: %v", err)
	}

	// The same store, same chunks, answered by the other backend.
	vhits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgvector, Candidates: 2})
	if err != nil || used != store.BackendPgvector || len(vhits) == 0 {
		t.Fatalf("pgvector on the same chunks: %q %v %+v", used, err, vhits)
	}
	for _, h := range vhits {
		if h.LexScore != 0 {
			t.Errorf("pgvector must not expose a lexical score: %+v", h)
		}
	}
}

// TestPgSearchStemmingTokenizerBuildsAndSearches is the regression for the
// tokenizer the documentation has always named. "en_stem" is not a tokenizer
// *type* pg_search knows -- it rejects the index DDL with "unknown tokenizer
// type: en_stem" -- so setting PG_SEARCH_TOKENIZER to it used to make every
// pg_search store fall back to pgvector for the life of the process, quietly
// and permanently. Only a live server can catch that: the DDL is a string
// until ParadeDB parses it.
func TestPgSearchStemmingTokenizerBuildsAndSearches(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	cfg.PgSearchTokenizer = "en_stem"
	s := testdb.OpenWith(t, cfg)
	testdb.RequirePgSearch(t, s)

	r := seedStore(t, ctx, s, "stemmed", store.BackendPgSearch)
	q := []float32{1, 0, 0, 0}
	hits, used, err := s.SearchWithBackend(ctx, r.ID, "zyxquux", q,
		store.SearchOptions{Mode: store.SearchHybrid, Backend: store.BackendPgSearch, Candidates: 2})
	if err != nil {
		t.Fatalf("stemming tokenizer: %v", err)
	}
	if used != store.BackendPgSearch {
		t.Fatalf("backend = %q, want pg_search: the index DDL was rejected and the search degraded", used)
	}
	var lexical *store.SearchHit
	for i := range hits {
		if hits[i].Index == 2 {
			lexical = &hits[i]
		}
	}
	if lexical == nil || lexical.LexScore <= 0 {
		t.Fatalf("BM25 should still score under a stemming analyser: %+v", hits)
	}
}
