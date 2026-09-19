// Package obs is the observability wiring: it declares every metric the
// gateway exports and gives each subsystem a small, nil-safe recorder to
// call. The registry itself lives in internal/metrics and knows nothing
// about Ragmux; this package is where the names, the labels and — above all
// — the cardinality decisions are made.
//
// # Cardinality
//
// Every label in this file is bounded by the size of the installation, never
// by traffic. The rules, in full:
//
//   - An HTTP route label is the chi route pattern, read after the handler
//     ran, never r.URL.Path. /admin/api/rag-stores/{id}/documents is one
//     series; the path would be one series per document id.
//   - A model label is the model connection's ModelName, resolved
//     server-side, never the model string the client sent. An
//     attacker-controlled label value is an unbounded-memory bug.
//   - project and store labels are numeric ids.
//   - A failure reason is a fixed constant from a closed set, never
//     err.Error().
//   - Nothing is ever labelled with a header, an error message, a filename,
//     a query string, a user agent or an IP address.
//
// A nil *Metrics is valid and every method on it does nothing, so a test or
// a command that does not want metrics simply leaves the field unset.
package obs

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/store"
)

// Bucket sets. They are chosen for what the gateway actually does: a chat
// completion legitimately takes thirty seconds, so an HTTP histogram that
// stops at ten would put every real completion in +Inf.
var (
	httpBuckets     = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}
	ttftBuckets     = []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30}
	ragBuckets      = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}
	ingestBuckets   = []float64{1, 5, 15, 30, 60, 120, 300, 600, 900}
	documentGaugeTT = 30 * time.Second
	// documentQueryTimeout bounds the cached backlog query. It runs inline
	// on the scrape that finds the cache stale, so it gets its own budget
	// rather than the scraper's patience.
	documentQueryTimeout = 3 * time.Second
)

// Ingest job outcomes, the values of ragmux_ingest_jobs_total{outcome}.
const (
	IngestReady     = "ready"
	IngestFailed    = "failed"
	IngestCancelled = "cancelled"
	IngestLeaseLost = "lease_lost"
)

// Claim outcomes, the values of ragmux_ingest_claims_total{outcome}.
const (
	ClaimClaimed = "claimed"
	ClaimEmpty   = "empty"
	ClaimError   = "error"
)

// Rerank failure reasons. This set is closed on purpose: the label may never
// carry err.Error(), which is unbounded and frequently quotes an upstream
// body.
const (
	RerankTimeout     = "timeout"
	RerankUpstream    = "upstream"
	RerankParse       = "parse"
	RerankUnavailable = "unavailable"
)

// Metrics holds every family this process exports.
type Metrics struct {
	reg *metrics.Registry

	httpRequests *metrics.CounterVec
	httpDuration *metrics.HistogramVec
	httpInFlight *metrics.GaugeVec
	httpBytes    *metrics.CounterVec

	gwRequests    *metrics.CounterVec
	gwDuration    *metrics.HistogramVec
	gwUpstream    *metrics.HistogramVec
	gwTTFT        *metrics.HistogramVec
	gwChunks      *metrics.CounterVec
	gwStreams     *metrics.GaugeVec
	gwTokens      *metrics.CounterVec
	gwEstimated   *metrics.CounterVec
	gwCost        *metrics.FloatCounterVec
	gwErrors      *metrics.CounterVec
	gwDisconnects *metrics.CounterVec

	limitsDenied *metrics.CounterVec

	ragRetrieval    *metrics.HistogramVec
	ragEmbed        *metrics.HistogramVec
	ragRerank       *metrics.HistogramVec
	ragRerankFailed *metrics.CounterVec
	ragSearches     *metrics.CounterVec
	ragHits         *metrics.CounterVec
	searchBackends  *metrics.GaugeVec

	ingestJobs      *metrics.CounterVec
	ingestDuration  *metrics.HistogramVec
	ingestChunks    *metrics.CounterVec
	ingestClaims    *metrics.CounterVec
	ingestLeaseLost *metrics.CounterVec
}

