package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// upstream is a scripted OpenAI-compatible server. Tests swap the chat
// handler per case; embeddings return a fixed vector unless embedFail is set.
type upstream struct {
	srv       *httptest.Server
	chat      atomic.Pointer[http.HandlerFunc]
	embedFail atomic.Bool
	// seenCancel is closed by blocking handlers once their request context
	// ended, which proves the gateway tore the upstream call down.
	seenCancel chan struct{}
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{seenCancel: make(chan struct{}, 1)}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			if u.embedFail.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"embedding backend down"}}`))
				return
			}
			var req struct {
				Input []string `json:"input"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/v1/chat/completions":
			if h := u.chat.Load(); h != nil {
				(*h)(w, r)
				return
			}
			okChat(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) setChat(h http.HandlerFunc) { u.chat.Store(&h) }

// okChat is the default chat handler: a plain answer with usage, streamed as
// two content chunks when requested.
func okChat(w http.ResponseWriter, r *http.Request) {
	var req provider.ChatRequest
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hel"}}}})
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "lo"}, "finish_reason": "stop"}}})
		writeSSE(w, map[string]any{"id": "c", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 2}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello"}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6}})
}

func writeSSE(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

type env struct {
	t    *testing.T
	st   *store.Store
	up   *upstream
	srv  *httptest.Server
	conn *store.ModelConnection
	proj *store.Project
	key  string
	gw   *Gateway
}

// newEnv wires a gateway against a fresh schema and one project whose model
// connection points at the scripted upstream. The api key used upstream
// looks like a real one so redaction can be checked.
func newEnv(t *testing.T, mutate func(p *store.Project)) *env {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	up := newUpstream(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "mock", ProviderType: "custom_openai",
		BaseURL: up.srv.URL + "/v1", APIKey: "sk-secretsecretsecret123", ModelName: "mock-model"})
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Project{Name: "p", ModelConnectionID: conn.ID}
	if mutate != nil {
		mutate(p)
	}
	proj, key, err := st.CreateProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	provCfg := func(c *store.ModelConnection) provider.Config {
		return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName, Timeout: 10 * time.Second}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	embedders := func(c *store.ModelConnection) (provider.Embedder, error) { return provider.NewEmbedder(provCfg(c)) }
	gw := &Gateway{Store: st, Log: log, Limiter: &limits.Limiter{Store: st},
		Providers: func(c *store.ModelConnection) (provider.Provider, error) { return provider.New(provCfg(c)) },
		Retriever: rag.NewRetriever(st, embedders)}
	r := chi.NewRouter()
	r.Route("/v1", gw.Routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &env{t: t, st: st, up: up, srv: srv, conn: conn, proj: proj, key: key, gw: gw}
}

// attachRAG creates a RAG store with one ingested document and attaches it
// to the project so retrieval runs on every chat call.
func (e *env) attachRAG() {
	e.t.Helper()
	ctx := context.Background()
	rs, err := e.st.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: e.conn.ID, ChunkSize: 200, TopK: 2})
	if err != nil {
		e.t.Fatal(err)
	}
	doc, err := e.st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "a.txt", Mime: "text/plain", SizeBytes: 5}, []byte("apples are red"))
	if err != nil {
		e.t.Fatal(err)
	}
	embedders := func(c *store.ModelConnection) (provider.Embedder, error) {
		return provider.NewEmbedder(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName})
	}
	ing := rag.NewIngester(ctx, e.st, embedders, 1, e.gw.Log)
	defer ing.Stop()
	if err := ing.Process(ctx, doc.ID); err != nil {
		e.t.Fatal(err)
	}
	e.proj.RAGStoreID = &rs.ID
	if _, err := e.st.UpdateProject(ctx, e.proj); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) post(ctx context.Context, path string, body any, bearer string) *http.Response {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case string:
		rdr = strings.NewReader(b)
	case nil:
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (e *env) chat(body any) (*http.Response, map[string]any) {
	e.t.Helper()
	resp := e.post(context.Background(), "/v1/chat/completions", body, e.key)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("non-JSON response %d: %s", resp.StatusCode, raw)
	}
	return resp, out
}

