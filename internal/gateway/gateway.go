// Package gateway serves the OpenAI-compatible /v1 API backed by project API
// keys, applying the RAG pipeline and recording metrics.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/tracing"
)

// ProviderFactory builds a chat adapter for a model connection.
type ProviderFactory func(conn *store.ModelConnection) (provider.Provider, error)

// Gateway holds dependencies for the /v1 routes.
type Gateway struct {
	Store     *store.Store
	Providers ProviderFactory
	Retriever *rag.Retriever
	Log       *slog.Logger
	// MaxBodyBytes caps chat request bodies.
	MaxBodyBytes int64
	// Limiter enforces per-project rate limits and budgets; nil disables them.
	Limiter *limits.Limiter
	// Prices estimates the cost of a completed request; nil records none.
	Prices *pricing.Cache
	// Metrics records the gateway counters at the deferred chokepoint of a
	// finished request; nil records nothing.
	Metrics *obs.Metrics
	// Tracer opens the request span every downstream span hangs beneath
	// (named "{operation} {model}" once the request is routed, per the GenAI
	// conventions) and one gateway.attempt child span per upstream attempt;
	// nil is a disabled tracer.
	Tracer *tracing.Tracer
	// Rand is the random source of weighted target selection: it returns a
	// value in [0, 1). Nil uses math/rand/v2's concurrency-safe generator;
	// tests inject a seeded one.
	Rand func() float64
	// UpstreamTimeout is the total time budget of one request's upstream
	// attempts, retries and fallbacks together. Zero means no budget.
	UpstreamTimeout time.Duration
	// MaxRetries is how often a transient failure is repeated on the same
	// target before the route moves on. Zero disables retries; fallback to the
	// next target is not affected.
	MaxRetries int
	// CondenseTimeout is the time budget of the query condense call (ADR-024),
	// cut to the request's remaining upstream budget. Zero means 5 s; it is
	// not a setting of the product, tests shorten it.
	CondenseTimeout time.Duration
	// Sleep waits between a failed attempt and its retry. Nil uses a timer;
	// tests inject one that records the wait instead of taking it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Breakers holds the per-connection circuit breakers (ADR-020); nil
	// disables them.
	Breakers *Breakers
	// Rerankers builds the rerank API adapter of a connection (POST /v1/rerank
	// on a Cohere or Voyage connection); nil leaves such a connection unusable.
	Rerankers func(*store.ModelConnection) (provider.Reranker, error)
	// LLMRerank configures the chat-model reranker, used by POST /v1/rerank
	// when the connection that serves it is a chat connection.
	LLMRerank rag.LLMReranker
}

// requestObs collects the facts about one chat completion that the request
// log does not carry: the provider's error type, which is a bounded label
// set where the message is not, and the streaming timings. It is filled on
// the way through and read once at the deferred chokepoint.
type requestObs struct {
	// errType is provider.Error.Type; empty when nothing failed.
	errType string
	// upstream is the time inside the provider call. For a stream it covers
	// the whole stream, not just the header phase.
	upstream time.Duration
	// ttft is the wait for a stream's first chunk; zero when none arrived.
	ttft time.Duration
	// responseModel is the model the upstream reported, recorded before the
	// response is rewritten to the client's model string; empty until a
	// response or a first chunk arrives. See noteResponseModel.
	responseModel string
	// finishReasons is the distinct set of finish reasons seen, each mapped
	// onto the closed set in knownFinishReasons.
	finishReasons []string
}

// maxResponseModelLen caps gen_ai.response.model. An upstream is not
// trusted to keep its model string short.
const maxResponseModelLen = 128

// noteResponseModel records gen_ai.response.model once per request. The
// value is the model the upstream reported, unless it is empty or merely
// echoes the client's model string back: several adapters copy the request
// model into the response, and that string is attacker-controlled, so the
// connection's model name stands in for it.
func (o *requestObs) noteResponseModel(upstream, clientModel, connModel string) {
	if o.responseModel != "" {
		return
	}
	m := upstream
	if m == "" || (m == clientModel && clientModel != connModel) {
		m = connModel
	}
	if len(m) > maxResponseModelLen {
		// Cut on a rune boundary so the exported value stays valid UTF-8.
		n := maxResponseModelLen
		for n > 0 && !utf8.RuneStart(m[n]) {
			n--
		}
		m = m[:n]
	}
	o.responseModel = m
}

// knownFinishReasons is the closed set gen_ai.response.finish_reasons may
// carry; anything else an upstream sends is recorded as "other".
var knownFinishReasons = map[string]bool{
	"stop": true, "length": true, "tool_calls": true, "content_filter": true, "function_call": true,
}

// noteFinishReason adds one choice's finish reason to the distinct set.
func (o *requestObs) noteFinishReason(reason *string) {
	if reason == nil || *reason == "" {
		return
	}
	r := *reason
	if !knownFinishReasons[r] {
		r = "other"
	}
	for _, have := range o.finishReasons {
		if have == r {
			return
		}
	}
	o.finishReasons = append(o.finishReasons, r)
}

type ctxKey struct{}
type principalKey struct{}
type requestIDKey struct{}

// principal is the identity behind a /v1 request. A project key resolves to
// nothing but its project; a user key adds the key row, its owner and the
// projects it grants. project is nil only when a multi-grant key named no
// project, which chat refuses and the model listings answer across grants.
type principal struct {
	project *store.Project
	key     *store.APIKey
	user    *store.User
	grants  []*store.Project
}

// subject builds the limit subject: the project tier always applies, the key
// tier only for user keys.
func (pr *principal) subject() limits.Subject {
	s := limits.Subject{Project: pr.project}
	if pr.key != nil {
		s.KeyID = &pr.key.ID
		s.Key = limits.Caps{RPM: pr.key.RateLimitRPM, TPM: pr.key.RateLimitTPM,
			DailyTokens: pr.key.BudgetDailyTokens, MonthlyTokens: pr.key.BudgetMonthlyTokens}
	}
	return s
}

// imageTenant is the identity the image fetch ceiling shares its slots
// between. The project is the tenant boundary everywhere else here -- limits,
// budgets, request logs -- so it is the one that must not be starved by
// another; a multi-grant key that named no project falls back to the key,
// which is as far as its identity goes.
func (pr *principal) imageTenant() string {
	switch {
	case pr == nil:
		return ""
	case pr.project != nil:
		return "project:" + strconv.FormatInt(pr.project.ID, 10)
	case pr.key != nil:
		return "key:" + strconv.FormatInt(pr.key.ID, 10)
	}
	return ""
}

// attribute stamps the request log with the key and owner that paid for the
// request; a project's default key leaves both nil.
func (pr *principal) attribute(rec *store.RequestLog) {
	if pr == nil || pr.key == nil {
		return
	}
	rec.APIKeyID, rec.UserID = &pr.key.ID, &pr.key.UserID
}