// New declares the metric set on reg.
func New(reg *metrics.Registry) *Metrics {
	m := &Metrics{reg: reg}

	m.httpRequests = reg.Counter("ragmux_http_requests_total",
		"HTTP requests by chi route pattern, method and status.", "route", "method", "status")
	m.httpDuration = reg.Histogram("ragmux_http_request_duration_seconds",
		"HTTP request latency by chi route pattern and method.", httpBuckets, "route", "method")
	m.httpInFlight = reg.Gauge("ragmux_http_requests_in_flight",
		"HTTP requests currently being served.")
	m.httpBytes = reg.Counter("ragmux_http_response_bytes_total",
		"Response body bytes written, by chi route pattern.", "route")

	m.gwRequests = reg.Counter("ragmux_gateway_requests_total",
		"Chat completions by project, model, HTTP status and whether the response was streamed.",
		"project", "model", "status", "streamed")
	m.gwDuration = reg.Histogram("ragmux_gateway_request_duration_seconds",
		"End-to-end chat completion latency, from the gateway handler's first line to its last.",
		httpBuckets, "project", "model", "streamed")
	m.gwUpstream = reg.Histogram("ragmux_gateway_upstream_duration_seconds",
		"Time spent inside the provider call, excluding the gateway's own work.",
		httpBuckets, "provider", "model")
	m.gwTTFT = reg.Histogram("ragmux_gateway_time_to_first_token_seconds",
		"Time from the start of a streaming provider call to its first chunk.",
		ttftBuckets, "provider", "model")
	m.gwChunks = reg.Counter("ragmux_gateway_stream_chunks_total",
		"Stream chunks relayed to clients, by provider type.", "provider")
	m.gwStreams = reg.Gauge("ragmux_gateway_streams_active",
		"Streaming chat completions currently in flight.")
	m.gwTokens = reg.Counter("ragmux_gateway_tokens_total",
		"Tokens by project, model and kind (prompt or completion), as reported or estimated.",
		"project", "model", "kind")
	m.gwEstimated = reg.Counter("ragmux_gateway_tokens_estimated_total",
		"Requests whose token counts are Ragmux estimates because the provider reported none. "+
			"Kept separate because a token series that silently mixes estimated and reported counts "+
			"is a trap for anyone doing cost maths.",
		"project", "model")
	m.gwCost = reg.FloatCounter("ragmux_gateway_cost_usd_total",
		"Estimated spend in USD by project and model, from the same price table the request log uses.",
		"project", "model")
	m.gwErrors = reg.Counter("ragmux_gateway_errors_total",
		"Failed chat completions by project, provider type and provider error type.",
		"project", "provider", "type")
	m.gwDisconnects = reg.Counter("ragmux_gateway_client_disconnects_total",
		"Chat completions the client abandoned before they finished (status 499).")

	m.limitsDenied = reg.Counter("ragmux_limits_denied_total",
		"Requests refused by a rate limit or a token budget, by project and deny reason.",
		"project", "reason")

	m.ragRetrieval = reg.Histogram("ragmux_rag_retrieval_duration_seconds",
		"Query embedding plus the database search, by store, effective backend and search mode.",
		ragBuckets, "store", "backend", "mode")
	m.ragEmbed = reg.Histogram("ragmux_rag_embed_duration_seconds",
		"Time spent embedding the retrieval query, by provider type.", ragBuckets, "provider")
	m.ragRerank = reg.Histogram("ragmux_rag_rerank_duration_seconds",
		"Time spent reranking retrieval candidates, by rerank backend.", ragBuckets, "backend")
	m.ragRerankFailed = reg.Counter("ragmux_rag_rerank_failures_total",
		"Reranks that fell back to the fused order, by backend and a fixed reason.", "backend", "reason")
	m.ragSearches = reg.Counter("ragmux_rag_searches_total",
		"Retrievals by store and the backend that actually answered, which differs from the "+
			"store's configured backend when the search fell back to pgvector.", "store", "used")
	m.ragHits = reg.Counter("ragmux_rag_hits_total",
		"Passages returned by retrieval, by store.", "store")
	m.searchBackends = reg.Gauge("ragmux_search_backend_available",
		"1 when this server can answer with that search backend, 0 when a store configured for it falls back.",
		"backend")

	m.ingestJobs = reg.Counter("ragmux_ingest_jobs_total",
		"Finished ingestion jobs by outcome.", "outcome")
	m.ingestDuration = reg.Histogram("ragmux_ingest_duration_seconds",
		"Wall time of one document's parse, chunk, embed and store cycle.", ingestBuckets)
	m.ingestChunks = reg.Counter("ragmux_ingest_chunks_total",
		"Chunks written by ingestion.")
	m.ingestClaims = reg.Counter("ragmux_ingest_claims_total",
		"Dispatcher claim attempts by outcome.", "outcome")
	m.ingestLeaseLost = reg.Counter("ragmux_ingest_lease_lost_total",
		"Jobs abandoned because another replica took the document's lease.")

	return m
}

// Registry is the registry this set was declared on.
func (m *Metrics) Registry() *metrics.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// ---- gateway ----

