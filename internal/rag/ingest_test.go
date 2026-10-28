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

	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/obs"
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

func (n noEmbedder) Embed(context.Context, []string) ([][]float32, *provider.Usage, error) {
	n.t.Error("embedder must not be called for a document over the chunk limit")
	return nil, nil, errors.New("unexpected")
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

func (p *progressEmbedder) Embed(ctx context.Context, in []string) ([][]float32, *provider.Usage, error) {
	p.mu.Lock()
	doc := p.doc
	p.mu.Unlock()
	d, err := p.st.GetDocument(ctx, doc)
	if err != nil {
		p.t.Errorf("read document %d: %v", doc, err)
		return nil, nil, err
	}
	p.mu.Lock()
	p.seen = append(p.seen, d.ProgressPercent)
	p.mu.Unlock()
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil, nil
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

// usageEmbedder reports the given usage on every call (nil: none).
type usageEmbedder struct{ usage *provider.Usage }

func (u usageEmbedder) Embed(_ context.Context, in []string) ([][]float32, *provider.Usage, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0, 0}
	}
	return out, u.usage, nil
}

// TestIngestRecordsEmbedUsage pins the ingest accounting: the tokens the
// upstream reports are counted as reported, and an upstream that reports
// none is counted as an estimate from the chunk text.
func TestIngestRecordsEmbedUsage(t *testing.T) {
	cases := []struct {
		name   string
		usage  *provider.Usage
		wantRe string
	}{
		{"reported", &provider.Usage{PromptTokens: 11, TotalTokens: 11}, `ragmux_ingest_embed_tokens_total{source="reported"} 11`},
		{"estimated", nil, `ragmux_ingest_embed_tokens_total{source="estimated"} `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			text := "# One\n\nA short paragraph of text to embed.\n"
			doc, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "one.md", SizeBytes: int64(len(text))}, []byte(text))
			if err != nil {
				t.Fatal(err)
			}
			reg := metrics.New(metrics.Options{})
			factory := func(*store.ModelConnection) (provider.Embedder, error) { return usageEmbedder{tc.usage}, nil }
			ing := NewIngester(ctx, st, factory, 1, discard(), Settings{PollInterval: time.Hour, Metrics: obs.New(reg)})
			defer ing.Stop()
			awaitStatus(t, ing, doc.ID, store.DocReady)
			out := reg.Text()
			if !strings.Contains(out, tc.wantRe) {
				t.Errorf("metrics do not contain %q:\n%s", tc.wantRe, out)
			}
			if tc.usage == nil && strings.Contains(out, `source="estimated"} 0`) {
				t.Error("estimated tokens must be positive")
			}
		})
	}
}

// dedupEmbedder counts embedding calls and the texts it was given and returns
// a vector derived from the text, so equal text gets an equal vector.
type dedupEmbedder struct {
	mu    sync.Mutex
	calls int
	texts []string
}

func (d *dedupEmbedder) Embed(_ context.Context, in []string) ([][]float32, *provider.Usage, error) {
	d.mu.Lock()
	d.calls++
	d.texts = append(d.texts, in...)
	d.mu.Unlock()
	out := make([][]float32, len(in))
	for i, s := range in {
		sum := 0
		for _, b := range []byte(s) {
			sum += int(b)
		}
		out[i] = []float32{float32(sum%97 + 1), float32(len(s)), 1}
	}
	return out, nil, nil
}

func (d *dedupEmbedder) embedCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// dedupFixture is one store with an ingester over a counting embedder.
type dedupFixture struct {
	st   *store.Store
	ing  *Ingester
	emb  *dedupEmbedder
	conn *store.ModelConnection
	rs   *store.RAGStore
}

func newDedupFixture(t *testing.T, contextual bool) *dedupFixture {
	t.Helper()
	st := testdb.Open(t)
	ctx := context.Background()
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai", BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := st.CreateRAGStore(ctx, &store.RAGStore{Name: "s", EmbeddingConnectionID: conn.ID, ChunkSize: 200, ContextualChunks: contextual})
	if err != nil {
		t.Fatal(err)
	}
	emb := &dedupEmbedder{}
	ing := NewIngester(ctx, st, func(*store.ModelConnection) (provider.Embedder, error) { return emb, nil }, 1, discard(),
		Settings{PollInterval: time.Hour})
	t.Cleanup(ing.Stop)
	return &dedupFixture{st: st, ing: ing, emb: emb, conn: conn, rs: rs}
}

// dedupText splits into several chunks at ChunkSize 200.
func dedupText() string {
	var b strings.Builder
	b.WriteString("# Fruit\n\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "Paragraph %d is about fruit and goes on for a while so the splitter has to cut: apples, pears, plums, cherries, quinces and figs.\n\n", i)
	}
	return b.String()
}

