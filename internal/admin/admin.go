// Package admin exposes the management REST API and the embedded dashboard.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/limits"
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
}

// Routes mounts /admin handlers.
func (a *Admin) Routes(r chi.Router) {
	r.Post("/api/login", a.login)
	r.Group(func(r chi.Router) {
		r.Use(a.Auth.Middleware)
		editor := r.With(auth.RequireRole(auth.RoleEditor))
		adminOnly := r.With(auth.RequireRole(auth.RoleAdmin))

		r.Post("/api/logout", a.logout)
		r.Get("/api/me", a.me)
		r.Post("/api/me/password", a.changePassword)

		r.Get("/api/provider-types", a.providerTypes)

		// Model connections: viewers may read and test, editors mutate.
		r.Get("/api/models", a.listConnections)
		editor.Post("/api/models", a.createConnection)
		r.Get("/api/models/{id}", a.getConnection)
		editor.Put("/api/models/{id}", a.updateConnection)
		editor.Delete("/api/models/{id}", a.deleteConnection)
		r.Post("/api/models/{id}/test", a.testConnection)

		// RAG stores and documents: same split; search is a read.
		r.Get("/api/rag-stores", a.listRAGStores)
		editor.Post("/api/rag-stores", a.createRAGStore)
		r.Get("/api/rag-stores/{id}", a.getRAGStore)
		editor.Put("/api/rag-stores/{id}", a.updateRAGStore)
		editor.Delete("/api/rag-stores/{id}", a.deleteRAGStore)
		r.Get("/api/rag-stores/{id}/documents", a.listDocuments)
		editor.Post("/api/rag-stores/{id}/documents", a.uploadDocument)
		r.Post("/api/rag-stores/{id}/search", a.searchRAGStore)
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

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(status)}})
}