// GatewayRequest is everything one finished chat completion contributes.
// It is filled at the gateway's single deferred chokepoint, which the
// success, error, mid-stream-failure and client-disconnect paths all reach.
type GatewayRequest struct {
	// Project is the numeric project id as a string.
	Project string
	// Model is the model connection's ModelName. It is deliberately not the
	// model the client asked for: that string is attacker-controlled and
	// would make this label unbounded.
	Model string
	// Provider is the connection's provider type (openai, anthropic, ...).
	Provider string
	Status   int
	Streamed bool
	// Duration is the whole handler, Upstream only the provider call and
	// TimeToFirstToken only the wait for a stream's first chunk; the last
	// two are zero when they did not happen.
	Duration         time.Duration
	Upstream         time.Duration
	TimeToFirstToken time.Duration
	PromptTokens     int
	CompletionTokens int
	// Estimated reports that the token counts are Ragmux's estimate rather
	// than the provider's report.
	Estimated bool
	CostUSD   float64
	// ErrorType is provider.Error.Type, a bounded set; empty on success.
	ErrorType string
	// ClientDisconnected reports the 499 path.
	ClientDisconnected bool
}

// RecordGateway records one finished chat completion.
func (m *Metrics) RecordGateway(r GatewayRequest) {
	if m == nil {
		return
	}
	streamed := strconv.FormatBool(r.Streamed)
	m.gwRequests.With(r.Project, r.Model, strconv.Itoa(r.Status), streamed).Inc()
	m.gwDuration.With(r.Project, r.Model, streamed).Observe(r.Duration.Seconds())
	if r.Upstream > 0 {
		m.gwUpstream.With(r.Provider, r.Model).Observe(r.Upstream.Seconds())
	}
	if r.TimeToFirstToken > 0 {
		m.gwTTFT.With(r.Provider, r.Model).Observe(r.TimeToFirstToken.Seconds())
	}
	if r.PromptTokens > 0 {
		m.gwTokens.With(r.Project, r.Model, "prompt").Add(uint64(r.PromptTokens))
	}
	if r.CompletionTokens > 0 {
		m.gwTokens.With(r.Project, r.Model, "completion").Add(uint64(r.CompletionTokens))
	}
	if r.Estimated {
		m.gwEstimated.With(r.Project, r.Model).Inc()
	}
	m.gwCost.With(r.Project, r.Model).Add(r.CostUSD)
	if r.ErrorType != "" {
		m.gwErrors.With(r.Project, r.Provider, r.ErrorType).Inc()
	}
	if r.ClientDisconnected {
		m.gwDisconnects.With().Inc()
	}
}

// Stream is the metric state of one in-flight stream. It exists because of
// the registry's allocation rule: With(vals...) costs an allocation, so the
// per-chunk path must resolve its series once, here, and never inside the
// loop.
type Stream struct {
	chunks *metrics.Counter
	active *metrics.Gauge
}

// StreamStarted marks a stream as in flight and resolves its per-chunk
// series. The caller must pair it with Ended.
func (m *Metrics) StreamStarted(providerType string) *Stream {
	if m == nil {
		return nil
	}
	s := &Stream{chunks: m.gwChunks.With(providerType), active: m.gwStreams.With()}
	s.active.Inc()
	return s
}

// Chunk counts one relayed stream chunk. It is one atomic add.
func (s *Stream) Chunk() {
	if s == nil {
		return
	}
	s.chunks.Inc()
}

// Ended marks the stream finished, however it finished.
func (s *Stream) Ended() {
	if s == nil {
		return
	}
	s.active.Dec()
}

// RecordLimitDenied counts one request refused by a limit. reason is a
// limits.Reason* constant, a closed set.
func (m *Metrics) RecordLimitDenied(project, reason string) {
	if m == nil {
		return
	}
	m.limitsDenied.With(project, reason).Inc()
}

// ---- rag ----

// RecordRetrieval records one completed retrieval. backend is the backend
// that actually answered, which is not always the one the store asked for.
func (m *Metrics) RecordRetrieval(storeID, backend, mode string, hits int, d time.Duration) {
	if m == nil {
		return
	}
	m.ragRetrieval.With(storeID, backend, mode).Observe(d.Seconds())
	m.ragSearches.With(storeID, backend).Inc()
	if hits > 0 {
		m.ragHits.With(storeID).Add(uint64(hits))
	}
}

// RecordEmbedQuery records the query embedding call of a retrieval.
func (m *Metrics) RecordEmbedQuery(providerType string, d time.Duration) {
	if m == nil {
		return
	}
	m.ragEmbed.With(providerType).Observe(d.Seconds())
}