// statusClientClosed is recorded in the request log when the client
// disconnected before the completion finished (nginx's 499 convention). It is
// never sent on the wire.
const (
	statusClientClosed = 499
	errClientClosed    = "client closed request"
)

// bodyReadDeadline bounds reading a chat request body; it is cleared once the
// body is in memory so streaming responses are never cut short by it.
const bodyReadDeadline = 60 * time.Second

// projectHeader lets a key that grants several projects name the one a
// request is for, by name or by id.
const projectHeader = "X-Ragmux-Project"

// projectsHeader lists the valid values of projectHeader when a request left
// the choice open and the key grants more than one project.
const projectsHeader = "X-Ragmux-Projects"

// Routes mounts /v1 handlers on the router. Scopes are enforced per route so
// a key can be issued for model listings without chat. Pipeline endpoints
// check their own scope and project in the authenticate step; the listings
// answer across every granted project and only need the scope middleware.
func (g *Gateway) Routes(r chi.Router) {
	r.Use(g.requestID)
	r.Use(g.identify)
	r.Post("/chat/completions", g.chatCompletions)
	r.Post("/embeddings", g.embeddings)
	r.Post("/retrieve", g.retrieve)
	r.Post("/rerank", g.rerank)
	models := r.With(g.requireScope(store.ScopeModels, "model listings"))
	models.Get("/models", g.listModels)
	models.Get("/models/{id}", g.getModel)
}

// maxRequestIDLen bounds a client-supplied X-Request-Id.
const maxRequestIDLen = 128

// validRequestID reports whether a client-supplied X-Request-Id may be used
// as is: 1 to maxRequestIDLen bytes of ASCII letters, digits and . _ : -. The
// set excludes whitespace, control characters, quotes and separators, so the
// value is safe in a header, a log line and a SQL text column.
func validRequestID(v string) bool {
	if v == "" || len(v) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// requestID gives every /v1 request one id and returns it in the X-Request-Id
// response header, errors included: it runs before identify, so a 401 carries
// it too. A valid X-Request-Id from the client is used; an absent or invalid
// one is replaced, never rejected, by the id obs.RequestID generated for the
// request (or a fresh one when that middleware is not installed).
//
// The id is kept in this package's own context key and nowhere else: the
// generated id that obs.RequestID stores for spans, metrics and the access
// log is left alone, so no client-chosen byte reaches a span or a label
// (PRD rule 10). The client's value reaches only the response header that
// echoes it back, the request log row and, as client_req_id beside the
// generated req_id, log lines.
func (g *Gateway) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = middleware.GetReqID(r.Context())
			if id == "" {
				id = newRequestID()
			}
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// newRequestID is the fallback id for a router without obs.RequestID.
func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// requestIDFrom is the id requestID settled on for this request.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeErrorCode(w, status, typ, "", msg)
}

// writeErrorCode is writeError with a machine-readable code; an empty code
// renders as null, which is the shape every pre-0.4 client already sees.
func writeErrorCode(w http.ResponseWriter, status int, typ, code, msg string) {
	var c any
	if code != "" {
		c = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": c}})
}

// invalidKey is the answer to any credential that does not resolve. Unknown
// keys keep exactly this body, so existing client handling is untouched; a
// retired key gets a code on top, which only ever reaches someone who
// already holds the key bytes.
func invalidKey(w http.ResponseWriter, code, msg string) {
	if code == "" {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid project api key")
		return
	}
	writeErrorCode(w, http.StatusUnauthorized, "invalid_api_key", code, msg)
}

// identify resolves the bearer credential into a principal on the request
// context. It stays a router middleware so every /v1 path, including unknown
// ones, answers 401 before anything else; the pipeline's authenticate step
// then applies scope and project selection to the principal found here.
func (g *Gateway) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing Authorization: Bearer <project api key> header")
			return
		}
		key := strings.TrimSpace(h[7:])
		if !strings.HasPrefix(key, store.ProjectKeyPrefix) && !strings.HasPrefix(key, store.GatewayKeyPrefix) {
			invalidKey(w, "", "")
			return
		}
		ctx := r.Context()
		gk, err := g.Store.ResolveGatewayKey(ctx, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				g.explainMiss(w, r, key)
				return
			}
			g.Log.Error("project lookup", "err", err)
			writeError(w, http.StatusInternalServerError, "server_error", "project lookup failed")
			return
		}
		pr := &principal{project: gk.Project, key: gk.Key, user: gk.Owner}
		if pr.key != nil {
			grants, err := g.Store.ListProjectsByIDs(ctx, pr.key.ProjectIDs)
			if err != nil {
				g.Log.Error("key grants", "key", pr.key.ID, "err", err)
				writeError(w, http.StatusInternalServerError, "server_error", "project lookup failed")
				return
			}
			pr.grants = grants
			if !g.selectProject(w, r, pr) {
				return
			}
		}
		ctx = context.WithValue(ctx, principalKey{}, pr)
		ctx = provider.WithImageTenant(ctx, pr.imageTenant())
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKey{}, pr.project)))
	})
}

// explainMiss answers a credential that did not resolve. The extra lookup
// runs only on a miss, so the hot path stays a single query.
func (g *Gateway) explainMiss(w http.ResponseWriter, r *http.Request, key string) {
	if !strings.HasPrefix(key, store.GatewayKeyPrefix) {
		invalidKey(w, "", "")
		return
	}
	state, err := g.Store.GetAPIKeyState(r.Context(), key)
	if err != nil {
		g.Log.Error("api key state", "err", err)
		invalidKey(w, "", "")
		return
	}
	switch state {
	case store.KeyStateRevoked:
		invalidKey(w, "key_revoked", "this api key has been revoked")
	case store.KeyStateExpired:
		invalidKey(w, "key_expired", "this api key has expired")
	case store.KeyStateOwnerInactive:
		invalidKey(w, "key_owner_inactive", "the account owning this api key is deactivated")
	default:
		invalidKey(w, "", "")
	}
}

// selectProject resolves which granted project a user key's request is for:
// the X-Ragmux-Project header (a name, or an id when numeric) wins, then the
// key's default project, then a single grant. A key that grants several and
// names none leaves pr.project nil, which only chat refuses.
//
// Grants, not project_members, decide here. Membership is a dashboard
// visibility concept; requiring both would let an admin editing a member
// list silently break a production key. Membership is checked once, when the
// grant is created.
func (g *Gateway) selectProject(w http.ResponseWriter, r *http.Request, pr *principal) bool {
	want := strings.TrimSpace(r.Header.Get(projectHeader))
	if want != "" {
		for _, p := range pr.grants {
			// A numeric value is an id; the name match stays exact so a
			// project called "12" is still reachable by name.
			if p.Name == want || strconv.FormatInt(p.ID, 10) == want {
				pr.project = p
				return true
			}
		}
		// One fixed message for "no such project" and "not granted" alike, so
		// a key holder cannot probe for project names.
		writeErrorCode(w, http.StatusForbidden, "invalid_request_error", "project_not_granted",
			"this api key is not authorised for the requested project")
		return false
	}
	if pr.key.DefaultProjectID != nil {
		for _, p := range pr.grants {
			if p.ID == *pr.key.DefaultProjectID {
				pr.project = p
				return true
			}
		}
	}
	if len(pr.grants) == 1 {
		pr.project = pr.grants[0]
	}
	return true
}

