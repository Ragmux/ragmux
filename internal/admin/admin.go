// Package admin exposes the management REST API and the embedded dashboard.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/httpx"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/netguard"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
)

// Admin holds dependencies for management routes.
type Admin struct {
	Store          *store.Store
	Auth           *auth.Service
	Ingester       *rag.Ingester
	Retriever      *rag.Retriever
	Providers      func(conn *store.ModelConnection) (provider.Provider, error)
	Log            *slog.Logger
	MaxUploadBytes int64
	WebFS          fs.FS
	// Limiter throttles login attempts; nil disables limiting.
	Limiter *auth.LoginLimiter
	// Usage reads project rate-limit counters; nil builds one on the Store.
	Usage *limits.Limiter
	// ProviderConfig builds the provider configuration for a connection the
	// same way the gateway does (hardened HTTP client, limits); nil falls
	// back to a bare configuration.
	ProviderConfig func(conn *store.ModelConnection) provider.Config
	// AllowPrivateUpstreams and PrivateAllowlist mirror the netguard policy
	// of the outbound client so a base_url pointing at a private address is
	// rejected when it is saved rather than on first use.
	AllowPrivateUpstreams bool
	PrivateAllowlist      map[string]bool
}

// Body read budgets. JSON handlers get a short one; login shorter still so a
// slow-loris cannot pin the bcrypt path; uploads get room for a large file.
const (
	loginReadDeadline  = 10 * time.Second
	jsonReadDeadline   = 30 * time.Second
	uploadReadDeadline = 5 * time.Minute
)

// Routes mounts /admin handlers.
func (a *Admin) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(requestIDHeader, httpx.NoStore, httpx.ReadDeadline(jsonReadDeadline))
		r.With(httpx.ReadDeadline(loginReadDeadline)).Post("/api/login", a.login)
		// First-run setup: unauthenticated by design, refuses once any user exists.
		r.Get("/api/setup", a.setupStatus)
		r.With(httpx.ReadDeadline(loginReadDeadline)).Post("/api/setup", a.setup)
		r.Group(a.authenticated)
	})
	if a.WebFS != nil {
		fileServer := http.FileServer(http.FS(a.WebFS))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
		})
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			http.StripPrefix("/admin", fileServer).ServeHTTP(w, r)
		})
	}
}

// requestIDHeader echoes chi's request id so error responses and logs can
// be matched; fail reads it back from the header.
func requestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set("X-Request-Id", id)
		}
		next.ServeHTTP(w, r)
	})
}

// authenticated mounts every route behind the session middleware.
func (a *Admin) authenticated(r chi.Router) {
	r.Use(a.Auth.Middleware)
	editor := r.With(auth.RequireRole(auth.RoleEditor))
	adminOnly := r.With(auth.RequireRole(auth.RoleAdmin))

	r.Post("/api/logout", a.logout)
	r.Get("/api/me", a.me)
	r.Post("/api/me/password", a.changePassword)

	r.Get("/api/provider-types", a.providerTypes)

	// Model connections: viewers may read, editors mutate and test (a
	// test spends provider quota and is audited as a write).
	r.Get("/api/models", a.listConnections)
	editor.Post("/api/models", a.createConnection)
	r.Get("/api/models/{id}", a.getConnection)
	editor.Put("/api/models/{id}", a.updateConnection)
	editor.Delete("/api/models/{id}", a.deleteConnection)
	editor.Post("/api/models/{id}/test", a.testConnection)

	// RAG stores and documents: same split; search is a read.
	r.Get("/api/rag-stores", a.listRAGStores)
	editor.Post("/api/rag-stores", a.createRAGStore)
	r.Get("/api/rag-stores/{id}", a.getRAGStore)
	editor.Put("/api/rag-stores/{id}", a.updateRAGStore)
	editor.Delete("/api/rag-stores/{id}", a.deleteRAGStore)
	r.Get("/api/rag-stores/{id}/documents", a.listDocuments)
	editor.With(httpx.ReadDeadline(uploadReadDeadline)).Post("/api/rag-stores/{id}/documents", a.uploadDocument)
	r.Post("/api/rag-stores/{id}/search", a.searchRAGStore)
	editor.Post("/api/rag-stores/{id}/reprocess", a.reprocessStore)
	r.Get("/api/documents/{id}", a.getDocument)
	editor.Delete("/api/documents/{id}", a.deleteDocument)
	editor.Post("/api/documents/{id}/reprocess", a.reprocessDocument)

	// Projects: membership is checked inside the handlers (404 for
	// non-members); mutation additionally needs the editor role.
	r.Get("/api/projects", a.listProjects)
	editor.Post("/api/projects", a.createProject)
	r.Get("/api/projects/{id}", a.getProject)
	editor.Put("/api/projects/{id}", a.updateProject)
	editor.Delete("/api/projects/{id}", a.deleteProject)
	editor.Post("/api/projects/{id}/rotate-key", a.rotateKey)
	r.Get("/api/projects/{id}/metrics", a.projectMetrics)
	r.Get("/api/projects/{id}/usage", a.projectUsage)
	r.Get("/api/projects/{id}/members", a.listMembers)
	editor.Put("/api/projects/{id}/members", a.setMembers)

	r.Get("/api/metrics/summary", a.metricsSummary)
	r.Get("/api/metrics/requests", a.recentRequests)
	r.Get("/api/system", a.systemInfo)

	// User management and the audit trail are admin-only; the lite user
	// list lets editors pick project members.
	editor.Get("/api/users/lite", a.listUsersLite)
	adminOnly.Get("/api/users", a.listUsers)
	adminOnly.Post("/api/users", a.createUser)
	adminOnly.Get("/api/users/{id}", a.getUser)
	adminOnly.Put("/api/users/{id}", a.updateUser)
	adminOnly.Delete("/api/users/{id}", a.deleteUser)
	adminOnly.Post("/api/users/{id}/reset-password", a.resetPassword)
	adminOnly.Post("/api/users/{id}/sessions/revoke", a.revokeSessions)
	adminOnly.Get("/api/audit", a.listAudit)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(status)}})
}

