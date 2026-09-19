package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// discard is the logger the ingester tests hand to NewIngester.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// awaitStatus queues a document the way an upload does and waits for the
// dispatcher to bring it to want.
//
// The tests drive the queue rather than calling into the pipeline directly:
// the dispatcher is the only path production has, and it is what turns a
// failed job into a status and an error message on the row. A test that
// bypassed it would not notice a pipeline that leaves documents stuck in
// "processing".
func awaitStatus(t *testing.T, ing *Ingester, id int64, want string) *store.Document {
	t.Helper()
	if err := ing.Enqueue(id); err != nil {
		t.Fatalf("enqueue document %d: %v", id, err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for {
		d, err := ing.store.GetDocument(ctx, id)
		if err != nil {
			t.Fatalf("read document %d: %v", id, err)
		}
		if d.Status == want {
			return d
		}
		last = d.Status
		if time.Now().After(deadline) {
			t.Fatalf("document %d is %q after 60s, want %q (error %q)", id, last, want, d.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type noEmbedder struct{ t *testing.T }

func (n noEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	n.t.Error("embedder must not be called for a document over the chunk limit")
	return nil, errors.New("unexpected")
}

func TestIngestRejectsTooManyChunks(t *testing.T) {
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
	ing := NewIngester(ctx, st, factory, 1, discard(),
		Settings{PollInterval: 50 * time.Millisecond, MaxChunksPerDocument: 3})
	defer ing.Stop()
	// The dispatcher path is also what records the reason: a job that fails
	// must leave the row "failed" with the operator-facing message on it,
	// not sitting in "processing" behind a live lease.
	got := awaitStatus(t, ing, doc.ID, store.DocFailed)
	if !strings.Contains(got.Error, "MAX_CHUNKS_PER_DOCUMENT") {
		t.Fatalf("document error = %q, want the chunk cap", got.Error)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a failed document must not be claimed again", got.Attempts)
	}
}

// TestEnqueueReportsClusterBacklog covers the changed meaning of the 503 on
// upload: the ceiling is the pending documents of the whole cluster, not
// the free room in this process's channel.
func TestEnqueueReportsClusterBacklog(t *testing.T) {
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
	for i := 0; i < 2; i++ {
		if _, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "d.md", SizeBytes: 1}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	// Built by hand: no dispatcher, so the two documents stay pending and
	// the backlog is what Enqueue looks at. Each ingester counts once,
	// which is also what the one-second cache does in production.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	roomy := &Ingester{store: st, log: log, maxPending: 5, notify: make(chan struct{}, 1)}
	if err := roomy.Enqueue(1); err != nil {
		t.Errorf("backlog of 2 under a ceiling of 5: %v", err)
	}
	full := &Ingester{store: st, log: log, maxPending: 1, notify: make(chan struct{}, 1)}
	if err := full.Enqueue(2); !errors.Is(err, ErrQueueFull) {
		t.Errorf("backlog of 2 over a ceiling of 1: %v", err)
	}
}

// progressEmbedder records the document's progress_percent as seen at the
// start of every Embed call and returns fixed-width vectors.
//
// Embed runs on an ingestion worker goroutine while the test goroutine sets
// the document to watch and reads the recording back, so both fields are
// behind the mutex. Errors are reported with Error, never Fatal: Fatal ends
// the goroutine it is called on, which off the test goroutine would leave
// the worker gone and the test waiting for a status that never lands.
type progressEmbedder struct {
	t  *testing.T
	st *store.Store

	mu   sync.Mutex
	doc  int64
	seen []int
}

func (p *progressEmbedder) watch(doc int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.doc, p.seen = doc, nil
}

func (p *progressEmbedder) recorded() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.seen...)
}

func (p *progressEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	p.mu.Lock()
	doc := p.doc
	p.mu.Unlock()
	d, err := p.st.GetDocument(ctx, doc)
	if err != nil {
		p.t.Errorf("read document %d: %v", doc, err)
		return nil, err
	}
	p.mu.Lock()
	p.seen = append(p.seen, d.ProgressPercent)
	p.mu.Unlock()
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func TestIngestReportsProgressAndPageCount(t *testing.T) {
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
	emb := &progressEmbedder{t: t, st: st}
	// Set before the ingester exists: the dispatcher polls from the moment
	// NewIngester returns and the document is already pending, so the
	// embedder has to know which row to watch before that first poll.
	emb.watch(md.ID)
	// A poll interval long enough never to fire leaves Enqueue's kick as the
	// only thing that starts a claim, so the test decides when each document
	// is picked up and can point the embedder at it first.
	factory := func(*store.ModelConnection) (provider.Embedder, error) { return emb, nil }
	ing := NewIngester(ctx, st, factory, 1, discard(), Settings{PollInterval: time.Hour, EmbedBatchSize: 1})
	defer ing.Stop()
	got := awaitStatus(t, ing, md.ID, store.DocReady)
	if got.ProgressPercent != 100 || got.PageCount != nil {
		t.Errorf("markdown after ingest: progress=%d pages=%v", got.ProgressPercent, got.PageCount)
	}
	// One chunk per batch: the first call sees 20 (chunked), each later
	// call the share of the batches already embedded; 100 is written
	// together with the ready status.
	n := got.ChunkCount
	var want []int
	for i := 0; i < n; i++ {
		want = append(want, 20+80*i/n)
	}
	if seen := emb.recorded(); n < 2 || fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("progress seen by embedder = %v, want %v", seen, want)
	}

	pdfData := buildPDF(t, "Hello page one", "Second page here", "Third page")
	pdf, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "three.pdf", SizeBytes: int64(len(pdfData))}, pdfData)
	if err != nil {
		t.Fatal(err)
	}
	emb.watch(pdf.ID)
	got = awaitStatus(t, ing, pdf.ID, store.DocReady)
	if got.PageCount == nil || *got.PageCount != 3 || got.ProgressPercent != 100 {
		t.Errorf("pdf after ingest: pages=%v progress=%d", got.PageCount, got.ProgressPercent)
	}

	// A failure keeps the last value instead of resetting it. The cap is an
	// ingester-wide setting now, so this is a second ingester rather than an
	// assignment to a field the running workers are reading; the document
	// goes back to pending the way a reprocess puts it there.
	ing.Stop()
	if err := st.SetDocumentStatus(ctx, md.ID, store.DocPending, ""); err != nil {
		t.Fatal(err)
	}
	strict := NewIngester(ctx, st, factory, 1, discard(),
		Settings{PollInterval: time.Hour, EmbedBatchSize: 1, MaxChunksPerDocument: 1})
	defer strict.Stop()
	got = awaitStatus(t, strict, md.ID, store.DocFailed)
	if !strings.Contains(got.Error, "MAX_CHUNKS_PER_DOCUMENT") {
		t.Errorf("document error = %q, want the chunk cap", got.Error)
	}
	if got.ProgressPercent != 10 {
		t.Errorf("progress after failing at chunking = %d, want 10", got.ProgressPercent)
	}
}
