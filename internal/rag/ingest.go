package rag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/tracing"
)

// EmbedderFactory builds an embedder for a model connection.
type EmbedderFactory func(conn *store.ModelConnection) (provider.Embedder, error)

// Settings are the ingestion tunables that the dispatcher goroutine reads.
// They are passed to NewIngester rather than assigned afterwards because
// the dispatcher starts polling immediately. A zero field takes its
// default.
type Settings struct {
	// Lease is how long a claim stays valid without a heartbeat.
	Lease time.Duration
	// PollInterval is how often the dispatcher looks for work when nothing
	// kicks it.
	PollInterval time.Duration
	// MaxAttempts caps how often a document may be claimed.
	MaxAttempts int
	// MaxPending is the cluster-wide backlog ceiling Enqueue enforces.
	MaxPending int
	// MaxChunksPerDocument fails a document that splits into more chunks
	// than this before anything is embedded; 0 means DefaultMaxChunks.
	MaxChunksPerDocument int
	// EmbedBatchSize is how many chunks go to the embedder in one call;
	// 0 means defaultEmbedBatch.
	EmbedBatchSize int
	// Metrics counts claims, jobs and chunks; nil records nothing.
	Metrics *obs.Metrics
	// Tracer opens the ingest.document span; nil is a disabled tracer.
	Tracer *tracing.Tracer
}

// Ingester processes uploaded documents in the background. One dispatcher
// goroutine claims documents out of the database and hands them to a fixed
// set of workers, so the database sees one poll per replica rather than one
// per worker.
type Ingester struct {
	store   *store.Store
	factory EmbedderFactory
	log     *slog.Logger
	// owner identifies this process in documents.claimed_by. A restart
	// gets a new owner, so the leases of the previous process simply
	// expire instead of being mistaken for this one's.
	owner       string
	lease       time.Duration
	poll        time.Duration
	maxAttempts int
	maxPending  int
	workers     int
	// jobs is unbuffered: the dispatcher only claims a document once a
	// worker can take it, so nothing is claimed that is not being worked
	// on and no claim sits idle holding a lease.
	jobs   chan *store.Document
	notify chan struct{}
	// busy counts the documents handed out but not finished.
	busy   atomic.Int64
	wg     sync.WaitGroup
	cancel context.CancelFunc
	// stopping is closed by Stop so workers finish their current job and
	// leave the rest of the queue to the other replicas.
	stopping  chan struct{}
	stopOnce  sync.Once
	batchSize int
	// backlog caches the cluster backlog depth Enqueue checks.
	backlogMu sync.Mutex
	backlogAt time.Time
	backlogN  int
	// maxChunks, metrics and tracer all come in through Settings rather
	// than as exported fields: the dispatcher polls from the moment
	// NewIngester returns, so anything assigned afterwards would be a data
	// race with the worker that is already reading it.
	maxChunks int
	metrics   *obs.Metrics
	tracer    *tracing.Tracer
}

// DefaultMaxChunks is the chunk cap used when Settings.MaxChunksPerDocument
// is 0.
const DefaultMaxChunks = 20000

// defaultEmbedBatch is how many chunks go to the embedder in one call when
// Settings.EmbedBatchSize is 0.
const defaultEmbedBatch = 32

// processTimeout bounds one document's parse, embed and store cycle.
const processTimeout = 15 * time.Minute

// Defaults for Settings.
const (
	// DefaultLease is deliberately far shorter than processTimeout: the
	// heartbeat renews it while the job runs, and a lease sized to the job
	// would leave a crashed replica's document untouchable for 15 minutes.
	DefaultLease = 2 * time.Minute
	// DefaultPollInterval is the idle poll; uploads kick the dispatcher, so
	// this only covers work another replica queued.
	DefaultPollInterval = 5 * time.Second
	// DefaultMaxAttempts caps the claims one document may collect.
	DefaultMaxAttempts = 5
	// DefaultMaxPending is the cluster-wide backlog Enqueue accepts.
	DefaultMaxPending = 1024
)

// backlogTTL is how long Enqueue reuses a backlog count. Uploads arrive one
// request at a time and the ceiling is a coarse guard, so a second of
// staleness is cheaper than a COUNT per upload.
const backlogTTL = time.Second

// ErrQueueFull is returned by Enqueue when the ingestion backlog is full.
// Since the queue moved into the database this is a property of the
// cluster, not of one replica's channel.
var ErrQueueFull = errors.New("the ingestion backlog is full (MAX_PENDING_DOCUMENTS), retry later")

