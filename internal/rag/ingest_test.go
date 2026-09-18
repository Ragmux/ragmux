package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

type noEmbedder struct{ t *testing.T }

func (n noEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	n.t.Error("embedder must not be called for a document over the chunk limit")
	return nil, errors.New("unexpected")
}

func TestProcessRejectsTooManyChunks(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai", BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := st.CreateRAGStore(ctx, &store.RAGStore{Name: "s", EmbeddingConnectionID: conn.ID, ChunkSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	// Every heading starts a new section and therefore a new chunk.
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "# Section %d\n\nA paragraph of text.\n\n", i)
	}
	text := b.String()
	doc, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "many.md", SizeBytes: int64(len(text))}, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(*store.ModelConnection) (provider.Embedder, error) { return noEmbedder{t}, nil }
	ing := NewIngester(ctx, st, factory, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer ing.Stop()
	ing.MaxChunksPerDocument = 3
	err = ing.Process(ctx, doc.ID)
	if err == nil || !strings.Contains(err.Error(), "MAX_CHUNKS_PER_DOCUMENT") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnqueueReportsFullQueue(t *testing.T) {
	ing := &Ingester{queue: make(chan int64, 1), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ing.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	if err := ing.Enqueue(2); !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v", err)
	}
}

// progressEmbedder records the document's progress_percent as seen at the
// start of every Embed call and returns fixed-width vectors.
type progressEmbedder struct {
	t    *testing.T
	st   *store.Store
	doc  int64
	seen []int
}

func (p *progressEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	d, err := p.st.GetDocument(ctx, p.doc)
	if err != nil {
		p.t.Fatal(err)
	}
	p.seen = append(p.seen, d.ProgressPercent)
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func TestProcessReportsProgressAndPageCount(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai", BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := st.CreateRAGStore(ctx, &store.RAGStore{Name: "s", EmbeddingConnectionID: conn.ID, ChunkSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, "# Section %d\n\nA paragraph of text.\n\n", i)
	}
	md, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "four.md", SizeBytes: int64(b.Len())}, []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	emb := &progressEmbedder{t: t, st: st, doc: md.ID}
	ing := NewIngester(ctx, st, func(*store.ModelConnection) (provider.Embedder, error) { return emb, nil }, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer ing.Stop()
	ing.batchSize = 1
	if err := ing.Process(ctx, md.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDocument(ctx, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.DocReady || got.ProgressPercent != 100 || got.PageCount != nil {
		t.Errorf("markdown after ingest: status=%s progress=%d pages=%v", got.Status, got.ProgressPercent, got.PageCount)
	}
	// One chunk per batch: the first call sees 20 (chunked), each later
	// call the share of the batches already embedded; 100 is written
	// together with the ready status.
	n := got.ChunkCount
	var want []int
	for i := 0; i < n; i++ {
		want = append(want, 20+80*i/n)
	}
	if n < 2 || fmt.Sprint(emb.seen) != fmt.Sprint(want) {
		t.Errorf("progress seen by embedder = %v, want %v", emb.seen, want)
	}

	pdfData := buildPDF(t, "Hello page one", "Second page here", "Third page")
	pdf, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "three.pdf", SizeBytes: int64(len(pdfData))}, pdfData)
	if err != nil {
		t.Fatal(err)
	}
	emb.doc = pdf.ID
	if err := ing.Process(ctx, pdf.ID); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetDocument(ctx, pdf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PageCount == nil || *got.PageCount != 3 || got.ProgressPercent != 100 {
		t.Errorf("pdf after ingest: pages=%v progress=%d", got.PageCount, got.ProgressPercent)
	}

	// A failure keeps the last value instead of resetting it.
	ing.MaxChunksPerDocument = 1
	if err := ing.Process(ctx, md.ID); err == nil {
		t.Fatal("expected chunk limit error")
	}
	got, err = st.GetDocument(ctx, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProgressPercent != 10 {
		t.Errorf("progress after failing at chunking = %d, want 10", got.ProgressPercent)
	}
}