// fail maps store errors to status codes. Unexpected errors are logged with
// the request id and answered with a fixed message that carries only that id,
// so internal details (SQL, hosts, paths) never reach the client.
func (a *Admin) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrLastAdmin):
		writeErr(w, http.StatusConflict, "cannot remove the last active admin")
	case store.IsUniqueViolation(err):
		writeErr(w, http.StatusConflict, "an item with that name already exists")
	case store.IsForeignKeyViolation(err):
		writeErr(w, http.StatusConflict, "item is still referenced by a project or RAG store")
	default:
		reqID := w.Header().Get("X-Request-Id")
		a.Log.Error("admin request failed", "err", err, "req_id", reqID)
		writeErr(w, http.StatusInternalServerError, "internal error (request id "+reqID+")")
	}
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// errUnsupportedMediaType is returned by decode when a cookie-authenticated
// request (or a login) does not declare a JSON body. Browsers cannot send
// application/json cross-origin without a CORS preflight, so requiring it
// closes the simple-request CSRF path for good measure.
var errUnsupportedMediaType = errors.New("Content-Type must be application/json")

func decode(r *http.Request, v any) error {
	if !auth.IsBearer(r) && !isJSON(r) {
		return errUnsupportedMediaType
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

func isJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// badBody answers a failed decode: 415 for a missing JSON content type,
// otherwise 400.
func badBody(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnsupportedMediaType) {
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, "invalid body")
}

// ---- auth ----

// Login field limits, checked before any database or bcrypt work.
const (
	maxUsernameLen      = 64
	maxLoginPasswordLen = 1024
	// maxPasswordLen is bcrypt's input limit; longer passwords are refused
	// when set rather than silently truncated.
	maxPasswordLen = 72
)

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		// Bearer asks for the token in the body (API clients); the dashboard
		// relies on the cookie alone so the token never touches page scripts.
		Bearer bool `json:"bearer"`
	}
	if err := decodeLogin(r, &in); err != nil {
		badBody(w, err)
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > maxUsernameLen || in.Password == "" || len(in.Password) > maxLoginPasswordLen {
		writeErr(w, http.StatusBadRequest, "username (1-64 characters) and password (1-1024 characters) are required")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)
	if !a.allowAttempt(w, r, in.Username, ip) {
		return
	}
	u, tok, err := a.Auth.Login(ctx, in.Username, in.Password)
	if a.Limiter != nil {
		if rerr := a.Limiter.Record(ctx, in.Username, ip, err == nil); rerr != nil {
			a.Log.Error("record login attempt", "err", rerr)
		}
	}
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			a.auditAs(r, nil, "login.failure", "user", nil, map[string]any{"username": in.Username})
			writeErr(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		a.fail(w, err)
		return
	}
	if err := a.Store.TouchLastLogin(ctx, u.ID); err != nil {
		a.Log.Error("touch last login", "err", err)
	}
	a.auditAs(r, u, "login.success", "user", &u.ID, nil)
	a.Auth.SetCookie(w, r, tok)
	out := map[string]any{"user": u}
	if in.Bearer {
		out["token"] = tok
	}
	writeJSON(w, http.StatusOK, out)
}