// checkProject refuses a request that could not be routed: either the key
// grants several projects and named none, or its last grant was removed, in
// which case it fails closed rather than falling back to anything. It
// answers the client itself and reports whether the request may proceed.
func checkProject(w http.ResponseWriter, pr *principal) bool {
	if pr == nil || pr.project != nil {
		return true
	}
	if len(pr.grants) == 0 {
		writeErrorCode(w, http.StatusForbidden, "invalid_request_error", "project_not_granted",
			"this api key is not authorised for the requested project")
		return false
	}
	names := make([]string, len(pr.grants))
	for i, p := range pr.grants {
		names[i] = p.Name
	}
	w.Header().Set(projectsHeader, strings.Join(names, ","))
	writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", "project_required",
		"this api key grants several projects; name one in the "+projectHeader+" header")
	return false
}

// checkScope enforces one gateway scope. It is a no-op for a project's
// default key, which has no scopes and is not narrowed by any. It answers
// the client itself and reports whether the request may proceed.
func checkScope(w http.ResponseWriter, pr *principal, scope, what string) bool {
	if pr != nil && pr.key != nil && !pr.key.HasScope(scope) {
		writeErrorCode(w, http.StatusForbidden, "insufficient_scope", "insufficient_scope",
			"api key is not authorised for "+what)
		return false
	}
	return true
}

// requireScope is checkScope as a route middleware, for the endpoints that
// do not run the pipeline.
func (g *Gateway) requireScope(scope, what string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !checkScope(w, principalFrom(r.Context()), scope, what) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func projectFrom(ctx context.Context) *store.Project {
	p, _ := ctx.Value(ctxKey{}).(*store.Project)
	return p
}

func principalFrom(ctx context.Context) *principal {
	pr, _ := ctx.Value(principalKey{}).(*principal)
	return pr
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	models, ok := g.visibleModels(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func (g *Gateway) getModel(w http.ResponseWriter, r *http.Request) {
	models, ok := g.visibleModels(w, r)
	if !ok {
		return
	}
	// A single-project credential keeps answering for any {id}, the way it
	// always has; across several grants the id has to name one of them.
	if len(models) == 1 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models[0])
		return
	}
	id := chi.URLParam(r, "id")
	for _, m := range models {
		if m["id"] == id {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m)
			return
		}
	}
	writeError(w, http.StatusNotFound, "invalid_request_error", "no such model")
}

// visibleModels lists the models the credential can reach: the selected
// project's connection, or — for a key that grants several and named none —
// one entry per granted project's connection, deduplicated by model name.
func (g *Gateway) visibleModels(w http.ResponseWriter, r *http.Request) ([]map[string]any, bool) {
	ctx := r.Context()
	projects := []*store.Project{}
	if p := projectFrom(ctx); p != nil {
		projects = append(projects, p)
	} else if pr := principalFrom(ctx); pr != nil {
		projects = pr.grants
	}
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, p := range projects {
		routed, isRouted, err := g.routedModels(r, p)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
			return nil, false
		}
		if isRouted {
			for _, m := range routed {
				if id, _ := m["id"].(string); !seen[id] {
					seen[id] = true
					out = append(out, m)
				}
			}
			continue
		}
		conn, err := g.Store.GetConnection(ctx, p.ModelConnectionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
			return nil, false
		}
		if seen[conn.ModelName] {
			continue
		}
		seen[conn.ModelName] = true
		out = append(out, modelObject(conn))
	}
	return out, true
}

func modelObject(conn *store.ModelConnection) map[string]any {
	created := time.Now().Unix()
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", conn.CreatedAt); err == nil {
		created = t.Unix()
	}
	return map[string]any{"id": conn.ModelName, "object": "model", "created": created, "owned_by": conn.ProviderType}
}

// chatCompletions is POST /v1/chat/completions: the pipeline steps in order,
// with the chat-specific parts (body decoding, provider adapter, the
// prepareChat hook) between them.
func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, chatOperation)
	if !ok {
		return
	}
	defer c.span.End()

	req, opts, ok := g.decodeChat(c)
	if !ok {
		return
	}
	// What the client asked for, before an empty model is defaulted below.
	c.requestedModel = req.Model
	if !g.resolveRoute(c, req.Model) {
		return
	}
	prov, ok, buildErr := g.chatProvider(c)
	if !ok {
		return
	}
	if req.Model == "" {
		req.Model = c.conn.ModelName
	}

	// This estimate covers the client's messages and the project prompt. The
	// retrieved context does not exist yet and is not retrieved before the
	// check, so throttled clients do not cost an embedding call; it is added
	// by checkContextLimits once retrieval has run.
	estimate := (len(c.project.SystemPrompt) + messageChars(req.Messages) + 3) / 4
	if !g.checkLimits(c, estimate, req.Stream) {
		return
	}

	prep := g.prepareChat(c, prov, &req, opts)
	g.openRecord(c, req.Stream, prep.apply)
	defer g.record(c)
	if !g.checkContextLimits(c, estimate, prep.contextBlock) {
		return
	}

	// A nil prov (the first target's adapter could not be built) leaves provConn
	// unset and records the connection and its error as failedConn and
	// failedErr: providerFor hands that error back without building again, and
	// the walk falls back.
	var provConn, failedConn *store.ModelConnection
	if prov != nil {
		provConn = c.conn
	} else {
		failedConn = c.conn
	}
	g.invoke(c, &chatInvocation{gw: g, prov: prov, provConn: provConn, failedConn: failedConn, failedErr: buildErr, req: req, promptChars: messageChars(req.Messages),
		opts: prep.opts, sources: prep.sources, contextBlock: prep.contextBlock})
}

// chatInvocation is the chat endpoint's upstreamCall.
type chatInvocation struct {
	gw *Gateway
	// prov is the adapter for provConn, the connection it was built for: the
	// first target to begin with, then whichever target was tried last. A
	// target is only given a new adapter when invoke moves to another one.
	prov     provider.Provider
	provConn *store.ModelConnection
	// failedConn is the connection whose adapter could not be built before the
	// walk began, and failedErr the target_config error that stands for it:
	// the build is tried once per request, not again at the walk.
	failedConn *store.ModelConnection
	failedErr  error
	// req is the request as it goes upstream; req.Model is the client's model
	// string, which the response is rewritten back to.
	req provider.ChatRequest
	// promptChars is the size of the prompt after context injection, the basis
	// of the usage estimate when the upstream reports none.
	promptChars int
	// opts, sources and contextBlock come from prepareChat. They shape the
	// "ragmux" object of the response: in the JSON body, or in the first chunk
	// of a stream.
	opts         ragmuxRequest
	sources      []ragSource
	contextBlock string
}