// errLeaseLost ends a job whose lease was taken over by another replica.
var errLeaseLost = errors.New("ingestion lease lost")

// NewIngester starts the dispatcher and n workers.
func NewIngester(ctx context.Context, st *store.Store, factory EmbedderFactory, workers int, log *slog.Logger, settings ...Settings) *Ingester {
	if workers < 1 {
		workers = 1
	}
	if log == nil {
		log = slog.Default()
	}
	var s Settings
	if len(settings) > 0 {
		s = settings[0]
	}
	ctx, cancel := context.WithCancel(ctx)
	ing := &Ingester{
		store: st, factory: factory, log: log, owner: newOwner(),
		lease: or(s.Lease, DefaultLease), poll: or(s.PollInterval, DefaultPollInterval),
		maxAttempts: orInt(s.MaxAttempts, DefaultMaxAttempts), maxPending: orInt(s.MaxPending, DefaultMaxPending),
		workers: workers, jobs: make(chan *store.Document), notify: make(chan struct{}, 1),
		cancel: cancel, stopping: make(chan struct{}),
		batchSize: orInt(s.EmbedBatchSize, defaultEmbedBatch),
		maxChunks: orInt(s.MaxChunksPerDocument, DefaultMaxChunks),
		metrics:   s.Metrics, tracer: s.Tracer,
	}
	for i := 0; i < workers; i++ {
		ing.wg.Add(1)
		go ing.worker(ctx) //nolint:gosec // G118: the worker runs on the caller's context; Background is only used to record a failure after cancellation
	}
	ing.wg.Add(1)
	go ing.dispatch(ctx)
	return ing
}

func or(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// newOwner names this process in documents.claimed_by: the host it runs on
// (readable in a dashboard or a log), the pid, and a random suffix so a
// restart under the same hostname and a recycled pid still reads as a
// different owner.
func newOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return host + "/" + strconv.Itoa(os.Getpid())
	}
	return host + "/" + strconv.Itoa(os.Getpid()) + "/" + hex.EncodeToString(b)
}

// Owner is how this ingester identifies itself in documents.claimed_by.
func (ing *Ingester) Owner() string { return ing.owner }

// Enqueue accepts a document that is already stored as pending and wakes
// the dispatcher.
//
// It keeps its signature and its ErrQueueFull contract (the upload API
// answers 503 with it), but the backpressure changed meaning in 0.4: the
// check is the cluster's backlog depth against MAX_PENDING_DOCUMENTS, not
// the free room in this process's channel. A 503 now means every replica
// together is behind, which is the number an operator can act on.
func (ing *Ingester) Enqueue(docID int64) error {
	n, err := ing.backlogDepth()
	if err != nil {
		// The document is stored as pending either way, so a failed count
		// must not turn into a refused upload: the dispatcher finds it.
		ing.log.Warn("ingest backlog check", "doc", docID, "err", err)
	} else if n > ing.maxPending {
		return ErrQueueFull
	}
	ing.Kick()
	return nil
}

// Kick wakes the dispatcher so it polls now instead of at the next
// INGEST_POLL_INTERVAL tick. The channel holds one token: a kick that
// arrives while another is pending is already covered by it.
func (ing *Ingester) Kick() {
	select {
	case ing.notify <- struct{}{}:
	default:
	}
}

// backlogDepth returns the number of pending documents across the cluster,
// reusing the last count for backlogTTL.
func (ing *Ingester) backlogDepth() (int, error) {
	ing.backlogMu.Lock()
	defer ing.backlogMu.Unlock()
	if !ing.backlogAt.IsZero() && time.Since(ing.backlogAt) < backlogTTL {
		return ing.backlogN, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := ing.store.CountPendingDocuments(ctx)
	if err != nil {
		return 0, err
	}
	ing.backlogN, ing.backlogAt = n, time.Now()
	return n, nil
}

// Stop cancels the workers immediately and waits for them to return.
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
	// Nothing is heartbeating any more, so whatever this process still
	// owned can go straight back into the queue instead of waiting out its
	// lease. That keeps a single-instance restart as instant as it was
	// before the queue moved into the database.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, err := ing.store.ReleaseDocuments(ctx, ing.owner); err != nil {
		ing.log.Warn("release claimed documents", "err", err)
	} else if n > 0 {
		ing.log.Info("released unfinished documents for the next start", "count", n)
	}
	return graceful
}

// dispatch claims documents for the free workers on a kick or on a tick.
// One goroutine per replica polls, whatever the worker count.
func (ing *Ingester) dispatch(ctx context.Context) {
	defer ing.wg.Done()
	t := time.NewTicker(ing.poll)
	defer t.Stop()
	for {
		ing.claimAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ing.stopping:
			return
		case <-t.C:
		case <-ing.notify:
		}
	}
}