func errorField(t *testing.T, out map[string]any, field string) any {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %v", out)
	}
	return e[field]
}

// lastLog waits for the deferred request log write and returns the newest row.
func (e *env) lastLog() *store.RequestLog {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 1)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(rows) > 0 {
			return rows[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("no request log written")
	return nil
}

var userMsg = []map[string]any{{"role": "user", "content": "hi"}}

func TestAuthentication(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name, bearer string
		wantType     string
	}{
		{"missing header", "", "invalid_request_error"},
		{"wrong prefix", "abc", "invalid_api_key"},
		{"unknown key", "sk-proj-doesnotexist", "invalid_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := e.post(context.Background(), "/v1/models", nil, c.bearer)
			defer resp.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != http.StatusUnauthorized || errorField(t, out, "type") != c.wantType {
				t.Errorf("status %d body %v", resp.StatusCode, out)
			}
		})
	}
}

func TestModelsShape(t *testing.T) {
	e := newEnv(t, nil)
	resp := e.post(context.Background(), "/v1/models", nil, e.key)
	defer resp.Body.Close()
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "mock-model" ||
		list.Data[0].Object != "model" || list.Data[0].OwnedBy != "custom_openai" || list.Data[0].Created == 0 {
		t.Errorf("unexpected /v1/models: %+v", list)
	}
	one := e.post(context.Background(), "/v1/models/mock-model", nil, e.key)
	defer one.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(one.Body).Decode(&m)
	if m["id"] != "mock-model" || m["object"] != "model" {
		t.Errorf("unexpected /v1/models/{id}: %v", m)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, nil)
	resp, out := e.chat(`{"model": "x", "messages": [`)
	if resp.StatusCode != 400 || errorField(t, out, "type") != "invalid_request_error" ||
		!strings.HasPrefix(errorField(t, out, "message").(string), "malformed JSON body") {
		t.Errorf("malformed: %d %v", resp.StatusCode, out)
	}
	resp, out = e.chat(map[string]any{"model": "x", "messages": []any{}})
	if resp.StatusCode != 400 || errorField(t, out, "message") != "messages is required" {
		t.Errorf("empty messages: %d %v", resp.StatusCode, out)
	}
	e.gw.MaxBodyBytes = 64
	resp, out = e.chat(map[string]any{"model": "x", "messages": []map[string]any{{"role": "user", "content": strings.Repeat("a", 100)}}})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || errorField(t, out, "message") != "request body too large" {
		t.Errorf("too large: %d %v", resp.StatusCode, out)
	}
}

func TestChatSuccessRecordsUsage(t *testing.T) {
	e := newEnv(t, nil)
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 || out["model"] != "mock-model" {
		t.Fatalf("chat: %d %v", resp.StatusCode, out)
	}
	rec := e.lastLog()
	if rec.StatusCode != 200 || rec.PromptTokens != 5 || rec.CompletionTokens != 1 || rec.Estimated || rec.Streamed {
		t.Errorf("log = %+v", rec)
	}
}

func TestUpstream429IsRelayed(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limited"}}`))
	})
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 429 || errorField(t, out, "message") != "slow down" || errorField(t, out, "code") != "rate_limited" ||
		errorField(t, out, "type") != "rate_limit_error" {
		t.Errorf("relay: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.StatusCode != 429 || rec.Error != "slow down" {
		t.Errorf("log = %+v", rec)
	}
}

