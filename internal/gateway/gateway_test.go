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
	"sort"
	"strconv"
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
	// owner is created lazily by userKey and owns every key it mints.
	owner *store.User
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

// userKey mints an "sk-user-" key owned by a lazily created editor and
// returns the key row and its plaintext credential. mutate shapes the key
// before it is written (scopes, limits, expiry); nil keeps the defaults.
func (e *env) userKey(name string, projects []int64, mutate func(*store.APIKey)) (*store.APIKey, string) {
	e.t.Helper()
	ctx := context.Background()
	if e.owner == nil {
		u, err := e.st.CreateUser(ctx, "owner", "h", "editor")
		if err != nil {
			e.t.Fatal(err)
		}
		e.owner = u
	}
	k := &store.APIKey{Kind: store.KindGateway, Name: name, UserID: e.owner.ID,
		Scopes: store.DefaultGatewayScopes, ProjectIDs: projects}
	if mutate != nil {
		mutate(k)
	}
	out, raw, err := e.st.CreateAPIKey(ctx, k)
	if err != nil {
		e.t.Fatal(err)
	}
	return out, raw
}

// newProject adds a second project on the same connection.
func (e *env) newProject(name string, mutate func(*store.Project)) *store.Project {
	e.t.Helper()
	p := &store.Project{Name: name, ModelConnectionID: e.conn.ID}
	if mutate != nil {
		mutate(p)
	}
	out, _, err := e.st.CreateProject(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
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
	ing := rag.NewIngester(ctx, e.st, embedders, 1, e.gw.Log,
		rag.Settings{PollInterval: 20 * time.Millisecond})
	defer ing.Stop()
	// Ingestion is the dispatcher's job; there is no synchronous path, so
	// the fixture queues the document and waits for the row to say ready.
	if err := ing.Enqueue(doc.ID); err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		d, err := e.st.GetDocument(ctx, doc.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		if d.Status == store.DocReady {
			break
		}
		if d.Status == store.DocFailed || time.Now().After(deadline) {
			e.t.Fatalf("fixture document is %q: %s", d.Status, d.Error)
		}
		time.Sleep(10 * time.Millisecond)
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

func TestRAGSourcesHeaderAndIncludeContext(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	var upstreamBody atomic.Pointer[string]
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		upstreamBody.Store(&s)
		r.Body = io.NopCloser(bytes.NewReader(body))
		okChat(w, r)
	})
	msgs := []map[string]any{{"role": "user", "content": "what colour are apples?"}}

	// Without the ragmux field the body is untouched and only headers change.
	resp, out := e.chat(map[string]any{"messages": msgs})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, ok := out["ragmux"]; ok {
		t.Errorf("ragmux field without opt-in: %v", out)
	}
	var sources []map[string]any
	if err := json.Unmarshal([]byte(resp.Header.Get("x-ragmux-rag-sources")), &sources); err != nil || len(sources) != 1 ||
		sources[0]["filename"] != "a.txt" || sources[0]["document_id"] == nil || sources[0]["score"] == nil {
		t.Errorf("sources header %q: %v %v", resp.Header.Get("x-ragmux-rag-sources"), sources, err)
	}
	if rec := e.lastLog(); !rec.RAGUsed || rec.RAGHits != 1 {
		t.Errorf("log = %+v", rec)
	}

	// Opting in adds the sources and the injected context to the JSON body,
	// and the field never reaches the upstream.
	resp, out = e.chat(map[string]any{"messages": msgs, "ragmux": map[string]any{"include_context": true}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-sources") == "" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	rm, _ := out["ragmux"].(map[string]any)
	if rm == nil {
		t.Fatalf("no ragmux field: %v", out)
	}
	if list, _ := rm["sources"].([]any); len(list) != 1 || list[0].(map[string]any)["filename"] != "a.txt" {
		t.Errorf("sources: %v", rm["sources"])
	}
	if c, _ := rm["context"].(string); !strings.Contains(c, "<context>") || !strings.Contains(c, "apples are red") {
		t.Errorf("context: %q", rm["context"])
	}
	if out["choices"] == nil || out["usage"] == nil {
		t.Errorf("OpenAI fields lost: %v", out)
	}
	if b := upstreamBody.Load(); b == nil || strings.Contains(*b, "ragmux") || strings.Contains(*b, "include_context") {
		t.Errorf("ragmux field reached upstream: %v", b)
	}

	// Streaming responses carry the header but the body stays plain SSE.
	sresp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"messages": msgs, "stream": true, "ragmux": map[string]any{"include_context": true}}, e.key)
	defer sresp.Body.Close()
	raw, _ := io.ReadAll(sresp.Body)
	if sresp.StatusCode != 200 || sresp.Header.Get("Content-Type") != "text/event-stream" || sresp.Header.Get("x-ragmux-rag-sources") == "" {
		t.Fatalf("stream: %d %v", sresp.StatusCode, sresp.Header)
	}
	if strings.Contains(string(raw), "ragmux") || !strings.HasSuffix(strings.TrimSpace(string(raw)), "data: [DONE]") {
		t.Errorf("stream body: %s", raw)
	}
	if b := upstreamBody.Load(); b == nil || strings.Contains(*b, "ragmux") {
		t.Errorf("ragmux field reached upstream on stream: %v", b)
	}
}

func TestSourcesHeaderIsCappedToWholeEntries(t *testing.T) {
	long := strings.Repeat("x", 300) + ".pdf"
	var src []ragSource
	for i := 0; i < 20; i++ {
		src = append(src, ragSource{DocumentID: int64(i), Filename: long, Section: "Ünite " + strings.Repeat("y", 100), Page: i, Score: 0.5})
	}
	h := sourcesHeader(src)
	if len(h) > maxSourcesHeaderBytes {
		t.Fatalf("header is %d bytes", len(h))
	}
	var got []ragSource
	if err := json.Unmarshal([]byte(h), &got); err != nil {
		t.Fatalf("header is not a JSON array: %v (%q)", err, h)
	}
	if len(got) == 0 || len(got) >= len(src) || got[0].Filename != long || got[0].Section != src[0].Section {
		t.Errorf("kept %d of %d entries; first %+v", len(got), len(src), got[0])
	}
	for _, r := range h {
		if r >= 0x80 {
			t.Fatalf("non-ASCII byte in header: %q", h)
		}
	}
	// A single entry that is too large yields an empty array, never a cut.
	huge := []ragSource{{Filename: strings.Repeat("z", 3000)}}
	if h := sourcesHeader(huge); h != "[]" {
		t.Errorf("oversized single entry: %q", h)
	}
	if h := sourcesHeader(nil); h != "[]" {
		t.Errorf("nil sources: %q", h)
	}
}

// ---- user-owned gateway keys ----

// call is post with extra request headers, for the project selection header.
func (e *env) call(method, path string, body any, bearer string, headers map[string]string) (*http.Response, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp, out
}

// logFor waits for the deferred request log of one project.
func (e *env) logFor(projectID int64) *store.RequestLog {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &projectID}, 1)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(rows) > 0 {
			return rows[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("no request log written for project %d", projectID)
	return nil
}

func TestUserKeyHappyPathAndAttribution(t *testing.T) {
	e := newEnv(t, nil)
	k, raw := e.userKey("ci", []int64{e.proj.ID}, nil)
	resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, raw, nil)
	if resp.StatusCode != 200 || out["model"] != "mock-model" {
		t.Fatalf("chat with a user key: %d %v", resp.StatusCode, out)
	}
	rec := e.lastLog()
	if rec.APIKeyID == nil || *rec.APIKeyID != k.ID || rec.UserID == nil || *rec.UserID != e.owner.ID {
		t.Errorf("attribution = %+v", rec)
	}
	// last_used_at moves on the first call and is stamped from the deferred
	// record path, so it may land just after the response.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.st.GetAPIKey(context.Background(), k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastUsedAt != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("last_used_at was never stamped")
}

