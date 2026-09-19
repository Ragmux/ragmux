package store_test

import (
	"context"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestHybridSearchFusesVectorAndFullText(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, ChunkOverlap: 10, TopK: 3, ContextualChunks: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.SearchMode != store.SearchHybrid || r.FTSConfig != "simple" || r.RerankCandidates != 15 || !r.ContextualChunks {
		t.Fatalf("defaults: %+v", r)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := []*store.Chunk{
		{Index: 0, Content: "apples grow on trees", Embedding: []float32{1, 0, 0, 0},
			Metadata: map[string]any{"section": "Fruit > Apples", "page": 3}, EmbedText: "fruit.txt · Fruit > Apples\n\napples grow on trees"},
		{Index: 1, Content: "bananas are yellow", Embedding: []float32{0.7, 0.7, 0, 0}},
		{Index: 2, Content: "the zyxquux protocol is obscure", Embedding: []float32{0, 0, 0, 1}},
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	q := []float32{1, 0, 0, 0}

	// Hybrid: the vector side ranks the apple chunk first, the full-text
	// side finds the rare term; both must be in the top two.
	hits, err := s.Search(ctx, r.ID, "zyxquux", q, store.SearchOptions{Mode: store.SearchHybrid, Candidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hybrid hits: %+v", hits)
	}
	got := map[int]store.SearchHit{}
	for _, h := range hits {
		got[h.Index] = h
	}
	a, okA := got[0]
	c, okC := got[2]
	if !okA || !okC {
		t.Fatalf("hybrid top-2 should contain apple and zyxquux chunks: %+v", hits)
	}
	if a.VectorRank != 1 || a.FTSRank != 0 || a.Section != "Fruit > Apples" || a.Page != 3 {
		t.Errorf("apple hit: %+v", a)
	}
	if c.FTSRank == 0 || c.Distance < 0.99 || c.Score <= 0 {
		t.Errorf("zyxquux hit: %+v", c)
	}
	if a.Score != c.Score {
		t.Errorf("apple (both lists) (vector rank 1) and zyxquux (fts rank 1) should tie: %f vs %f", a.Score, c.Score)
	}

	// Vector mode: apple first, the far chunk absent.
	vhits, err := s.Search(ctx, r.ID, "zyxquux", q, store.SearchOptions{Mode: store.SearchVector, Candidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(vhits) != 2 || vhits[0].Index != 0 || vhits[1].Index != 1 || vhits[0].VectorRank != 1 || vhits[0].FTSRank != 0 {
		t.Errorf("vector hits: %+v", vhits)
	}

	// max_distance drops far candidates even when full-text matched them.
	fhits, err := s.Search(ctx, r.ID, "zyxquux", q, store.SearchOptions{Mode: store.SearchHybrid, Candidates: 5, MaxDistance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range fhits {
		if h.Index == 2 {
			t.Errorf("zyxquux chunk (distance 1) should be filtered by max_distance: %+v", fhits)
		}
	}
	if len(fhits) != 2 {
		t.Errorf("expected apple and banana within 0.5: %+v", fhits)
	}

	// Blank / stop-word-only queries must not error in hybrid mode.
	for _, blank := range []string{"", "   ", "the and or"} {
		bh, err := s.Search(ctx, r.ID, blank, q, store.SearchOptions{Mode: store.SearchHybrid, Candidates: 3})
		if err != nil {
			t.Fatalf("blank query %q: %v", blank, err)
		}
		if len(bh) == 0 || bh[0].Index != 0 {
			t.Errorf("blank query %q should fall back to vector order: %+v", blank, bh)
		}
	}
	// A query with an unknown text search configuration is an error, not a crash.
	if _, err := s.Search(ctx, r.ID, "apples", q, store.SearchOptions{Mode: store.SearchHybrid, FTSConfig: "nope", Candidates: 3}); err == nil {
		t.Error("expected an error for an unknown fts config")
	}
	if ok, err := s.TextSearchConfigExists(ctx, "english"); err != nil || !ok {
		t.Errorf("english config: %v %v", ok, err)
	}
	if ok, _ := s.TextSearchConfigExists(ctx, "nope"); ok {
		t.Error("nope should not exist")
	}

	// Store settings round-trip through update.
	r.Rerank, r.RerankCandidates, r.MaxDistance, r.SearchMode, r.FTSConfig, r.ContextualChunks = true, 20, 0.35, store.SearchVector, "english", false
	r2, err := s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Rerank || r2.RerankCandidates != 20 || r2.MaxDistance < 0.349 || r2.MaxDistance > 0.351 || r2.SearchMode != store.SearchVector || r2.FTSConfig != "english" || r2.ContextualChunks {
		t.Errorf("updated store: %+v", r2)
	}

	// Reprocess-all flips every document to pending.
	ids, err := s.ResetStoreDocuments(ctx, r.ID)
	if err != nil || len(ids) != 1 || ids[0] != doc.ID {
		t.Fatalf("reset documents: %v %v", ids, err)
	}
	if d, _ := s.GetDocument(ctx, doc.ID); d.Status != store.DocPending {
		t.Errorf("document status after reset: %+v", d)
	}
}
