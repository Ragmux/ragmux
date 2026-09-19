// Package gateway serves the OpenAI-compatible /v1 API backed by project API
// keys, applying the RAG pipeline and recording metrics.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/httpx"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
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
}

type ctxKey struct{}
type principalKey struct{}

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
// a key can be issued for model listings without chat, and requireProject
// only guards chat: the listings answer across every granted project.
func (g *Gateway) Routes(r chi.Router) {
	r.Use(g.authenticate)
	r.With(g.requireScope(store.ScopeChat, "chat completions"), g.requireProject).
		Post("/chat/completions", g.chatCompletions)
	models := r.With(g.requireScope(store.ScopeModels, "model listings"))
	models.Get("/models", g.listModels)
	models.Get("/models/{id}", g.getModel)
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

func (g *Gateway) authenticate(next http.Handler) http.Handler {
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

// requireProject refuses a request that could not be routed: either the key
// grants several projects and named none, or its last grant was removed, in
// which case it fails closed rather than falling back to anything.
func (g *Gateway) requireProject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := principalFrom(r.Context())
		if pr != nil && pr.project == nil {
			if len(pr.grants) == 0 {
				writeErrorCode(w, http.StatusForbidden, "invalid_request_error", "project_not_granted",
					"this api key is not authorised for the requested project")
				return
			}
			names := make([]string, len(pr.grants))
			for i, p := range pr.grants {
				names[i] = p.Name
			}
			w.Header().Set(projectsHeader, strings.Join(names, ","))
			writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", "project_required",
				"this api key grants several projects; name one in the "+projectHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireScope enforces one gateway scope. It is a no-op for a project's
// default key, which has no scopes and is not narrowed by any.
func (g *Gateway) requireScope(scope, what string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pr := principalFrom(r.Context())
			if pr != nil && pr.key != nil && !pr.key.HasScope(scope) {
				writeErrorCode(w, http.StatusForbidden, "insufficient_scope", "insufficient_scope",
					"api key is not authorised for "+what)
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

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	p := projectFrom(ctx)
	log := g.Log.With("project", p.ID)

	max := g.MaxBodyBytes
	if max <= 0 {
		max = 4 << 20
	}
	httpx.Deadline(w, bodyReadDeadline)
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	httpx.Deadline(w, 0)
	if err != nil || int64(len(body)) > max {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}
	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	clientModel := req.Model

	conn, err := g.Store.GetConnection(ctx, p.ModelConnectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
		return
	}
	prov, err := g.Providers(conn)
	if err != nil {
		// The factory error names the provider type or URL; clients only
		// need to know the project is misconfigured.
		log.Error("provider setup", "connection", conn.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
		return
	}
	if clientModel == "" {
		clientModel = conn.ModelName
	}
	req.Model = clientModel

	// Limits are checked before RAG retrieval so throttled clients do not
	// cost an embedding call. The token estimate covers the client's
	// messages and the project prompt; retrieved context is not included.
	pr := principalFrom(ctx)
	subject := limits.Subject{Project: p}
	if pr != nil {
		subject = pr.subject()
	}
	var decision limits.Decision
	if g.Limiter != nil {
		est := (len(p.SystemPrompt) + messageChars(req.Messages) + 3) / 4
		decision, err = g.Limiter.CheckSubject(ctx, subject, est)
		if err != nil {
			log.Error("limit check", "err", err)
			writeError(w, http.StatusInternalServerError, "server_error", "limit check failed")
			return
		}
		setLimitHeaders(w.Header(), decision)
		if !decision.Allowed {
			rec := &store.RequestLog{ProjectID: p.ID, ModelName: conn.ModelName, Streamed: req.Stream,
				StatusCode: http.StatusTooManyRequests, Error: decision.Reason, LatencyMs: time.Since(start).Milliseconds()}
			pr.attribute(rec)
			if err := g.Store.InsertRequestLog(ctx, rec); err != nil {
				log.Error("write request log", "err", err)
			}
			writeLimitError(w, decision)
			return
		}
	}

	// Project-level system prompt, if configured, goes first.
	if p.SystemPrompt != "" {
		req.Messages = rag.InjectContext(req.Messages, p.SystemPrompt)
	}

	// The ragmux request object is ours; it must not reach the upstream.
	opts := ragmuxOptions(req.Extra)
	delete(req.Extra, "ragmux")

	// RAG pipeline. The hit count and sources headers are set here, before
	// either the JSON or the streaming path writes the status line.
	var (
		ragUsed      bool
		ragHits      int
		sources      []ragSource
		contextBlock string
	)
	if p.RAGStoreID != nil && g.Retriever != nil {
		rs, err := g.Store.GetRAGStore(ctx, *p.RAGStoreID)
		if err == nil {
			q := rag.LastUserQuery(req.Messages)
			hits, err := g.Retriever.Search(ctx, rs, q, rs.TopK, prov, conn.ModelName)
			if err != nil {
				log.Warn("rag retrieval failed; continuing without context", "err", err)
			} else if len(hits) > 0 {
				contextBlock = rag.FormatContext(hits)
				req.Messages = rag.InjectContext(req.Messages, contextBlock)
				ragUsed, ragHits = true, len(hits)
				sources = ragSources(hits)
				w.Header().Set("x-ragmux-rag-sources", sourcesHeader(sources))
			}
			w.Header().Set("x-ragmux-rag-hits", strconv.Itoa(len(hits)))
		} else {
			log.Warn("rag store missing", "id", *p.RAGStoreID, "err", err)
		}
	}

	rec := &store.RequestLog{ProjectID: p.ID, ModelName: conn.ModelName, RAGUsed: ragUsed, RAGHits: ragHits, Streamed: req.Stream}
	pr.attribute(rec)
	promptChars := messageChars(req.Messages)
	defer func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		if err := g.Store.InsertRequestLog(context.Background(), rec); err != nil {
			log.Error("write request log", "err", err)
		}
		if g.Limiter != nil {
			// The minute request slot was already reserved by Check when an
			// RPM limit is set; otherwise count the request here.
			if err := g.Limiter.RecordSubject(context.Background(), subject, rec.PromptTokens, rec.CompletionTokens, !decision.Reserved); err != nil {
				log.Error("record usage", "err", err)
			}
		}
		if pr != nil && pr.key != nil {
			// At most one write per key per minute; the statement itself
			// throttles, so a busy key does not rewrite its row per request.
			if err := g.Store.TouchAPIKeyUsed(context.Background(), pr.key.ID); err != nil {
				log.Error("touch api key", "key", pr.key.ID, "err", err)
			}
		}
	}()

	if req.Stream {
		g.stream(w, r, prov, req, rec, promptChars)
		return
	}

	resp, err := prov.Chat(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			// The client went away while the upstream call was running; the
			// provider already saw the cancellation through ctx.
			rec.StatusCode, rec.Error = statusClientClosed, errClientClosed
			return
		}
		status, pe := providerError(err)
		rec.StatusCode, rec.Error = status, pe.Message
		log.Warn("upstream error", "status", status, "msg", pe.Message)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(pe.ErrorJSON())
		return
	}
	resp.Model = clientModel
	fillUsage(rec, resp.Usage, promptChars, completionChars(resp))
	rec.StatusCode = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	if opts.IncludeContext {
		if b, err := withRagmuxField(resp, sources, contextBlock); err == nil {
			_, _ = w.Write(b)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ragmuxRequest is the optional "ragmux" object clients may add to a chat
// request. It is stripped before the request goes upstream.
type ragmuxRequest struct {
	// IncludeContext asks for the injected sources and context block in the
	// (non-streaming) response body.
	IncludeContext bool `json:"include_context"`
}

func ragmuxOptions(extra map[string]json.RawMessage) ragmuxRequest {
	var o ragmuxRequest
	if raw, ok := extra["ragmux"]; ok {
		// A malformed object is treated as absent; it is still stripped.
		_ = json.Unmarshal(raw, &o)
	}
	return o
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

// withRagmuxField adds a top-level "ragmux" object to the serialised
// response without disturbing the OpenAI fields.
func withRagmuxField(resp *provider.ChatResponse, sources []ragSource, contextBlock string) ([]byte, error) {
	base, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	if sources == nil {
		sources = []ragSource{}
	}
	extra, err := json.Marshal(map[string]any{"sources": sources, "context": contextBlock})
	if err != nil {
		return nil, err
	}
	m["ragmux"] = extra
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, prov provider.Provider, req provider.ChatRequest, rec *store.RequestLog, promptChars int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by server")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

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
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	var usage *provider.Usage
	compChars := 0
	clientGone := false
	for chunk := range out {
		if clientGone {
			continue // drain until the provider notices the cancellation
		}
		sendHeaders()
		chunk.Model = req.Model
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil {
				compChars += len(*c.Delta.Content)
			}
		}
		b, _ := json.Marshal(chunk)
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
	if clientGone || r.Context().Err() != nil {
		// The client disconnected before the completion finished. cancel()
		// (deferred, or called above) has already torn down the upstream
		// request through ctx; nothing can be written to the client.
		rec.StatusCode, rec.Error = statusClientClosed, errClientClosed
		fillUsage(rec, usage, promptChars, compChars)
		return
	}
	if err != nil && !headersSent {
		status, pe := providerError(err)
		rec.StatusCode, rec.Error = status, pe.Message
		g.Log.Warn("upstream error", "project", rec.ProjectID, "status", status, "msg", pe.Message)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(pe.ErrorJSON())
		return
	}
	sendHeaders()
	if err != nil {
		// Mid-stream failure: surface it as an SSE error event then end.
		_, pe := providerError(err)
		rec.StatusCode, rec.Error = http.StatusBadGateway, pe.Message
		g.Log.Warn("upstream stream failed", "project", rec.ProjectID, "msg", pe.Message)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", pe.ErrorJSON())
	} else {
		rec.StatusCode = http.StatusOK
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	fillUsage(rec, usage, promptChars, compChars)
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

func fillUsage(rec *store.RequestLog, u *provider.Usage, promptChars, compChars int) {
	if u != nil && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		rec.PromptTokens, rec.CompletionTokens = u.PromptTokens, u.CompletionTokens
		return
	}
	rec.Estimated = true
	rec.PromptTokens = (promptChars + 3) / 4
	rec.CompletionTokens = (compChars + 3) / 4
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