// allowAttempt consults the login limiter for a username/address pair and
// answers 429 when the pair is throttled or locked out. It returns false
// when the caller must stop. A nil limiter allows everything.
func (a *Admin) allowAttempt(w http.ResponseWriter, r *http.Request, username, ip string) bool {
	if a.Limiter == nil {
		return true
	}
	allowed, retryAfter, locked, err := a.Limiter.Check(r.Context(), username, ip)
	if err != nil {
		a.fail(w, err)
		return false
	}
	if allowed {
		return true
	}
	a.throttled(w, r, username, retryAfter, locked)
	return false
}

// throttled writes the 429 response (with Retry-After) and audits it.
func (a *Admin) throttled(w http.ResponseWriter, r *http.Request, username string, retryAfter time.Duration, locked bool) {
	action := "login.rate_limited"
	if locked {
		action = "login.locked"
	}
	a.auditAs(r, nil, action, "user", nil, map[string]any{"username": username})
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"message": "too many login attempts, try again later", "type": "rate_limited"}})
}

// decodeLogin is decode for the one endpoint that has no session yet: the
// JSON content type is required regardless of an Authorization header.
func decodeLogin(r *http.Request, v any) error {
	if !isJSON(r) {
		return errUnsupportedMediaType
	}
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	_ = a.Auth.Logout(r.Context(), r)
	a.Auth.ClearCookie(w, r)
	a.audit(r, "logout", "user", nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientIP is the request address (from the proxy headers when
// TRUST_PROXY_HEADERS is set) without the port.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// audit records an action performed by the authenticated user. Failures are
// logged rather than surfaced so they never break the request itself.
func (a *Admin) audit(r *http.Request, action, targetType string, targetID *int64, details map[string]any) {
	a.auditAs(r, auth.UserFrom(r.Context()), action, targetType, targetID, details)
}

func (a *Admin) auditAs(r *http.Request, actor *store.User, action, targetType string, targetID *int64, details map[string]any) {
	entry := &store.AuditLog{Action: action, TargetType: targetType, TargetID: targetID, Details: details, IP: clientIP(r)}
	if actor != nil {
		id := actor.ID
		entry.ActorUserID = &id
		entry.ActorUsername = actor.Username
	}
	if err := a.Store.InsertAuditLog(r.Context(), entry); err != nil {
		a.Log.Error("write audit log", "action", action, "err", err)
	}
}

func ptr(id int64) *int64 { return &id }

func (a *Admin) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, auth.UserFrom(r.Context()))
}

func (a *Admin) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if msg := checkNewPassword(in.New); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	u := auth.UserFrom(r.Context())
	if len(in.Current) > maxLoginPasswordLen || !auth.CheckPassword(u.PasswordHash, in.Current) {
		writeErr(w, http.StatusForbidden, "current password is wrong")
		return
	}
	h, err := auth.HashPassword(in.New)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.UpdateUserPassword(r.Context(), u.ID, h); err != nil {
		a.fail(w, err)
		return
	}
	// Every other session of the account is revoked: a changed password is
	// how users react to a suspected leak, and the leaked session must die.
	if err := a.Store.DeleteUserSessionsExcept(r.Context(), u.ID, store.HashToken(auth.TokenFromRequest(r))); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "password.change", "user", ptr(u.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) providerTypes(w http.ResponseWriter, r *http.Request) {
	type pt struct {
		Type       string `json:"type"`
		Label      string `json:"label"`
		DefaultURL string `json:"default_base_url"`
		Embeddings bool   `json:"supports_embeddings"`
		NeedsKey   bool   `json:"requires_api_key"`
	}
	out := []pt{
		{"openai", "OpenAI", "https://api.openai.com/v1", true, true},
		{"anthropic", "Anthropic", "https://api.anthropic.com", false, true},
		{"gemini", "Google Gemini", "https://generativelanguage.googleapis.com/v1beta", true, true},
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", false, true},
		{"ollama", "Ollama", "http://localhost:11434", true, false},
		{"custom_openai", "Custom OpenAI-compatible (vLLM, LM Studio, ...)", "http://localhost:8000/v1", true, false},
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- model connections ----

type connInput struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key"`
	ModelName    string `json:"model_name"`
}

// modelNamePattern bounds model names to the characters providers use; the
// name ends up in request paths (Gemini) and bodies.
var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)

func (in connInput) validate(ctx context.Context, a *Admin) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if !store.IsValidProviderType(in.ProviderType) {
		return fmt.Errorf("provider_type must be one of %s", strings.Join(store.ValidProviderTypes, ", "))
	}
	if strings.TrimSpace(in.ModelName) == "" {
		return errors.New("model_name is required")
	}
	if !modelNamePattern.MatchString(in.ModelName) || strings.Contains(in.ModelName, "..") {
		return errors.New("model_name may only contain letters, digits and . _ : / @ - (max 128 characters, no leading / and no ..)")
	}
	if in.ProviderType == "custom_openai" && in.BaseURL == "" {
		return errors.New("base_url is required for custom_openai")
	}
	if in.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("base_url must be an http:// or https:// URL")
	}
	if u.Hostname() == "" {
		return errors.New("base_url must include a host")
	}
	if u.User != nil {
		return errors.New("base_url must not contain credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return errors.New("base_url must not contain a query string or fragment")
	}
	// Reject a host that only resolves to private addresses now so the
	// admin sees the problem when saving; the dialer checks again on use.
	if err := netguard.CheckHost(ctx, u.Hostname(), a.AllowPrivateUpstreams, a.PrivateAllowlist); err != nil {
		return err
	}
	return nil
}