func TestUserKeyRejections(t *testing.T) {
	e := newEnv(t, nil)
	past := time.Now().UTC().Add(-time.Hour)
	revoked, revokedRaw := e.userKey("revoked", []int64{e.proj.ID}, nil)
	if _, err := e.st.RevokeAPIKey(context.Background(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, expiredRaw := e.userKey("expired", []int64{e.proj.ID}, func(k *store.APIKey) { k.ExpiresAt = &past })
	_, liveRaw := e.userKey("live", []int64{e.proj.ID}, nil)

	cases := []struct {
		name, bearer string
		wantCode     any
		wantMsg      string
	}{
		// An unknown key of either shape keeps exactly the pre-0.4 body.
		{"unknown project key", "sk-proj-doesnotexist", nil, "invalid project api key"},
		{"unknown user key", store.GatewayKeyPrefix + strings.Repeat("x", 43), nil, "invalid project api key"},
		{"revoked", revokedRaw, "key_revoked", "this api key has been revoked"},
		{"expired", expiredRaw, "key_expired", "this api key has expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, out := e.call(http.MethodGet, "/v1/models", nil, c.bearer, nil)
			if resp.StatusCode != http.StatusUnauthorized || errorField(t, out, "type") != "invalid_api_key" ||
				errorField(t, out, "code") != c.wantCode || errorField(t, out, "message") != c.wantMsg {
				t.Errorf("status %d body %v", resp.StatusCode, out)
			}
		})
	}
	// Deactivating the owner narrows every key it holds on the next request.
	if _, err := e.st.UpdateUser(context.Background(), e.owner.ID, "editor", false); err != nil {
		t.Fatal(err)
	}
	resp, out := e.call(http.MethodGet, "/v1/models", nil, liveRaw, nil)
	if resp.StatusCode != http.StatusUnauthorized || errorField(t, out, "code") != "key_owner_inactive" {
		t.Errorf("deactivated owner: %d %v", resp.StatusCode, out)
	}
}

func TestUserKeyProjectSelection(t *testing.T) {
	e := newEnv(t, nil)
	staging := e.newProject("staging", nil)
	other := e.newProject("not-granted", nil)
	_, multi := e.userKey("multi", []int64{e.proj.ID, staging.ID}, nil)

	// Several grants and no header: 400 naming the valid values.
	resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, multi, nil)
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "project_required" {
		t.Fatalf("ambiguous: %d %v", resp.StatusCode, out)
	}
	if got := resp.Header.Get(projectsHeader); got != "p,staging" {
		t.Errorf("%s = %q", projectsHeader, got)
	}
	// The header picks one, by name and by id alike.
	for _, want := range []string{"staging", strconv.FormatInt(staging.ID, 10)} {
		resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, multi,
			map[string]string{projectHeader: want})
		if resp.StatusCode != 200 {
			t.Fatalf("header %q: %d %v", want, resp.StatusCode, out)
		}
	}
	if rec := e.logFor(staging.ID); rec.ProjectID != staging.ID {
		t.Errorf("routed to %d", rec.ProjectID)
	}
	// A project the key does not grant, and one that does not exist at all,
	// get the same answer so names cannot be probed.
	for _, name := range []string{"not-granted", strconv.FormatInt(other.ID, 10), "no-such-project"} {
		resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, multi,
			map[string]string{projectHeader: name})
		if resp.StatusCode != http.StatusForbidden || errorField(t, out, "code") != "project_not_granted" ||
			errorField(t, out, "message") != "this api key is not authorised for the requested project" {
			t.Errorf("%q: %d %v", name, resp.StatusCode, out)
		}
	}
	// A default project resolves the ambiguity without a header.
	_, withDefault := e.userKey("default", []int64{e.proj.ID, staging.ID}, func(k *store.APIKey) {
		k.DefaultProjectID = &staging.ID
	})
	if resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, withDefault, nil); resp.StatusCode != 200 {
		t.Errorf("default project: %d %v", resp.StatusCode, out)
	}
}

