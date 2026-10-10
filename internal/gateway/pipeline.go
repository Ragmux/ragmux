package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ragmux/ragmux/internal/httpx"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
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
	// name is gen_ai.operation.name on the request span.
	name string
	// span is the name of the span every downstream span hangs beneath.
	span string
	// scope is the gateway scope the key must hold, and what names the endpoint
	// in the insufficient_scope message.
	scope, what string
}

// chatOperation is POST /v1/chat/completions.
var chatOperation = operation{
	name:  "chat",
	span:  "gateway.chat_completion",
	scope: store.ScopeChat,
	what:  "chat completions",
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
		log:       g.Log.With("project", p.ID),
		principal: pr, project: p, projectID: strconv.FormatInt(p.ID, 10),
		subject: limits.Subject{Project: p},
		obs:     &requestObs{},
	}
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

// resolveRoute picks the model connections that serve the call. Today that is
// always the project's own connection; this is the seam where routing across
// several connections plugs in: it fills c.targets, invoke walks them, and
// everything else reads c.conn and never the project's connection id.
//
// The contract: when it returns true, c.targets holds at least one connection
// and c.conn is the first of them. When no connection can serve the call it
// answers the client itself and returns false, so no later step ever sees an
// empty route or a nil c.conn.
func (g *Gateway) resolveRoute(c *call) bool {
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
	c.principal.attribute(rec)
	if err := g.Store.InsertRequestLog(c.ctx(), rec); err != nil {
		c.log.Error("write request log", "err", err)
	}
	writeLimitError(c.w, decision)
	return false
}

// openRecord starts the request log row record will write. It is called once
// the call is past checkLimits and the endpoint's own hook has run, so a
// request that never reached the upstream leaves no row of this kind. fill, if
// not nil, sets the endpoint's own columns on the fresh row.
func (g *Gateway) openRecord(c *call, streamed bool, fill func(*store.RequestLog)) {
	c.rec = &store.RequestLog{ProjectID: c.project.ID, ModelName: c.conn.ModelName, Streamed: streamed}
	c.principal.attribute(c.rec)
	if fill != nil {
		fill(c.rec)
	}
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
// A failed attempt ends the walk unless continuesPastFailure says the failure
// may be recovered at the next target. That predicate is the extension point
// for retry and fallback (ADR-020 D7, Faz 06) and it answers false for every
// error today, so exactly one attempt is made whatever resolveRoute returns:
// repeating a non-idempotent POST, or moving to another target, is a decision
// per failure class that has not been taken yet.
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
	var err error
	for _, target := range c.targets {
		c.conn = target
		if err = up.attempt(c, target); err == nil {
			return
		}
		if !continuesPastFailure(err) {
			break
		}
	}
	g.failInvoke(c, err)
}

// continuesPastFailure reports whether a failed attempt may be followed by an
// attempt at the next target. Faz 06 classifies err here (status, network
// error, whether any byte was written); until then no failure continues.
func continuesPastFailure(error) bool { return false }

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
	g.priceRequest(conn, rec)
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