// providerFor returns the adapter of conn, building it the first time invoke
// reaches a target other than the one prov was made for. A connection whose
// adapter cannot be built is that target's own fault, so the walk may go on.
func (in *chatInvocation) providerFor(c *call, conn *store.ModelConnection) (provider.Provider, error) {
	if conn == in.provConn {
		return in.prov, nil
	}
	if conn == in.failedConn && in.failedErr != nil {
		return nil, in.failedErr
	}
	prov, err := in.gw.Providers(conn)
	if err != nil {
		// The factory error names the provider type or URL; it stays in the
		// log, as in chatProvider.
		c.log.Error("provider setup", "connection", conn.ID, "err", err)
		return nil, newTargetConfigError()
	}
	in.prov, in.provConn = prov, conn
	// The warnings prepareChat set describe what the first adapter ignores;
	// nothing has been written yet, so they can still be replaced.
	c.w.Header().Del("Warning")
	setRequestWarnings(c.w.Header(), prov, in.req)
	// The ragmux warnings are about the request, not the adapter: they stay.
	addRagmuxWarnings(c.w.Header(), in.opts.Warnings)
	return prov, nil
}

// attempt is one chat call, streamed or not. A stream answers the client
// itself once its first chunk arrives and is settled from then on; before that,
// like a plain call, it returns the provider's error untouched when it fails.
func (in *chatInvocation) attempt(c *call, conn *store.ModelConnection) error {
	req := in.req
	prov, err := in.providerFor(c, conn)
	if err != nil {
		return err
	}
	inline, err := in.opts.inlineField(in.sources, in.contextBlock)
	if err != nil {
		// A marshal failure of plain strings and numbers is not expected;
		// the answer is still worth more than the opt-in.
		c.log.Error("ragmux response field", "err", err)
		inline = nil
	}
	if req.Stream {
		return in.gw.stream(c.w, c.r, c.budgetLeft(), prov, conn, req, c.rec, c.obs, in.promptChars, withInlineRagmux(inline))
	}

	ctx, cancel := c.attemptContext()
	defer cancel()
	upstreamStart := time.Now()
	resp, err := prov.Chat(ctx, req)
	c.obs.upstream = time.Since(upstreamStart)
	if err != nil {
		return err
	}
	c.obs.noteResponseModel(resp.Model, req.Model, conn.ModelName)
	for i := range resp.Choices {
		c.obs.noteFinishReason(resp.Choices[i].FinishReason)
	}
	resp.Model = req.Model
	fillUsage(c.rec, resp.Usage, in.promptChars, completionChars(resp))
	c.rec.StatusCode = http.StatusOK
	c.w.Header().Set("Content-Type", "application/json")
	if inline != nil {
		if b, err := withRagmuxField(resp, inline); err == nil {
			_, _ = c.w.Write(b)
			return nil
		}
	}
	_ = json.NewEncoder(c.w).Encode(resp)
	return nil
}

// decodeChat reads and validates the chat request body, answering the client
// itself when it cannot.
func (g *Gateway) decodeChat(c *call) (provider.ChatRequest, ragmuxRequest, bool) {
	var req provider.ChatRequest
	var opts ragmuxRequest
	body, ok := g.readBody(c)
	if !ok {
		return req, opts, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return req, opts, false
	}
	if len(req.Messages) == 0 {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return req, opts, false
	}
	// The ragmux request object is ours; it must not reach the upstream. It is
	// parsed here, with the rest of the body, so a malformed one is refused
	// like any other malformed request, before a route is resolved or a limit
	// is charged.
	opts, err := parseRagmux(req.Extra)
	delete(req.Extra, "ragmux")
	if err != nil {
		writeErrorCode(c.w, http.StatusBadRequest, "invalid_request_error", errInvalidRagmux, err.Error())
		return req, opts, false
	}
	return req, opts, true
}

// chatProvider builds the chat adapter for the routed connection. A route with
// other targets does not fail on it (ADR-020's target_config class): it
// returns a nil adapter and the target_config error, which invoke reports as
// that target's attempt before moving on to the next one.
func (g *Gateway) chatProvider(c *call) (provider.Provider, bool, error) {
	prov, err := g.Providers(c.conn)
	if err != nil {
		// The factory error names the provider type or URL; clients only
		// need to know the project is misconfigured.
		c.log.Error("provider setup", "connection", c.conn.ID, "err", err)
		if len(c.targets) > 1 {
			return nil, true, newTargetConfigError()
		}
		writeError(c.w, http.StatusInternalServerError, "server_error", "model connection unavailable")
		return nil, false, nil
	}
	return prov, true, nil
}

// newTargetConfigError is the error of a target whose adapter cannot be built.
func newTargetConfigError() error {
	return &targetConfigError{pe: &provider.Error{
		Status: http.StatusInternalServerError, Type: "server_error", Message: "model connection unavailable"}}
}

// chatPrep is what prepareChat hands on to invoke and the request log.
type chatPrep struct {
	opts    ragmuxRequest
	ragUsed bool
	ragHits int
	// skipReason is why retrieval did not run for a project that has a store:
	// an obs.RAGSkip* value, empty when it ran.
	skipReason   string
	sources      []ragSource
	contextBlock string
}

// apply sets the RAG columns of the request log row.
func (p chatPrep) apply(rec *store.RequestLog) {
	rec.RAGUsed, rec.RAGHits, rec.RAGSkipReason = p.ragUsed, p.ragHits, p.skipReason
}