// A key whose last grant was removed routes nowhere and must fail closed
// rather than fall back to the owner's projects.
func TestUserKeyWithNoGrantsFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	k, raw := e.userKey("orphan", []int64{e.proj.ID}, nil)
	k.ProjectIDs = nil
	if _, err := e.st.UpdateAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, raw, nil)
	if resp.StatusCode != http.StatusForbidden || errorField(t, out, "code") != "project_not_granted" {
		t.Errorf("no grants: %d %v", resp.StatusCode, out)
	}
	// Deleting the granted project empties the set the same way.
	k2, raw2 := e.userKey("dangling", []int64{e.proj.ID}, nil)
	_ = k2
	if err := e.st.DeleteProject(context.Background(), e.proj.ID); err != nil {
		t.Fatal(err)
	}
	resp, out = e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, raw2, nil)
	if resp.StatusCode != http.StatusForbidden || errorField(t, out, "code") != "project_not_granted" {
		t.Errorf("deleted project: %d %v", resp.StatusCode, out)
	}
}

func TestUserKeyScopes(t *testing.T) {
	e := newEnv(t, nil)
	_, modelsOnly := e.userKey("models-only", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.Scopes = []string{store.ScopeModels}
	})
	_, chatOnly := e.userKey("chat-only", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.Scopes = []string{store.ScopeChat}
	})
	resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, modelsOnly, nil)
	if resp.StatusCode != http.StatusForbidden || errorField(t, out, "type") != "insufficient_scope" ||
		errorField(t, out, "message") != "api key is not authorised for chat completions" {
		t.Errorf("chat without the scope: %d %v", resp.StatusCode, out)
	}
	if resp, _ := e.call(http.MethodGet, "/v1/models", nil, modelsOnly, nil); resp.StatusCode != 200 {
		t.Errorf("models with the scope: %d", resp.StatusCode)
	}
	if resp, out := e.call(http.MethodGet, "/v1/models", nil, chatOnly, nil); resp.StatusCode != http.StatusForbidden ||
		errorField(t, out, "code") != "insufficient_scope" {
		t.Errorf("models without the scope: %d %v", resp.StatusCode, out)
	}
	// A project's default key has no scopes and is narrowed by none.
	if resp, _ := e.call(http.MethodGet, "/v1/models", nil, e.key, nil); resp.StatusCode != 200 {
		t.Errorf("project key on models: %d", resp.StatusCode)
	}
}