func (a *Admin) listConnections(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListConnections(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) createConnection(w http.ResponseWriter, r *http.Request) {
	var in connInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := in.validate(r.Context(), a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.Store.CreateConnection(r.Context(), &store.ModelConnection{
		Name: strings.TrimSpace(in.Name), ProviderType: in.ProviderType, BaseURL: strings.TrimSpace(in.BaseURL),
		APIKey: strings.TrimSpace(in.APIKey), ModelName: strings.TrimSpace(in.ModelName)})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "model.create", "model", ptr(c.ID), map[string]any{"name": c.Name, "provider_type": c.ProviderType, "model_name": c.ModelName})
	writeJSON(w, http.StatusCreated, c)
}

func (a *Admin) getConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	c, err := a.Store.GetConnection(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *Admin) updateConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	var in connInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := in.validate(r.Context(), a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.Store.UpdateConnection(r.Context(), &store.ModelConnection{ID: id,
		Name: strings.TrimSpace(in.Name), ProviderType: in.ProviderType, BaseURL: strings.TrimSpace(in.BaseURL),
		APIKey: strings.TrimSpace(in.APIKey), ModelName: strings.TrimSpace(in.ModelName)})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "model.update", "model", ptr(c.ID), map[string]any{"name": c.Name, "provider_type": c.ProviderType,
		"model_name": c.ModelName, "api_key_changed": in.APIKey != ""})
	writeJSON(w, http.StatusOK, c)
}

func (a *Admin) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if err := a.Store.DeleteConnection(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "model.delete", "model", ptr(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// testConnection sends a tiny prompt (or embedding) through the connection.
func (a *Admin) testConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	c, err := a.Store.GetConnection(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	_ = decode(r, &in)
	a.audit(r, "model.test", "model", ptr(c.ID), map[string]any{"mode": in.Mode})
	start := time.Now()
	if in.Mode == "embedding" {
		pc := a.providerConfig(c)
		pc.Timeout = 60 * time.Second
		emb, err := provider.NewEmbedder(pc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		vecs, err := emb.Embed(r.Context(), []string{"ping"})
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dimensions": len(vecs[0]), "latency_ms": time.Since(start).Milliseconds()})
		return
	}
	prov, err := a.Providers(c)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Reasoning models spend part of the budget thinking before answering.
	maxTok := 256
	req := provider.ChatRequest{Model: c.ModelName, MaxTokens: &maxTok,
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("Reply with the single word: pong")}}}
	resp, err := prov.Chat(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds()})
		return
	}
	reply := ""
	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != nil {
		reply = *resp.Choices[0].Message.Content
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reply": reply, "usage": resp.Usage, "latency_ms": time.Since(start).Milliseconds()})
}

// providerConfig builds the provider configuration through the gateway's
// factory when one is wired, so tests use the same hardened client.
func (a *Admin) providerConfig(c *store.ModelConnection) provider.Config {
	if a.ProviderConfig != nil {
		return a.ProviderConfig(c)
	}
	return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName}
}

// ---- RAG stores ----

type ragInput struct {
	Name                  string   `json:"name"`
	EmbeddingConnectionID int64    `json:"embedding_connection_id"`
	ChunkSize             int      `json:"chunk_size"`
	ChunkOverlap          int      `json:"chunk_overlap"`
	TopK                  int      `json:"top_k"`
	SearchMode            string   `json:"search_mode"`
	FTSConfig             string   `json:"fts_config"`
	Rerank                bool     `json:"rerank"`
	RerankCandidates      int      `json:"rerank_candidates"`
	MaxDistance           *float64 `json:"max_distance"`
	// ContextualChunks defaults to true when omitted.
	ContextualChunks *bool `json:"contextual_chunks"`
}

