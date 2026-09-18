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

// statusClientClosed is recorded in the request log when the client
// disconnected before the completion finished (nginx's 499 convention). It is
// never sent on the wire.
const (
	statusClientClosed = 499
	errClientClosed    = "client closed request"
)

// Routes mounts /v1 handlers on the router.
func (g *Gateway) Routes(r chi.Router) {
	r.Use(g.authenticate)
	r.Post("/chat/completions", g.chatCompletions)
	r.Get("/models", g.listModels)
	r.Get("/models/{id}", g.getModel)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil}})
}

func (g *Gateway) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing Authorization: Bearer <project api key> header")
			return
		}
		key := strings.TrimSpace(h[7:])
		if !strings.HasPrefix(key, "sk-proj-") {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid project api key")
			return
		}
		p, err := g.Store.GetProjectByKey(r.Context(), key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid project api key")
				return
			}
			g.Log.Error("project lookup", "err", err)
			writeError(w, http.StatusInternalServerError, "server_error", "project lookup failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func projectFrom(ctx context.Context) *store.Project {
	p, _ := ctx.Value(ctxKey{}).(*store.Project)
	return p
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r.Context())
	conn, err := g.Store.GetConnection(r.Context(), p.ModelConnectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   []map[string]any{modelObject(conn)},
	})
}

func (g *Gateway) getModel(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r.Context())
	conn, err := g.Store.GetConnection(r.Context(), p.ModelConnectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "model connection unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modelObject(conn))
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
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
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
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if clientModel == "" {
		clientModel = conn.ModelName
	}
	req.Model = clientModel

	// Limits are checked before RAG retrieval so throttled clients do not
	// cost an embedding call. The token estimate covers the client's
	// messages and the project prompt; retrieved context is not included.
	var decision limits.Decision
	if g.Limiter != nil {
		est := (len(p.SystemPrompt) + messageChars(req.Messages) + 3) / 4
		decision, err = g.Limiter.Check(ctx, p, est)
		if err != nil {
			log.Error("limit check", "err", err)
			writeError(w, http.StatusInternalServerError, "server_error", "limit check failed")
			return
		}
		setLimitHeaders(w.Header(), p, decision)
		if !decision.Allowed {
			rec := &store.RequestLog{ProjectID: p.ID, ModelName: conn.ModelName, Streamed: req.Stream,
				StatusCode: http.StatusTooManyRequests, Error: decision.Reason, LatencyMs: time.Since(start).Milliseconds()}
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

	// RAG pipeline. The hit count header is set here, before either the
	// JSON or the streaming path writes the status line.
	ragUsed := false
	if p.RAGStoreID != nil && g.Retriever != nil {
		rs, err := g.Store.GetRAGStore(ctx, *p.RAGStoreID)
		if err == nil {
			q := rag.LastUserQuery(req.Messages)
			hits, err := g.Retriever.Search(ctx, rs, q, rs.TopK, prov, conn.ModelName)
			if err != nil {
				log.Warn("rag retrieval failed; continuing without context", "err", err)
			} else if len(hits) > 0 {
				req.Messages = rag.InjectContext(req.Messages, rag.FormatContext(hits))
				ragUsed = true
			}
			w.Header().Set("x-ragmux-rag-hits", strconv.Itoa(len(hits)))
		} else {
			log.Warn("rag store missing", "id", *p.RAGStoreID, "err", err)
		}
	}

	rec := &store.RequestLog{ProjectID: p.ID, ModelName: conn.ModelName, RAGUsed: ragUsed, Streamed: req.Stream}
	promptChars := messageChars(req.Messages)
	defer func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		if err := g.Store.InsertRequestLog(context.Background(), rec); err != nil {
			log.Error("write request log", "err", err)
		}
		if g.Limiter != nil {
			// The minute request slot was already reserved by Check when an
			// RPM limit is set; otherwise count the request here.
			if err := g.Limiter.Record(context.Background(), p.ID, rec.PromptTokens, rec.CompletionTokens, !decision.Reserved); err != nil {
				log.Error("record usage", "err", err)
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
	json.NewEncoder(w).Encode(resp)
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
		errc <- prov.ChatStream(ctx, req, out)
		close(out)
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
// that are configured produce headers.
func setLimitHeaders(h http.Header, p *store.Project, d limits.Decision) {
	if p.RateLimitRPM > 0 {
		h.Set("x-ratelimit-limit-requests", strconv.Itoa(d.LimitRequests))
		h.Set("x-ratelimit-remaining-requests", strconv.Itoa(d.RemainingRequests))
		h.Set("x-ratelimit-reset-requests", strconv.Itoa(ceilSeconds(d.ResetRequests)))
	}
	if p.BudgetDailyTokens > 0 {
		h.Set("x-ragmux-budget-daily-remaining", strconv.FormatInt(max(d.DailyLimit-d.DailyUsed, 0), 10))
	}
	if p.BudgetMonthlyTokens > 0 {
		h.Set("x-ragmux-budget-monthly-remaining", strconv.FormatInt(max(d.MonthlyLimit-d.MonthlyUsed, 0), 10))
	}
}

func ceilSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// writeLimitError answers a denied request with an OpenAI-style 429.
func writeLimitError(w http.ResponseWriter, d limits.Decision) {
	typ, msg := "rate_limit_exceeded", ""
	switch d.Reason {
	case limits.ReasonRPM:
		msg = fmt.Sprintf("Rate limit reached: %d requests per minute for this project.", d.LimitRequests)
	case limits.ReasonTPM:
		msg = "Rate limit reached: tokens per minute for this project."
	case limits.ReasonBudgetDaily:
		typ = "insufficient_quota"
		msg = fmt.Sprintf("Daily token budget exhausted (%d of %d tokens used).", d.DailyUsed, d.DailyLimit)
	case limits.ReasonBudgetMonthly:
		typ = "insufficient_quota"
		msg = fmt.Sprintf("Monthly token budget exhausted (%d of %d tokens used).", d.MonthlyUsed, d.MonthlyLimit)
	default:
		msg = "Rate limit reached for this project."
	}
	retry := ceilSeconds(d.RetryAfter)
	msg += fmt.Sprintf(" Retry after %d seconds.", retry)
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": d.Reason}})
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