// prepareChat is the chat-only hook between checkLimits and invoke: it shapes
// the messages that go upstream with the project's system prompt and the RAG
// context. Other endpoints have no such step.
func (g *Gateway) prepareChat(c *call, prov provider.Provider, req *provider.ChatRequest, opts ragmuxRequest) chatPrep {
	p, ctx, w := c.project, c.ctx(), c.w
	// A narrowing that cannot narrow anything is not refused, because no
	// document reaches the context anyway; it earns a warning (PRD rule 12).
	if p.RAGStoreID == nil {
		switch {
		case opts.InlineSources:
			opts.Warnings = append(opts.Warnings, "ragmux.sources has no effect: this project has no RAG store; the list is empty")
		case opts.IncludeContext:
			opts.Warnings = append(opts.Warnings, "ragmux.include_context has no effect: this project has no RAG store; the sources and the context are empty")
		}
	}
	prep := chatPrep{opts: opts}

	// Project-level system prompt, if configured, goes first.
	//
	// The prompt injected here is the stable prefix most worth an Anthropic
	// cache breakpoint, but the gateway does not mark one: a project-level
	// cache_prompt flag needs a column, a migration and dashboard work of its
	// own. Clients that mark their own content parts are relayed unchanged in
	// the meantime, except in the system message the RAG context lands in
	// under the "system" placement (see below).
	if p.SystemPrompt != "" {
		req.Messages = rag.InjectContext(req.Messages, p.SystemPrompt)
	}

	// RAG pipeline. The hit count and sources headers are set here, before
	// either the JSON or the streaming path writes the status line.
	//
	// prov may be nil when the first target's adapter could not be built.
	// Retrieval embeds through the store's own connection and uses prov only
	// for the LLM rerank backend, which a nil prov skips; the walk still
	// answers from another target with the same context (ADR-020).
	//
	// Whether to retrieve at all is rag.SkipReason's decision (ADR-024). A
	// project without a store never gets to it: there is nothing to skip.
	ragOn := p.RAGStoreID != nil && g.Retriever != nil
	if ragOn {
		prep.skipReason = rag.SkipReason(p.RetrievalPolicy, opts.RetrievalOff,
			p.QueryStrategy == store.QueryStrategyCondense, req.Messages)
	}
	if prep.skipReason != "" {
		g.Metrics.RecordRAGSkipped(c.projectID, prep.skipReason)
		w.Header().Set("x-ragmux-rag-skip", prep.skipReason)
		w.Header().Set("x-ragmux-rag-hits", "0")
	} else if ragOn {
		rs, err := g.Store.GetRAGStore(ctx, *p.RAGStoreID)
		if err == nil {
			q := rag.LastUserQuery(req.Messages)
			if p.QueryStrategy == store.QueryStrategyCondense {
				// A side call of the request like the rerank below: it falls
				// back to q, and its tokens go on the row through c.sideCharges.
				q = g.condenseQuery(c, prov, req.Messages, q)
			}
			// The rerank of this retrieval is a side call of the chat request:
			// its tokens (an LLM rerank is a chat call, an API rerank a scoring
			// call) go on this request's log row through c.sideCharges.
			var rerankSpend *rerankUsageProvider
			// The LLM rerank runs on the first target's adapter, so that
			// target's price covers it even when the walk later answers from
			// a fallback. Captured now: c.conn moves with the walk.
			rerankType, rerankModel := c.conn.ProviderType, c.conn.ModelName
			searchProv := prov
			if prov != nil {
				rerankSpend = &rerankUsageProvider{Provider: prov}
				searchProv = rerankSpend
			}
			res, err := g.Retriever.SearchWith(ctx, rs, q, rs.TopK, searchProv, c.conn.ModelName)
			var hits []store.SearchHit
			if res != nil {
				hits = res.Hits
				c.addSideCharge(llmCharge(rerankSpend, rerankType, rerankModel, false))
				c.addSideCharge(apiCharge(res.RerankSpend, false))
			}
			if err != nil {
				c.log.Warn("rag retrieval failed; continuing without context", "err", err)
			} else if len(hits) > 0 {
				prep.contextBlock = rag.FormatContext(hits)
				req.Messages = injectRetrieved(req.Messages, prep.contextBlock, p.ContextPlacement)
				prep.ragUsed, prep.ragHits = true, len(hits)
				prep.sources = ragSources(hits)
				w.Header().Set("x-ragmux-rag-sources", sourcesHeader(prep.sources))
			}
			w.Header().Set("x-ragmux-rag-hits", strconv.Itoa(len(hits)))
		} else {
			c.log.Warn("rag store missing", "id", *p.RAGStoreID, "err", err)
		}
	}
	// Warnings go out before the first byte of either response shape; a
	// stream's headers are only written once its first chunk arrives.
	setRequestWarnings(w.Header(), prov, *req)
	addRagmuxWarnings(w.Header(), opts.Warnings)
	return prep
}

// injectRetrieved puts the retrieved-context block where the project's
// context_placement says.
//
// "system" is the original behaviour: the block leads the system message and
// the client's cache_control is dropped there, because a per-query prefix can
// never be read back from the cache. "before_last_user" leaves the system
// message and every breakpoint alone and puts the block at the head of the
// last user message, behind the cached prefix. Any other value -- the store
// already maps unknown ones to the default -- is treated as "system".
func injectRetrieved(msgs []provider.Message, block, placement string) []provider.Message {
	if placement == store.ContextPlacementBeforeLastUser {
		return rag.InjectBeforeLastUser(msgs, block)
	}
	return rag.InjectRetrievedContext(msgs, block)
}

// ragmuxRequest is what the optional "ragmux" object of a chat request asked
// for. The object is stripped before the request goes upstream.
//
// A request can only narrow what the project does or add information to the
// answer; it can never switch retrieval on or change a project setting
// (ADR-024). Everything it asked for that cannot be honoured is in Warnings.
type ragmuxRequest struct {
	// IncludeContext asks for the injected sources and context block in the
	// response: the body, or the first chunk of a stream. It implies
	// InlineSources.
	IncludeContext bool
	// InlineSources is ragmux.sources = "inline": the sources in the response
	// body, or in the first chunk of a stream, instead of only in the
	// x-ragmux-rag-sources header (which stops at 2 KB).
	InlineSources bool
	// RetrievalOff is ragmux.retrieval = "off": skip retrieval for this request.
	RetrievalOff bool
	// Warnings are the texts of the Warning headers the object earns.
	Warnings []string
}

// errInvalidRagmux is the code of the 400 an unusable ragmux object gets.
const errInvalidRagmux = "invalid_ragmux"

// ragmuxProjectSettings are the names of project settings. A request that
// names one gets a warning that says so, instead of the generic one.
var ragmuxProjectSettings = map[string]bool{
	"context_placement": true, "retrieval_policy": true, "query_strategy": true,
	"history_turns_for_query": true, "query_model_connection_id": true,
	"top_k": true, "system_prompt": true, "rag_store_id": true,
}

// ragmuxKeyName is the shape of a client key that may be echoed in a Warning
// header; any other key is reported without its name.
var ragmuxKeyName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// maxRagmuxWarnings bounds the Warning headers one request can earn: the
// object can carry as many keys as the body limit allows. addRagmuxWarnings
// applies it.
const maxRagmuxWarnings = 8