func (a *Admin) validateRAG(r *http.Request, in *ragInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if in.ChunkSize <= 0 {
		in.ChunkSize = 1000
	}
	if in.ChunkSize < 200 || in.ChunkSize > 20000 {
		return errors.New("chunk_size must be between 200 and 20000")
	}
	if in.ChunkOverlap < 0 || in.ChunkOverlap >= in.ChunkSize {
		return errors.New("chunk_overlap must be >= 0 and smaller than chunk_size")
	}
	if in.TopK <= 0 {
		in.TopK = 5
	}
	if in.TopK > 50 {
		return errors.New("top_k must be <= 50")
	}
	if in.SearchMode == "" {
		in.SearchMode = store.SearchHybrid
	}
	if in.SearchMode != store.SearchVector && in.SearchMode != store.SearchHybrid {
		return errors.New("search_mode must be \"vector\" or \"hybrid\"")
	}
	in.FTSConfig = strings.ToLower(strings.TrimSpace(in.FTSConfig))
	if in.FTSConfig == "" {
		in.FTSConfig = "simple"
	}
	if ok, err := a.Store.TextSearchConfigExists(r.Context(), in.FTSConfig); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("fts_config %q is not a text search configuration on this server", in.FTSConfig)
	}
	if in.RerankCandidates <= 0 {
		in.RerankCandidates = 15
	}
	if in.RerankCandidates > 100 {
		return errors.New("rerank_candidates must be between 1 and 100")
	}
	if in.MaxDistance == nil {
		in.MaxDistance = new(float64)
	}
	if *in.MaxDistance < 0 || *in.MaxDistance > 2 {
		return errors.New("max_distance must be between 0 (off) and 2")
	}
	if in.ContextualChunks == nil {
		t := true
		in.ContextualChunks = &t
	}
	conn, err := a.Store.GetConnection(r.Context(), in.EmbeddingConnectionID)
	if err != nil {
		return errors.New("embedding_connection_id does not reference an existing model connection")
	}
	if !provider.SupportsEmbeddings(conn.ProviderType) {
		return fmt.Errorf("provider %q cannot be used for embeddings", conn.ProviderType)
	}
	return nil
}

func (in *ragInput) toStore(id int64) *store.RAGStore {
	return &store.RAGStore{ID: id, Name: strings.TrimSpace(in.Name),
		EmbeddingConnectionID: in.EmbeddingConnectionID, ChunkSize: in.ChunkSize, ChunkOverlap: in.ChunkOverlap, TopK: in.TopK,
		SearchMode: in.SearchMode, FTSConfig: in.FTSConfig, Rerank: in.Rerank, RerankCandidates: in.RerankCandidates,
		MaxDistance: *in.MaxDistance, ContextualChunks: *in.ContextualChunks}
}