// RecordRerank records one rerank attempt. reason is "" on success and one
// of the Rerank* constants otherwise; it is never an error message.
func (m *Metrics) RecordRerank(backend, reason string, d time.Duration) {
	if m == nil {
		return
	}
	m.ragRerank.With(backend).Observe(d.Seconds())
	if reason != "" {
		m.ragRerankFailed.With(backend, reason).Inc()
	}
}

// ---- ingestion ----

// RecordIngestJob records one finished ingestion job. outcome is one of the
// Ingest* constants; d is zero for an outcome with no meaningful duration.
func (m *Metrics) RecordIngestJob(outcome string, chunks int, d time.Duration) {
	if m == nil {
		return
	}
	m.ingestJobs.With(outcome).Inc()
	if d > 0 {
		m.ingestDuration.With().Observe(d.Seconds())
	}
	if chunks > 0 {
		m.ingestChunks.With().Add(uint64(chunks))
	}
	if outcome == IngestLeaseLost {
		m.ingestLeaseLost.With().Inc()
	}
}

// RecordIngestClaim records one dispatcher claim attempt. outcome is one of
// the Claim* constants.
func (m *Metrics) RecordIngestClaim(outcome string) {
	if m == nil {
		return
	}
	m.ingestClaims.With(outcome).Inc()
}

// ---- scrape-time collectors ----

// RegisterStore adds the collectors that read the database or the pool:
// the connection pool gauges (free, no query), the detected search backends
// (read once at Open) and the cached documents-by-status gauge.
func (m *Metrics) RegisterStore(st *store.Store) {
	if m == nil || st == nil {
		return
	}
	m.registerPool(st.DB())
	m.searchBackends.With(store.BackendPgvector).Set(1)
	m.searchBackends.With(store.BackendPgSearch).Set(boolGauge(st.Caps().PgSearch))

	m.reg.CachedGaugeFunc("ragmux_documents",
		"Documents by ingestion status; the pending count is the cluster's ingestion backlog. "+
			"Cached for 30 s so a scrape storm cannot become a query storm.",
		"status", documentGaugeTT, func() (map[string]float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), documentQueryTimeout)
			defer cancel()
			return st.DocumentStatusCounts(ctx)
		})
}

// registerPool exports the pgx pool statistics. Stat() reads counters the
// pool already keeps, so these gauges cost no query and are evaluated
// inline during the scrape.
func (m *Metrics) registerPool(p *pgxpool.Pool) {
	if p == nil {
		return
	}
	m.reg.GaugeFunc("ragmux_db_pool_total_conns", "Connections the pool currently holds.",
		func() float64 { return float64(p.Stat().TotalConns()) })
	m.reg.GaugeFunc("ragmux_db_pool_idle_conns", "Idle connections in the pool.",
		func() float64 { return float64(p.Stat().IdleConns()) })
	m.reg.GaugeFunc("ragmux_db_pool_acquired_conns", "Connections currently checked out.",
		func() float64 { return float64(p.Stat().AcquiredConns()) })
	m.reg.GaugeFunc("ragmux_db_pool_max_conns", "Configured pool ceiling (DB_MAX_CONNS).",
		func() float64 { return float64(p.Stat().MaxConns()) })
	m.reg.GaugeFunc("ragmux_db_pool_acquires_total", "Successful acquires since start.",
		func() float64 { return float64(p.Stat().AcquireCount()) })
	m.reg.GaugeFunc("ragmux_db_pool_empty_acquires_total",
		"Acquires that had to wait because the pool was empty; a rising rate means DB_MAX_CONNS is the bottleneck.",
		func() float64 { return float64(p.Stat().EmptyAcquireCount()) })
}

// TraceCounters is the pair of counters internal/tracing keeps. It is an
// interface so this package does not depend on the tracer's type, and so a
// disabled tracer needs no special case.
type TraceCounters interface {
	Dropped() uint64
	ExportFailures() uint64
}

// RegisterTracing exports the exporter's own failure counters, so a
// collector that is refusing spans is visible in the same scrape as
// everything else.
//
// They are registered as scrape-time gauges because the registry has no
// counter-valued function collector; the values are monotonic all the same
// and rate() over them behaves.
func (m *Metrics) RegisterTracing(tr TraceCounters) {
	if m == nil || tr == nil {
		return
	}
	m.reg.GaugeFunc("ragmux_tracing_spans_dropped_total",
		"Spans discarded because the export queue was full.",
		func() float64 { return float64(tr.Dropped()) })
	m.reg.GaugeFunc("ragmux_tracing_export_failures_total",
		"Span export attempts abandoned after their retry.",
		func() float64 { return float64(tr.ExportFailures()) })
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
