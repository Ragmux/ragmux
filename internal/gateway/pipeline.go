package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/httpx"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/tracing"
)

// The request pipeline every /v1 endpoint that spends upstream capacity goes
// through. A handler runs these steps in this order and stops at the first one
// that has already answered the client:
//
//	authenticate   scope, project requirement, request span, call state
//	(decode)       the endpoint's own step: read and validate its request body
//	resolveRoute   which model connections serve the request (c.targets)
//	checkLimits    rate limits and budgets; a denial is answered and logged here
//	(prepare)      an optional endpoint hook that shapes what goes upstream;
//	               chat's is prepareChat (system prompt and RAG)
//	openRecord     opens the request log row; must run before record is deferred
//	defer record   the chokepoint: request log, usage, cost, metrics, span
//	invoke         tries the targets in order through the endpoint's upstreamCall
//
// The body is decoded between authenticate and resolveRoute because that is
// where the chat handler always read it, so the order in which a client sees
// errors is the one it has always seen. record dereferences c.rec and c.conn,
// so a handler defers it only after openRecord and resolveRoute have run; a
// request answered earlier (a refusal, a limit denial) never reaches it.
//
// A new endpoint supplies an operation, its own decode and prepare steps and
// an upstreamCall; the other steps are shared.

// operation describes the endpoint a call belongs to. It is the only thing a
// new endpoint has to say about itself to reuse the steps.
type operation struct {
	// name is gen_ai.operation.name on the request span, and the first word of
	// its name once the model is known. Reranking has no registered value in
	// the GenAI convention, so "rerank" is a custom one.
	name string
	// span is the name of the span every downstream span hangs beneath.
	span string
	// scope is the gateway scope the key must hold, and what names the endpoint
	// in the insufficient_scope message.
	scope, what string
	// endpoint is the request_logs.endpoint value of the rows it writes.
	endpoint string
}

// chatOperation is POST /v1/chat/completions.
var chatOperation = operation{
	name:  "chat",
	span:  "gateway.chat_completion",
	scope: store.ScopeChat,
	what:  "chat completions",

	endpoint: "chat.completions",
}

// embeddingsOperation is POST /v1/embeddings.
var embeddingsOperation = operation{
	name:  "embeddings",
	span:  "gateway.embeddings",
	scope: store.ScopeEmbeddings,
	what:  "embeddings",

	endpoint: "embeddings",
}

// retrieveOperation is POST /v1/retrieve.
var retrieveOperation = operation{
	name:  "retrieval",
	span:  "gateway.retrieve",
	scope: store.ScopeRetrieve,
	what:  "retrieval",

	endpoint: "retrieve",
}

// rerankOperation is POST /v1/rerank.
var rerankOperation = operation{
	name:  "rerank",
	span:  "gateway.rerank",
	scope: store.ScopeRerank,
	what:  "reranking",

	endpoint: "rerank",
}

// sideCharge is what one side call spent, with the connection that did the
// work. A side call is a call made outside the upstream attempts that the
// endpoint's own usage figures do not include: the rerank inside a chat or
// retrieve request. Its tokens count toward limits and usage on the row, but
// the row's connection is not the one that did the work, so record prices the
// charge on its own, with the price of providerType/model (ADR-021), and adds
// the result to the request's cost.
type sideCharge struct {
	prompt, completion int
	// searchUnits is what a rerank API billed per search unit (Cohere).
	searchUnits int
	estimated   bool
	// providerType and model name the connection that did the work; empty
	// when it is unknown, which prices the charge at zero.
	providerType, model string
	// inRow reports that the endpoint already added the tokens to the row
	// (/v1/retrieve does, so its response can show them); record then leaves
	// them alone when it adds the rest.
	inRow bool
}

// isZero reports whether the charge spent nothing.
func (s sideCharge) isZero() bool {
	return s.prompt == 0 && s.completion == 0 && s.searchUnits == 0
}

// llmCharge is the charge of an LLM rerank: the chat calls counted by spend,
// run on the connection providerType/model. A nil spend, a search without an
// LLM rerank, spent nothing.
func llmCharge(spend *rerankUsageProvider, providerType, model string, inRow bool) sideCharge {
	if spend == nil {
		return sideCharge{}
	}
	return sideCharge{prompt: spend.prompt, completion: spend.completion, estimated: spend.estimated,
		providerType: providerType, model: model, inRow: inRow}
}