// claimAll claims documents until the queue is empty or every worker is
// busy. There is no Resume step any more: this loop finds every pending
// document and every processing one whose lease expired, on every replica,
// on every pass.
func (ing *Ingester) claimAll(ctx context.Context) {
	for ing.busy.Load() < int64(ing.workers) {
		doc, err := ing.store.ClaimDocument(ctx, ing.owner, ing.lease, ing.maxAttempts)
		if errors.Is(err, store.ErrNotFound) {
			ing.metrics.RecordIngestClaim(obs.ClaimEmpty)
			return
		}
		if err != nil {
			ing.metrics.RecordIngestClaim(obs.ClaimError)
			if ctx.Err() == nil {
				ing.log.Warn("claim document for ingestion", "err", err)
			}
			return
		}
		ing.metrics.RecordIngestClaim(obs.ClaimClaimed)
		ing.busy.Add(1)
		select {
		case ing.jobs <- doc:
		case <-ctx.Done():
			ing.busy.Add(-1)
			return
		case <-ing.stopping:
			ing.busy.Add(-1)
			return
		}
	}
}

func (ing *Ingester) worker(ctx context.Context) {
	defer ing.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ing.stopping:
			return
		case doc := <-ing.jobs:
			ing.runJob(ctx, doc)
			ing.busy.Add(-1)
			// A worker just came free; let the dispatcher refill it now
			// rather than at the next tick.
			ing.Kick()
		}
	}
}

func (ing *Ingester) runJob(ctx context.Context, doc *store.Document) {
	start := time.Now()
	chunks, err := ing.process(ctx, doc)
	took := time.Since(start)
	switch {
	case err == nil:
		ing.metrics.RecordIngestJob(obs.IngestReady, chunks, took)
	case errors.Is(err, errLeaseLost):
		// The row belongs to whoever claimed it next; writing a status
		// here would overwrite their work.
		ing.metrics.RecordIngestJob(obs.IngestLeaseLost, 0, took)
		ing.log.Warn("lost ingestion lease; another replica took over", "doc", doc.ID, "file", doc.Filename)
	case ctx.Err() != nil:
		// Shutdown, not a bad document: Stop puts it back in the queue.
		ing.metrics.RecordIngestJob(obs.IngestCancelled, 0, took)
		ing.log.Info("ingestion cancelled by shutdown", "doc", doc.ID)
	default:
		ing.metrics.RecordIngestJob(obs.IngestFailed, 0, took)
		ing.log.Error("ingest failed", "doc", doc.ID, "err", err)
		_ = ing.store.SetDocumentStatus(context.Background(), doc.ID, store.DocFailed, truncate(err.Error(), 1000))
	}
}

// process runs the pipeline for a document this ingester holds the lease
// on, renewing that lease while it works. It is bounded by processTimeout.
//
// A lease that expires while this replica is alive but slow (a long GC
// pause, a database blip that ate the heartbeats) can have the document
// processed twice. That costs embedding money, not data integrity:
// ReplaceDocumentChunks is a single transactional delete-and-insert with
// the status write in it, so the last writer wins cleanly and no half
// state is visible. The heartbeat, and the context it cancels the moment
// the claim is gone, is what keeps that rare.
func (ing *Ingester) process(ctx context.Context, doc *store.Document) (int, error) {
	// Ingestion is its own trace root. A document reprocessed from the
	// dashboard would otherwise hang a job that may run for minutes off the
	// HTTP request that only queued it, and that request's span closes in
	// milliseconds.
	ctx, span := ing.tracer.Start(tracing.ContextWithSpanContext(ctx, tracing.SpanContext{}),
		"ingest.document", tracing.KindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Int64("ragmux.document.id", doc.ID),
		tracing.Int64("ragmux.document.store_id", doc.RAGStoreID),
		tracing.Int("ragmux.document.attempt", doc.Attempts),
	)

	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	var lost atomic.Bool
	beats := make(chan struct{})
	var beatWG sync.WaitGroup
	beatWG.Add(1)
	go func() {
		defer beatWG.Done()
		// A third of the lease: two heartbeats may be lost before another
		// replica is allowed to take the document over.
		t := time.NewTicker(ing.lease / 3)
		defer t.Stop()
		for {
			select {
			case <-beats:
				return
			case <-t.C:
				err := ing.store.ExtendDocumentLease(ctx, doc.ID, ing.owner, ing.lease)
				if errors.Is(err, store.ErrNotFound) {
					lost.Store(true)
					cancel()
					return
				}
				if err != nil && ctx.Err() == nil {
					ing.log.Warn("extend ingestion lease", "doc", doc.ID, "err", err)
				}
			}
		}
	}()
	defer func() {
		close(beats)
		beatWG.Wait()
	}()

	chunks, err := ing.pipeline(ctx, doc)
	if lost.Load() {
		span.RecordError(errLeaseLost)
		return 0, errLeaseLost
	}
	if err != nil {
		// The pipeline's errors are ours: a parse failure, the chunk cap, or
		// a provider error that transportError has already redacted.
		span.RecordError(err)
		return 0, err
	}
	span.SetAttributes(tracing.Int("ragmux.document.chunks", chunks))
	span.SetStatusOK()
	if err := ing.store.ClearDocumentClaim(ctx, doc.ID, ing.owner); err != nil {
		ing.log.Warn("clear ingestion claim", "doc", doc.ID, "err", err)
	}
	return chunks, nil
}