func TestUpstream500BecomesBadGateway(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusBadGateway || errorField(t, out, "message") != "boom" {
		t.Errorf("500 relay: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.StatusCode != http.StatusBadGateway {
		t.Errorf("log = %+v", rec)
	}
	// Streaming requests that fail before any chunk get the same JSON error.
	resp, out = e.chat(map[string]any{"messages": userMsg, "stream": true})
	if resp.StatusCode != http.StatusBadGateway || errorField(t, out, "message") != "boom" {
		t.Errorf("stream 500 relay: %d %v", resp.StatusCode, out)
	}
}

func TestUpstreamErrorEchoingKeyIsRedacted(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid key %s (header %s)","type":"auth"}}`, strings.TrimPrefix(auth, "Bearer "), auth)
	})
	resp, out := e.chat(map[string]any{"messages": userMsg})
	msg := errorField(t, out, "message").(string)
	if resp.StatusCode != 401 || strings.Contains(msg, "secretsecret") || !strings.Contains(msg, "[redacted]") {
		t.Errorf("not redacted: %d %q", resp.StatusCode, msg)
	}
	if rec := e.lastLog(); strings.Contains(rec.Error, "secretsecret") {
		t.Errorf("request log leaks key: %q", rec.Error)
	}
}

// readSSE collects the data payloads of an SSE body.
func readSSE(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func TestStreamSuccess(t *testing.T) {
	e := newEnv(t, nil)
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg, "stream": true}, e.key)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readSSE(t, resp.Body)
	if len(events) != 4 || events[3] != "[DONE]" {
		t.Fatalf("events = %v", events)
	}
	var chunk provider.StreamChunk
	_ = json.Unmarshal([]byte(events[0]), &chunk)
	if chunk.Model != "mock-model" || chunk.Object != "chat.completion.chunk" || *chunk.Choices[0].Delta.Content != "Hel" {
		t.Errorf("first chunk = %s", events[0])
	}
	rec := e.lastLog()
	if rec.StatusCode != 200 || !rec.Streamed || rec.PromptTokens != 7 || rec.CompletionTokens != 2 {
		t.Errorf("log = %+v", rec)
	}
}

func TestStreamDiesMidway(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "partial"}}}})
		// Close the connection without a terminator.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	})
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg, "stream": true}, e.key)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	events := readSSE(t, resp.Body)
	if len(events) != 3 || events[2] != "[DONE]" {
		t.Fatalf("events = %v", events)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[1]), &env); err != nil || env.Error.Message == "" {
		t.Errorf("second event is not an error object: %s", events[1])
	}
	rec := e.lastLog()
	if rec.StatusCode != http.StatusBadGateway || rec.Error == "" || !rec.Estimated || rec.CompletionTokens == 0 {
		t.Errorf("log = %+v", rec)
	}
}

func TestClientDisconnectDuringStream(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "first"}}}})
		<-r.Context().Done()
		e.up.seenCancel <- struct{}{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp := e.post(ctx, "/v1/chat/completions", map[string]any{"messages": userMsg, "stream": true}, e.key)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// Read the first chunk, then hang up.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			break
		}
	}
	cancel()
	select {
	case <-e.up.seenCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request context was not cancelled after the client left")
	}
	rec := e.lastLog()
	if rec.StatusCode != 499 || rec.Error != "client closed request" || !rec.Streamed {
		t.Errorf("log = %+v", rec)
	}
}

func TestClientDisconnectDuringJSONCall(t *testing.T) {
	e := newEnv(t, nil)
	started := make(chan struct{}, 1)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		// A real server reads the body first; Go only watches the connection
		// for a client hang-up once the body has been consumed.
		_, _ = io.ReadAll(r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		e.up.seenCancel <- struct{}{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(map[string]any{"messages": userMsg})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+e.key)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never called")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("client request should have failed after cancel")
	}
	select {
	case <-e.up.seenCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request context was not cancelled after the client left")
	}
	rec := e.lastLog()
	if rec.StatusCode != 499 || rec.Error != "client closed request" {
		t.Errorf("log = %+v", rec)
	}
}

