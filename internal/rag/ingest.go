package rag

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/muhammetsafak/ragmux/internal/provider"
	"github.com/muhammetsafak/ragmux/internal/store"
)

// EmbedderFactory builds an embedder for a model connection.
type EmbedderFactory func(conn *store.ModelConnection) (provider.Embedder, error)

// Ingester processes uploaded documents in the background.
type Ingester struct {
	store     *store.Store
	factory   EmbedderFactory
	log       *slog.Logger
	queue     chan int64
	wg        sync.WaitGroup
	cancel    context.CancelFunc
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
	ing := &Ingester{store: st, factory: factory, log: log, queue: make(chan int64, 1024), cancel: cancel, batchSize: 32}
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

// Stop halts workers and waits for in-flight jobs.
func (ing *Ingester) Stop() {
	ing.cancel()
	ing.wg.Wait()
}

func (ing *Ingester) worker(ctx context.Context) {
	defer ing.wg.Done()
	for {
		select {
		case <-ctx.Done():
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
	text, err := ExtractText(ing.store.DocumentPath(doc))
	if err != nil {
		return err
	}
	pieces := Split(text, rs.ChunkSize, rs.ChunkOverlap)
	if len(pieces) == 0 {
		return fmt.Errorf("document produced no text chunks")
	}
	chunks := make([]*store.Chunk, len(pieces))
	for i, p := range pieces {
		chunks[i] = &store.Chunk{Index: p.Index, Content: p.Content, TokenEstimate: EstimateTokens(p.Content)}
	}
	for i := 0; i < len(chunks); i += ing.batchSize {
		end := i + ing.batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		inputs := make([]string, end-i)
		for j := i; j < end; j++ {
			inputs[j-i] = chunks[j].Content
		}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
