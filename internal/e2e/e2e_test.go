package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// mockUpstream is an OpenAI-compatible server that embeds by keyword and
// echoes the system prompt it received, so the test can verify RAG injection.
func mockUpstream(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			json.Unmarshal(body, &req)
			data := make([]map[string]any, len(req.Input))
			for i, in := range req.Input {
				data[i] = map[string]any{"index": i, "embedding": keywordVec(in)}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/v1/chat/completions":
			var req provider.ChatRequest
			json.Unmarshal(body, &req)
			sys, lastUser := "", ""
			for _, m := range req.Messages {
				if m.Role == "system" {
					sys = m.Text()
				}
				if m.Role == "user" {
					lastUser = m.Text()
				}
			}
			// Rerank prompts get the passages back in reverse order.
			if strings.Contains(lastUser, "Return only a JSON array") {
				n := 0
				for _, m := range passageRe.FindAllStringSubmatch(lastUser, -1) {
					if v, _ := strconv.Atoi(m[1]); v > n {
						n = v
					}
				}
				var order []string
				for i := n; i >= 1; i-- {
					order = append(order, strconv.Itoa(i))
				}
				json.NewEncoder(w).Encode(map[string]any{"id": "r", "model": req.Model,
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "[" + strings.Join(order, ",") + "]"}, "finish_reason": "stop"}}})
				return
			}
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "SYS:" + sys}}}}))
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"id": "c", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 11, "completion_tokens": 3}}))
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "SYS:" + sys}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}})
		default:
			http.NotFound(w, r)
		}
	}))
}

var passageRe = regexp.MustCompile(`(?m)^\[(\d+)\] `)

func keywordVec(s string) []float32 {
	s = strings.ToLower(s)
	v := make([]float32, 4)
	for i, kw := range []string{"apple", "banana", "cherry", "durian"} {
		if strings.Contains(s, kw) {
			v[i] = 1
		}
	}
	if v[0]+v[1]+v[2]+v[3] == 0 {
		v[3] = 0.01
	}
	return v
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type env struct {
	t       *testing.T
	srv     *httptest.Server
	session string
	store   *store.Store
	usage   *limits.Limiter
}

// newEnv wires the whole application against the schema described by cfg.
// Calling it twice with the same cfg is the test's equivalent of restarting
// the container against the same database.
func newEnv(t *testing.T, cfg store.OpenConfig) *env {
	return newEnvWith(t, cfg, true, nil)
}

// newEnvWith is newEnv with control over the bootstrap admin: with
// bootstrap false the users table is left as it is (empty for a fresh
// schema) and no session is opened, which is how the first-run setup flow
// is exercised.
// tune, when set, adjusts the admin before it is served (settings main.go
// takes from the configuration).
func newEnvWith(t *testing.T, cfg store.OpenConfig, bootstrap bool, tune func(*admin.Admin)) *env {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := testdb.OpenWith(t, cfg)
	if n, _ := st.CountUsers(ctx); n == 0 && bootstrap {
		h, _ := auth.HashPassword("password123")
		if _, err := st.CreateUser(ctx, "admin", h, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	provCfg := func(c *store.ModelConnection) provider.Config {
		return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName}
	}
	providers := func(c *store.ModelConnection) (provider.Provider, error) { return provider.New(provCfg(c)) }
	embedders := func(c *store.ModelConnection) (provider.Embedder, error) { return provider.NewEmbedder(provCfg(c)) }
	ing := rag.NewIngester(ctx, st, embedders, 1, log)
	t.Cleanup(ing.Stop)
	ing.Resume(ctx)
	ret := rag.NewRetriever(st, embedders)
	authSvc := &auth.Service{Store: st, TTL: time.Hour}
	usage := &limits.Limiter{Store: st}
	gw := &gateway.Gateway{Store: st, Providers: providers, Retriever: ret, Log: log, Limiter: usage}
	// The mock upstreams listen on loopback, which the save-time base_url
	// check would otherwise reject.
	adm := &admin.Admin{Store: st, Auth: authSvc, Ingester: ing, Retriever: ret, Providers: providers, Log: log,
		Limiter: auth.DefaultLoginLimiter(st), Usage: usage, ProviderConfig: provCfg, AllowPrivateUpstreams: true}
	if tune != nil {
		tune(adm)
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Route("/v1", gw.Routes)
	r.Route("/admin", adm.Routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, store: st, usage: usage}
	if !bootstrap {
		return e
	}
	res := e.call("POST", "/admin/api/login", map[string]any{"username": "admin", "password": "password123", "bearer": true}, "")
	e.session = res["token"].(string)
	return e
}

func (e *env) call(method, path string, body any, bearer string) map[string]any {
	e.t.Helper()
	out, _ := e.callRaw(method, path, body, bearer)
	return out
}

// callRaw is call plus the response headers (for Retry-After).
func (e *env) callRaw(method, path string, body any, bearer string) (map[string]any, http.Header) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(mustJSON(body))
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if bearer == "" {
		bearer = e.session
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		var arr []any
		if json.Unmarshal(raw, &arr) == nil {
			return map[string]any{"_list": arr, "_status": float64(resp.StatusCode)}, resp.Header
		}
		e.t.Fatalf("%s %s: non-JSON response %d: %s", method, path, resp.StatusCode, raw)
	}
	out["_status"] = float64(resp.StatusCode)
	return out, resp.Header
}

func (e *env) login(username, password string) string {
	e.t.Helper()
	r := e.call("POST", "/admin/api/login", map[string]any{"username": username, "password": password, "bearer": true}, "x")
	if r["_status"] != float64(200) {
		e.t.Fatalf("login %s: %v", username, r)
	}
	return r["token"].(string)
}

func (e *env) upload(storeID int64, name, content string) map[string]any {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/admin/api/rag-stores/%d/documents", e.srv.URL, storeID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+e.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	out["_status"] = float64(resp.StatusCode)
	return out
}

func (e *env) waitReady(docID int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", docID), nil, "")
		switch d["status"] {
		case "ready":
			return
		case "failed":
			e.t.Fatalf("document failed: %v", d["error"])
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("document never became ready")
}

func TestFullPipelineAndPersistence(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	cfg := testdb.Config(t)
	e := newEnv(t, cfg)

	// Unauthenticated admin call is rejected.
	if r := e.call("GET", "/admin/api/models", nil, "nope"); r["_status"] != float64(401) {
		t.Fatalf("expected 401, got %v", r["_status"])
	}

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) || conn["api_key_masked"] == "secret" {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))

	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	docID := int64(doc["id"].(float64))
	if doc["progress_percent"] != float64(0) || doc["page_count"] != nil {
		t.Errorf("fresh document: %v", doc)
	}
	e.waitReady(docID)
	if d := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", docID), nil, ""); d["progress_percent"] != float64(100) || d["page_count"] != nil {
		t.Errorf("ready document: %v", d)
	}

	// Vector search returns the banana chunk for a banana query.
	sr := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID), map[string]any{"query": "tell me about banana"}, "")
	hits := sr["hits"].([]any)
	if len(hits) == 0 || !strings.Contains(hits[0].(map[string]any)["content"].(string), "Banana") {
		t.Fatalf("search hits: %v", hits)
	}
	if _, ok := sr["retrieval_latency_ms"].(float64); !ok || sr["rerank_latency_ms"] != nil {
		t.Errorf("search latencies without rerank: %v", sr)
	}

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	apiKey := proj["api_key"].(string)
	projID := int64(proj["project"].(map[string]any)["id"].(float64))

	// Chat through the gateway: RAG context must reach the upstream system prompt.
	chat := e.call("POST", "/v1/chat/completions", map[string]any{"model": "whatever", "messages": []map[string]string{{"role": "user", "content": "What color is a banana?"}}}, apiKey)
	if chat["_status"] != float64(200) {
		t.Fatalf("chat: %v", chat)
	}
	content := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "Bananas are yellow") || strings.Contains(content, "Cherries") {
		t.Fatalf("rag context not injected correctly: %q", content)
	}
	if chat["model"] != "whatever" {
		t.Errorf("model should echo client's value, got %v", chat["model"])
	}

	// Wrong key rejected.
	if r := e.call("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}}, "sk-proj-bogus"); r["_status"] != float64(401) {
		t.Errorf("expected 401 for bad key, got %v", r["_status"])
	}

	// Streaming path.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(mustJSON(map[string]any{"stream": true,
		"messages": []map[string]string{{"role": "user", "content": "apple?"}}})))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type %q body %s", ct, raw)
	}
	if !strings.Contains(string(raw), "Apples are red") || !strings.HasSuffix(strings.TrimSpace(string(raw)), "data: [DONE]") {
		t.Fatalf("stream body: %s", raw)
	}

	// Metrics recorded.
	time.Sleep(50 * time.Millisecond)
	m := e.call("GET", fmt.Sprintf("/admin/api/projects/%d/metrics", projID), nil, "")
	w := m["window"].(map[string]any)
	if w["requests"] != float64(2) || w["prompt_tokens"] != float64(21) || w["rag_requests"] != float64(2) {
		t.Errorf("metrics: %v", w)
	}

	// Rotate key: old one dies.
	rot := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/rotate-key", projID), nil, "")
	newKey := rot["api_key"].(string)
	if r := e.call("GET", "/v1/models", nil, apiKey); r["_status"] != float64(401) {
		t.Errorf("old key should be rejected: %v", r)
	}
	if r := e.call("GET", "/v1/models", nil, newKey); r["_status"] != float64(200) {
		t.Errorf("new key should work: %v", r)
	}

	// Simulate a container restart: close the pool and reopen everything on
	// the same database with the same SECRET_KEY.
	e.srv.Close()
	e.store.Close()
	e2 := newEnv(t, cfg)
	if r := e2.call("GET", "/v1/models", nil, newKey); r["_status"] != float64(200) {
		t.Fatalf("project key lost after restart: %v", r)
	}
	chat2 := e2.call("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "cherry?"}}}, newKey)
	c2 := chat2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(c2, "Cherries are small") {
		t.Fatalf("vectors lost after restart: %q", c2)
	}
	// Documents, their raw content and the encrypted provider key all live
	// in the database and are still usable.
	d := e2.call("GET", fmt.Sprintf("/admin/api/documents/%d", docID), nil, "")
	if d["status"] != "ready" || d["chunk_count"] != float64(3) {
		t.Errorf("document after restart: %v", d)
	}
	stored, err := e2.store.DocumentContent(context.Background(), docID)
	if err != nil || !strings.Contains(string(stored), "# Banana") {
		t.Errorf("document content after restart: %v %q", err, stored)
	}
	models := e2.call("GET", "/admin/api/models", nil, "")
	if l := models["_list"].([]any); len(l) != 1 || l[0].(map[string]any)["api_key_masked"] != "****" {
		t.Errorf("connections after restart: %v", models)
	}
	sys := e2.call("GET", "/admin/api/system", nil, "")
	db := sys["database"].(map[string]any)
	if db["pgvector_version"] == "" || db["migrations_version"] != float64(12) || sys["secret_key_source"] != "env" {
		t.Errorf("system info: %v", sys)
	}

	// Reprocessing re-reads the stored bytes and rebuilds the chunks.
	if r := e2.call("POST", fmt.Sprintf("/admin/api/documents/%d/reprocess", docID), nil, ""); r["_status"] != float64(202) {
		t.Fatalf("reprocess: %v", r)
	}
	e2.waitReady(docID)
	sr2 := e2.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID), map[string]any{"query": "cherry"}, "")
	if hits := sr2["hits"].([]any); len(hits) == 0 || !strings.Contains(hits[0].(map[string]any)["content"].(string), "Cherr") {
		t.Errorf("search after reprocess: %v", hits)
	}

	// Deleting the store cascades through documents, chunks and embeddings.
	if r := e2.call("DELETE", fmt.Sprintf("/admin/api/rag-stores/%d", storeID), nil, ""); r["_status"] != float64(200) {
		t.Fatalf("delete store: %v", r)
	}
	if r := e2.call("GET", fmt.Sprintf("/admin/api/documents/%d", docID), nil, ""); r["_status"] != float64(404) {
		t.Errorf("document should be gone with its store: %v", r)
	}
}