func (a *Admin) listRAGStores(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListRAGStores(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) createRAGStore(w http.ResponseWriter, r *http.Request) {
	var in ragInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := a.validateRAG(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rs, err := a.Store.CreateRAGStore(r.Context(), in.toStore(0))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "rag_store.create", "rag_store", ptr(rs.ID), map[string]any{"name": rs.Name})
	writeJSON(w, http.StatusCreated, rs)
}

func (a *Admin) getRAGStore(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	rs, err := a.Store.GetRAGStore(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (a *Admin) updateRAGStore(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	var in ragInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := a.validateRAG(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.Store.GetRAGStore(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	rs, err := a.Store.UpdateRAGStore(r.Context(), in.toStore(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.fail(w, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Chunking and embedding-text settings only take effect when documents
	// are processed again; tell the caller when that is worth doing.
	reprocess := rs.ChunkCount > 0 && (before.ChunkSize != rs.ChunkSize || before.ChunkOverlap != rs.ChunkOverlap ||
		before.ContextualChunks != rs.ContextualChunks)
	a.audit(r, "rag_store.update", "rag_store", ptr(rs.ID), map[string]any{"name": rs.Name, "reprocess_recommended": reprocess})
	writeJSON(w, http.StatusOK, ragStoreResponse{RAGStore: rs, ReprocessRecommended: reprocess})
}

// ragStoreResponse is a store plus the reprocess hint returned by updates.
type ragStoreResponse struct {
	*store.RAGStore
	ReprocessRecommended bool `json:"reprocess_recommended"`
}

func (a *Admin) reprocessStore(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if _, err := a.Store.GetRAGStore(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	ids, err := a.Store.ResetStoreDocuments(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	queued := 0
	for i, docID := range ids {
		if err := a.Ingester.Enqueue(docID); err != nil {
			// Everything not queued yet would otherwise sit in "pending"
			// until the next restart; mark it so the dashboard shows why.
			for _, rest := range ids[i:] {
				_ = a.Store.SetDocumentStatus(r.Context(), rest, store.DocFailed, err.Error())
			}
			a.audit(r, "rag_store.reprocess_all", "rag_store", ptr(id), map[string]any{"documents": len(ids), "queued": queued, "error": err.Error()})
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		queued++
	}
	a.audit(r, "rag_store.reprocess_all", "rag_store", ptr(id), map[string]any{"documents": len(ids)})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "documents": len(ids)})
}

func (a *Admin) deleteRAGStore(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if err := a.Store.DeleteRAGStore(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "rag_store.delete", "rag_store", ptr(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) searchRAGStore(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	rs, err := a.Store.GetRAGStore(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	// Overrides let the dashboard try settings before saving them.
	var in struct {
		Query       string   `json:"query"`
		TopK        int      `json:"top_k"`
		Mode        string   `json:"mode"`
		Rerank      *bool    `json:"rerank"`
		MaxDistance *float64 `json:"max_distance"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if in.Mode != "" {
		if in.Mode != store.SearchVector && in.Mode != store.SearchHybrid {
			writeErr(w, http.StatusBadRequest, "mode must be \"vector\" or \"hybrid\"")
			return
		}
		rs.SearchMode = in.Mode
	}
	if in.Rerank != nil {
		rs.Rerank = *in.Rerank
	}
	// top_k is clamped to the store's own bounds; 0 means "use the store's
	// top_k", which validateRAG already keeps within 1..50.
	if in.TopK < 0 {
		in.TopK = 0
	}
	if in.TopK > 50 {
		in.TopK = 50
	}
	if in.MaxDistance != nil {
		if *in.MaxDistance < 0 || *in.MaxDistance > 2 {
			writeErr(w, http.StatusBadRequest, "max_distance must be between 0 (off) and 2")
			return
		}
		rs.MaxDistance = *in.MaxDistance
	}
	// Reranking uses the chat model of a project linked to this store; the
	// embedding connection cannot chat.
	var prov provider.Provider
	model := ""
	if rs.Rerank {
		if conn, err := a.rerankConnection(r, rs.ID); err == nil {
			if p, err := a.Providers(conn); err == nil {
				prov, model = p, conn.ModelName
			}
		}
	}
	start := time.Now()
	res, err := a.Retriever.SearchWith(r.Context(), rs, in.Query, in.TopK, prov, model)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": res.Hits, "mode": res.Mode, "reranked": res.Reranked,
		"latency_ms": time.Since(start).Milliseconds()})
}

// rerankConnection finds a chat connection for reranking searches from the
// admin API: the model connection of the first project using the store.
func (a *Admin) rerankConnection(r *http.Request, storeID int64) (*store.ModelConnection, error) {
	projects, err := a.Store.ListProjects(r.Context())
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.RAGStoreID != nil && *p.RAGStoreID == storeID {
			return a.Store.GetConnection(r.Context(), p.ModelConnectionID)
		}
	}
	return nil, store.ErrNotFound
}

// ---- documents ----

func (a *Admin) listDocuments(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if _, err := a.Store.GetRAGStore(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	docs, err := a.Store.ListDocuments(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func (a *Admin) uploadDocument(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if _, err := a.Store.GetRAGStore(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	max := a.MaxUploadBytes
	if max <= 0 {
		max = 50 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := r.ParseMultipartForm(8 << 20); err != nil { //nolint:gosec // G120: the body is bounded by MaxBytesReader above
		writeErr(w, http.StatusBadRequest, "multipart form too large or malformed: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		files = r.MultipartForm.File["files"]
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "no file field in form (use 'file')")
		return
	}
	var created []*store.Document
	for _, fh := range files {
		name := filepath.Base(fh.Filename)
		if !rag.IsSupported(name) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported file type for %q (pdf, docx, html, txt, md)", name))
			return
		}
		src, err := fh.Open()
		if err != nil {
			a.fail(w, err)
			return
		}
		// The whole request body is already bounded by MaxBytesReader, so a
		// single file can never exceed the upload limit.
		data, err := io.ReadAll(io.LimitReader(src, max))
		_ = src.Close()
		if err != nil {
			a.fail(w, err)
			return
		}
		if !rag.SniffOK(name, data) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q does not look like a %s file", name, strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")))
			return
		}
		doc, err := a.Store.CreateDocument(r.Context(), &store.Document{RAGStoreID: id, Filename: name,
			Mime: fh.Header.Get("Content-Type"), SizeBytes: int64(len(data))}, data)
		if err != nil {
			a.fail(w, err)
			return
		}
		if err := a.Ingester.Enqueue(doc.ID); err != nil {
			_ = a.Store.SetDocumentStatus(r.Context(), doc.ID, store.DocFailed, err.Error())
			a.audit(r, "document.upload", "document", ptr(doc.ID), map[string]any{"rag_store_id": id, "filename": name, "size_bytes": len(data), "error": err.Error()})
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		a.audit(r, "document.upload", "document", ptr(doc.ID), map[string]any{"rag_store_id": id, "filename": name, "size_bytes": len(data)})
		created = append(created, doc)
	}
	if len(created) == 1 {
		writeJSON(w, http.StatusAccepted, created[0])
		return
	}
	writeJSON(w, http.StatusAccepted, created)
}

func (a *Admin) getDocument(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	d, err := a.Store.GetDocument(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *Admin) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	if err := a.Store.DeleteDocument(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "document.delete", "document", ptr(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) reprocessDocument(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	d, err := a.Store.GetDocument(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.SetDocumentStatus(r.Context(), d.ID, store.DocPending, ""); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Ingester.Enqueue(d.ID); err != nil {
		_ = a.Store.SetDocumentStatus(r.Context(), d.ID, store.DocFailed, err.Error())
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	a.audit(r, "document.reprocess", "document", ptr(d.ID), nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// ---- projects ----

type projectInput struct {
	Name                string  `json:"name"`
	ModelConnectionID   int64   `json:"model_connection_id"`
	RAGStoreID          *int64  `json:"rag_store_id"`
	SystemPrompt        string  `json:"system_prompt"`
	MemberUserIDs       []int64 `json:"member_user_ids"`
	RateLimitRPM        int     `json:"rate_limit_rpm"`
	RateLimitTPM        int     `json:"rate_limit_tpm"`
	BudgetDailyTokens   int64   `json:"budget_daily_tokens"`
	BudgetMonthlyTokens int64   `json:"budget_monthly_tokens"`
}

// project builds the store record from validated input (id and key aside).
func (in *projectInput) project() *store.Project {
	return &store.Project{Name: strings.TrimSpace(in.Name), ModelConnectionID: in.ModelConnectionID,
		RAGStoreID: in.RAGStoreID, SystemPrompt: in.SystemPrompt,
		RateLimitRPM: in.RateLimitRPM, RateLimitTPM: in.RateLimitTPM,
		BudgetDailyTokens: in.BudgetDailyTokens, BudgetMonthlyTokens: in.BudgetMonthlyTokens}
}

// limitFields lists the limit columns with their current values.
func limitFields(p *store.Project) map[string]int64 {
	return map[string]int64{"rate_limit_rpm": int64(p.RateLimitRPM), "rate_limit_tpm": int64(p.RateLimitTPM),
		"budget_daily_tokens": p.BudgetDailyTokens, "budget_monthly_tokens": p.BudgetMonthlyTokens}
}

// loadProject fetches a project the caller may see; non-members get 404.
func (a *Admin) loadProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	id, _ := idParam(r)
	ok, err := auth.CanAccessProject(r.Context(), a.Store, auth.UserFrom(r.Context()), id)
	if err != nil {
		a.fail(w, err)
		return nil, false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return nil, false
	}
	p, err := a.Store.GetProject(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return nil, false
	}
	return p, true
}

func (a *Admin) validateProject(r *http.Request, in *projectInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if _, err := a.Store.GetConnection(r.Context(), in.ModelConnectionID); err != nil {
		return errors.New("model_connection_id does not reference an existing model connection")
	}
	if in.RAGStoreID != nil {
		if *in.RAGStoreID == 0 {
			in.RAGStoreID = nil
		} else if _, err := a.Store.GetRAGStore(r.Context(), *in.RAGStoreID); err != nil {
			return errors.New("rag_store_id does not reference an existing RAG store")
		}
	}
	if in.RateLimitRPM < 0 || in.RateLimitTPM < 0 || in.BudgetDailyTokens < 0 || in.BudgetMonthlyTokens < 0 {
		return errors.New("rate limits and budgets must be 0 (unlimited) or positive")
	}
	return nil
}

func (a *Admin) listProjects(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	var list []*store.Project
	var err error
	if auth.Role(u.Role).AtLeast(auth.RoleAdmin) {
		list, err = a.Store.ListProjects(r.Context())
	} else {
		list, err = a.Store.ListProjectsForUser(r.Context(), u.ID)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) createProject(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := a.validateProject(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u := auth.UserFrom(r.Context())
	// The creator is always a member so editors keep access to what they
	// made; project and members are written in one transaction.
	members := append([]int64{u.ID}, in.MemberUserIDs...)
	p, key, err := a.Store.CreateProjectWithMembers(r.Context(), in.project(), members)
	if err != nil {
		if store.IsForeignKeyViolation(err) {
			writeErr(w, http.StatusBadRequest, "member_user_ids contains an unknown user")
			return
		}
		a.fail(w, err)
		return
	}
	a.audit(r, "project.create", "project", ptr(p.ID), map[string]any{"name": p.Name, "member_ids": p.MemberIDs, "limits": limitFields(p)})
	writeJSON(w, http.StatusCreated, map[string]any{"project": p, "api_key": key})
}

func (a *Admin) getProject(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *Admin) updateProject(w http.ResponseWriter, r *http.Request) {
	before, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	id, _ := idParam(r)
	var in projectInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := a.validateProject(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	np := in.project()
	np.ID = id
	p, err := a.Store.UpdateProject(r.Context(), np)
	if err != nil {
		a.fail(w, err)
		return
	}
	details := map[string]any{"name": p.Name}
	old, cur := limitFields(before), limitFields(p)
	changed := map[string]any{}
	for k, v := range cur {
		if old[k] != v {
			changed[k] = map[string]int64{"from": old[k], "to": v}
		}
	}
	if len(changed) > 0 {
		details["limits_changed"] = changed
	}
	a.audit(r, "project.update", "project", ptr(p.ID), details)
	writeJSON(w, http.StatusOK, p)
}

func (a *Admin) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteProject(r.Context(), p.ID); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.delete", "project", ptr(p.ID), map[string]any{"name": p.Name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) rotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	key, err := a.Store.RotateProjectKey(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	p, _ = a.Store.GetProject(r.Context(), p.ID)
	a.audit(r, "project.rotate_key", "project", ptr(p.ID), map[string]any{"name": p.Name})
	writeJSON(w, http.StatusOK, map[string]any{"project": p, "api_key": key})
}

func (a *Admin) listMembers(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	list, err := a.Store.ListProjectMembers(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// setMembers replaces the member set. Editors must keep themselves in it so
// they cannot lock themselves out of a project they manage.
func (a *Admin) setMembers(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	var in struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	u := auth.UserFrom(r.Context())
	if !auth.Role(u.Role).AtLeast(auth.RoleAdmin) && !containsID(in.UserIDs, u.ID) {
		writeErr(w, http.StatusBadRequest, "you cannot remove yourself from a project")
		return
	}
	if err := a.Store.SetProjectMembers(r.Context(), p.ID, in.UserIDs); err != nil {
		if store.IsForeignKeyViolation(err) {
			writeErr(w, http.StatusBadRequest, "user_ids contains an unknown user")
			return
		}
		a.fail(w, err)
		return
	}
	list, err := a.Store.ListProjectMembers(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.members_update", "project", ptr(p.ID), map[string]any{"name": p.Name, "member_ids": in.UserIDs})
	writeJSON(w, http.StatusOK, list)
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// ---- metrics ----

func sinceParam(r *http.Request) time.Time {
	d := 24 * time.Hour
	if v := r.URL.Query().Get("window"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return time.Now().Add(-d)
}

func (a *Admin) projectMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	a.metrics(w, r, store.MetricsFilter{ProjectID: &p.ID})
}

// projectUsage reports the current rate-limit and budget counters.
func (a *Admin) projectUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	l := a.Usage
	if l == nil {
		l = &limits.Limiter{Store: a.Store}
	}
	u, err := l.Usage(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// scopedFilter restricts global metrics to member projects for non-admins.
func scopedFilter(r *http.Request) store.MetricsFilter {
	u := auth.UserFrom(r.Context())
	if auth.Role(u.Role).AtLeast(auth.RoleAdmin) {
		return store.MetricsFilter{}
	}
	return store.MetricsFilter{UserID: &u.ID}
}

func (a *Admin) metricsSummary(w http.ResponseWriter, r *http.Request) {
	a.metrics(w, r, scopedFilter(r))
}

func (a *Admin) metrics(w http.ResponseWriter, r *http.Request, f store.MetricsFilter) {
	ctx := r.Context()
	window, err := a.Store.Summarize(ctx, f, sinceParam(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	total, err := a.Store.Summarize(ctx, f, time.Time{})
	if err != nil {
		a.fail(w, err)
		return
	}
	daily, err := a.Store.DailySeries(ctx, f, 14)
	if err != nil {
		a.fail(w, err)
		return
	}
	recent, err := a.Store.RecentRequests(ctx, f, 50)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": window, "total": total, "daily": daily, "recent": recent})
}

func (a *Admin) recentRequests(w http.ResponseWriter, r *http.Request) {
	f := scopedFilter(r)
	if v := r.URL.Query().Get("project_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad project_id")
			return
		}
		ok, err := auth.CanAccessProject(r.Context(), a.Store, auth.UserFrom(r.Context()), id)
		if err != nil {
			a.fail(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		f = store.MetricsFilter{ProjectID: &id}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.Store.RecentRequests(r.Context(), f, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) systemInfo(w http.ResponseWriter, r *http.Request) {
	info, err := a.Store.DatabaseInfo(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	backup, err := a.Store.BackupInfo(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database":          info,
		"backup":            backup,
		"secret_key_source": a.Store.SecretKeySource,
		"version":           Version,
	})
}

// Version is stamped at build time via -ldflags.
var Version = "dev"