// parseRagmux reads the "ragmux" request object. A missing one is fine; one
// that is not a JSON object is the caller's error (the 400 invalid_ragmux).
// Fields it understands but cannot honour, and fields it does not know, earn a
// warning and are otherwise ignored (PRD rule 12).
func parseRagmux(extra map[string]json.RawMessage) (ragmuxRequest, error) {
	var o ragmuxRequest
	raw, ok := extra["ragmux"]
	if !ok {
		return o, nil
	}
	var fields map[string]json.RawMessage
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &fields) != nil {
		return o, errors.New("ragmux must be a JSON object")
	}

	if v, ok := fields["include_context"]; ok {
		if err := json.Unmarshal(v, &o.IncludeContext); err != nil {
			o.IncludeContext = false
			o.Warnings = append(o.Warnings, "ragmux.include_context must be a boolean; ignored")
		}
	}
	if v, ok := fields["retrieval"]; ok {
		var mode string
		if json.Unmarshal(v, &mode) == nil && mode == "off" {
			o.RetrievalOff = true
		} else {
			// "always" and "auto" would widen the project's setting.
			o.Warnings = append(o.Warnings, "ragmux.retrieval accepts only the value off; ignored")
		}
	}

	if v, ok := fields["sources"]; ok {
		var mode string
		if json.Unmarshal(v, &mode) == nil && mode == "inline" {
			o.InlineSources = true
		} else {
			o.Warnings = append(o.Warnings, "ragmux.sources accepts only the value inline; ignored")
		}
	}

	// Map order is random; the headers must not be.
	var rest []string
	for k := range fields {
		if k != "include_context" && k != "retrieval" && k != "sources" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		switch {
		case ragmuxProjectSettings[k]:
			o.Warnings = append(o.Warnings, "ragmux."+k+" is a project setting and cannot be set by a request; ignored")
		case ragmuxKeyName.MatchString(k):
			o.Warnings = append(o.Warnings, "unknown ragmux field "+k+"; ignored")
		default:
			o.Warnings = append(o.Warnings, "unknown ragmux field; ignored")
		}
	}
	return o, nil
}

// addRagmuxWarnings adds one `Warning: 299 ragmux "<text>"` header per text, at
// most maxRagmuxWarnings of them: the last stands for the ones left out. The
// bound is applied here, where every warning has been collected, so a warning
// added after parsing cannot exceed it.
func addRagmuxWarnings(h http.Header, texts []string) {
	if n := len(texts); n > maxRagmuxWarnings {
		kept := texts[: maxRagmuxWarnings-1 : maxRagmuxWarnings-1]
		texts = append(kept, strconv.Itoa(n-maxRagmuxWarnings+1)+" more ragmux warnings omitted")
	}
	for _, text := range texts {
		h.Add("Warning", `299 ragmux "`+warnTextEscaper.Replace(text)+`"`)
	}
}

// ragSource is one injected hit as exposed to the client: where the passage
// came from and how it ranked, never its content.
type ragSource struct {
	DocumentID int64   `json:"document_id"`
	Filename   string  `json:"filename"`
	Section    string  `json:"section"`
	Page       int     `json:"page"`
	Score      float64 `json:"score"`
}

func ragSources(hits []store.SearchHit) []ragSource {
	out := make([]ragSource, len(hits))
	for i, h := range hits {
		out[i] = ragSource{DocumentID: h.DocumentID, Filename: h.Filename, Section: h.Section, Page: h.Page, Score: h.Score}
	}
	return out
}

// maxSourcesHeaderBytes bounds x-ragmux-rag-sources so long file names or a
// large top_k cannot push the response head past proxy header limits.
const maxSourcesHeaderBytes = 2048

// sourcesHeader renders the sources as compact ASCII JSON, dropping trailing
// entries until the array fits; the result is always a complete array.
func sourcesHeader(sources []ragSource) string {
	for n := len(sources); n > 0; n-- {
		b, err := json.Marshal(sources[:n])
		if err != nil {
			return "[]"
		}
		v := asciiJSON(b)
		if len(v) <= maxSourcesHeaderBytes {
			return v
		}
	}
	return "[]"
}

