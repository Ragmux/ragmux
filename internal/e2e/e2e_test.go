package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
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
			sys := ""
			for _, m := range req.Messages {
				if m.Role == "system" {
					sys = m.Text()
				}
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
	dataDir string
}

func newEnv(t *testing.T, dataDir string, upstream string) *env {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, dataDir, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if n, _ := st.CountUsers(ctx); n == 0 {
		h, _ := auth.HashPassword("password123")
		if _, err := st.CreateUser(ctx, "admin", h); err != nil {
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
	gw := &gateway.Gateway{Store: st, Providers: providers, Retriever: ret, Log: log}
	adm := &admin.Admin{Store: st, Auth: authSvc, Ingester: ing, Retriever: ret, Providers: providers, Log: log}
	r := chi.NewRouter()
	r.Route("/v1", gw.Routes)
	r.Route("/admin", adm.Routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, dataDir: dataDir}
	res := e.call("POST", "/admin/api/login", map[string]string{"username": "admin", "password": "password123"}, "")
	e.session = res["token"].(string)
	return e
}

func (e *env) call(method, path string, body any, bearer string) map[string]any {
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
			return map[string]any{"_list": arr, "_status": float64(resp.StatusCode)}
		}
		e.t.Fatalf("%s %s: non-JSON response %d: %s", method, path, resp.StatusCode, raw)
	}
	out["_status"] = float64(resp.StatusCode)
	return out
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
	dataDir := t.TempDir()
	e := newEnv(t, dataDir, up.URL)

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
		"chunk_size": 100, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))

	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))

	// Vector search returns the banana chunk for a banana query.
	sr := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID), map[string]any{"query": "tell me about banana"}, "")
	hits := sr["hits"].([]any)
	if len(hits) == 0 || !strings.Contains(hits[0].(map[string]any)["content"].(string), "Banana") {
		t.Fatalf("search hits: %v", hits)
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

	// Simulate a container restart: reopen everything on the same data dir.
	e.srv.Close()
	e2 := newEnv(t, dataDir, up.URL)
	if r := e2.call("GET", "/v1/models", nil, newKey); r["_status"] != float64(200) {
		t.Fatalf("project key lost after restart: %v", r)
	}
	chat2 := e2.call("POST", "/v1/chat/completions", map[string]any{"messages": []map[string]string{{"role": "user", "content": "cherry?"}}}, newKey)
	c2 := chat2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(c2, "Cherries are small") {
		t.Fatalf("vectors lost after restart: %q", c2)
	}
	for _, f := range []string{"ragmux.db", "secret.key", "uploads"} {
		if _, err := os.Stat(dataDir + "/" + f); err != nil {
			t.Errorf("missing %s in data dir", f)
		}
	}
}