// upload creates a document in storeID the way the upload endpoint does and
// processes it to ready.
func (f *dedupFixture) upload(t *testing.T, storeID int64, name, text, metadata string) *store.Document {
	t.Helper()
	d := &store.Document{RAGStoreID: storeID, Filename: name, SizeBytes: int64(len(text))}
	if metadata != "" {
		d.Metadata = []byte(metadata)
	}
	doc, err := f.st.CreateDocument(context.Background(), d, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return awaitStatus(t, f.ing, doc.ID, store.DocReady)
}

func (f *dedupFixture) vectors(t *testing.T, docID int64) []store.ChunkVector {
	t.Helper()
	dims := f.rs.Dimensions
	if r, err := f.st.GetRAGStore(context.Background(), f.rs.ID); err == nil {
		dims = r.Dimensions
	}
	v, err := f.st.DocumentChunkVectors(context.Background(), docID, dims)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestIdenticalUploadReusesEmbeddings: the same bytes uploaded twice to one
// store are embedded once; the second document gets its own chunks, the same
// vectors and keeps the metadata of its own upload.
func TestIdenticalUploadReusesEmbeddings(t *testing.T) {
	f := newDedupFixture(t, false)
	text := dedupText()
	first := f.upload(t, f.rs.ID, "fruit.md", text, "")
	calls := f.emb.embedCalls()
	if calls == 0 || first.ChunkCount < 2 {
		t.Fatalf("setup: %d embed calls, %d chunks", calls, first.ChunkCount)
	}
	if want := fmt.Sprintf("%x", store.ContentHash([]byte(text))); first.ContentSHA256 != want {
		t.Errorf("content_sha256 = %q, want %q", first.ContentSHA256, want)
	}

	second := f.upload(t, f.rs.ID, "copy-of-fruit.md", text, `{"lang":"tr"}`)
	if got := f.emb.embedCalls(); got != calls {
		t.Errorf("embedding upstream called %d more time(s) for an identical upload", got-calls)
	}
	if second.ID == first.ID || second.ChunkCount != first.ChunkCount {
		t.Errorf("second document %d has %d chunks, first has %d", second.ID, second.ChunkCount, first.ChunkCount)
	}
	if string(second.Metadata) != `{"lang": "tr"}` && string(second.Metadata) != `{"lang":"tr"}` {
		t.Errorf("second document lost its own metadata: %s", second.Metadata)
	}
	a, b := f.vectors(t, first.ID), f.vectors(t, second.ID)
	if len(a) != len(b) || len(a) != first.ChunkCount {
		t.Fatalf("vectors: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Content != b[i].Content || fmt.Sprint(a[i].Embedding) != fmt.Sprint(b[i].Embedding) {
			t.Errorf("chunk %d differs between the documents", i)
		}
	}
	dups, err := f.st.FindDuplicateDocuments(context.Background(), second, false, 3)
	if err != nil || len(dups) != 1 || dups[0].ID != first.ID {
		t.Errorf("FindDuplicateDocuments = %v, %v; want document %d", dups, err, first.ID)
	}

	// The twin owns its chunks and vectors: deleting the document they were
	// copied from (ON DELETE CASCADE) must leave them untouched.
	ctx := context.Background()
	count := func(docID int64) (chunks, vecs int) {
		t.Helper()
		if err := f.st.DB().QueryRow(ctx, `SELECT count(*), count(e.chunk_id) FROM chunks c
			LEFT JOIN chunk_embeddings_3 e ON e.chunk_id = c.id WHERE c.document_id = $1`, docID).Scan(&chunks, &vecs); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantChunks, wantVecs := count(second.ID)
	if err := f.st.DeleteDocument(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if gotChunks, gotVecs := count(second.ID); gotChunks != wantChunks || gotVecs != wantVecs || wantVecs != second.ChunkCount {
		t.Errorf("after deleting the source: %d chunks / %d vectors, want %d / %d (chunk_count %d)",
			gotChunks, gotVecs, wantChunks, wantVecs, second.ChunkCount)
	}
	if after := f.vectors(t, second.ID); len(after) != len(b) || fmt.Sprint(after[0].Embedding) != fmt.Sprint(b[0].Embedding) {
		t.Error("the twin's vectors changed when its source was deleted")
	}
}

// TestSameContentInAnotherStoreIsEmbedded: dedup never crosses a store.
func TestSameContentInAnotherStoreIsEmbedded(t *testing.T) {
	f := newDedupFixture(t, false)
	other, err := f.st.CreateRAGStore(context.Background(), &store.RAGStore{Name: "other", EmbeddingConnectionID: f.conn.ID, ChunkSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	text := dedupText()
	f.upload(t, f.rs.ID, "fruit.md", text, "")
	calls := f.emb.embedCalls()
	f.upload(t, other.ID, "fruit.md", text, "")
	if got := f.emb.embedCalls(); got <= calls {
		t.Errorf("same content in another store was not embedded (calls %d -> %d)", calls, got)
	}
}

// TestChangedChunkingIsEmbedded: a duplicate whose text would now be split
// differently than the stored one must not borrow its vectors.
func TestChangedChunkingIsEmbedded(t *testing.T) {
	f := newDedupFixture(t, false)
	text := dedupText()
	f.upload(t, f.rs.ID, "fruit.md", text, "")
	calls := f.emb.embedCalls()

	rs := *f.rs
	rs.ChunkSize = 400
	if _, err := f.st.UpdateRAGStore(context.Background(), &rs); err != nil {
		t.Fatal(err)
	}
	second := f.upload(t, f.rs.ID, "fruit-again.md", text, "")
	if got := f.emb.embedCalls(); got <= calls {
		t.Errorf("a duplicate under new chunking reused old vectors (calls %d -> %d)", calls, got)
	}
	if first := f.vectors(t, second.ID); len(first) == 0 {
		t.Error("second document has no chunks")
	}

	// The oldest document is now stale, the one just ingested is current: a
	// third upload must find the current one, not give up on the stale one.
	calls = f.emb.embedCalls()
	third := f.upload(t, f.rs.ID, "fruit-third.md", text, "")
	if got := f.emb.embedCalls(); got != calls {
		t.Errorf("a duplicate of the current document was embedded (calls %d -> %d)", calls, got)
	}
	if len(f.vectors(t, third.ID)) != len(f.vectors(t, second.ID)) {
		t.Error("third document does not share the current chunking")
	}
}

// TestContextualChunksNeedTheSameFilename: with contextual chunks the file
// name is part of the embedded text, so a renamed copy is embedded while a
// same-named copy is not.
func TestContextualChunksNeedTheSameFilename(t *testing.T) {
	f := newDedupFixture(t, true)
	text := dedupText()
	f.upload(t, f.rs.ID, "fruit.md", text, "")
	calls := f.emb.embedCalls()

	f.upload(t, f.rs.ID, "renamed.md", text, "")
	afterRename := f.emb.embedCalls()
	if afterRename <= calls {
		t.Errorf("renamed copy reused vectors that embed the old file name (calls %d -> %d)", calls, afterRename)
	}
	f.upload(t, f.rs.ID, "fruit.md", text, "")
	if got := f.emb.embedCalls(); got != afterRename {
		t.Errorf("same-named copy was embedded again (calls %d -> %d)", afterRename, got)
	}
}

// TestReprocessEmbedsAgain: an explicit reprocess is a request to embed
// again, so it never reuses a sibling's vectors.
func TestReprocessEmbedsAgain(t *testing.T) {
	f := newDedupFixture(t, false)
	text := dedupText()
	f.upload(t, f.rs.ID, "fruit.md", text, "")
	second := f.upload(t, f.rs.ID, "copy.md", text, "")
	calls := f.emb.embedCalls()
	if err := f.st.SetDocumentStatus(context.Background(), second.ID, store.DocPending, ""); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, f.ing, second.ID, store.DocReady)
	if got := f.emb.embedCalls(); got <= calls {
		t.Errorf("reprocess did not embed (calls %d -> %d)", calls, got)
	}
}

// TestLegacyRowGetsItsHashWhenProcessed: a row without a hash (written
// before 0025) is hashed lazily, then counts as a duplicate source.
func TestLegacyRowGetsItsHashWhenProcessed(t *testing.T) {
	f := newDedupFixture(t, false)
	ctx := context.Background()
	text := dedupText()
	doc, err := f.st.CreateDocument(ctx, &store.Document{RAGStoreID: f.rs.ID, Filename: "old.md"}, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB().Exec(ctx, "UPDATE documents SET content_sha256 = NULL WHERE id = $1", doc.ID); err != nil {
		t.Fatal(err)
	}
	done := awaitStatus(t, f.ing, doc.ID, store.DocReady)
	if want := fmt.Sprintf("%x", store.ContentHash([]byte(text))); done.ContentSHA256 != want {
		t.Errorf("hash after processing = %q, want %q", done.ContentSHA256, want)
	}
	calls := f.emb.embedCalls()
	f.upload(t, f.rs.ID, "old.md", text, "")
	if got := f.emb.embedCalls(); got != calls {
		t.Errorf("a duplicate of a lazily hashed row was embedded (calls %d -> %d)", calls, got)
	}
}