// asciiJSON escapes non-ASCII runes as \uXXXX so the value is safe in a
// header regardless of how intermediaries treat raw UTF-8.
func asciiJSON(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

// inlineField is the JSON value of the "ragmux" field the request asked to get
// back, or nil when it asked for none. Both response shapes carry the same
// object: the sources, and the context block when include_context asked for it.
// A request without hits or without retrieval gets an empty list, not a missing
// one.
func (o ragmuxRequest) inlineField(sources []ragSource, contextBlock string) ([]byte, error) {
	if !o.IncludeContext && !o.InlineSources {
		return nil, nil
	}
	if sources == nil {
		sources = []ragSource{}
	}
	m := map[string]any{"sources": sources}
	if o.IncludeContext {
		m["context"] = contextBlock
	}
	return json.Marshal(m)
}

// withRagmuxField adds a top-level "ragmux" object to the serialised
// response without disturbing the OpenAI fields.
func withRagmuxField(resp *provider.ChatResponse, extra []byte) ([]byte, error) {
	base, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	m["ragmux"] = extra
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// withRagmuxChunk adds the "ragmux" object to one serialised stream chunk. The
// object is appended after the chunk's own fields, which keep their order and
// bytes.
func withRagmuxChunk(chunk, extra []byte) []byte {
	n := len(chunk)
	if n == 0 || chunk[n-1] != '}' {
		return chunk
	}
	out := make([]byte, 0, n+len(extra)+10)
	out = append(out, chunk[:n-1]...)
	out = append(out, `,"ragmux":`...)
	out = append(out, extra...)
	return append(out, '}')
}

// streamOption tunes one stream call.
type streamOption func(*streamConfig)

type streamConfig struct {
	// inline is the "ragmux" object for the first chunk that has choices; nil
	// leaves every chunk as the upstream's.
	inline []byte
}

// withInlineRagmux makes the stream carry the given "ragmux" object, once, on
// the first chunk that has choices. The object goes there rather than in a
// chunk of its own because a chunk with an empty choices array breaks clients
// that index choices[0].
func withInlineRagmux(extra []byte) streamOption {
	return func(c *streamConfig) { c.inline = extra }
}

// stream relays one streamed chat call to the client.
//
// It returns an error only when the upstream failed before the first chunk, so
// that nothing has been written and rec is untouched: the caller may then try
// another target. Once a chunk has arrived the response is the stream's own
// and every outcome is settled in rec. firstByte, if positive, is how long the
// upstream has to produce that first chunk; it does not limit the stream
// afterwards.
func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, firstByte time.Duration, prov provider.Provider,
	conn *store.ModelConnection, req provider.ChatRequest, rec *store.RequestLog, obsv *requestObs, promptChars int,
	options ...streamOption) error {
	var cfg streamConfig
	for _, o := range options {
		o(&cfg)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by server")
		return nil
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var budgetHit atomic.Bool
	var budgetTimer *time.Timer
	if firstByte > 0 {
		budgetTimer = time.AfterFunc(firstByte, func() {
			budgetHit.Store(true)
			cancel()
		})
		defer budgetTimer.Stop()
	}

	// The chunk counter is resolved once, here: the registry's With costs an
	// allocation, and a per-chunk lookup in a streaming handler is exactly
	// the mistake its allocation note exists to prevent.
	sm := g.Metrics.StreamStarted(conn.ProviderType)
	defer sm.Ended()
	upstreamStart := time.Now()

	out := make(chan provider.StreamChunk, 16)
	errc := make(chan error, 1)
	go func() {
		// A panic inside a provider adapter would otherwise kill the process
		// (the HTTP server's recover does not cover this goroutine). Turn it
		// into a stream error and always close out exactly once.
		var err error
		defer func() {
			if p := recover(); p != nil {
				g.Log.Error("provider stream panicked", "project", rec.ProjectID, "panic", p)
				err = fmt.Errorf("provider stream panicked: %v", p)
			}
			errc <- err
			close(out)
		}()
		err = prov.ChatStream(ctx, req, out)
	}()

	headersSent := false
	sendHeaders := func() {
		if headersSent {
			return
		}
		headersSent = true
		if budgetTimer != nil {
			budgetTimer.Stop()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	// Every adapter emits a usage-only trailer, and the OpenAI-compatible
	// one always asks its upstream for one, because the token counts are
	// what the request log, the budgets and the cost are built from. The
	// client only sees that trailer when it asked for it: OpenAI sends none
	// without stream_options.include_usage, and a chunk with an empty
	// choices array is exactly what breaks a client indexing choices[0].
	wantUsage := req.IncludeUsage()

	var usage *provider.Usage
	compChars := 0
	clientGone := false
	for chunk := range out {
		sm.Chunk()
		if obsv.ttft == 0 {
			obsv.ttft = time.Since(upstreamStart)
		}
		// Recorded before the client check so a disconnect does not hide
		// what the upstream reported. req.Model is the client's string here.
		obsv.noteResponseModel(chunk.Model, req.Model, conn.ModelName)
		for i := range chunk.Choices {
			obsv.noteFinishReason(chunk.Choices[i].FinishReason)
		}
		if clientGone {
			continue // drain until the provider notices the cancellation
		}
		sendHeaders()
		chunk.Model = req.Model
		if chunk.Usage != nil {
			usage = chunk.Usage
			if !wantUsage {
				chunk.Usage = nil
				if len(chunk.Choices) == 0 {
					continue // a trailer the client never asked for
				}
			}
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil {
				compChars += len(*c.Delta.Content)
			}
		}
		b, _ := json.Marshal(chunk)
		if cfg.inline != nil && len(chunk.Choices) > 0 {
			b = withRagmuxChunk(b, cfg.inline)
			cfg.inline = nil
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			// Writing to a closed client: stop the upstream call. The
			// goroutine exits once ChatStream observes ctx and closes out.
			clientGone = true
			cancel()
			continue
		}
		flusher.Flush()
	}
	err := <-errc
	obsv.upstream = time.Since(upstreamStart)
	if clientGone || r.Context().Err() != nil {
		// The client disconnected before the completion finished. cancel()
		// (deferred, or called above) has already torn down the upstream
		// request through ctx; nothing can be written to the client.
		rec.StatusCode, rec.Error = statusClientClosed, errClientClosed
		fillUsage(rec, usage, promptChars, compChars)
		return nil
	}
	if budgetHit.Load() && !headersSent {
		// The first-byte budget cut the call off; the context error that
		// reached us is ours, not the client's.
		err = &provider.Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream request cancelled or timed out"}
	}
	if err != nil && !headersSent {
		// Nothing was written: the caller decides whether to try another
		// target or to answer with this error.
		return err
	}
	sendHeaders()
	if cfg.inline != nil {
		// No chunk with choices came, so the field had nowhere to go. A chunk of
		// the gateway's own would break clients that index choices[0]; the
		// headers are out, so no Warning either: the log says it.
		g.Log.Warn("ragmux inline sources not delivered: the stream had no chunk with choices", "project", rec.ProjectID)
	}
	if err != nil {
		// Mid-stream failure: surface it as an SSE error event then end.
		_, pe := providerError(err)
		rec.StatusCode, rec.Error, obsv.errType = http.StatusBadGateway, pe.Message, pe.Type
		g.Log.Warn("upstream stream failed", "project", rec.ProjectID, "msg", pe.Message)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", pe.ErrorJSON())
	} else {
		rec.StatusCode = http.StatusOK
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	fillUsage(rec, usage, promptChars, compChars)
	return nil
}

func messageChars(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text())
	}
	return n
}

// setLimitHeaders exposes the request budget the way OpenAI does (reset as
// integer seconds) plus Ragmux-specific remaining token budgets. Only limits
// that are configured produce headers; the Decision already carries whichever
// of the project and key tiers has less headroom.
func setLimitHeaders(h http.Header, d limits.Decision) {
	if d.LimitRequests > 0 {
		h.Set("x-ratelimit-limit-requests", strconv.Itoa(d.LimitRequests))
		h.Set("x-ratelimit-remaining-requests", strconv.Itoa(d.RemainingRequests))
		h.Set("x-ratelimit-reset-requests", strconv.Itoa(ceilSeconds(d.ResetRequests)))
	}
	if d.DailyLimit > 0 {
		h.Set("x-ragmux-budget-daily-remaining", strconv.FormatInt(max(d.DailyLimit-d.DailyUsed, 0), 10))
	}
	if d.MonthlyLimit > 0 {
		h.Set("x-ragmux-budget-monthly-remaining", strconv.FormatInt(max(d.MonthlyLimit-d.MonthlyUsed, 0), 10))
	}
}

func ceilSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// writeLimitError answers a denied request with an OpenAI-style 429. "scope"
// says which tier denied; the wording stays "for this project" so existing
// messages are unchanged for the tier that always existed.
func writeLimitError(w http.ResponseWriter, d limits.Decision) {
	subject := "this project"
	if d.Scope == limits.ScopeKey {
		subject = "this api key"
	}
	typ, msg := "rate_limit_exceeded", ""
	switch d.Reason {
	case limits.ReasonRPM:
		msg = fmt.Sprintf("Rate limit reached: %d requests per minute for %s.", d.LimitRequests, subject)
	case limits.ReasonTPM:
		msg = "Rate limit reached: tokens per minute for " + subject + "."
	case limits.ReasonBudgetDaily:
		typ = "insufficient_quota"
		msg = fmt.Sprintf("Daily token budget exhausted (%d of %d tokens used).", d.DailyUsed, d.DailyLimit)
	case limits.ReasonBudgetMonthly:
		typ = "insufficient_quota"
		msg = fmt.Sprintf("Monthly token budget exhausted (%d of %d tokens used).", d.MonthlyUsed, d.MonthlyLimit)
	default:
		msg = "Rate limit reached for " + subject + "."
	}
	retry := ceilSeconds(d.RetryAfter)
	msg += fmt.Sprintf(" Retry after %d seconds.", retry)
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "code": d.Reason, "scope": d.Scope}})
}