// apiCharge is the charge of an API rerank call (Cohere, Voyage): the tokens
// and search units it spent, on the connection the reranker names.
func apiCharge(spend *rag.RerankSpend, inRow bool) sideCharge {
	if spend.IsZero() {
		return sideCharge{}
	}
	return sideCharge{prompt: spend.PromptTokens, completion: spend.CompletionTokens,
		searchUnits: spend.SearchUnits, estimated: spend.Estimated,
		providerType: spend.ProviderType, model: spend.Model, inRow: inRow}
}

// errSpanCondense is what the rag.condense span records for a failed call: a
// fixed value, never the error, which can quote the upstream.
var errSpanCondense = errors.New("condense_failed")

// condenseQuery is the "condense" query strategy (ADR-024): it asks a chat
// model to turn the last turns of msgs into one standalone search query and
// returns it. It returns fallback -- the last user message -- when there is
// nothing to condense (one user message) and whenever the call fails, times
// out or answers with something unusable; the request goes on either way and
// the client is told nothing (PRD rule 8).
//
// The model is the project's query_model_connection_id, else the first route
// target, the one whose adapter (prov) the LLM rerank uses. The call is a
// single attempt: no retry, no fallback, no circuit breaker. Its tokens are a
// side charge priced with the connection that did the work (ADR-021), counted
// even when the reply turned out to be unusable.
//
// Neither the prompt nor the reply reaches a log line, a span or a metric
// label (PRD rule 10): they carry the outcome and the connection only.
func (g *Gateway) condenseQuery(c *call, prov provider.Provider, msgs []provider.Message, fallback string) string {
	prompt, ok := rag.BuildCondensePrompt(msgs, c.project.HistoryTurnsForQuery)
	if !ok {
		return fallback
	}
	// prepareChat runs before invoke, so c.conn is still the first target.
	work, conn := prov, c.conn
	if id := c.project.QueryModelConnectionID; id != nil {
		work, conn = g.queryModelAdapter(c, *id)
	}

	ctx, span := g.Tracer.Start(c.ctx(), "rag.condense", tracing.KindClient)
	defer span.End()
	if work == nil {
		// The adapter could not be built: nothing was sent.
		// conn is nil only when the project's query model could not be loaded;
		// the configured id then names it.
		id := int64(0)
		switch {
		case conn != nil:
			id = conn.ID
		case c.project.QueryModelConnectionID != nil:
			id = *c.project.QueryModelConnectionID
		}
		c.log.Warn("query condense unavailable; searching with the last user message",
			"outcome", obs.CondenseError, "connection", id)
		g.finishCondense(span, conn, obs.CondenseError)
		return fallback
	}
	spend := &rerankUsageProvider{Provider: work}
	query, err := rag.Condense(ctx, spend, conn.ModelName, prompt, g.condenseTimeout(c))
	c.addSideCharge(llmCharge(spend, conn.ProviderType, conn.ModelName, false))
	outcome := rag.CondenseOutcome(err)
	g.finishCondense(span, conn, outcome)
	if err != nil {
		attrs := []any{"outcome", outcome, "connection", conn.ID}
		var pe *provider.Error
		if errors.As(err, &pe) {
			// The status only: the message can quote the prompt back.
			attrs = append(attrs, "status", pe.Status)
		}
		c.log.Warn("query condense failed; searching with the last user message", attrs...)
		return fallback
	}
	return query
}

// finishCondense counts the call and closes its span's attributes; conn is nil
// when the project's query model could not be loaded.
func (g *Gateway) finishCondense(span *tracing.Span, conn *store.ModelConnection, outcome string) {
	g.Metrics.RecordQueryCondense(outcome)
	if !span.IsRecording() {
		return
	}
	if conn != nil {
		span.SetAttributes(tracing.String("gen_ai.provider.name", tracing.GenAIProviderName(conn.ProviderType)))
	}
	span.SetAttributes(tracing.String("ragmux.condense.outcome", outcome))
	if outcome == obs.CondenseOK {
		span.SetStatusOK()
	} else {
		span.RecordError(errSpanCondense)
	}
}

// queryModelAdapter loads the project's query_model_connection_id and builds
// its adapter. A nil adapter means it cannot be used; the connection may be
// nil too, when it could not be loaded at all.
func (g *Gateway) queryModelAdapter(c *call, id int64) (provider.Provider, *store.ModelConnection) {
	conn, err := g.Store.GetConnection(c.ctx(), id)
	if err != nil {
		c.log.Error("query model connection", "connection", id, "err", err)
		return nil, nil
	}
	prov, err := g.Providers(conn)
	if err != nil {
		// The factory error names the provider type or URL: log only.
		c.log.Error("query model provider setup", "connection", id, "err", err)
		return nil, conn
	}
	return prov, conn
}

