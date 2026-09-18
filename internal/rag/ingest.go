package rag

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// EmbedderFactory builds an embedder for a model connection.
type EmbedderFactory func(conn *store.ModelConnection) (provider.Embedder, error)

// Ingester processes uploaded documents in the background.
type Ingester struct {
	store   *store.Store
	factory EmbedderFactory
	log     *slog.Logger
	queue   chan int64
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	// stopping is closed by Stop so workers finish their current job and
	// leave the remaining queue for the next process (see Resume).
	stopping  chan struct{}
	stopOnce  sync.Once
	batchSize int
}

// NewIngester starts n worker goroutines.
func NewIngester(ctx context.Context, st *store.Store, factory EmbedderFactory, workers int, log *slog.Logger) *Ingester {
	if workers < 1 {
		workers = 1
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	ing := &Ingester{store: st, factory: factory, log: log, queue: make(chan int64, 1024), cancel: cancel,
		stopping: make(chan struct{}), batchSize: 32}
	for i := 0; i < workers; i++ {
		ing.wg.Add(1)
		go ing.worker(ctx)
	}
	return ing
}

// Enqueue schedules a document for processing.
func (ing *Ingester) Enqueue(docID int64) {
	select {
	case ing.queue <- docID:
	default:
		ing.log.Warn("ingest queue full; document will be picked up on next restart", "doc", docID)
	}
}

// Resume re-queues documents left unfinished by a previous process.
func (ing *Ingester) Resume(ctx context.Context) error {
	docs, err := ing.store.ListDocumentsByStatus(ctx, store.DocPending, store.DocProcessing)
	if err != nil {
		return err
	}
	for _, d := range docs {
		ing.Enqueue(d.ID)
	}
	if len(docs) > 0 {
		ing.log.Info("resumed unfinished document ingestion", "count", len(docs))
	}
	return nil
}

// Stop cancels the workers immediately and waits for them to return; a job
// in progress is aborted and left for Resume on the next start.
func (ing *Ingester) Stop() {
	ing.stop(0)
}

// StopWithTimeout waits up to d for in-flight jobs, then cancels the worker
// context so a stuck embedding call cannot hold shutdown up. It reports
// whether every job finished in time.
func (ing *Ingester) StopWithTimeout(d time.Duration) bool {
	return ing.stop(d)
}

func (ing *Ingester) stop(d time.Duration) bool {
	ing.stopOnce.Do(func() { close(ing.stopping) })
	done := make(chan struct{})
	go func() {
		ing.wg.Wait()
		close(done)
	}()
	graceful := true
	if d > 0 {
		select {
		case <-done:
		case <-time.After(d):
			graceful = false
			ing.log.Warn("ingestion jobs still running after grace period; cancelling", "grace", d)
		}
	}
	ing.cancel()
	<-done
	return graceful
}

func (ing *Ingester) worker(ctx context.Context) {
	defer ing.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ing.stopping:
			return
		case id := <-ing.queue:
			if err := ing.Process(ctx, id); err != nil {
				ing.log.Error("ingest failed", "doc", id, "err", err)
				_ = ing.store.SetDocumentStatus(context.Background(), id, store.DocFailed, truncate(err.Error(), 1000))
			}
		}
	}
}

// Process runs the full pipeline for a single document synchronously.
func (ing *Ingester) Process(ctx context.Context, docID int64) error {
	doc, err := ing.store.GetDocument(ctx, docID)
	if err != nil {
		return err
	}
	if err := ing.store.SetDocumentStatus(ctx, docID, store.DocProcessing, ""); err != nil {
		return err
	}
	rs, err := ing.store.GetRAGStore(ctx, doc.RAGStoreID)
	if err != nil {
		return err
	}
	conn, err := ing.store.GetConnection(ctx, rs.EmbeddingConnectionID)
	if err != nil {
		return fmt.Errorf("embedding connection: %w", err)
	}
	embedder, err := ing.factory(conn)
	if err != nil {
		return err
	}

	start := time.Now()
	data, err := ing.store.DocumentContent(ctx, docID)
	if err != nil {
		return fmt.Errorf("load document content: %w", err)
	}
	parsed, err := Extract(doc.Filename, data)
	if err != nil {
		return err
	}
	pieces := SplitBlocks(parsed.Blocks, rs.ChunkSize, rs.ChunkOverlap)
	if len(pieces) == 0 {
		return fmt.Errorf("document produced no text chunks")
	}
	title := parsed.Title
	if title == "" {
		title = doc.Filename
	}
	chunks := make([]*store.Chunk, len(pieces))
	inputs := make([]string, len(pieces))
	for i, p := range pieces {
		c := &store.Chunk{Index: p.Index, Content: p.Content, TokenEstimate: EstimateTokens(p.Content),
			Metadata: chunkMetadata(title, p)}
		inputs[i] = p.Content
		if rs.ContextualChunks {
			c.EmbedText = ContextualText(doc.Filename, p)
			inputs[i] = c.EmbedText
		}
		chunks[i] = c
	}
	for i := 0; i < len(chunks); i += ing.batchSize {
		end := i + ing.batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		inputs := inputs[i:end]
		vecs, err := embedder.Embed(ctx, inputs)
		if err != nil {
			return fmt.Errorf("embed batch %d: %w", i/ing.batchSize, err)
		}
		for j := range vecs {
			chunks[i+j].Embedding = vecs[j]
		}
	}
	if err := ing.store.ReplaceDocumentChunks(ctx, doc, chunks); err != nil {
		return fmt.Errorf("store chunks: %w", err)
	}
	ing.log.Info("document ingested", "doc", docID, "file", doc.Filename, "chunks", len(chunks),
		"dims", len(chunks[0].Embedding), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// ContextualText is what gets embedded when contextual chunks are enabled:
// the filename and section path prefixed to the chunk content, so the
// vector carries where the passage sits in the document.
func ContextualText(filename string, c Chunk) string {
	head := filename
	if c.Section != "" {
		head += " · " + c.Section
	}
	return head + "\n\n" + c.Content
}

// chunkMetadata builds the JSON metadata stored with a chunk.
func chunkMetadata(title string, c Chunk) map[string]any {
	m := map[string]any{"title": title}
	if c.Section != "" {
		m["section"] = c.Section
	}
	if c.Page > 0 {
		m["page"] = c.Page
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