// writeProviderError answers with a provider error in OpenAI's envelope,
// carrying its retry hint as a Retry-After header.
func writeProviderError(w http.ResponseWriter, status int, pe *provider.Error) {
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w.Header(), pe)
	w.WriteHeader(status)
	_, _ = w.Write(pe.ErrorJSON())
}

// setRetryAfter writes the error's retry hint. The gateway's own figure
// (RetryAfter, seconds) wins over the upstream's; the upstream's is relayed
// verbatim, in whichever of the two RFC 9110 forms it came.
func setRetryAfter(h http.Header, pe *provider.Error) {
	switch {
	case pe.RetryAfter > 0:
		h.Set("Retry-After", strconv.Itoa(pe.RetryAfter))
	case pe.UpstreamRetryAfter != "":
		h.Set("Retry-After", pe.UpstreamRetryAfter)
	}
}

// warnTextEscaper keeps a warning text inside its quoted-string.
var warnTextEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// setRequestWarnings adds one `Warning: 299 ragmux "<text>"` header per part
// of the request the adapter forwards but ignores. 299 is RFC 7234's
// "miscellaneous persistent warning"; RFC 9111 obsoleted that RFC and
// deprecated the header, which is kept for visibility. The request still
// succeeds.
func setRequestWarnings(h http.Header, prov provider.Provider, req provider.ChatRequest) {
	rw, ok := prov.(provider.RequestWarner)
	if !ok {
		return
	}
	for _, text := range rw.RequestWarnings(req) {
		h.Add("Warning", `299 ragmux "`+warnTextEscaper.Replace(text)+`"`)
	}
}

func providerError(err error) (int, *provider.Error) {
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Status == 0 {
			pe.Status = http.StatusBadGateway
		}
		return pe.Status, pe
	}
	if errors.Is(err, context.Canceled) {
		return statusClientClosed, &provider.Error{Status: statusClientClosed, Type: "client_closed", Message: errClientClosed}
	}
	return http.StatusBadGateway, &provider.Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: provider.Redact(err.Error())}
}

// priceRequest fills the cost fields of a finished request from the price
// table. A model nobody priced, or a table that cannot be read, leaves the
// cost at zero with cost_source "none" — a missing price is reported as
// missing, never guessed.
//
// searchUnits is what the call billed in search units, priced with the
// connection's per_search_unit; it is zero for every call but a Cohere rerank.
// A row that has a per_search_unit rate prices such a call by search units
// alone, never by tokens as well.
func (g *Gateway) priceRequest(conn *store.ModelConnection, rec *store.RequestLog, searchUnits int) {
	rec.CostSource = store.CostSourceNone
	if g.Prices == nil || conn == nil {
		return
	}
	// The request context may already be cancelled (a client disconnect is
	// exactly when this runs), and the cache usually answers without the
	// database anyway, so the lookup gets a background context.
	p, ok := g.Prices.Lookup(context.Background(), conn.ProviderType, conn.ModelName)
	if !ok {
		return
	}
	rec.CostMicros = p.BilledMicros(rec.PromptTokens, rec.CachedPromptTokens, rec.CacheWriteTokens, rec.CompletionTokens, searchUnits)
	rec.CostSource = p.Source
	// The row stores micros (exact in BIGINT); CostUSD is the same figure in
	// dollars, which is what the cost counter adds up.
	rec.CostUSD = float64(rec.CostMicros) / 1e6
}

// priceSideCharges adds the cost of each side charge to the finished row, every
// one priced with the connection that did the work. A charge whose connection
// has no price adds nothing: the request still succeeded, and the row's
// cost_source stays what the main call's price gave it, so a missing price on
// a rerank connection never reads as a priced zero on the whole row.
func (g *Gateway) priceSideCharges(rec *store.RequestLog, charges []sideCharge) {
	if g.Prices == nil {
		return
	}
	for _, ch := range charges {
		if ch.providerType == "" {
			continue
		}
		p, ok := g.Prices.Lookup(context.Background(), ch.providerType, ch.model)
		if !ok {
			continue
		}
		rec.CostMicros = addMicros(rec.CostMicros, p.BilledMicros(ch.prompt, 0, 0, ch.completion, ch.searchUnits))
	}
	rec.CostUSD = float64(rec.CostMicros) / 1e6
}

// addMicros is a + b saturated at the int64 ceiling; both are non-negative.
func addMicros(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// fillUsage copies the upstream's token counts onto the log row, falling
// back to a character estimate when it reported none. It is the one place
// tokens enter request_logs, so it is also where a broken or hostile
// upstream is stopped: a negative count never reaches the cost, the budget
// counters or the metrics, where it would subtract from spend that really
// happened.
//
// A negative count is replaced half by half rather than zeroed. A request
// that produced text and came back with completion_tokens: -78 really did
// spend output tokens, and logging it as 0 would under-charge the budget
// and leave the row looking exact; the character estimate is the same
// answer as for an upstream that reported nothing, and rec.Estimated says
// so on every surface that shows the row.
func fillUsage(rec *store.RequestLog, u *provider.Usage, promptChars, compChars int) {
	promptEstimate := (nonNegative(promptChars) + 3) / 4
	compEstimate := (nonNegative(compChars) + 3) / 4
	if u != nil && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		rec.PromptTokens, rec.CompletionTokens = u.PromptTokens, u.CompletionTokens
		rec.CachedPromptTokens, rec.CacheWriteTokens = u.CachedTokens(), u.CacheWriteTokens()
		if rec.PromptTokens < 0 {
			rec.PromptTokens, rec.Estimated = promptEstimate, true
		}
		if rec.CompletionTokens < 0 {
			rec.CompletionTokens, rec.Estimated = compEstimate, true
		}
		// The cache split has no characters to estimate from, so an unusable
		// one drops to zero. It does not set Estimated: that flag renders as
		// "~" beside the prompt and completion counts and says those two are
		// guesses, which would be a lie about numbers the upstream reported
		// exactly. The row also shows no cache pill, which is the honest
		// reading of a split that arrived unusable.
		//
		// Zero is a defined reading, not a conservative one. CostMicros bills
		// prompt - cached - written at the input rate, so zeroing moves those
		// tokens into that share: the estimate lands low wherever cache_write
		// is dearer than input, which is every Anthropic row in prices.json,
		// and high wherever cache_read is cheaper, which is every row that
		// sets one. Either way it stays what cost_micros always is, an
		// estimate rather than a bill; what it must not be is negative.
		rec.CachedPromptTokens = nonNegative(rec.CachedPromptTokens)
		rec.CacheWriteTokens = nonNegative(rec.CacheWriteTokens)
		return
	}
	rec.Estimated = true
	rec.PromptTokens, rec.CompletionTokens = promptEstimate, compEstimate
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func completionChars(resp *provider.ChatResponse) int {
	n := 0
	for _, c := range resp.Choices {
		if c.Message.Content != nil {
			n += len(*c.Message.Content)
		}
	}
	return n
}