// condenseTimeout is the time budget of one condense call: the fixed one
// (CondenseTimeout, 5 s by default), cut to what is left of the request's
// upstream budget. The budget starts when invoke does, so before that it is
// the whole UpstreamTimeout.
func (g *Gateway) condenseTimeout(c *call) time.Duration {
	limit := g.CondenseTimeout
	if limit <= 0 {
		limit = rag.DefaultCondenseTimeout
	}
	left := c.budgetLeft()
	if left == 0 {
		left = g.UpstreamTimeout
	}
	if left > 0 && left < limit {
		limit = left
	}
	return limit
}

// call is the state one request carries from step to step. Nothing in it is
// specific to an endpoint's request or response body.
type call struct {
	op    operation
	start time.Time
	w     http.ResponseWriter
	// r carries the traced context: the request's own context replaced by the
	// one the request span lives in.
	r    *http.Request
	span *tracing.Span
	log  *slog.Logger

	principal *principal
	project   *store.Project
	projectID string
	subject   limits.Subject

	// requestID is the id returned in X-Request-Id; requestedModel is the
	// model string the client sent, set by the endpoint's decode step. Both
	// end up on the request log row.
	requestID      string
	requestedModel string
	// routeName is the route that resolved the call; empty for the implicit
	// route (the project's own connection), which logs NULL.
	routeName string

	// targets is set by resolveRoute: the connections that may serve the call,
	// in the order invoke tries them. conn is the one serving it right now --
	// the first target until invoke moves on -- and what record reports.
	targets []*store.ModelConnection
	conn    *store.ModelConnection
	// decision is set by checkLimits; record reads Reserved from it.
	decision limits.Decision
	// rec and obs are the facts record writes out. rec exists once openRecord
	// has run.
	rec *store.RequestLog
	obs *requestObs
	// deadline ends the upstream budget (Gateway.UpstreamTimeout), set by
	// invoke when the budget is limited; the zero time is no limit. Attempts
	// read it through attemptContext and budgetLeft.
	deadline time.Time
	// sideCharges is what calls made outside the upstream attempts spent, such
	// as the rerank of a chat request's retrieval. fillUsage overwrites the
	// row's counts when the attempt finishes, so record adds the tokens of the
	// ones that are not already in the row on top, then prices each charge
	// with its own connection.
	sideCharges []sideCharge
	// searchUnits is what the call itself billed in search units, set by the
	// /v1/rerank attempt that went through a Cohere connection.
	searchUnits int
}

// addSideCharge records a side call's spend; one that spent nothing is dropped.
func (c *call) addSideCharge(ch sideCharge) {
	if !ch.isZero() {
		c.sideCharges = append(c.sideCharges, ch)
	}
}

func (c *call) ctx() context.Context { return c.r.Context() }

// authenticate takes the identity the router middleware (identify) resolved
// and enforces what the endpoint needs of it: the scope, then that the key
// ended up with a project. It then opens the request span and builds the call.
//
// The key lookup and project selection stay in the middleware because it also
// guards routes that do not reach a pipeline (the model listings) and answers
// 401 for unknown paths; running them again here would be a second lookup per
// request. The span starts last, so a refused request never has one.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, op operation) (*call, bool) {
	pr := principalFrom(r.Context())
	if !checkScope(w, pr, op.scope, op.what) || !checkProject(w, pr) {
		return nil, false
	}
	p := projectFrom(r.Context())
	if p == nil {
		// identify always sets one for a request that got this far.
		writeError(w, http.StatusInternalServerError, "server_error", "project lookup failed")
		return nil, false
	}
	start := time.Now()
	// Every span below -- rag.retrieve, rag.embed_query, rag.rerank and the
	// provider client spans -- hangs beneath this one, so the traced context
	// replaces the request's.
	ctx, span := g.Tracer.Start(r.Context(), op.span, tracing.KindInternal)
	c := &call{
		op: op, start: start, w: w, r: r.WithContext(ctx), span: span,
		principal: pr, project: p, projectID: strconv.FormatInt(p.ID, 10),
		subject:   limits.Subject{Project: p},
		requestID: requestIDFrom(r.Context()),
		obs:       &requestObs{},
	}
	c.log = callLogger(g.Log, p.ID, middleware.GetReqID(r.Context()), c.requestID)
	if pr != nil {
		c.subject = pr.subject()
	}
	return c, true
}