func TestRAGRetrievalFailureDoesNotBreakChat(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	var sawContext atomic.Bool
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "apples are red") {
			sawContext.Store(true)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		okChat(w, r)
	})
	// With embeddings working the document is retrieved.
	resp, _ := e.chat(map[string]any{"messages": []map[string]any{{"role": "user", "content": "what colour are apples?"}}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-hits") != "1" || !sawContext.Load() {
		t.Fatalf("retrieval: status %d hits %q ctx %v", resp.StatusCode, resp.Header.Get("x-ragmux-rag-hits"), sawContext.Load())
	}
	if rec := e.lastLog(); !rec.RAGUsed {
		t.Errorf("rag_used not recorded: %+v", rec)
	}
	// With the embedding upstream failing the chat still succeeds.
	sawContext.Store(false)
	e.up.embedFail.Store(true)
	resp, out := e.chat(map[string]any{"messages": []map[string]any{{"role": "user", "content": "what colour are apples?"}}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-hits") != "0" || sawContext.Load() {
		t.Errorf("degraded: status %d hits %q ctx %v body %v", resp.StatusCode, resp.Header.Get("x-ragmux-rag-hits"), sawContext.Load(), out)
	}
	if rec := e.lastLog(); rec.RAGUsed || rec.StatusCode != 200 {
		t.Errorf("log = %+v", rec)
	}
}

func TestRateLimitHeadersAndDenial(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1; p.BudgetDailyTokens = 1000 })
	resp, _ := e.chat(map[string]any{"messages": userMsg})
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("x-ratelimit-limit-requests") != "1" || h.Get("x-ratelimit-remaining-requests") != "0" ||
		h.Get("x-ratelimit-reset-requests") == "" || h.Get("x-ragmux-budget-daily-remaining") != "1000" {
		t.Fatalf("headers: %d %v", resp.StatusCode, h)
	}
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || errorField(t, out, "code") != limits.ReasonRPM ||
		errorField(t, out, "type") != "rate_limit_exceeded" {
		t.Errorf("denied: %d %v %v", resp.StatusCode, resp.Header, out)
	}
	if rec := e.lastLog(); rec.StatusCode != 429 || rec.Error != limits.ReasonRPM {
		t.Errorf("log = %+v", rec)
	}
}

func TestSystemPromptIsInjected(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.SystemPrompt = "Answer in Turkish." })
	var got string
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		var req provider.ChatRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			got = req.Messages[0].Text()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		okChat(w, r)
	})
	if resp, _ := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got != "Answer in Turkish." {
		t.Errorf("system prompt = %q", got)
	}
}

func TestProviderErrorMapping(t *testing.T) {
	if s, pe := providerError(context.Canceled); s != 499 || pe.Type != "client_closed" {
		t.Errorf("canceled: %d %+v", s, pe)
	}
	if s, pe := providerError(fmt.Errorf("dial failed for sk-abcdefghijklmnop")); s != 502 || strings.Contains(pe.Message, "abcdefghijk") {
		t.Errorf("generic: %d %+v", s, pe)
	}
	if s, _ := providerError(&provider.Error{Type: "x", Message: "m"}); s != 502 {
		t.Errorf("zero status should map to 502, got %d", s)
	}
	if ceilSeconds(1500*time.Millisecond) != 2 || ceilSeconds(0) != 0 {
		t.Error("ceilSeconds")
	}
}

// panicProvider blows up inside ChatStream, as a buggy adapter would.
type panicProvider struct{}

func (panicProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	panic("chat boom")
}

func (panicProvider) ChatStream(context.Context, provider.ChatRequest, chan<- provider.StreamChunk) error {
	panic("stream boom")
}

func TestStreamPanicIsRecovered(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.Providers = func(*store.ModelConnection) (provider.Provider, error) { return panicProvider{}, nil }
	resp, out := e.chat(map[string]any{"messages": userMsg, "stream": true})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(errorField(t, out, "message").(string), "panicked") {
		t.Errorf("panic should become a 502: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.StatusCode != http.StatusBadGateway {
		t.Errorf("log = %+v", rec)
	}
}

func TestProviderFactoryErrorIsGeneric(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.Providers = func(*store.ModelConnection) (provider.Provider, error) {
		return nil, errors.New("dial http://internal-host:11434: secret detail")
	}
	resp, out := e.chat(map[string]any{"messages": userMsg})
	msg := errorField(t, out, "message").(string)
	if resp.StatusCode != http.StatusInternalServerError || msg != "model connection unavailable" {
		t.Errorf("factory error leaked: %d %q", resp.StatusCode, msg)
	}
}