func TestRolesRateLimitAndAudit(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	me := e.call("GET", "/admin/api/me", nil, "")
	if me["role"] != "admin" || me["is_active"] != true {
		t.Fatalf("bootstrap user should be an active admin: %v", me)
	}
	adminID := int64(me["id"].(float64))

	// Admin creates an editor and a viewer.
	ed := e.call("POST", "/admin/api/users", map[string]any{"username": "ed", "password": "editorpass", "role": "editor"}, "")
	vw := e.call("POST", "/admin/api/users", map[string]any{"username": "vw", "password": "viewerpass", "role": "viewer"}, "")
	if status(ed) != 201 || status(vw) != 201 || ed["role"] != "editor" || vw["role"] != "viewer" {
		t.Fatalf("create users: %v %v", ed, vw)
	}
	editorID, viewerID := int64(ed["id"].(float64)), int64(vw["id"].(float64))
	if r := e.call("POST", "/admin/api/users", map[string]any{"username": "bad", "password": "x", "role": "owner"}, ""); status(r) != 400 {
		t.Errorf("bad role/password should be rejected: %v", r)
	}
	editorTok := e.login("ed", "editorpass")
	viewerTok := e.login("vw", "viewerpass")
	if r := e.call("GET", "/admin/api/me", nil, editorTok); r["role"] != "editor" || r["last_login_at"] == nil {
		t.Errorf("editor /me: %v", r)
	}

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "m", "provider_type": "ollama", "model_name": "x"}, "")
	connID := int64(conn["id"].(float64))

	// Editor creates project A (auto-member); admin creates project B without the editor.
	pa := e.call("POST", "/admin/api/projects", map[string]any{"name": "A", "model_connection_id": connID, "member_user_ids": []int64{viewerID}}, editorTok)
	if status(pa) != 201 {
		t.Fatalf("editor create project: %v", pa)
	}
	projA := pa["project"].(map[string]any)
	idA := int64(projA["id"].(float64))
	if ids := projA["member_ids"].([]any); len(ids) != 2 {
		t.Errorf("project A members should be editor + viewer: %v", ids)
	}
	pb := e.call("POST", "/admin/api/projects", map[string]any{"name": "B", "model_connection_id": connID}, "")
	idB := int64(pb["project"].(map[string]any)["id"].(float64))

	// Visibility: editor lists only A and gets 404 for B on every project route.
	if l := e.call("GET", "/admin/api/projects", nil, editorTok)["_list"].([]any); len(l) != 1 || l[0].(map[string]any)["name"] != "A" {
		t.Errorf("editor project list: %v", l)
	}
	if l := e.call("GET", "/admin/api/projects", nil, "")["_list"].([]any); len(l) != 2 {
		t.Errorf("admin project list: %v", l)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", fmt.Sprintf("/admin/api/projects/%d", idB)},
		{"PUT", fmt.Sprintf("/admin/api/projects/%d", idB)},
		{"DELETE", fmt.Sprintf("/admin/api/projects/%d", idB)},
		{"POST", fmt.Sprintf("/admin/api/projects/%d/rotate-key", idB)},
		{"GET", fmt.Sprintf("/admin/api/projects/%d/metrics", idB)},
		{"GET", fmt.Sprintf("/admin/api/projects/%d/members", idB)},
		{"GET", fmt.Sprintf("/admin/api/metrics/requests?project_id=%d", idB)},
	} {
		if r := e.call(c.method, c.path, map[string]any{"name": "B2", "model_connection_id": connID}, editorTok); status(r) != 404 {
			t.Errorf("editor %s %s on foreign project: %v", c.method, c.path, r)
		}
	}
	if r := e.call("GET", fmt.Sprintf("/admin/api/projects/%d", idA), nil, editorTok); status(r) != 200 {
		t.Errorf("editor own project: %v", r)
	}
	if r := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/rotate-key", idA), nil, editorTok); status(r) != 200 {
		t.Errorf("editor rotate own key: %v", r)
	}

	// Members: editor may not remove themselves; admin may.
	if r := e.call("PUT", fmt.Sprintf("/admin/api/projects/%d/members", idA), map[string]any{"user_ids": []int64{viewerID}}, editorTok); status(r) != 400 {
		t.Errorf("editor removing self: %v", r)
	}
	if r := e.call("PUT", fmt.Sprintf("/admin/api/projects/%d/members", idA), map[string]any{"user_ids": []int64{editorID, 99999}}, editorTok); status(r) != 400 {
		t.Errorf("unknown member: %v", r)
	}
	if r := e.call("PUT", fmt.Sprintf("/admin/api/projects/%d/members", idA), map[string]any{"user_ids": []int64{editorID}}, ""); status(r) != 200 || len(r["_list"].([]any)) != 1 {
		t.Errorf("admin set members: %v", r)
	}
	if r := e.call("GET", fmt.Sprintf("/admin/api/projects/%d", idA), nil, viewerTok); status(r) != 404 {
		t.Errorf("viewer removed from A should get 404: %v", r)
	}

	// Viewer: reads allowed, writes forbidden.
	if r := e.call("GET", "/admin/api/models", nil, viewerTok); status(r) != 200 {
		t.Errorf("viewer GET /models: %v", r)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/admin/api/models"}, {"PUT", fmt.Sprintf("/admin/api/models/%d", connID)},
		{"DELETE", fmt.Sprintf("/admin/api/models/%d", connID)}, {"POST", "/admin/api/rag-stores"},
		{"POST", "/admin/api/projects"}, {"GET", "/admin/api/users"}, {"GET", "/admin/api/audit"},
		{"POST", fmt.Sprintf("/admin/api/models/%d/test", connID)},
	} {
		r := e.call(c.method, c.path, map[string]any{"name": "n", "provider_type": "ollama", "model_name": "x"}, viewerTok)
		if status(r) != 403 || r["error"].(map[string]any)["type"] != "forbidden" {
			t.Errorf("viewer %s %s: %v", c.method, c.path, r)
		}
	}
	if r := e.call("GET", "/admin/api/users/lite", nil, viewerTok); status(r) != 403 {
		t.Errorf("viewer users/lite: %v", r)
	}
	if r := e.call("GET", "/admin/api/users/lite", nil, editorTok); status(r) != 200 || len(r["_list"].([]any)) != 3 {
		t.Errorf("editor users/lite: %v", r)
	}
	if r := e.call("GET", "/admin/api/users", nil, editorTok); status(r) != 403 {
		t.Errorf("editor GET /users: %v", r)
	}
	// Non-admin global metrics are scoped to member projects (empty here, but must not fail).
	if r := e.call("GET", "/admin/api/metrics/summary", nil, viewerTok); status(r) != 200 || r["window"].(map[string]any)["requests"] != float64(0) {
		t.Errorf("viewer metrics summary: %v", r)
	}

	// Rate limit: five wrong passwords lock the username for a minute, even with the right one.
	for i := 0; i < 5; i++ {
		if r := e.call("POST", "/admin/api/login", map[string]string{"username": "vw", "password": "wrong"}, "x"); status(r) != 401 {
			t.Fatalf("attempt %d: %v", i+1, r)
		}
	}
	r6, hdr := e.callRaw("POST", "/admin/api/login", map[string]string{"username": "vw", "password": "viewerpass"}, "x")
	if status(r6) != 429 || r6["error"].(map[string]any)["type"] != "rate_limited" || hdr.Get("Retry-After") != "60" {
		t.Errorf("6th login should be rate limited: %v %v", r6, hdr)
	}
	// Other users from the same address are still fine below the IP budget.
	e.login("ed", "editorpass")

	// Audit log.
	audit := e.call("GET", "/admin/api/audit?limit=200", nil, "")["entries"].([]any)
	seen := map[string]bool{}
	for _, a := range audit {
		seen[a.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"login.success", "login.failure", "user.create", "project.create", "project.rotate_key", "project.members_update", "model.create"} {
		if !seen[want] {
			t.Errorf("audit log missing %s (have %v)", want, seen)
		}
	}
	filtered := e.call("GET", "/admin/api/audit?action=login.failure", nil, "")["entries"].([]any)
	if len(filtered) != 5 {
		t.Errorf("login.failure entries = %d, want 5", len(filtered))
	}
	if d := filtered[0].(map[string]any); d["details"].(map[string]any)["username"] != "vw" || d["actor_user_id"] != nil {
		t.Errorf("login.failure entry: %v", d)
	}
	newest := audit[0].(map[string]any)["created_at"].(string)
	older := e.call("GET", "/admin/api/audit?before="+newest, nil, "")["entries"].([]any)
	if len(older) >= len(audit) {
		t.Errorf("before cursor should exclude the newest entries: %d vs %d", len(older), len(audit))
	}

	// Deactivation invalidates existing sessions; the user can no longer log in.
	if r := e.call("PUT", fmt.Sprintf("/admin/api/users/%d", editorID), map[string]any{"is_active": false}, ""); status(r) != 200 || r["is_active"] != false {
		t.Fatalf("deactivate editor: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, editorTok); status(r) != 401 {
		t.Errorf("deactivated session should be rejected: %v", r)
	}
	if r := e.call("POST", "/admin/api/login", map[string]string{"username": "ed", "password": "editorpass"}, "x"); status(r) != 401 {
		t.Errorf("deactivated login: %v", r)
	}
	if r := e.call("PUT", fmt.Sprintf("/admin/api/users/%d", editorID), map[string]any{"is_active": true}, ""); status(r) != 200 {
		t.Fatalf("reactivate editor: %v", r)
	}

	// Reset password revokes sessions; the new password works.
	editorTok = e.login("ed", "editorpass")
	if r := e.call("POST", fmt.Sprintf("/admin/api/users/%d/reset-password", editorID), map[string]any{"new_password": "newpass123"}, ""); status(r) != 200 {
		t.Fatalf("reset password: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, editorTok); status(r) != 401 {
		t.Errorf("session should be revoked after password reset: %v", r)
	}
	editorTok = e.login("ed", "newpass123")
	if r := e.call("POST", fmt.Sprintf("/admin/api/users/%d/sessions/revoke", editorID), nil, ""); status(r) != 200 {
		t.Fatalf("revoke sessions: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, editorTok); status(r) != 401 {
		t.Errorf("session should be revoked: %v", r)
	}

	// Last-admin protection and self-protection.
	if r := e.call("PUT", fmt.Sprintf("/admin/api/users/%d", adminID), map[string]any{"role": "editor"}, ""); status(r) != 409 {
		t.Errorf("demoting the last admin: %v", r)
	}
	if r := e.call("PUT", fmt.Sprintf("/admin/api/users/%d", adminID), map[string]any{"is_active": false}, ""); status(r) != 400 {
		t.Errorf("deactivating yourself: %v", r)
	}
	if r := e.call("DELETE", fmt.Sprintf("/admin/api/users/%d", adminID), nil, ""); status(r) != 400 {
		t.Errorf("deleting yourself: %v", r)
	}
	a2 := e.call("POST", "/admin/api/users", map[string]any{"username": "admin2", "password": "adminpass2", "role": "admin"}, "")
	admin2ID := int64(a2["id"].(float64))
	admin2Tok := e.login("admin2", "adminpass2")
	if r := e.call("PUT", fmt.Sprintf("/admin/api/users/%d", adminID), map[string]any{"role": "editor"}, admin2Tok); status(r) != 200 || r["role"] != "editor" {
		t.Errorf("demoting one of two admins: %v", r)
	}
	if r := e.call("GET", "/admin/api/users", nil, ""); status(r) != 403 {
		t.Errorf("demoted admin should lose user management immediately: %v", r)
	}
	if r := e.call("DELETE", fmt.Sprintf("/admin/api/users/%d", admin2ID), nil, admin2Tok); status(r) != 400 {
		t.Errorf("admin2 deleting itself: %v", r)
	}
	if r := e.call("DELETE", fmt.Sprintf("/admin/api/users/%d", viewerID), nil, admin2Tok); status(r) != 200 {
		t.Errorf("delete viewer: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, viewerTok); status(r) != 401 {
		t.Errorf("deleted user's session: %v", r)
	}
	// Audit rows written by the deleted user keep the username; the actor id is nulled.
	if l := e.call("GET", "/admin/api/audit?action=user.delete", nil, admin2Tok)["entries"].([]any); len(l) != 1 {
		t.Errorf("user.delete audit: %v", l)
	}
}

func TestProjectLimitsAndBudgets(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	// Pin the limiter clock so the calls below cannot straddle a minute boundary.
	fixed := time.Date(2026, 9, 18, 12, 0, 30, 0, time.UTC)
	e.usage.Now = func() time.Time { return fixed }
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	errField := func(r map[string]any, k string) any { return r["error"].(map[string]any)[k] }
	msg := map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello there"}}}

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))

	// Validation: negative limits are rejected.
	if r := e.call("POST", "/admin/api/projects", map[string]any{"name": "neg", "model_connection_id": connID, "rate_limit_rpm": -1}, ""); status(r) != 400 {
		t.Fatalf("negative limit should be rejected: %v", r)
	}

	// Project with rpm=2.
	pr := e.call("POST", "/admin/api/projects", map[string]any{"name": "rpm", "model_connection_id": connID, "rate_limit_rpm": 2}, "")
	if status(pr) != 201 || pr["project"].(map[string]any)["rate_limit_rpm"] != float64(2) {
		t.Fatalf("create rpm project: %v", pr)
	}
	rpmKey := pr["api_key"].(string)
	rpmID := int64(pr["project"].(map[string]any)["id"].(float64))
	for i := 1; i <= 2; i++ {
		r, h := e.callRaw("POST", "/v1/chat/completions", msg, rpmKey)
		if status(r) != 200 {
			t.Fatalf("rpm call %d: %v", i, r)
		}
		if h.Get("x-ratelimit-limit-requests") != "2" || h.Get("x-ratelimit-remaining-requests") != fmt.Sprint(2-i) || h.Get("x-ratelimit-reset-requests") == "" {
			t.Errorf("rpm call %d headers: %v", i, h)
		}
	}
	r3, h3 := e.callRaw("POST", "/v1/chat/completions", msg, rpmKey)
	if status(r3) != 429 || errField(r3, "type") != "rate_limit_exceeded" || errField(r3, "code") != "rate_limit_rpm" {
		t.Fatalf("third rpm call: %v", r3)
	}
	if h3.Get("Retry-After") != "30" || h3.Get("x-ratelimit-remaining-requests") != "0" || h3.Get("x-ratelimit-reset-requests") != "30" {
		t.Errorf("429 headers: %v", h3)
	}
	// Streaming requests are throttled too, before any SSE header is sent.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(mustJSON(map[string]any{"stream": true, "messages": msg["messages"]})))
	req.Header.Set("Authorization", "Bearer "+rpmKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		t.Errorf("streaming 429: %d %v", resp.StatusCode, resp.Header)
	}

	// Project with a daily budget of 5 tokens: the first call passes (usage
	// is only known afterwards), the second is refused.
	pb := e.call("POST", "/admin/api/projects", map[string]any{"name": "budget", "model_connection_id": connID, "budget_daily_tokens": 5}, "")
	budgetKey := pb["api_key"].(string)
	budgetID := int64(pb["project"].(map[string]any)["id"].(float64))
	r1, h1 := e.callRaw("POST", "/v1/chat/completions", msg, budgetKey)
	if status(r1) != 200 || h1.Get("x-ragmux-budget-daily-remaining") != "5" || h1.Get("x-ratelimit-limit-requests") != "" {
		t.Fatalf("first budget call: %v %v", r1, h1)
	}
	r2, h2 := e.callRaw("POST", "/v1/chat/completions", msg, budgetKey)
	if status(r2) != 429 || errField(r2, "type") != "insufficient_quota" || errField(r2, "code") != "budget_daily" {
		t.Fatalf("second budget call: %v", r2)
	}
	if h2.Get("x-ragmux-budget-daily-remaining") != "0" || h2.Get("Retry-After") == "" {
		t.Errorf("budget 429 headers: %v", h2)
	}
	u := e.call("GET", fmt.Sprintf("/admin/api/projects/%d/usage", budgetID), nil, "")
	day := u["day"].(map[string]any)
	if day["tokens"] != float64(12) || day["requests"] != float64(1) || day["token_limit"] != float64(5) || day["token_percent"] != float64(100) || day["resets_at"] == nil {
		t.Errorf("budget usage: %v", u)
	}
	u = e.call("GET", fmt.Sprintf("/admin/api/projects/%d/usage", rpmID), nil, "")
	minute := u["minute"].(map[string]any)
	if minute["requests"] != float64(2) || minute["request_limit"] != float64(2) || minute["tokens"] != float64(24) {
		t.Errorf("rpm usage: %v", u)
	}

	// Editing limits is audited with the changed fields.
	if r := e.call("PUT", fmt.Sprintf("/admin/api/projects/%d", budgetID), map[string]any{"name": "budget", "model_connection_id": connID, "budget_daily_tokens": 1000, "rate_limit_tpm": 50}, ""); status(r) != 200 || r["budget_daily_tokens"] != float64(1000) {
		t.Fatalf("update limits: %v", r)
	}
	upd := e.call("GET", "/admin/api/audit?action=project.update", nil, "")["entries"].([]any)
	if len(upd) != 1 {
		t.Fatalf("project.update audit entries: %v", upd)
	}
	changed := upd[0].(map[string]any)["details"].(map[string]any)["limits_changed"].(map[string]any)
	if changed["budget_daily_tokens"].(map[string]any)["to"] != float64(1000) || changed["rate_limit_tpm"] == nil || changed["rate_limit_rpm"] != nil {
		t.Errorf("limits_changed: %v", changed)
	}
	// With the budget raised the project is usable again.
	if r := e.call("POST", "/v1/chat/completions", msg, budgetKey); status(r) != 200 {
		t.Errorf("after raising budget: %v", r)
	}

	// A project without limits is unaffected and carries no limit headers.
	pz := e.call("POST", "/admin/api/projects", map[string]any{"name": "free", "model_connection_id": connID}, "")
	freeKey := pz["api_key"].(string)
	for i := 0; i < 4; i++ {
		r, h := e.callRaw("POST", "/v1/chat/completions", msg, freeKey)
		if status(r) != 200 || h.Get("x-ratelimit-limit-requests") != "" || h.Get("x-ragmux-budget-daily-remaining") != "" {
			t.Fatalf("unlimited call %d: %v %v", i, r, h)
		}
	}

	// 429s are visible in the metrics.
	m := e.call("GET", "/admin/api/metrics/summary", nil, "")
	if w := m["window"].(map[string]any); w["rate_limited"] != float64(3) || m["total"].(map[string]any)["rate_limited"] != float64(3) {
		t.Errorf("rate_limited in summary: %v", m["window"])
	}
	pm := e.call("GET", fmt.Sprintf("/admin/api/projects/%d/metrics", rpmID), nil, "")
	if w := pm["window"].(map[string]any); w["rate_limited"] != float64(2) || w["requests"] != float64(4) {
		t.Errorf("rpm project metrics: %v", w)
	}
	recent := pm["recent"].([]any)
	if r0 := recent[0].(map[string]any); r0["status_code"] != float64(429) || r0["error"] != "rate_limit_rpm" {
		t.Errorf("recent 429 row: %v", r0)
	}
}

// minimalDOCX builds a Word document with a heading and paragraphs.
func minimalDOCX(t *testing.T, heading string, paras ...string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	body := `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>` + heading + `</w:t></w:r></w:p>`
	for _, p := range paras {
		body += `<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`
	}
	for name, content := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`,
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(content))
	}
	zw.Close()
	return buf.String()
}

func TestRAGFormatsHybridRerankAndReprocess(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	hitContents := func(r map[string]any) []string {
		var out []string
		for _, h := range r["hits"].([]any) {
			out = append(out, h.(map[string]any)["content"].(string))
		}
		return out
	}
	hasContent := func(r map[string]any, sub string) bool {
		for _, c := range hitContents(r) {
			if strings.Contains(c, sub) {
				return true
			}
		}
		return false
	}

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))

	// Validation of the new settings.
	bad := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "bad", "embedding_connection_id": connID, "fts_config": "klingon"}, "")
	if status(bad) != 400 || !strings.Contains(bad["error"].(map[string]any)["message"].(string), "fts_config") {
		t.Fatalf("unknown fts_config should be rejected: %v", bad)
	}
	if r := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "bad", "embedding_connection_id": connID, "search_mode": "magic"}, ""); status(r) != 400 {
		t.Fatalf("unknown search_mode should be rejected: %v", r)
	}
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "docs", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "fts_config": "english"}, "")
	if status(rs) != 201 || rs["search_mode"] != "hybrid" || rs["fts_config"] != "english" || rs["contextual_chunks"] != true || rs["rerank_candidates"] != float64(15) || rs["max_distance"] != float64(0) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	search := func(body map[string]any) map[string]any {
		t.Helper()
		r := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID), body, "")
		if status(r) != 200 {
			t.Fatalf("search %v: %v", body, r)
		}
		return r
	}

	// Five formats ingest.
	uploads := map[string]string{
		"fruits.md":   "# Apple\n\nApples are red and crunchy.\n\n# Banana\n\nBananas are yellow and soft.\n\n# Cherry\n\nCherries are small and sweet.",
		"proto.txt":   "The zyxquux protocol is an obscure handshake used by nobody.",
		"guide.docx":  minimalDOCX(t, "Docker", "Run the container with the durian flag."),
		"page.html":   `<html><head><title>Ops Guide</title></head><body><nav>Home</nav><h1>Deploy</h1><h2>Steps</h2><p>Ship the release on Friday.</p><script>x()</script></body></html>`,
		"broken.docx": "<html>not a zip</html>",
	}
	docIDs := map[string]int64{}
	for name, content := range uploads {
		r := e.upload(storeID, name, content)
		if name == "broken.docx" {
			if status(r) != 400 {
				t.Errorf("non-zip docx should be rejected: %v", r)
			}
			continue
		}
		if status(r) != 202 {
			t.Fatalf("upload %s: %v", name, r)
		}
		docIDs[name] = int64(r["id"].(float64))
	}
	for name, id := range docIDs {
		e.waitReady(id)
		if d := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", id), nil, ""); d["chunk_count"].(float64) < 1 {
			t.Errorf("%s: %v", name, d)
		}
	}
	if r := e.upload(storeID, "x.exe", "MZ"); status(r) != 400 {
		t.Errorf("unsupported extension: %v", r)
	}

	// Section metadata comes back with hits for DOCX and HTML.
	if r := search(map[string]any{"query": "durian", "mode": "vector"}); !hasContent(r, "durian flag") || r["hits"].([]any)[0].(map[string]any)["section"] != "Docker" || r["mode"] != "vector" {
		t.Errorf("docx section: %v", r)
	}
	if r := search(map[string]any{"query": "release friday", "top_k": 10}); !hasContent(r, "Ship the release") {
		t.Errorf("html hit: %v", r)
	} else {
		for _, h := range r["hits"].([]any) {
			if hm := h.(map[string]any); strings.Contains(hm["content"].(string), "Ship the release") && hm["section"] != "Deploy > Steps" {
				t.Errorf("html section: %v", hm)
			}
		}
	}

	// Hybrid finds the exact term the keyword embedder knows nothing about;
	// vector mode does not.
	hy := search(map[string]any{"query": "apple zyxquux"})
	if hy["mode"] != "hybrid" || hy["reranked"] != false || !hasContent(hy, "zyxquux") || !hasContent(hy, "Apples") {
		t.Errorf("hybrid top-2 should hold apple and zyxquux: %v", hitContents(hy))
	}
	for _, h := range hy["hits"].([]any) {
		hm := h.(map[string]any)
		if strings.Contains(hm["content"].(string), "zyxquux") && (hm["fts_rank"] != float64(1) || hm["score"].(float64) <= 0) {
			t.Errorf("zyxquux hit ranks: %v", hm)
		}
	}
	// Every non-apple chunk sits at the same cosine distance from this query,
	// so only the first hit is deterministic: ask for one.
	if vec := search(map[string]any{"query": "apple zyxquux", "mode": "vector", "top_k": 1}); hasContent(vec, "zyxquux") || !strings.Contains(hitContents(vec)[0], "Apples") {
		t.Errorf("vector mode: %v", hitContents(vec))
	}

	// The similarity threshold drops unrelated context: a query with no
	// fruit keyword sits at distance ~1 from every fruit chunk.
	if r := search(map[string]any{"query": "zyxquux", "max_distance": 0.5, "top_k": 10}); hasContent(r, "Apples") || hasContent(r, "Bananas") || !hasContent(r, "zyxquux") {
		t.Errorf("max_distance: %v", hitContents(r))
	}

	// Project linked to the store: chat carries the hit count header.
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	apiKey := proj["api_key"].(string)
	chat, hdr := e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "apple zyxquux"}}}, apiKey)
	if status(chat) != 200 || hdr.Get("x-ragmux-rag-hits") != "2" {
		t.Fatalf("chat: %v %v", chat, hdr)
	}
	content := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "(fruits.md · Apple)") || !strings.Contains(content, "(proto.txt)") {
		t.Errorf("context labels: %q", content)
	}
	if _, h := e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "nothing matches this"}}}, apiKey); h.Get("x-ragmux-rag-hits") == "" {
		t.Errorf("hit header should be present (possibly 0) when a store is linked: %v", h)
	}
	free := e.call("POST", "/admin/api/projects", map[string]any{"name": "free", "model_connection_id": connID}, "")
	if _, h := e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}, free["api_key"].(string)); h.Get("x-ragmux-rag-hits") != "" {
		t.Errorf("hit header should be absent without a store: %v", h)
	}
	// Streaming responses carry it too.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(mustJSON(map[string]any{"stream": true, "messages": []map[string]string{{"role": "user", "content": "banana"}}})))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("x-ragmux-rag-hits") != "2" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("stream headers: %v", resp.Header)
	}

	// Rerank override: the mock reverses the candidate list, so the top hit changes.
	rr := search(map[string]any{"query": "apple zyxquux", "rerank": true})
	if rr["reranked"] != true || hitContents(rr)[0] == hitContents(hy)[0] {
		t.Errorf("rerank should reorder: %v vs %v", hitContents(rr), hitContents(hy))
	}
	if _, ok := rr["rerank_latency_ms"].(float64); !ok {
		t.Errorf("rerank_latency_ms should be set: %v", rr)
	}
	if _, ok := rr["retrieval_latency_ms"].(float64); !ok || hy["rerank_latency_ms"] != nil {
		t.Errorf("latency fields: reranked %v, plain %v", rr, hy)
	}

	// Updating retrieval settings; chunking changes recommend reprocessing.
	upd := e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", storeID), map[string]any{"name": "docs", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_candidates": 5, "max_distance": 0.9, "search_mode": "hybrid", "fts_config": "simple"}, "")
	if status(upd) != 200 || upd["rerank"] != true || upd["reprocess_recommended"] != false || upd["max_distance"].(float64) < 0.89 {
		t.Fatalf("update store: %v", upd)
	}
	upd = e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", storeID), map[string]any{"name": "docs", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "contextual_chunks": false}, "")
	if status(upd) != 200 || upd["reprocess_recommended"] != true || upd["contextual_chunks"] != false {
		t.Fatalf("update contextual_chunks: %v", upd)
	}
	// With rerank stored on the store the gateway reranks with the project's chat model.
	chat, hdr = e.callRaw("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "apple zyxquux"}}}, apiKey)
	content = chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if status(chat) != 200 || hdr.Get("x-ragmux-rag-hits") != "2" || strings.Contains(content, "[1] (fruits.md · Apple)") {
		t.Errorf("reranked chat should not lead with the apple chunk: %v %q", hdr, content)
	}

	// Reprocess-all flips every document to pending, then they come back ready.
	re := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/reprocess", storeID), nil, "")
	if status(re) != 202 || re["documents"] != float64(4) {
		t.Fatalf("reprocess all: %v", re)
	}
	for _, id := range docIDs {
		e.waitReady(id)
	}
	if r := e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d", storeID), nil, ""); r["document_count"] != float64(4) || r["chunk_count"].(float64) < 6 {
		t.Errorf("store after reprocess: %v", r)
	}
	if l := e.call("GET", "/admin/api/audit?action=rag_store.reprocess_all", nil, "")["entries"].([]any); len(l) != 1 {
		t.Errorf("reprocess_all audit: %v", l)
	}
	if r := search(map[string]any{"query": "apple zyxquux", "rerank": false}); !hasContent(r, "zyxquux") {
		t.Errorf("search after reprocess: %v", hitContents(r))
	}
}

// TestLoginSurfaceAndSessions covers the login hardening: the token is only
// returned on request, cookie sessions need same-origin JSON requests,
// oversized credentials are refused before any work, and a password change
// signs the other sessions out.
func TestLoginSurfaceAndSessions(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }

	// Without "bearer": true the body has no token; the cookie carries the session.
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/api/login", strings.NewReader(`{"username":" admin ","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 200 || body["token"] != nil || body["user"] == nil {
		t.Fatalf("cookie login: %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Request-Id") == "" {
		t.Errorf("api response headers: %v", resp.Header)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("session cookie not set: %v", resp.Cookies())
	}
	do := func(method, path, body, contentType, origin string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		req.AddCookie(cookie)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, out := do("GET", "/admin/api/me", "", "", "https://evil.example"); st != 200 || out["username"] != "admin" {
		t.Errorf("cookie GET: %d %v", st, out)
	}
	same := "http://" + strings.TrimPrefix(e.srv.URL, "http://")
	if st, _ := do("POST", "/admin/api/models", `{"name":"m","provider_type":"ollama","model_name":"x"}`, "application/json", "https://evil.example"); st != 403 {
		t.Errorf("cookie + foreign origin should be refused: %d", st)
	}
	if st, _ := do("POST", "/admin/api/models", `{"name":"m","provider_type":"ollama","model_name":"x"}`, "text/plain", same); st != 415 {
		t.Errorf("cookie + non-JSON content type should be refused: %d", st)
	}
	if st, out := do("POST", "/admin/api/models", `{"name":"m","provider_type":"ollama","model_name":"x"}`, "application/json; charset=utf-8", same); st != 201 {
		t.Errorf("cookie + same origin JSON: %d %v", st, out)
	}
	// Login itself needs the JSON content type but no origin check.
	if st, _ := do("POST", "/admin/api/login", `{"username":"admin","password":"password123"}`, "text/plain", "https://evil.example"); st != 415 {
		t.Errorf("login with a non-JSON content type: %d", st)
	}

	// Field limits are enforced before the limiter or bcrypt run: none of
	// these count as failed attempts.
	for _, c := range []map[string]any{
		{"username": "", "password": "password123"},
		{"username": "admin", "password": ""},
		{"username": strings.Repeat("a", 65), "password": "password123"},
		{"username": "admin", "password": strings.Repeat("p", 1025)},
	} {
		if r := e.call("POST", "/admin/api/login", c, "x"); status(r) != 400 {
			t.Errorf("login %v: %v", c, r)
		}
	}
	if byUser, _, _ := e.store.CountFailedLoginAttempts(context.Background(), "admin", "", time.Now().Add(-time.Hour)); byUser != 0 {
		t.Errorf("rejected logins were recorded as attempts: %d", byUser)
	}
	if r := e.call("POST", "/admin/api/users", map[string]any{"username": "long", "password": strings.Repeat("p", 73)}, ""); status(r) != 400 {
		t.Errorf("password over bcrypt's 72 bytes must be refused: %v", r)
	}

	// Changing the password keeps the current session and drops the others.
	other := e.login("admin", "password123")
	if r := e.call("POST", "/admin/api/me/password", map[string]any{"current_password": "password123", "new_password": "password456"}, ""); status(r) != 200 {
		t.Fatalf("change password: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, ""); status(r) != 200 {
		t.Errorf("current session should survive the password change: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, other); status(r) != 401 {
		t.Errorf("other session should be revoked: %v", r)
	}
	if st, _ := do("GET", "/admin/api/me", "", "", ""); st != 401 {
		t.Errorf("cookie session should be revoked: %d", st)
	}
}

// TestStoreQuotas covers the per-store upload quotas: the store's own
// max_documents / max_bytes, the instance-wide ceiling, multi-file uploads
// and the fact that reprocessing is never blocked.
func TestStoreQuotas(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	cfg := testdb.Config(t)
	e := newEnvWith(t, cfg, true, func(a *admin.Admin) { a.MaxDocumentsPerStore = 3 })
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	errCode := func(r map[string]any) string {
		if e, ok := r["error"].(map[string]any); ok {
			c, _ := e["code"].(string)
			return c
		}
		return ""
	}
	errMsg := func(r map[string]any) string {
		if e, ok := r["error"].(map[string]any); ok {
			m, _ := e["message"].(string)
			return m
		}
		return ""
	}

	if r := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "neg", "embedding_connection_id": connID, "max_documents": -1}, ""); status(r) != 400 {
		t.Fatalf("negative max_documents should be rejected: %v", r)
	}

	// max_documents: 1 -> the second upload is refused before anything is written.
	one := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "one", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "max_documents": 1}, "")
	if status(one) != 201 || one["max_documents"] != float64(1) || one["max_bytes"] != float64(0) || one["bytes_used"] != float64(0) {
		t.Fatalf("create store: %v", one)
	}
	oneID := int64(one["id"].(float64))
	first := e.upload(oneID, "a.md", "# Apples\n\nApples are red.")
	if status(first) != 202 {
		t.Fatalf("first upload: %v", first)
	}
	e.waitReady(int64(first["id"].(float64)))
	second := e.upload(oneID, "b.md", "# Bananas\n\nBananas are yellow.")
	if status(second) != 422 || errCode(second) != "store_quota" || !strings.Contains(errMsg(second), "1 of 1 documents") {
		t.Fatalf("second upload should hit the document quota: %v", second)
	}
	got := e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d", oneID), nil, "")
	if got["document_count"] != float64(1) || got["bytes_used"] != float64(len("# Apples\n\nApples are red.")) {
		t.Fatalf("usage after refused upload: %v", got)
	}
	// Reprocessing adds nothing and is never blocked, even at the limit.
	if r := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/reprocess", oneID), nil, ""); status(r) != 202 {
		t.Fatalf("reprocess at quota: %v", r)
	}
	e.waitReady(int64(first["id"].(float64)))
	// Deleting frees the slot.
	e.call("DELETE", fmt.Sprintf("/admin/api/documents/%d", int64(first["id"].(float64))), nil, "")
	if r := e.upload(oneID, "b.md", "# Bananas\n\nBananas are yellow."); status(r) != 202 {
		t.Fatalf("upload after delete: %v", r)
	}

	// max_bytes small -> an oversized upload is refused, a fitting one accepted.
	small := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "small", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "max_bytes": 40}, "")
	if status(small) != 201 || small["max_bytes"] != float64(40) {
		t.Fatalf("create store: %v", small)
	}
	smallID := int64(small["id"].(float64))
	if r := e.upload(smallID, "big.md", "# Big\n\n"+strings.Repeat("Cherries are sweet. ", 5)); status(r) != 422 || errCode(r) != "store_quota" || !strings.Contains(errMsg(r), "bytes") {
		t.Fatalf("oversized upload: %v", r)
	}
	if r := e.upload(smallID, "ok.md", "# Ok\n\nCherries are sweet."); status(r) != 202 {
		t.Fatalf("fitting upload: %v", r)
	}
	if r := e.upload(smallID, "ok2.md", "# Ok\n\nCherries are sweet."); status(r) != 422 || errCode(r) != "store_quota" {
		t.Fatalf("second upload should exceed the byte quota with the running total: %v", r)
	}

	// Instance ceiling (MaxDocumentsPerStore = 3) applies to a store without
	// its own quota, and a multi-file upload checks the running total: the
	// third file of one request does not fit, the first two stay.
	open := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "open", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0}, "")
	openID := int64(open["id"].(float64))
	if r := e.upload(openID, "a.md", "# A\n\nApples are red."); status(r) != 202 {
		t.Fatalf("upload 1: %v", r)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range []string{"b.md", "c.md", "d.md"} {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte("# " + name + "\n\nBananas are yellow."))
	}
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/admin/api/rag-stores/%d/documents", e.srv.URL, openID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+e.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var multi map[string]any
	json.NewDecoder(resp.Body).Decode(&multi)
	resp.Body.Close()
	if resp.StatusCode != 422 || errCode(multi) != "store_quota" || !strings.Contains(errMsg(multi), "3 of 3 documents") {
		t.Fatalf("multi-file upload over the instance ceiling: %d %v", resp.StatusCode, multi)
	}
	if got := e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d", openID), nil, ""); got["document_count"] != float64(3) {
		t.Fatalf("the files that fit should have been kept: %v", got)
	}
	// A store quota below the ceiling wins; one above it is capped by the ceiling.
	if r := e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", openID), map[string]any{"name": "open", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "max_documents": 10}, ""); status(r) != 200 || r["max_documents"] != float64(10) {
		t.Fatalf("raise store quota: %v", r)
	}
	if r := e.upload(openID, "e.md", "# E\n\nApples are red."); status(r) != 422 || !strings.Contains(errMsg(r), "3 of 3 documents") {
		t.Fatalf("instance ceiling should still apply: %v", r)
	}
}