// readBody reads the request body up to MaxBodyBytes. The read deadline is
// cleared once the body is in memory so streaming responses are never cut
// short by it. It answers the client itself on failure.
func (g *Gateway) readBody(c *call) ([]byte, bool) {
	max := g.MaxBodyBytes
	if max <= 0 {
		max = 4 << 20
	}
	httpx.Deadline(c.w, bodyReadDeadline)
	body, err := io.ReadAll(io.LimitReader(c.r.Body, max+1))
	httpx.Deadline(c.w, 0)
	if err != nil || int64(len(body)) > max {
		writeError(c.w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return nil, false
	}
	return body, true
}

// resolveRoute picks the model connections that serve the call, in the order
// invoke tries them. model is the client's model string.
//
// A project without chat routes keeps the behaviour it always had: its own
// connection serves every request and the model string is only echoed. A
// project with chat routes resolves the string as ADR-018 sets out (a route
// name, else the implicit route, else model_not_found), and a route yields its
// targets ordered by priority and weighted choice (ADR-017). Each target is an
// effective connection: a copy, with the target's model override applied.
//
// The contract: when it returns true, c.targets holds at least one connection
// and c.conn is the first of them. When no connection can serve the call it
// answers the client itself and returns false, so no later step ever sees an
// empty route or a nil c.conn.
func (g *Gateway) resolveRoute(c *call, model string) bool {
	targets, name, routed, matched, err := g.resolveChatTargets(c, model)
	switch {
	case err != nil:
		c.log.Error("resolve route", "err", err)
		return noRoute(c)
	case routed && !matched:
		modelNotFound(c, model)
		return false
	case routed:
		c.routeName = name
		return acceptTargets(c, targets)
	}
	conn, err := g.Store.GetConnection(c.ctx(), c.project.ModelConnectionID)
	if err != nil {
		return noRoute(c)
	}
	return acceptTargets(c, []*store.ModelConnection{conn})
}

// acceptTargets installs the routed targets on the call, or refuses the call
// when there are none.
func acceptTargets(c *call, targets []*store.ModelConnection) bool {
	if len(targets) == 0 {
		return noRoute(c)
	}
	c.targets = targets
	c.conn = targets[0]
	if c.span.IsRecording() {
		c.nameRequestSpan()
	}
	return true
}

// noRoute answers a call no connection can serve.
func noRoute(c *call) bool {
	writeError(c.w, http.StatusInternalServerError, "server_error", "model connection unavailable")
	return false
}

// checkLimits runs before any retrieval so throttled clients do not cost an
// embedding call. estimate is the endpoint's token estimate for the request;
// streamed is only what the request log row of a denial says. A denial is
// answered and logged here: nothing was sent upstream, so the row costs
// nothing, and it does not reach record.
func (g *Gateway) checkLimits(c *call, estimate int, streamed bool) bool {
	if g.Limiter == nil {
		return true
	}
	decision, err := g.Limiter.CheckSubject(c.ctx(), c.subject, estimate)
	if err != nil {
		c.log.Error("limit check", "err", err)
		writeError(c.w, http.StatusInternalServerError, "server_error", "limit check failed")
		return false
	}
	c.decision = decision
	setLimitHeaders(c.w.Header(), decision)
	if decision.Allowed {
		return true
	}
	// Reason is one of the limits.Reason* constants, a closed set.
	g.Metrics.RecordLimitDenied(c.projectID, decision.Reason)
	rec := &store.RequestLog{ProjectID: c.project.ID, ModelName: c.conn.ModelName, Streamed: streamed,
		StatusCode: http.StatusTooManyRequests, Error: decision.Reason, CostSource: store.CostSourceNone,
		LatencyMs: time.Since(c.start).Milliseconds()}
	// Nothing went upstream, so there is no attempt to count.
	c.stamp(rec, 0)
	c.principal.attribute(rec)
	if err := g.Store.InsertRequestLog(c.ctx(), rec); err != nil {
		c.log.Error("write request log", "err", err)
	}
	writeLimitError(c.w, decision)
	return false
}

// checkContextLimits is the second half of the chat token-limit check. The
// first (checkLimits) runs before retrieval and sees only the client's
// messages and the project prompt; the retrieved context can add thousands of
// tokens, so once it is known the per-minute token limit is checked again
// against the whole prompt. estimate is the figure checkLimits used.
//
// This is a second, request-slot-free check rather than a move of retrieval
// ahead of checkLimits: the first check keeps shielding the embedding and
// rerank calls from a client that is already over RPM or a budget, and a
// repeated CheckSubject would take a second RPM slot. It runs after
// openRecord, so a denial leaves the row the retrieval's own spend (rerank
// tokens) is recorded on and the request is counted once, as a 429, like any
// denied request. Nothing has gone upstream, so the attempt count is zero.
func (g *Gateway) checkContextLimits(c *call, estimate int, contextBlock string) bool {
	if g.Limiter == nil || contextBlock == "" {
		return true
	}
	total := estimate + (len(contextBlock)+3)/4
	decision, err := g.Limiter.CheckTokens(c.ctx(), c.subject, c.decision, total)
	if err != nil {
		c.log.Error("limit check", "err", err)
		c.rec.StatusCode, c.rec.Error = http.StatusInternalServerError, "limit check failed"
		c.rec.Attempt = 0
		writeError(c.w, http.StatusInternalServerError, "server_error", "limit check failed")
		return false
	}
	if decision.Allowed {
		return true
	}
	g.Metrics.RecordLimitDenied(c.projectID, decision.Reason)
	// The retrieval was thrown away: no context went to the model, so neither
	// the headers nor the row (which feeds the RAG-use figures) may say it did.
	// Its rerank spend stays on the row as a side charge.
	c.w.Header().Del("x-ragmux-rag-hits")
	c.w.Header().Del("x-ragmux-rag-sources")
	c.rec.RAGUsed, c.rec.RAGHits = false, 0
	c.rec.StatusCode, c.rec.Error = http.StatusTooManyRequests, decision.Reason
	c.rec.Attempt = 0
	writeLimitError(c.w, decision)
	return false
}

// callLogger is the logger a call's lines go through. req_id is the id
// obs.RequestID generated: the access log, the span and the metrics carry that
// one, so a gateway line joins to them on it. When the client's own
// X-Request-Id was used (requestID differs from generated), it is logged
// beside it as client_req_id, the value the client and request_logs know the
// request by. generated is empty when no obs.RequestID sits in front of the
// router; requestID is then the only id there is.
func callLogger(base *slog.Logger, projectID int64, generated, requestID string) *slog.Logger {
	if generated == "" {
		generated = requestID
	}
	log := base.With("project", projectID, "req_id", generated)
	if requestID != generated {
		log = log.With("client_req_id", requestID)
	}
	return log
}

// openRecord starts the request log row record will write. It is called once
// the call is past checkLimits and the endpoint's own hook has run, so a
// request that never reached the upstream leaves no row of this kind. fill, if
// not nil, sets the endpoint's own columns on the fresh row.
func (g *Gateway) openRecord(c *call, streamed bool, fill func(*store.RequestLog)) {
	c.rec = &store.RequestLog{ProjectID: c.project.ID, ModelName: c.conn.ModelName, Streamed: streamed}
	// invoke overwrites attempt, fallback_from and model_name once it knows
	// which attempt answered.
	c.stamp(c.rec, 1)
	c.principal.attribute(c.rec)
	if fill != nil {
		fill(c.rec)
	}
}

// stamp writes the identity columns every request log row carries: the
// request id, the endpoint, the model the client asked for and the route that
// resolved it.
func (c *call) stamp(rec *store.RequestLog, attempt int) {
	rec.RequestID = c.requestID
	rec.Endpoint = c.op.endpoint
	rec.RequestedModel = c.requestedModel
	rec.Route = c.routeName
	rec.Attempt = attempt
}

// upstreamCall is the endpoint-specific half of invoke: one attempt at one
// target. Everything an endpoint says about an upstream call -- the provider
// method, the response shape, the usage it reports -- lives behind it; the
// common handling of a failed attempt stays in invoke.
//
// The contract an implementation keeps:
//
//   - It sets c.obs.upstream to the time the provider call took.
//   - It returns nil once the call is settled: the client has been answered
//     and c.rec carries the status, error and token counts. A stream is settled
//     whatever happens to it, because it answers the client itself once its
//     headers are out.
//   - It returns an error only when nothing has reached the client and c.rec
//     is untouched. invoke classifies that error and answers for it. That is
//     the only state in which an attempt may safely be repeated at another
//     target, which is why the loop lives in invoke and not in the endpoint.
type upstreamCall interface {
	attempt(c *call, conn *store.ModelConnection) error
}

// invoke makes the upstream call: it tries the call's targets in order and
// answers the client from the first settled attempt, or from the failure that
// ends the walk. c.rec and c.obs are filled for record to turn into the log row
// and metrics.
//
// A failed attempt is classified (classifyAttempt, ADR-020): a transient
// failure is repeated on the same target up to Gateway.MaxRetries times, a
// failure that is the target's own moves the walk to the next target, and one
// that belongs to the request ends it. All attempts share the upstream budget
// (Gateway.UpstreamTimeout): each is cut off when it runs out, and a backoff
// that would not fit in what is left is not waited. The client is answered
// from the last attempt's error, so a route behaves like its last target when
// every one of them fails.
//
// Retry and fallback are only possible because an attempt that returns an
// error has written nothing (see upstreamCall); the request limit check,
// system prompt and RAG ran once before, and usage is recorded once after.
//
// Afterwards the request log row says what happened: attempt is the number of
// upstream calls made, fallback_from the first target's connection when the
// walk moved on, and model_name the model of the target that answered.
func (g *Gateway) invoke(c *call, up upstreamCall) {
	if len(c.targets) == 0 {
		// resolveRoute guarantees at least one target; this only keeps a
		// future caller that skips it from reaching a nil error below.
		c.rec.StatusCode, c.rec.Error = http.StatusInternalServerError, "no upstream target for the request"
		c.obs.errType = "server_error"
		c.log.Error("no upstream target")
		writeProviderError(c.w, http.StatusInternalServerError, &provider.Error{
			Status: http.StatusInternalServerError, Type: "server_error", Message: c.rec.Error})
		return
	}
	if g.UpstreamTimeout > 0 {
		c.deadline = time.Now().Add(g.UpstreamTimeout)
	}
	first := c.targets[0]
	attempts := 0
	defer func() {
		c.rec.Attempt = attempts
		c.rec.ModelName = c.conn.ModelName
		if c.conn != first {
			id := first.ID
			c.rec.FallbackFrom = &id
		}
	}()

	var err error
	forced := false      // every candidate was open: the primary is tried once, without retries
	fallbackReason := "" // a fallback is counted once the next target is really tried
	for i, target := range c.targets {
		var probe probeToken
		if !forced {
			var ok bool
			if ok, probe = g.Breakers.allow(target); !ok {
				if i < len(c.targets)-1 || err != nil {
					continue // an open target is skipped (ADR-020)
				}
				// Every candidate is open: the primary goes once, with no
				// retry and no fail-fast answer.
				i, target, forced, probe = 0, c.targets[0], true, 0
			}
		}
		if fallbackReason != "" {
			g.Metrics.RecordUpstreamFallback(fallbackReason)
		}
		c.conn = target
		var cl attemptClass
		for retries := 0; ; retries++ {
			attempts++
			err = g.runAttempt(c, up, target, first, attempts, retries)
			g.Breakers.report(target, probe, outcomeOf(c, err))
			if err == nil {
				return
			}
			var tc *targetConfigError
			if errors.As(err, &tc) {
				// No call left the gateway, so it is not an attempt: attempt
				// counts upstream calls only (zero when none was made).
				attempts--
			}
			cl = classifyAttempt(err)
			g.logAttempt(c, target, attempts, cl, err)
			if c.ctx().Err() != nil || c.budgetSpent() || !cl.retry || retries >= g.MaxRetries || forced || g.Breakers.isOpen(target.ID) {
				break
			}
			wait := retryWait(retries, g.rng()(), cl.retryAfter)
			if !c.deadline.IsZero() && wait >= time.Until(c.deadline) {
				// Waiting would eat the rest of the budget and leave nothing
				// for the attempt the wait is for.
				break
			}
			g.Metrics.RecordUpstreamRetry(cl.reason)
			if g.sleep(c.ctx(), wait) != nil {
				break
			}
		}
		if i == len(c.targets)-1 || !cl.fallback || forced || c.ctx().Err() != nil || c.budgetSpent() {
			break
		}
		fallbackReason = cl.reason
	}
	g.failInvoke(c, err)
}

// runAttempt makes one upstream call of the walk inside its own span, so a
// trace shows every retry and every fallback as a separate child of the
// request span rather than folding them into one bar. n is the attempt's
// number (as counted by invoke), retries how many times this target was
// already tried.
//
// The attempt's context replaces the call's for its duration: the provider
// spans (provider.chat, rag.embed_query, ...) are started from c.ctx() inside
// up.attempt and so hang beneath the attempt, not beside it. While tracing is
// off, or the trace is not sampled, nothing here costs more than the branch.
func (g *Gateway) runAttempt(c *call, up upstreamCall, target, first *store.ModelConnection, n, retries int) error {
	if !c.span.IsRecording() {
		return up.attempt(c, target)
	}
	outer := c.r
	ctx, span := g.Tracer.Start(outer.Context(), "gateway.attempt", tracing.KindInternal)
	c.r = outer.WithContext(ctx)
	err := up.attempt(c, target)
	c.r = outer

	// A target_config failure never left the gateway, so it is not an
	// attempt and has no number: invoke does not count it, and giving it one
	// would hand the same number to the next real attempt.
	var tc *targetConfigError
	if !errors.As(err, &tc) {
		span.SetAttributes(tracing.Int("ragmux.attempt", n))
	}
	span.SetAttributes(
		tracing.Int("ragmux.retry", retries),
		tracing.Int64("ragmux.connection.id", target.ID),
		tracing.String("gen_ai.operation.name", c.op.name),
		tracing.String("gen_ai.provider.name", tracing.GenAIProviderName(target.ProviderType)),
		tracing.String("gen_ai.request.model", target.ModelName),
		tracing.Bool("ragmux.fallback", target != first),
	)
	if c.routeName != "" {
		span.SetAttributes(tracing.String("ragmux.route", c.routeName))
	}
	if target != first {
		span.SetAttributes(tracing.Int64("ragmux.fallback.from_connection", first.ID))
	}
	if err != nil {
		// The class is a closed set (the obs.Upstream* reasons, or
		// "terminal"); the error's own text can quote the request back and
		// stays out of the span.
		class := classifyAttempt(err).reason
		if class == "" {
			class = "terminal"
		}
		span.SetAttributes(tracing.String("error.type", class))
		span.RecordError(errors.New(class))
	} else {
		span.SetStatusOK()
	}
	span.End()
	return err
}

// nameRequestSpan gives the request span its GenAI name once the model is
// known. The span opens before routing (it has to enclose the refusals that
// follow authentication), so until then it carries the gateway.* name from its
// operation, which is also what a request refused before a model was chosen
// keeps.
//
// Chat, embeddings and rerank follow the convention's "{operation} {model}".
// Retrieval names the data source instead, as the convention does for the
// operation it registers: its model is only the embedding model of the store,
// and the store is what the search is about.
func (c *call) nameRequestSpan() {
	switch {
	case c.conn == nil:
		return
	case c.op.name == retrieveOperation.name:
		if c.project.RAGStoreID != nil {
			c.span.SetName(c.op.name + " " + strconv.FormatInt(*c.project.RAGStoreID, 10))
		}
	default:
		c.span.SetName(c.op.name + " " + c.conn.ModelName)
	}
}

// logAttempt records one failed attempt. The message is the adapter's
// redacted one, never the raw error: an upstream's text is not trusted.
func (g *Gateway) logAttempt(c *call, target *store.ModelConnection, attempt int, cl attemptClass, err error) {
	var msg string
	var pe *provider.Error
	if errors.As(err, &pe) {
		msg = pe.Message
	} else {
		msg = provider.Redact(err.Error())
	}
	class := cl.reason
	if class == "" {
		class = "terminal"
	}
	c.log.Warn("upstream attempt failed", "connection", target.ID, "attempt", attempt, "class", class,
		"retry", cl.retry, "fallback", cl.fallback, "msg", msg)
}

// failInvoke answers the client for a failed upstream call.
func (g *Gateway) failInvoke(c *call, err error) {
	if c.ctx().Err() != nil {
		// The client went away while the upstream call was running; the
		// provider already saw the cancellation through the context.
		c.rec.StatusCode, c.rec.Error = statusClientClosed, errClientClosed
		return
	}
	status, pe := providerError(err)
	c.rec.StatusCode, c.rec.Error, c.obs.errType = status, pe.Message, pe.Type
	c.log.Warn("upstream error", "status", status, "msg", pe.Message)
	writeProviderError(c.w, status, pe)
}

// record is the one place the success, error, mid-stream-failure and 499 paths
// all pass with final token counts. It is deferred by the handler once the
// row is open, and writes the request log, charges the limiter, touches the
// key and reports metrics and span attributes. Cost is priced here rather than
// in fillUsage, which stays a pure function.
func (g *Gateway) record(c *call) {
	rec, conn, obsv := c.rec, c.conn, c.obs
	rec.LatencyMs = time.Since(c.start).Milliseconds()
	// Side tokens count toward limits and usage, so they go on the row; they
	// are priced apart, with the price of the connection that did the work,
	// because the row's connection is not that one.
	var sidePrompt, sideCompletion int
	for _, ch := range c.sideCharges {
		if !ch.inRow {
			rec.PromptTokens += ch.prompt
			rec.CompletionTokens += ch.completion
			rec.Estimated = rec.Estimated || ch.estimated
		}
		sidePrompt += ch.prompt
		sideCompletion += ch.completion
	}
	rec.PromptTokens -= sidePrompt
	rec.CompletionTokens -= sideCompletion
	g.priceRequest(conn, rec, c.searchUnits)
	rec.PromptTokens += sidePrompt
	rec.CompletionTokens += sideCompletion
	g.priceSideCharges(rec, c.sideCharges)
	if err := g.Store.InsertRequestLog(context.Background(), rec); err != nil {
		c.log.Error("write request log", "err", err)
	}
	if g.Limiter != nil {
		// The minute request slot was already reserved by Check when an
		// RPM limit is set; otherwise count the request here.
		if err := g.Limiter.RecordSubject(context.Background(), c.subject, rec.PromptTokens, rec.CompletionTokens, !c.decision.Reserved); err != nil {
			c.log.Error("record usage", "err", err)
		}
	}
	if pr := c.principal; pr != nil && pr.key != nil {
		// At most one write per key per minute; the statement itself
		// throttles, so a busy key does not rewrite its row per request.
		if err := g.Store.TouchAPIKeyUsed(context.Background(), pr.key.ID); err != nil {
			c.log.Error("touch api key", "key", pr.key.ID, "err", err)
		}
	}
	// The model label is conn.ModelName, never the model string the client
	// sent: that value is attacker-controlled and would make the label
	// unbounded. The project label is the numeric id.
	g.Metrics.RecordGateway(obs.GatewayRequest{
		Project: c.projectID, Model: conn.ModelName, Provider: conn.ProviderType,
		Status: rec.StatusCode, Streamed: rec.Streamed,
		Duration: time.Since(c.start), Upstream: obsv.upstream, TimeToFirstToken: obsv.ttft,
		PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens,
		Estimated: rec.Estimated, CostUSD: rec.CostUSD, ErrorType: obsv.errType,
		ClientDisconnected: rec.StatusCode == statusClientClosed,
		Unpriced:           rec.CostSource == store.CostSourceNone,
	})
	if c.span.IsRecording() {
		c.span.SetAttributes(
			tracing.Int64("ragmux.project.id", c.project.ID),
			tracing.String("gen_ai.operation.name", c.op.name),
			tracing.String("gen_ai.provider.name", tracing.GenAIProviderName(conn.ProviderType)),
			tracing.String("gen_ai.request.model", conn.ModelName),
			tracing.Bool("ragmux.stream", rec.Streamed),
			tracing.Int("gen_ai.usage.input_tokens", rec.PromptTokens),
			tracing.Int("gen_ai.usage.output_tokens", rec.CompletionTokens),
			tracing.Int("gen_ai.usage.cache_read.input_tokens", rec.CachedPromptTokens),
		)
		// The target that answered may not be the one the span was named for.
		c.nameRequestSpan()
		if c.op.name == retrieveOperation.name && c.project.RAGStoreID != nil {
			c.span.SetAttributes(tracing.String("gen_ai.data_source.id", strconv.FormatInt(*c.project.RAGStoreID, 10)))
		}
		if c.routeName != "" {
			c.span.SetAttributes(tracing.String("ragmux.route", c.routeName))
		}
		if rec.Attempt > 0 {
			c.span.SetAttributes(tracing.Int("ragmux.attempts", rec.Attempt))
		}
		if rec.FallbackFrom != nil {
			c.span.SetAttributes(tracing.Int64("ragmux.fallback.from_connection", *rec.FallbackFrom))
		}
		if obsv.responseModel != "" {
			c.span.SetAttributes(tracing.String("gen_ai.response.model", obsv.responseModel))
		}
		if len(obsv.finishReasons) > 0 {
			c.span.SetAttributes(tracing.StringSlice("gen_ai.response.finish_reasons", obsv.finishReasons))
		}
		if rec.StatusCode < 500 {
			c.span.SetStatusOK()
		}
	}
}