// pipeline parses, chunks, embeds and stores one claimed document.
func (ing *Ingester) pipeline(ctx context.Context, doc *store.Document) (int, error) {
	docID := doc.ID
	rs, err := ing.store.GetRAGStore(ctx, doc.RAGStoreID)
	if err != nil {
		return 0, err
	}
	conn, err := ing.store.GetConnection(ctx, rs.EmbeddingConnectionID)
	if err != nil {
		return 0, fmt.Errorf("embedding connection: %w", err)
	}
	embedder, err := ing.factory(conn)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	data, err := ing.store.DocumentContent(ctx, docID)
	if err != nil {
		return 0, fmt.Errorf("load document content: %w", err)
	}
	parsed, err := Extract(ctx, doc.Filename, data)
	if err != nil {
		return 0, err
	}
	var pages *int
	if parsed.PageCount > 0 {
		pages = &parsed.PageCount
	}
	ing.progress(ctx, docID, progressParsed, pages)
	pieces := SplitBlocks(parsed.Blocks, rs.ChunkSize, rs.ChunkOverlap)
	if len(pieces) == 0 {
		return 0, fmt.Errorf("document produced no text chunks")
	}
	// The cap bounds the memory held by the chunk slice and the single
	// ReplaceDocumentChunks write below, and the embedding calls it takes.
	maxChunks := ing.maxChunks
	if len(pieces) > maxChunks {
		return 0, fmt.Errorf("document splits into %d chunks, more than the limit of %d (MAX_CHUNKS_PER_DOCUMENT); raise the store's chunk_size or the limit", len(pieces), maxChunks)
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
	ing.progress(ctx, docID, progressChunked, nil)
	for i := 0; i < len(chunks); i += ing.batchSize {
		end := i + ing.batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		inputs := inputs[i:end]
		vecs, err := embedder.Embed(ctx, inputs)
		if err != nil {
			return 0, fmt.Errorf("embed batch %d: %w", i/ing.batchSize, err)
		}
		for j := range vecs {
			chunks[i+j].Embedding = vecs[j]
		}
		// One write per batch; the last step (100) is written together with
		// the ready status by ReplaceDocumentChunks.
		if end < len(chunks) {
			ing.progress(ctx, docID, progressChunked+(100-progressChunked)*end/len(chunks), nil)
		}
	}
	if err := ing.store.ReplaceDocumentChunks(ctx, doc, chunks, ing.owner); err != nil {
		return 0, fmt.Errorf("store chunks: %w", err)
	}
	ing.log.Info("document ingested", "doc", docID, "file", doc.Filename, "chunks", len(chunks),
		"dims", len(chunks[0].Embedding), "took", time.Since(start).Round(time.Millisecond))
	return len(chunks), nil
}

// Progress milestones of the pipeline, in percent; embedding batches fill
// the range between progressChunked and 100.
const (
	progressParsed  = 10
	progressChunked = 20
)

// progress records ingestion progress; a failed write only costs the
// dashboard a stale number, so it is logged and otherwise ignored.
func (ing *Ingester) progress(ctx context.Context, docID int64, percent int, pages *int) {
	if err := ing.store.SetDocumentProgress(ctx, docID, percent, pages, ing.owner); err != nil {
		// ErrNotFound here means the lease is gone and the row belongs to
		// another replica now; the job is about to end anyway, so this only
		// notes why the number stopped moving.
		ing.log.Warn("record ingest progress", "doc", docID, "err", err)
	}
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
