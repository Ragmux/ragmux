package maintenance

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// seedTurkish makes a store on language "en" with n chunks, then moves it to
// Turkish, which leaves its full-text column to be rebuilt.
func seedTurkish(t *testing.T, s *store.Store, n int) *store.RAGStore {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, TopK: 3, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := make([]*store.Chunk, n)
	for i := range chunks {
		chunks[i] = &store.Chunk{Index: i, Content: "Bu kitap çok güzel", Embedding: []float32{1, 0, 0, 0}}
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
		t.Fatal(err)
	}
	r.Language, r.FTSConfig = "tr", ""
	r, err = s.UpdateRAGStore(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.FTSStatus != store.FTSRebuilding {
		t.Fatalf("store is %q, want rebuilding", r.FTSStatus)
	}
	return r
}

func ftsMismatches(t *testing.T, s *store.Store, storeID int64) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(context.Background(),
		`SELECT count(*) FROM chunks WHERE rag_store_id = $1 AND fts IS DISTINCT FROM to_tsvector('turkish', content)`, storeID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunOnceRebuildsInBatches(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r := seedTurkish(t, s, 12)

	f := &FTSRebuilder{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), BatchSize: 5, Pause: time.Millisecond}
	f.RunOnce(ctx)

	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil || got.FTSStatus != store.FTSReady || got.FTSLanguageApplied == nil || *got.FTSLanguageApplied != "tr" {
		t.Fatalf("after RunOnce: %+v, %v", got, err)
	}
	if n := ftsMismatches(t, s, r.ID); n != 0 {
		t.Errorf("%d chunks with a wrong fts", n)
	}
}

// A process that dies between batches loses nothing: the next one reads the
// cursor and carries on, and a rebuild already done is not repeated.
func TestRebuildResumesFromCursor(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	r := seedTurkish(t, s, 12)

	// The "dead" process got two batches in.
	for range 2 {
		if res, err := s.RebuildFTSBatch(ctx, r.ID, 5); err != nil || res.Done || res.Rows != 5 {
			t.Fatalf("batch = %+v, %v", res, err)
		}
	}
	res, err := s.RebuildFTSBatch(ctx, r.ID, 5)
	if err != nil || !res.Done || res.Rows != 2 {
		t.Fatalf("the successor's batch = %+v, %v, want the 2 chunks left", res, err)
	}
}

// Run starts working on a Kick, not only on its poll, and stops with its
// context.
func TestRunPicksUpKickedWork(t *testing.T) {
	s := testdb.Open(t)
	f := NewFTSRebuilder(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.BatchSize, f.Pause, f.Poll = 5, time.Millisecond, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()

	r := seedTurkish(t, s, 7)
	f.Kick()
	f.Kick() // merged, never blocks

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := s.GetRAGStore(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.FTSStatus == store.FTSReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the kicked rebuild did not finish: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if n := ftsMismatches(t, s, r.ID); n != 0 {
		t.Errorf("%d chunks with a wrong fts", n)
	}
}