// fetch performs a request and returns the raw body and headers, for
// endpoints that do not answer JSON.
func (e *env) fetch(path, bearer string) (int, []byte, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if bearer == "" {
		bearer = e.session
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func TestMetricsExportAndSummaryOptions(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	ctx := context.Background()
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "m", "provider_type": "ollama", "model_name": "x"}, "")
	connID := int64(conn["id"].(float64))
	vw := e.call("POST", "/admin/api/users", map[string]any{"username": "vw", "password": "viewerpass", "role": "viewer"}, "")
	viewerID := int64(vw["id"].(float64))
	viewerTok := e.login("vw", "viewerpass")
	pa := e.call("POST", "/admin/api/projects", map[string]any{"name": "alpha", "model_connection_id": connID, "member_user_ids": []int64{viewerID}}, "")
	idA := int64(pa["project"].(map[string]any)["id"].(float64))
	pb := e.call("POST", "/admin/api/projects", map[string]any{"name": "beta", "model_connection_id": connID}, "")
	idB := int64(pb["project"].(map[string]any)["id"].(float64))

	// Rows are inserted directly: the export must render exactly what was
	// logged, including a hostile model name and error text.
	for _, l := range []store.RequestLog{
		{ProjectID: idA, ModelName: "x", StatusCode: 200, PromptTokens: 10, CompletionTokens: 3, LatencyMs: 40, RAGUsed: true, RAGHits: 2, Streamed: true},
		{ProjectID: idA, ModelName: "=cmd|' /C calc'!A0", StatusCode: 502, LatencyMs: 5, Error: "-2+3+cmd|' /C calc'!A0"},
		{ProjectID: idB, ModelName: "x", StatusCode: 429, Error: "rate_limit_rpm"},
	} {
		if err := e.store.InsertRequestLog(ctx, &l); err != nil {
			t.Fatal(err)
		}
	}

	code, raw, h := e.fetch("/admin/api/metrics/requests.csv?window=1h", "")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="ragmux-requests-`) {
		t.Fatalf("csv: %d %v", code, h)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 || lines[0] != "created_at,project_id,project_name,model_name,status_code,prompt_tokens,completion_tokens,estimated,latency_ms,streamed,rag_used,rag_hits,error" {
		t.Fatalf("csv lines: %q", lines)
	}
	if !strings.Contains(lines[1], fmt.Sprintf(",%d,alpha,x,200,10,3,false,40,true,true,2,", idA)) {
		t.Errorf("first row: %q", lines[1])
	}
	if !strings.Contains(lines[2], `,alpha,'=cmd|' /C calc'!A0,502,0,0,false,5,false,false,0,'-2+3+cmd|' /C calc'!A0`) {
		t.Errorf("escaped row: %q", lines[2])
	}
	if !strings.Contains(lines[3], ",beta,x,429,") {
		t.Errorf("third row: %q", lines[3])
	}

	// project_id narrows; the per-project route is equivalent; a viewer
	// sees only member projects and gets 404 on the others.
	if _, raw, _ := e.fetch(fmt.Sprintf("/admin/api/metrics/requests.csv?project_id=%d", idB), ""); strings.Count(string(raw), "\n") != 2 || !strings.Contains(string(raw), "beta") {
		t.Errorf("project_id filter: %q", raw)
	}
	code, raw, h = e.fetch(fmt.Sprintf("/admin/api/projects/%d/metrics.csv", idA), viewerTok)
	if code != 200 || strings.Count(string(raw), "\n") != 3 || strings.Contains(string(raw), "beta") ||
		!strings.HasPrefix(h.Get("Content-Disposition"), fmt.Sprintf(`attachment; filename="ragmux-project-%d-requests-`, idA)) {
		t.Errorf("viewer project csv: %d %v %q", code, h, raw)
	}
	if code, _, _ := e.fetch(fmt.Sprintf("/admin/api/projects/%d/metrics.csv", idB), viewerTok); code != 404 {
		t.Errorf("viewer foreign project csv: %d", code)
	}
	if code, _, _ := e.fetch(fmt.Sprintf("/admin/api/metrics/requests.csv?project_id=%d", idB), viewerTok); code != 404 {
		t.Errorf("viewer foreign project_id csv: %d", code)
	}
	if _, raw, _ := e.fetch("/admin/api/metrics/requests.csv", viewerTok); strings.Contains(string(raw), "beta") {
		t.Errorf("viewer global csv leaks beta: %q", raw)
	}
	if code, _, _ := e.fetch("/admin/api/metrics/requests.csv", "nope"); code != 401 {
		t.Errorf("unauthenticated csv: %d", code)
	}

	// Summary options: comparison window, per-project breakdown, series size.
	m := e.call("GET", "/admin/api/metrics/summary?window=1h&compare=1&by_project=1&days=3", nil, "")
	if status(m) != 200 || m["previous"] == nil || m["previous"].(map[string]any)["requests"] != float64(0) {
		t.Fatalf("summary with compare: %v", m)
	}
	projects := m["projects"].([]any)
	if len(projects) != 2 || projects[0].(map[string]any)["name"] != "alpha" || projects[0].(map[string]any)["requests"] != float64(2) ||
		projects[0].(map[string]any)["rag_requests"] != float64(1) || projects[1].(map[string]any)["rate_limited"] != float64(1) {
		t.Errorf("projects breakdown: %v", projects)
	}
	if rec := m["recent"].([]any)[2].(map[string]any); rec["rag_hits"] != float64(2) || rec["rag_used"] != true {
		t.Errorf("recent rag_hits: %v", rec)
	}
	plain := e.call("GET", "/admin/api/metrics/summary", nil, "")
	if _, ok := plain["previous"]; ok {
		t.Errorf("previous without compare: %v", plain)
	}
	if _, ok := plain["projects"]; ok {
		t.Errorf("projects without by_project: %v", plain)
	}
	vm := e.call("GET", "/admin/api/metrics/summary?by_project=1", nil, viewerTok)
	if list := vm["projects"].([]any); len(list) != 1 || list[0].(map[string]any)["project_id"] != float64(idA) {
		t.Errorf("viewer projects breakdown: %v", list)
	}
}
