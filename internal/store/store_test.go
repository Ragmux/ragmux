package store

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesFiles(t *testing.T) {
	s := openTest(t)
	for _, f := range []string{"ragmux.db", "secret.key", "uploads"} {
		if _, err := os.Stat(filepath.Join(s.DataDir, f)); err != nil {
			t.Errorf("expected %s in data dir: %v", f, err)
		}
	}
	if !s.VecAvailable {
		t.Errorf("expected sqlite-vec to be available")
	}
}

func TestCredentialsRoundTripAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConnection(ctx, &ModelConnection{Name: "o", ProviderType: "openai", APIKey: "sk-secret-123456", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKeyMasked != "sk-...3456" {
		t.Errorf("mask = %q", c.APIKeyMasked)
	}
	s.Close()

	s2, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetConnection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-secret-123456" {
		t.Errorf("api key after reopen = %q", got.APIKey)
	}
}

func TestVectorSearchMatchesBruteForce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	conn, err := s.CreateConnection(ctx, &ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, ChunkOverlap: 10, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &Document{RAGStoreID: r.ID, Filename: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := []*Chunk{
		{Index: 0, Content: "x axis", Embedding: []float32{1, 0, 0, 0}},
		{Index: 1, Content: "y axis", Embedding: []float32{0, 1, 0, 0}},
		{Index: 2, Content: "near x", Embedding: []float32{0.9, 0.1, 0, 0}},
		{Index: 3, Content: "z axis", Embedding: []float32{0, 0, 1, 0}},
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	q := []float32{1, 0, 0, 0}
	vecHits, err := s.SearchTopK(ctx, r.ID, q, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecHits) != 2 || vecHits[0].Content != "x axis" || vecHits[1].Content != "near x" {
		t.Fatalf("unexpected vec hits: %+v", vecHits)
	}
	rr, _ := s.GetRAGStore(ctx, r.ID)
	bfHits, err := s.searchBruteForce(ctx, rr, q, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range vecHits {
		if vecHits[i].ChunkID != bfHits[i].ChunkID {
			t.Errorf("hit %d: vec chunk %d vs brute-force chunk %d", i, vecHits[i].ChunkID, bfHits[i].ChunkID)
		}
		if d := vecHits[i].Distance - bfHits[i].Distance; d > 1e-4 || d < -1e-4 {
			t.Errorf("hit %d: distance mismatch %f vs %f", i, vecHits[i].Distance, bfHits[i].Distance)
		}
	}
	// Deleting the document removes chunks and vectors.
	if err := s.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	rr, _ = s.GetRAGStore(ctx, r.ID)
	if rr.ChunkCount != 0 {
		t.Errorf("chunk count after delete = %d", rr.ChunkCount)
	}
}

func TestProjectKeyLookup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	conn, _ := s.CreateConnection(ctx, &ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	p, key, err := s.CreateProject(ctx, &Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != len("sk-proj-")+43 || key[:8] != "sk-proj-" {
		t.Errorf("bad key %q", key)
	}
	if p.APIKeyPrefix != key[:15] {
		t.Errorf("prefix %q vs key %q", p.APIKeyPrefix, key)
	}
	got, err := s.GetProjectByKey(ctx, key)
	if err != nil || got.ID != p.ID {
		t.Fatalf("lookup by key: %v %+v", err, got)
	}
	if _, err := s.GetProjectByKey(ctx, "sk-proj-wrong"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	nk, err := s.RotateProjectKey(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProjectByKey(ctx, key); err != ErrNotFound {
		t.Errorf("old key should be revoked, got %v", err)
	}
	if _, err := s.GetProjectByKey(ctx, nk); err != nil {
		t.Errorf("new key lookup: %v", err)
	}
}