func TestUserKeyModelsAcrossGrants(t *testing.T) {
	e := newEnv(t, nil)
	conn2, err := e.st.CreateConnection(context.Background(), &store.ModelConnection{Name: "second",
		ProviderType: "custom_openai", BaseURL: e.up.srv.URL + "/v1", APIKey: "k", ModelName: "other-model"})
	if err != nil {
		t.Fatal(err)
	}
	staging := e.newProject("staging", func(p *store.Project) { p.ModelConnectionID = conn2.ID })
	// A third project reuses the first connection, so its model is a duplicate.
	dup := e.newProject("dup", nil)
	_, raw := e.userKey("multi", []int64{e.proj.ID, staging.ID, dup.ID}, nil)

	resp, out := e.call(http.MethodGet, "/v1/models", nil, raw, nil)
	data, _ := out["data"].([]any)
	if resp.StatusCode != 200 || len(data) != 2 {
		t.Fatalf("listing across grants: %d %v", resp.StatusCode, out)
	}
	ids := []string{data[0].(map[string]any)["id"].(string), data[1].(map[string]any)["id"].(string)}
	sort.Strings(ids)
	if ids[0] != "mock-model" || ids[1] != "other-model" {
		t.Errorf("models = %v", ids)
	}
	// With a header the listing narrows to that project's connection.
	resp, out = e.call(http.MethodGet, "/v1/models", nil, raw, map[string]string{projectHeader: "staging"})
	data, _ = out["data"].([]any)
	if resp.StatusCode != 200 || len(data) != 1 || data[0].(map[string]any)["id"] != "other-model" {
		t.Errorf("narrowed listing: %d %v", resp.StatusCode, out)
	}
	// {id} picks one of the grants, and an unknown one is a 404.
	if resp, out := e.call(http.MethodGet, "/v1/models/other-model", nil, raw, nil); resp.StatusCode != 200 ||
		out["id"] != "other-model" {
		t.Errorf("model by id: %d %v", resp.StatusCode, out)
	}
	if resp, _ := e.call(http.MethodGet, "/v1/models/nope", nil, raw, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown model id: %d", resp.StatusCode)
	}
}

// The key tier denies before the project ceiling is anywhere near reached.
func TestKeySubLimitDeniesBeforeTheProjectCeiling(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 100 })
	_, raw := e.userKey("throttled", []int64{e.proj.ID}, func(k *store.APIKey) { k.RateLimitRPM = 1 })

	resp, _ := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, raw, nil)
	if resp.StatusCode != 200 || resp.Header.Get("x-ratelimit-limit-requests") != "1" ||
		resp.Header.Get("x-ratelimit-remaining-requests") != "0" {
		t.Fatalf("first: %d %v", resp.StatusCode, resp.Header)
	}
	resp, out := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, raw, nil)
	env, _ := out["error"].(map[string]any)
	if resp.StatusCode != http.StatusTooManyRequests || errorField(t, out, "code") != limits.ReasonRPM ||
		env["scope"] != limits.ScopeKey || !strings.Contains(errorField(t, out, "message").(string), "for this api key") {
		t.Fatalf("denied by the key tier: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.StatusCode != 429 || rec.APIKeyID == nil {
		t.Errorf("denied request is not attributed: %+v", rec)
	}
}