func (a *Admin) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case store.IsUniqueViolation(err):
		writeErr(w, http.StatusConflict, "an item with that name already exists")
	case store.IsForeignKeyViolation(err):
		writeErr(w, http.StatusConflict, "item is still referenced by a project or RAG store")
	default:
		a.Log.Error("admin request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

// ---- auth ----

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)
	if a.Limiter != nil {
		allowed, retryAfter, locked, err := a.Limiter.Check(ctx, in.Username, ip)
		if err != nil {
			a.fail(w, err)
			return
		}
		if !allowed {
			action := "login.rate_limited"
			if locked {
				action = "login.locked"
			}
			a.auditAs(r, nil, action, "user", nil, map[string]any{"username": in.Username})
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
				"message": "too many login attempts, try again later", "type": "rate_limited"}})
			return
		}
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
	a.Auth.SetCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": u})
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	_ = a.Auth.Logout(r.Context(), r)
	a.Auth.ClearCookie(w)
	a.audit(r, "logout", "user", nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientIP is the address middleware.RealIP resolved, without the port.
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
	if err := decode(r, &in); err != nil || len(in.New) < 8 {
		writeErr(w, http.StatusBadRequest, "new_password must be at least 8 characters")
		return
	}
	u := auth.UserFrom(r.Context())
	if !auth.CheckPassword(u.PasswordHash, in.Current) {
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

func (in connInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if !store.IsValidProviderType(in.ProviderType) {
		return fmt.Errorf("provider_type must be one of %s", strings.Join(store.ValidProviderTypes, ", "))
	}
	if strings.TrimSpace(in.ModelName) == "" {
		return errors.New("model_name is required")
	}
	if in.BaseURL != "" && !strings.HasPrefix(in.BaseURL, "http://") && !strings.HasPrefix(in.BaseURL, "https://") {
		return errors.New("base_url must start with http:// or https://")
	}
	if in.ProviderType == "custom_openai" && in.BaseURL == "" {
		return errors.New("base_url is required for custom_openai")
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
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := in.validate(); err != nil {
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
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := in.validate(); err != nil {
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
		emb, err := provider.NewEmbedder(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName, Timeout: 60 * time.Second})
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
	maxTok := 16
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

// ---- RAG stores ----

type ragInput struct {
	Name                  string `json:"name"`
	EmbeddingConnectionID int64  `json:"embedding_connection_id"`
	ChunkSize             int    `json:"chunk_size"`
	ChunkOverlap          int    `json:"chunk_overlap"`
	TopK                  int    `json:"top_k"`
}

func (a *Admin) validateRAG(r *http.Request, in *ragInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if in.ChunkSize <= 0 {
		in.ChunkSize = 1000
	}
	if in.ChunkSize < 100 || in.ChunkSize > 20000 {
		return errors.New("chunk_size must be between 100 and 20000")
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
	conn, err := a.Store.GetConnection(r.Context(), in.EmbeddingConnectionID)
	if err != nil {
		return errors.New("embedding_connection_id does not reference an existing model connection")
	}
	if !provider.SupportsEmbeddings(conn.ProviderType) {
		return fmt.Errorf("provider %q cannot be used for embeddings", conn.ProviderType)
	}
	return nil
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
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.validateRAG(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rs, err := a.Store.CreateRAGStore(r.Context(), &store.RAGStore{Name: strings.TrimSpace(in.Name),
		EmbeddingConnectionID: in.EmbeddingConnectionID, ChunkSize: in.ChunkSize, ChunkOverlap: in.ChunkOverlap, TopK: in.TopK})
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
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.validateRAG(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rs, err := a.Store.UpdateRAGStore(r.Context(), &store.RAGStore{ID: id, Name: strings.TrimSpace(in.Name),
		EmbeddingConnectionID: in.EmbeddingConnectionID, ChunkSize: in.ChunkSize, ChunkOverlap: in.ChunkOverlap, TopK: in.TopK})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.fail(w, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.audit(r, "rag_store.update", "rag_store", ptr(rs.ID), map[string]any{"name": rs.Name})
	writeJSON(w, http.StatusOK, rs)
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
	var in struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	start := time.Now()
	hits, err := a.Retriever.Search(r.Context(), rs, in.Query, in.TopK)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if hits == nil {
		hits = []store.SearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "latency_ms": time.Since(start).Milliseconds()})
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
	if err := r.ParseMultipartForm(8 << 20); err != nil {
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
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported file type for %q (pdf, txt, md)", name))
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
		src.Close()
		if err != nil {
			a.fail(w, err)
			return
		}
		doc, err := a.Store.CreateDocument(r.Context(), &store.Document{RAGStoreID: id, Filename: name,
			Mime: fh.Header.Get("Content-Type"), SizeBytes: int64(len(data))}, data)
		if err != nil {
			a.fail(w, err)
			return
		}
		a.Ingester.Enqueue(doc.ID)
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
	a.Ingester.Enqueue(d.ID)
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
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.validateProject(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u := auth.UserFrom(r.Context())
	p, key, err := a.Store.CreateProject(r.Context(), in.project())
	if err != nil {
		a.fail(w, err)
		return
	}
	// The creator is always a member so editors keep access to what they made.
	members := append([]int64{u.ID}, in.MemberUserIDs...)
	if err := a.Store.SetProjectMembers(r.Context(), p.ID, members); err != nil {
		if store.IsForeignKeyViolation(err) {
			writeErr(w, http.StatusBadRequest, "member_user_ids contains an unknown user")
			return
		}
		a.fail(w, err)
		return
	}
	if p, err = a.Store.GetProject(r.Context(), p.ID); err != nil {
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
		writeErr(w, http.StatusBadRequest, "invalid body")
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
		writeErr(w, http.StatusBadRequest, "invalid body")
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
	writeJSON(w, http.StatusOK, map[string]any{
		"database":          info,
		"secret_key_source": a.Store.SecretKeySource,
		"version":           Version,
	})
}

// Version is stamped at build time via -ldflags.
var Version = "dev"
