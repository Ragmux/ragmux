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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/pricing"
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
	// embeds counts the embedding calls, which is how a test proves that
	// retrieval did or did not run.
	embeds atomic.Int64
	// events is the ordered log of what the upstream was asked: "embed:<input>"
	// for an embedding call and whatever a chat handler notes.
	evMu   sync.Mutex
	events []string
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
			u.embeds.Add(1)
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
			u.note("embed:" + strings.Join(req.Input, "|"))
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

// note appends to the ordered event log.
func (u *upstream) note(ev string) {
	u.evMu.Lock()
	defer u.evMu.Unlock()
	u.events = append(u.events, ev)
}

// seen returns the event log so far.
func (u *upstream) seen() []string {
	u.evMu.Lock()
	defer u.evMu.Unlock()
	return append([]string(nil), u.events...)
}

// resetEvents forgets the events so far, such as the embedding calls of a
// fixture document's ingestion.
func (u *upstream) resetEvents() {
	u.evMu.Lock()
	defer u.evMu.Unlock()
	u.events = nil
}

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
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: projects}
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

// saturatedProvider stands in for a gateway resource that ran out and knows
// when it will not have.
type saturatedProvider struct{}

func (saturatedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, &provider.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_exceeded",
		Code: "image_fetch_saturated", RetryAfter: 2, Message: "retry after 2 seconds"}
}

func (saturatedProvider) ChatStream(context.Context, provider.ChatRequest, chan<- provider.StreamChunk) error {
	return &provider.Error{Status: http.StatusTooManyRequests, Type: "rate_limit_exceeded",
		Code: "image_fetch_saturated", RetryAfter: 2, Message: "retry after 2 seconds"}
}

// A provider error that says when to come back reaches the client as a
// header, not only as prose in the body: Retry-After is what an HTTP client
// already knows how to honour.
func TestProviderErrorSetsRetryAfter(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.gw.Providers = func(*store.ModelConnection) (provider.Provider, error) {
				return saturatedProvider{}, nil
			}
			resp, out := e.chat(map[string]any{"messages": userMsg, "stream": stream})
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); got != "2" {
				t.Errorf("Retry-After = %q", got)
			}
			if errorField(t, out, "code") != "image_fetch_saturated" {
				t.Errorf("body = %v", out)
			}
		})
	}
}

// The image fetch ceiling shares its slots between projects, because the
// project is the tenant boundary everywhere else here.
func TestPrincipalImageTenant(t *testing.T) {
	proj := &principal{project: &store.Project{ID: 7}, key: &store.APIKey{ID: 9}}
	if got := proj.imageTenant(); got != "project:7" {
		t.Errorf("tenant = %q", got)
	}
	keyOnly := &principal{key: &store.APIKey{ID: 9}}
	if got := keyOnly.imageTenant(); got != "key:9" {
		t.Errorf("tenant = %q", got)
	}
	if got := (&principal{}).imageTenant(); got != "" {
		t.Errorf("tenant = %q", got)
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
	// Two content chunks and [DONE]. The upstream's usage trailer is not
	// among them: this request did not ask for one, and a client indexing
	// choices[0] would break on a chunk whose choices array is empty.
	events := readSSE(t, resp.Body)
	if len(events) != 3 || events[2] != "[DONE]" {
		t.Fatalf("events = %v", events)
	}
	var chunk provider.StreamChunk
	_ = json.Unmarshal([]byte(events[0]), &chunk)
	if chunk.Model != "mock-model" || chunk.Object != "chat.completion.chunk" || *chunk.Choices[0].Delta.Content != "Hel" {
		t.Errorf("first chunk = %s", events[0])
	}
	for _, ev := range events[:2] {
		var c provider.StreamChunk
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatalf("chunk %s: %v", ev, err)
		}
		if len(c.Choices) == 0 || c.Usage != nil {
			t.Errorf("chunk without include_usage = %s", ev)
		}
	}
	// Withholding the chunk does not lose the tokens: they are still what
	// the request log, the budgets and the cost are built from.
	rec := e.lastLog()
	if rec.StatusCode != 200 || !rec.Streamed || rec.PromptTokens != 7 || rec.CompletionTokens != 2 {
		t.Errorf("log = %+v", rec)
	}
}

func TestStreamUsageTrailerOnlyWhenAsked(t *testing.T) {
	e := newEnv(t, nil)
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{
		"messages": userMsg, "stream": true,
		"stream_options": map[string]any{"include_usage": true}}, e.key)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	events := readSSE(t, resp.Body)
	if len(events) != 4 || events[3] != "[DONE]" {
		t.Fatalf("events = %v", events)
	}
	var trailer provider.StreamChunk
	if err := json.Unmarshal([]byte(events[2]), &trailer); err != nil {
		t.Fatalf("trailer %s: %v", events[2], err)
	}
	if len(trailer.Choices) != 0 || trailer.Usage == nil {
		t.Fatalf("trailer = %s", events[2])
	}
	if trailer.Usage.PromptTokens != 7 || trailer.Usage.CompletionTokens != 2 || trailer.Model != "mock-model" {
		t.Errorf("trailer usage = %+v model %q", trailer.Usage, trailer.Model)
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

// upstreamMessage is one chat message as the upstream received it, content
// kept as the raw bytes that crossed the wire.
type upstreamMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// captureChat makes the upstream record the messages of every chat call and
// answer normally. The returned func gives the calls seen so far.
func captureChat(e *env) func() [][]upstreamMessage {
	var mu sync.Mutex
	var calls [][]upstreamMessage
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []upstreamMessage `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		calls = append(calls, req.Messages)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		okChat(w, r)
	})
	return func() [][]upstreamMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([][]upstreamMessage(nil), calls...)
	}
}

// TestBeforeLastUserKeepsClientPrefixAcrossTurns is the point of the
// "before_last_user" placement: over a three-turn conversation the system
// message -- with the client's cache_control -- and every earlier turn reach
// the upstream byte for byte as the client sent them, no system message is
// added, and the retrieved context is the leading part of the last user
// message only.
func TestBeforeLastUserKeepsClientPrefixAcrossTurns(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.ContextPlacement = store.ContextPlacementBeforeLastUser })
	e.attachRAG()
	seen := captureChat(e)

	system := `[{"type":"text","text":"You are a fruit expert.","cache_control":{"type":"ephemeral"}}]`
	turn := func(n int) string {
		return fmt.Sprintf(`[{"type":"text","text":"question %d about apples?","cache_control":{"type":"ephemeral"}}]`, n)
	}
	reply := func(n int) string { return fmt.Sprintf(`"answer %d"`, n) }
	var messages []string
	messages = append(messages, `{"role":"system","content":`+system+`}`)
	for n := 1; n <= 3; n++ {
		messages = append(messages, `{"role":"user","content":`+turn(n)+`}`)
		body := `{"messages":[` + strings.Join(messages, ",") + `]}`
		resp := e.post(context.Background(), "/v1/chat/completions", body, e.key)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("turn %d: status %d", n, resp.StatusCode)
		}
		messages = append(messages, `{"role":"assistant","content":`+reply(n)+`}`)
	}

	calls := seen()
	if len(calls) != 3 {
		t.Fatalf("upstream saw %d calls, want 3", len(calls))
	}
	for n, msgs := range calls {
		turnNo := n + 1
		if want := 1 + 2*turnNo - 1; len(msgs) != want {
			t.Fatalf("turn %d: %d messages, want %d (a message was added): %v", turnNo, len(msgs), want, msgs)
		}
		if msgs[0].Role != "system" || string(msgs[0].Content) != system {
			t.Errorf("turn %d: system message changed: %s", turnNo, msgs[0].Content)
		}
		// Every earlier turn is the client's own bytes.
		for i := 1; i < len(msgs)-1; i++ {
			var want string
			if msgs[i].Role == "user" {
				want = turn((i + 1) / 2)
			} else {
				want = reply(i / 2)
			}
			if string(msgs[i].Content) != want {
				t.Errorf("turn %d: message %d changed: %s, want %s", turnNo, i, msgs[i].Content, want)
			}
		}
		last := msgs[len(msgs)-1]
		var parts []struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if last.Role != "user" || json.Unmarshal(last.Content, &parts) != nil || len(parts) != 2 {
			t.Fatalf("turn %d: last message is not [context, client part]: %v", turnNo, last)
		}
		if !strings.Contains(parts[0].Text, "<context>") || !strings.Contains(parts[0].Text, "apples are red") || parts[0].CacheControl != nil {
			t.Errorf("turn %d: context is not the first part: %s", turnNo, last.Content)
		}
		if parts[1].Text != fmt.Sprintf("question %d about apples?", turnNo) || string(parts[1].CacheControl) != `{"type":"ephemeral"}` {
			t.Errorf("turn %d: client part changed: %s", turnNo, last.Content)
		}
	}
}

// TestSystemPlacementStaysTheDefault: a project that never set the placement
// keeps the context in the system message and the last user message plain.
func TestSystemPlacementStaysTheDefault(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	if e.proj.ContextPlacement != store.ContextPlacementSystem {
		t.Fatalf("default placement = %q", e.proj.ContextPlacement)
	}
	seen := captureChat(e)
	resp, _ := e.chat(map[string]any{"messages": []map[string]any{{"role": "user", "content": "what colour are apples?"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	calls := seen()
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0].Role != "system" ||
		!strings.Contains(string(calls[0][0].Content), "apples are red") || string(calls[0][1].Content) != `"what colour are apples?"` {
		t.Errorf("messages = %v", calls)
	}
}

// TestRetrievedContextCountsTowardTPM: the first limit check cannot see the
// retrieved context, so a request whose own messages fit under the per-minute
// token limit is still refused once retrieval makes the prompt exceed it. The
// same request without a RAG store passes, nothing reaches the upstream, and
// the request is counted once.
func TestRetrievedContextCountsTowardTPM(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitTPM = 50; p.RateLimitRPM = 10 })
	e.attachRAG()
	var upstreamCalls atomic.Int32
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		okChat(w, r)
	})
	msgs := []map[string]any{{"role": "user", "content": "what colour are apples?"}}

	before := limits.Windows(time.Now())
	resp, out := e.chat(map[string]any{"messages": msgs})
	if resp.StatusCode != 429 || errorField(t, out, "code") != limits.ReasonTPM || errorField(t, out, "scope") != limits.ScopeProject ||
		resp.Header.Get("Retry-After") == "" {
		t.Fatalf("with context: %d %v %v", resp.StatusCode, resp.Header, out)
	}
	if resp.Header.Get("x-ragmux-rag-sources") != "" || resp.Header.Get("x-ragmux-rag-hits") != "" {
		t.Errorf("retrieval headers on a denial: %v", resp.Header)
	}
	if upstreamCalls.Load() != 0 {
		t.Errorf("the denied request reached the upstream %d times", upstreamCalls.Load())
	}
	rec := e.lastLog()
	if rec.StatusCode != 429 || rec.Error != limits.ReasonTPM || rec.Attempt != 0 {
		t.Errorf("log = %+v", rec)
	}
	if rec.RAGUsed || rec.RAGHits != 0 {
		t.Errorf("the denied row claims the context was used: rag_used=%v rag_hits=%d", rec.RAGUsed, rec.RAGHits)
	}
	minute, _, _, err := e.st.GetUsage(context.Background(), e.proj.ID, before)
	if err != nil {
		t.Fatal(err)
	}
	if after := limits.Windows(time.Now()); after.Minute.Equal(before.Minute) && minute.Requests != 1 {
		t.Errorf("the denied request was counted %d times, want 1", minute.Requests)
	}

	// Without a store there is no context, and the bare messages are well
	// under the limit.
	_, otherKey, err := e.st.CreateProject(context.Background(), &store.Project{Name: "no-rag", ModelConnectionID: e.conn.ID, RateLimitTPM: 50})
	if err != nil {
		t.Fatal(err)
	}
	r2 := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": msgs}, otherKey)
	_ = r2.Body.Close()
	if r2.StatusCode != 200 || upstreamCalls.Load() != 1 {
		t.Errorf("without context: %d, upstream calls %d", r2.StatusCode, upstreamCalls.Load())
	}
}

// ---- retrieval policy and the ragmux request object (ADR-024) ----

// skipReasonOf waits for the n-th request log row of the project and returns
// its rag_skip_reason ("" for NULL). The column is write-only for the admin
// API, so the test reads it from the table.
func (e *env) skipReasonOf(n int) string {
	e.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var reason *string
		err := e.st.DB().QueryRow(ctx,
			`SELECT rag_skip_reason FROM request_logs WHERE project_id = $1 ORDER BY id OFFSET $2 LIMIT 1`,
			e.proj.ID, n-1).Scan(&reason)
		if err == nil {
			if reason == nil {
				return ""
			}
			return *reason
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("request log row %d never appeared", n)
	return ""
}

func msgsOf(roles ...string) []map[string]any {
	var out []map[string]any
	for i := 0; i+1 < len(roles); i += 2 {
		out = append(out, map[string]any{"role": roles[i], "content": roles[i+1]})
	}
	return out
}

func TestRetrievalPolicyDecisions(t *testing.T) {
	question := msgsOf("user", "what colour are apples?")
	hello := msgsOf("user", "Merhaba!")
	toolTurn := append(msgsOf("user", "what colour are apples?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "red"})

	cases := []struct {
		name     string
		policy   string
		msgs     []map[string]any
		ragmux   any // the ragmux request object, nil for none
		wantSkip string
	}{
		{"always retrieves", store.RetrievalPolicyAlways, question, nil, ""},
		{"always retrieves a greeting", store.RetrievalPolicyAlways, hello, nil, ""},
		{"auto retrieves a question", store.RetrievalPolicyAuto, question, nil, ""},
		{"auto skips a greeting", store.RetrievalPolicyAuto, hello, nil, "greeting"},
		{"auto skips an ack", store.RetrievalPolicyAuto, msgsOf("user", "ok"), nil, "ack"},
		{"auto retrieves a repeated question", store.RetrievalPolicyAuto,
			msgsOf("user", "what colour are apples?", "assistant", "red", "user", "What colour are apples"), nil, ""},
		{"off skips", store.RetrievalPolicyOff, question, nil, "policy_off"},
		{"a client cannot reopen off", store.RetrievalPolicyOff, question, map[string]any{"retrieval": "always"}, "policy_off"},
		{"a client cannot override auto", store.RetrievalPolicyAuto, hello, map[string]any{"retrieval": "always"}, "greeting"},
		{"client off", store.RetrievalPolicyAlways, question, map[string]any{"retrieval": "off"}, "client_off"},
		{"policy off wins over client off", store.RetrievalPolicyOff, question, map[string]any{"retrieval": "off"}, "policy_off"},
		{"tool turn under always", store.RetrievalPolicyAlways, toolTurn, nil, "tool_turn"},
		{"tool turn under auto", store.RetrievalPolicyAuto, toolTurn, nil, "tool_turn"},
		{"client off wins over tool turn", store.RetrievalPolicyAlways, toolTurn, map[string]any{"retrieval": "off"}, "client_off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(p *store.Project) { p.RetrievalPolicy = tc.policy })
			e.attachRAG()
			seen := captureChat(e)
			embedsBefore := e.up.embeds.Load()

			body := map[string]any{"messages": tc.msgs}
			if tc.ragmux != nil {
				body["ragmux"] = tc.ragmux
			}
			resp, out := e.chat(body)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			embeds := e.up.embeds.Load() - embedsBefore
			skip := resp.Header.Get("x-ragmux-rag-skip")
			rec := e.lastLog()
			reason := e.skipReasonOf(1)
			if skip != tc.wantSkip || reason != tc.wantSkip {
				t.Errorf("x-ragmux-rag-skip = %q, rag_skip_reason = %q, want %q", skip, reason, tc.wantSkip)
			}
			if tc.wantSkip != "" {
				if embeds != 0 {
					t.Errorf("retrieval was skipped but the embedding upstream was called %d times", embeds)
				}
				if resp.Header.Get("x-ragmux-rag-hits") != "0" || resp.Header.Get("x-ragmux-rag-sources") != "" {
					t.Errorf("skipped request headers: hits %q sources %q",
						resp.Header.Get("x-ragmux-rag-hits"), resp.Header.Get("x-ragmux-rag-sources"))
				}
				if rec.RAGUsed || rec.RAGHits != 0 {
					t.Errorf("log = %+v", rec)
				}
				calls := seen()
				if len(calls) != 1 {
					t.Fatalf("upstream chat calls = %d", len(calls))
				}
				for _, m := range calls[0] {
					if strings.Contains(string(m.Content), "<context>") {
						t.Errorf("context injected into a skipped request: %s", m.Content)
					}
				}
			} else {
				if embeds != 1 || resp.Header.Get("x-ragmux-rag-hits") != "1" || !rec.RAGUsed {
					t.Errorf("retrieval should have run: embeds %d hits %q log %+v",
						embeds, resp.Header.Get("x-ragmux-rag-hits"), rec)
				}
			}
		})
	}
}

// A project without a RAG store has nothing to skip: no header, no reason,
// whatever the policy says.
func TestRetrievalPolicyWithoutAStoreSkipsNothingToReport(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyOff })
	resp, _ := e.chat(map[string]any{"messages": msgsOf("user", "merhaba"), "ragmux": map[string]any{"retrieval": "off"}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-skip") != "" || resp.Header.Get("x-ragmux-rag-hits") != "" {
		t.Errorf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	if got := e.skipReasonOf(1); got != "" {
		t.Errorf("rag_skip_reason = %q, want NULL", got)
	}
}

// include_context on a skipped request answers with no sources and no context
// rather than failing.
func TestSkippedRetrievalWithIncludeContext(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyOff })
	e.attachRAG()
	resp, out := e.chat(map[string]any{"messages": msgsOf("user", "apples?"), "ragmux": map[string]any{"include_context": true}})
	rm, _ := out["ragmux"].(map[string]any)
	if resp.StatusCode != 200 || rm == nil {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if list, ok := rm["sources"].([]any); !ok || len(list) != 0 || rm["context"] != "" {
		t.Errorf("ragmux = %v", rm)
	}
	if resp.Header.Get("x-ragmux-rag-skip") != "policy_off" {
		t.Errorf("skip header = %q", resp.Header.Get("x-ragmux-rag-skip"))
	}
}

func TestMalformedRagmuxObjectIs400(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	embedsBefore := e.up.embeds.Load()
	var chatCalls atomic.Int64
	counted(e.up, &chatCalls)

	for _, raw := range []string{`"x"`, `1`, `true`, `[]`, `null`, `[{"retrieval":"off"}]`, `""`} {
		body := `{"messages":[{"role":"user","content":"what colour are apples?"}],"ragmux":` + raw + `}`
		resp, out := e.chat(body)
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_ragmux" ||
			errorField(t, out, "type") != "invalid_request_error" {
			t.Errorf("ragmux=%s: status %d body %v", raw, resp.StatusCode, out)
		}
		if msg, _ := errorField(t, out, "message").(string); strings.Contains(msg, "apples") {
			t.Errorf("error message echoes the request: %q", msg)
		}
	}
	if chatCalls.Load() != 0 || e.up.embeds.Load() != embedsBefore {
		t.Errorf("a refused request reached the upstream: chat %d, embeds %d", chatCalls.Load(), e.up.embeds.Load()-embedsBefore)
	}
	// The same body with an object is served.
	if resp, _ := e.chat(`{"messages":[{"role":"user","content":"hi"}],"ragmux":{}}`); resp.StatusCode != 200 {
		t.Errorf("empty ragmux object: status %d", resp.StatusCode)
	}
}

func TestRagmuxWarnings(t *testing.T) {
	cases := []struct {
		name   string
		ragmux map[string]any
		want   []string // substrings, one per expected Warning header, in order
	}{
		{"always is ignored", map[string]any{"retrieval": "always"},
			[]string{`ragmux.retrieval accepts only the value off`}},
		{"auto is ignored", map[string]any{"retrieval": "auto"},
			[]string{`ragmux.retrieval accepts only the value off`}},
		{"a non-string is ignored", map[string]any{"retrieval": true},
			[]string{`ragmux.retrieval accepts only the value off`}},
		{"unknown key", map[string]any{"foo": 1}, []string{`unknown ragmux field foo; ignored`}},
		{"project setting", map[string]any{"context_placement": "before_last_user", "top_k": 50},
			[]string{`ragmux.context_placement is a project setting`, `ragmux.top_k is a project setting`}},
		{"key outside the name shape is not echoed", map[string]any{"Foo-Bar9": 1, "x\"y": 1},
			[]string{`unknown ragmux field; ignored`, `unknown ragmux field; ignored`}},
		{"wrong include_context type", map[string]any{"include_context": "yes"},
			[]string{`ragmux.include_context must be a boolean`}},
		{"valid fields earn nothing", map[string]any{"retrieval": "off", "include_context": true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.attachRAG()
			resp, out := e.chat(map[string]any{"messages": msgsOf("user", "what colour are apples?"), "ragmux": tc.ragmux})
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			got := resp.Header.Values("Warning")
			if len(got) != len(tc.want) {
				t.Fatalf("Warning headers = %q, want %d", got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], `299 ragmux "`) || !strings.Contains(got[i], w) {
					t.Errorf("Warning[%d] = %q, want it to contain %q", i, got[i], w)
				}
			}
			for _, h := range got {
				if strings.Contains(h, "Foo-Bar9") || strings.Contains(h, `x\"y`) {
					t.Errorf("a client key name reached a header: %q", h)
				}
			}
			// None of it changed what the project does: retrieval ran unless
			// the (valid) retrieval=off said otherwise.
			wantHits := "1"
			if v, _ := tc.ragmux["retrieval"].(string); v == "off" {
				wantHits = "0"
			}
			if resp.Header.Get("x-ragmux-rag-hits") != wantHits {
				t.Errorf("x-ragmux-rag-hits = %q, want %q", resp.Header.Get("x-ragmux-rag-hits"), wantHits)
			}
		})
	}
}

// A project setting named in the request neither applies nor leaks upstream:
// the placement stays the project's.
func TestRagmuxCannotChangeProjectSettings(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	seen := captureChat(e)
	resp, _ := e.chat(map[string]any{"messages": msgsOf("user", "what colour are apples?"),
		"ragmux": map[string]any{"context_placement": "before_last_user", "retrieval_policy": "off", "top_k": 1}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-hits") != "1" || resp.Header.Get("x-ragmux-rag-skip") != "" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	calls := seen()
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0].Role != "system" {
		t.Fatalf("the project's system placement was not kept: %+v", calls)
	}
	if len(resp.Header.Values("Warning")) != 3 {
		t.Errorf("Warning = %q", resp.Header.Values("Warning"))
	}
}

func TestRagmuxWarningCountIsBounded(t *testing.T) {
	obj := map[string]any{}
	for i := 0; i < 200; i++ {
		obj[fmt.Sprintf("k_%c%c", 'a'+i/26, 'a'+i%26)] = i
	}
	h := http.Header{}
	o, err := parseRagmux(map[string]json.RawMessage{"ragmux": mustRaw(t, obj)})
	if err != nil {
		t.Fatal(err)
	}
	addRagmuxWarnings(h, o.Warnings)
	got := h.Values("Warning")
	if len(got) != maxRagmuxWarnings || !strings.Contains(got[len(got)-1], "more ragmux warnings omitted") {
		t.Errorf("%d warnings, last %q", len(got), got[len(got)-1])
	}
	// The order does not depend on map iteration.
	for i := 0; i < 5; i++ {
		o2, _ := parseRagmux(map[string]json.RawMessage{"ragmux": mustRaw(t, obj)})
		if strings.Join(o2.Warnings, "|") != strings.Join(o.Warnings, "|") {
			t.Fatal("warnings are not deterministic")
		}
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The warnings describe the request, so a fallback that replaces the adapter's
// own warnings must keep them.
func TestRagmuxWarningsSurviveAFallback(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 0
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusInternalServerError)
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)

	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg, "ragmux": map[string]any{"foo": 1}})
	if resp.StatusCode != 200 || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("status %d, calls %d/%d: %v", resp.StatusCode, hits1.Load(), hits2.Load(), out)
	}
	if got := resp.Header.Values("Warning"); len(got) != 1 || !strings.Contains(got[0], "unknown ragmux field foo") {
		t.Errorf("Warning = %q", got)
	}
}

// The stream path writes headers on its first chunk; the skip and warning
// headers must be there too, and the ragmux object must not reach the upstream.
func TestSkippedStreamCarriesTheHeaders(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyAuto })
	e.attachRAG()
	embedsBefore := e.up.embeds.Load()
	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"messages": msgsOf("user", "thanks"), "stream": true, "ragmux": map[string]any{"foo": 1}}, e.key)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-skip") != "greeting" || resp.Header.Get("x-ragmux-rag-hits") != "0" ||
		len(resp.Header.Values("Warning")) != 1 || e.up.embeds.Load() != embedsBefore {
		t.Errorf("status %d headers %v embeds %d", resp.StatusCode, resp.Header, e.up.embeds.Load()-embedsBefore)
	}
}

// ---- query_strategy: condense (ADR-024) ----

// condensePhrase is in every condense prompt and in nothing else a test sends.
const condensePhrase = "standalone search query"

// condenseReply answers a condense call with text and the given usage.
func condenseReply(text string, prompt, completion int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": prompt, "completion_tokens": completion}})
	}
}

// scriptCondense makes the upstream hand a condense prompt to cond and answer
// everything else normally, noting "chat:condense" and "chat:answer" in its
// event log. last receives the body of the newest condense call.
func (u *upstream) scriptCondense(cond http.HandlerFunc, last *atomic.Value) {
	u.setChat(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(condensePhrase)) {
			u.note("chat:condense")
			if last != nil {
				last.Store(string(body))
			}
			cond(w, r)
			return
		}
		u.note("chat:answer")
		okChat(w, r)
	})
}

// lockedBuffer is a log sink the server goroutines and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the gateway's log to a buffer.
func (e *env) captureLog() *lockedBuffer {
	buf := &lockedBuffer{}
	e.gw.Log = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// condenseEnv is a project on the condense strategy with a RAG store.
func condenseEnv(t *testing.T, mutate func(p *store.Project)) *env {
	t.Helper()
	e := newEnv(t, func(p *store.Project) {
		p.QueryStrategy = store.QueryStrategyCondense
		if mutate != nil {
			mutate(p)
		}
	})
	e.attachRAG()
	e.up.resetEvents()
	return e
}

var followUp = msgsOf("user", "Tell me about the Apple plan", "assistant", "The Apple plan costs 5 USD.", "user", "and the second one?")

func TestCondenseSearchesWithTheCondensedQuery(t *testing.T) {
	e := condenseEnv(t, nil)
	var last atomic.Value
	e.up.scriptCondense(condenseReply("price of the second plan", 40, 6), &last)

	resp, out := e.chat(map[string]any{"messages": followUp})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	// The condense call comes first, the embedding carries its output, then
	// the answer is generated.
	want := []string{"chat:condense", "embed:price of the second plan", "chat:answer"}
	if got := e.up.seen(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("upstream events = %q, want %q", got, want)
	}
	if resp.Header.Get("x-ragmux-rag-hits") != "1" || len(resp.Header.Values("Warning")) != 0 {
		t.Errorf("hits %q, Warning %q", resp.Header.Get("x-ragmux-rag-hits"), resp.Header.Values("Warning"))
	}

	// What the model was asked: one user message with the conversation and the
	// last message, run cold and short, on the first target's model.
	var req struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
		Messages    []struct {
			Role, Content string
		} `json:"messages"`
	}
	raw, _ := last.Load().(string)
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("condense request: %v: %s", err, raw)
	}
	if req.Model != "mock-model" || req.Temperature != 0 || req.MaxTokens != rag.CondenseMaxTokens || len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Errorf("condense request = %+v", req)
	}
	for _, w := range []string{"User: Tell me about the Apple plan", "Assistant: The Apple plan costs 5 USD.", "Last user message:\nand the second one?"} {
		if !strings.Contains(req.Messages[0].Content, w) {
			t.Errorf("condense prompt lacks %q:\n%s", w, req.Messages[0].Content)
		}
	}

	// Its tokens are on the row, on top of the answer's 5 and 1.
	rec := e.lastLog()
	if rec.PromptTokens != 5+40 || rec.CompletionTokens != 1+6 || !rec.RAGUsed {
		t.Errorf("row = prompt %d completion %d rag %v, want 45, 7, true", rec.PromptTokens, rec.CompletionTokens, rec.RAGUsed)
	}
}

// The default strategy never makes the call.
func TestLastUserStrategyDoesNotCondense(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	e.up.resetEvents()
	e.up.scriptCondense(condenseReply("never used", 40, 6), nil)
	if resp, out := e.chat(map[string]any{"messages": followUp}); resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	want := []string{"embed:and the second one?", "chat:answer"}
	if got := e.up.seen(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("upstream events = %q, want %q", got, want)
	}
}

// Where there is nothing to condense -- a single user message, no store, or a
// retrieval that is skipped -- no call is made, so no tokens are spent.
func TestCondenseIsNotCalledWithoutNeed(t *testing.T) {
	toolTurn := append(append([]map[string]any{}, followUp...),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "red"})
	cases := []struct {
		name   string
		mutate func(p *store.Project)
		body   map[string]any
		want   []string
	}{
		{"one user message", nil, map[string]any{"messages": msgsOf("user", "what colour are apples?")},
			[]string{"embed:what colour are apples?", "chat:answer"}},
		{"one user message after a system prompt", nil, map[string]any{"messages": msgsOf("system", "be brief", "user", "apples?")},
			[]string{"embed:apples?", "chat:answer"}},
		{"policy off", func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyOff },
			map[string]any{"messages": followUp}, []string{"chat:answer"}},
		{"client off", nil, map[string]any{"messages": followUp, "ragmux": map[string]any{"retrieval": "off"}},
			[]string{"chat:answer"}},
		{"tool turn", nil, map[string]any{"messages": toolTurn}, []string{"chat:answer"}},
		{"auto skips a greeting", func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyAuto },
			map[string]any{"messages": msgsOf("user", "q", "assistant", "a", "user", "merhaba")}, []string{"chat:answer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := condenseEnv(t, tc.mutate)
			reg := e.withMetrics()
			e.up.scriptCondense(condenseReply("condensed", 40, 6), nil)
			if resp, out := e.chat(tc.body); resp.StatusCode != 200 {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			if got := e.up.seen(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("upstream events = %q, want %q", got, tc.want)
			}
			if rec := e.lastLog(); rec.PromptTokens != 5 || rec.CompletionTokens != 1 {
				t.Errorf("row = prompt %d completion %d, want the answer's 5 and 1 only", rec.PromptTokens, rec.CompletionTokens)
			}
			if strings.Contains(reg.Text(), "ragmux_rag_query_condense_total{") {
				t.Errorf("a condense was counted:\n%s", reg.Text())
			}
		})
	}

	t.Run("no store", func(t *testing.T) {
		e := newEnv(t, func(p *store.Project) { p.QueryStrategy = store.QueryStrategyCondense })
		e.up.scriptCondense(condenseReply("condensed", 40, 6), nil)
		if resp, out := e.chat(map[string]any{"messages": followUp}); resp.StatusCode != 200 {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		if got := e.up.seen(); fmt.Sprint(got) != "[chat:answer]" {
			t.Errorf("upstream events = %q", got)
		}
	})
}

// A failed condense is invisible to the client: the request completes, the
// search runs on the last user message, and the failure is a warn log and a
// counter. It is one attempt however many retries the gateway is set to, and
// whatever tokens the call reported are still counted.
func TestCondenseFailureFallsBackToTheLastUserMessage(t *testing.T) {
	const secret = "SECRET-UPSTREAM-BODY"
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		timeout     time.Duration
		outcome     string
		wantPrompt  int // the row's prompt tokens: the answer's 5 plus what the call reported
		wantCompl   int
		wantLogText string
	}{
		{"upstream 500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"` + secret + `"}}`))
		}, 0, "error", 5, 1, "status=502"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, 150 * time.Millisecond, "timeout", 5, 1, ""},
		{"empty reply", condenseReply("   ", 40, 1), 0, "invalid", 45, 2, ""},
		{"two lines", condenseReply("Here it is:\n"+secret, 40, 9), 0, "invalid", 45, 10, ""},
		{"too long", condenseReply(strings.Repeat(secret, 30), 40, 9), 0, "invalid", 45, 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := condenseEnv(t, nil)
			e.gw.MaxRetries = 3
			e.gw.CondenseTimeout = tc.timeout
			reg := e.withMetrics()
			logs := e.captureLog()
			e.up.scriptCondense(tc.handler, nil)

			resp, out := e.chat(map[string]any{"messages": followUp})
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			// One condense attempt, then the search on the last user message.
			want := []string{"chat:condense", "embed:and the second one?", "chat:answer"}
			if got := e.up.seen(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("upstream events = %q, want %q", got, want)
			}
			if resp.Header.Get("x-ragmux-rag-hits") != "1" || len(resp.Header.Values("Warning")) != 0 {
				t.Errorf("hits %q, Warning %q", resp.Header.Get("x-ragmux-rag-hits"), resp.Header.Values("Warning"))
			}
			rec := e.lastLog()
			if rec.PromptTokens != tc.wantPrompt || rec.CompletionTokens != tc.wantCompl {
				t.Errorf("row = prompt %d completion %d, want %d and %d", rec.PromptTokens, rec.CompletionTokens, tc.wantPrompt, tc.wantCompl)
			}

			text := reg.Text()
			mustContain(t, text, fmt.Sprintf(`ragmux_rag_query_condense_total{outcome="%s"} 1`, tc.outcome))
			if n := strings.Count(text, "ragmux_rag_query_condense_total{"); n != 1 {
				t.Errorf("%d condense series, want 1:\n%s", n, text)
			}
			log := logs.String()
			if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "query condense failed") ||
				!strings.Contains(log, "outcome="+tc.outcome) {
				t.Errorf("no warn log for the %s outcome:\n%s", tc.outcome, log)
			}
			if tc.wantLogText != "" && !strings.Contains(log, tc.wantLogText) {
				t.Errorf("log lacks %q:\n%s", tc.wantLogText, log)
			}
			// PRD rule 10: neither the conversation nor the reply is logged.
			for _, banned := range []string{secret, "second one", "Apple plan", "Last user message"} {
				if strings.Contains(log, banned) {
					t.Errorf("the log carries %q:\n%s", banned, log)
				}
			}
		})
	}
}

func TestCondenseSuccessIsCounted(t *testing.T) {
	e := condenseEnv(t, nil)
	reg := e.withMetrics()
	logs := e.captureLog()
	e.up.scriptCondense(condenseReply(`"price of the second plan"`, 40, 6), nil)
	if resp, out := e.chat(map[string]any{"messages": followUp}); resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	mustContain(t, reg.Text(), `ragmux_rag_query_condense_total{outcome="ok"} 1`)
	if got := e.up.seen(); len(got) < 2 || got[1] != "embed:price of the second plan" {
		t.Errorf("the model's quotes were not stripped: %q", got)
	}
	if strings.Contains(logs.String(), "query condense") {
		t.Errorf("a success logged a condense line:\n%s", logs.String())
	}
}

// history_turns_for_query bounds what the model sees.
func TestCondenseHistoryTurnsBoundThePrompt(t *testing.T) {
	e := condenseEnv(t, func(p *store.Project) { p.HistoryTurnsForQuery = 1 })
	var last atomic.Value
	e.up.scriptCondense(condenseReply("q", 1, 1), &last)
	msgs := msgsOf("user", "OLDEST-QUESTION", "assistant", "OLDEST-ANSWER", "user", "NEWER-QUESTION", "assistant", "NEWER-ANSWER", "user", "now?")
	if resp, out := e.chat(map[string]any{"messages": msgs}); resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	raw, _ := last.Load().(string)
	for _, w := range []string{"NEWER-QUESTION", "NEWER-ANSWER", "now?"} {
		if !strings.Contains(raw, w) {
			t.Errorf("condense request lacks %q", w)
		}
	}
	for _, w := range []string{"OLDEST-QUESTION", "OLDEST-ANSWER"} {
		if strings.Contains(raw, w) {
			t.Errorf("condense request has %q, one turn back is the limit", w)
		}
	}
}

// query_model_connection_id sends the call to that connection and prices its
// tokens with it, not with the connection that answers the chat.
func TestCondenseUsesTheQueryModelConnection(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	up2 := newUpstream(t)
	qconn, err := e.st.CreateConnection(ctx, &store.ModelConnection{Name: "query", ProviderType: "openai",
		BaseURL: up2.srv.URL + "/v1", APIKey: "sk-secretsecretsecret123", ModelName: "gpt-4o-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pricing.Seed(ctx, e.st.DB(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	e.gw.Prices = pricing.NewCache(e.st.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.proj.QueryStrategy, e.proj.QueryModelConnectionID = store.QueryStrategyCondense, &qconn.ID
	e.attachRAG() // saves the project
	e.up.resetEvents()

	var last atomic.Value
	// 2M prompt and 1M completion tokens at gpt-4o-mini's 0.15 and 0.60 USD
	// per million: 300000 + 600000 micros. The chat connection is unpriced.
	up2.scriptCondense(condenseReply("price of the second plan", 2_000_000, 1_000_000), &last)

	if resp, out := e.chat(map[string]any{"messages": followUp}); resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if got := up2.seen(); fmt.Sprint(got) != "[chat:condense]" {
		t.Errorf("query model upstream events = %q", got)
	}
	if got := e.up.seen(); fmt.Sprint(got) != "[embed:price of the second plan]" {
		t.Errorf("chat upstream events = %q", got)
	}
	if raw, _ := last.Load().(string); !strings.Contains(raw, `"model":"gpt-4o-mini"`) {
		t.Errorf("condense request went out with another model: %s", raw)
	}
	rec := e.lastLog()
	if rec.PromptTokens != 5+2_000_000 || rec.CompletionTokens != 1+1_000_000 {
		t.Errorf("tokens = %d/%d", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.CostMicros != 900_000 {
		t.Errorf("cost = %d micros, want 900000 (the call priced at the query model)", rec.CostMicros)
	}
}

// Without a query model the call runs on the first route target, and its
// tokens are priced with that target.
func TestCondenseUsesTheFirstRouteTarget(t *testing.T) {
	e := condenseEnv(t, nil)
	conn2, up2 := e.secondConn("second", "second-model")
	e.addRoute("smart", store.RouteTarget{ConnectionID: conn2.ID, Weight: 1})
	up2.scriptCondense(condenseReply("price of the second plan", 40, 6), nil)

	resp, out := e.chat(map[string]any{"model": "smart", "messages": followUp})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if got := up2.seen(); fmt.Sprint(got) != "[chat:condense chat:answer]" {
		t.Errorf("route target events = %q", got)
	}
	// Retrieval embeds through the store's own connection (the project's).
	if got := e.up.seen(); fmt.Sprint(got) != "[embed:price of the second plan]" {
		t.Errorf("store connection events = %q", got)
	}
}

// A query model whose adapter cannot be built is a failed condense, not a
// failed request.
func TestCondenseWithAnUnusableQueryModelFallsBack(t *testing.T) {
	e := newEnv(t, nil)
	qconn, _ := e.secondConn("query", "query-model")
	e.proj.QueryStrategy, e.proj.QueryModelConnectionID = store.QueryStrategyCondense, &qconn.ID
	e.attachRAG()
	e.up.resetEvents()
	reg := e.withMetrics()
	logs := e.captureLog()
	real := e.gw.Providers
	e.gw.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
		if c.ID == qconn.ID {
			return nil, errors.New("no adapter for https://internal.example/v1")
		}
		return real(c)
	}

	resp, out := e.chat(map[string]any{"messages": followUp})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	// The default chat handler notes nothing: the search alone is seen, and it
	// ran on the last user message.
	if got := e.up.seen(); fmt.Sprint(got) != "[embed:and the second one?]" {
		t.Errorf("upstream events = %q", got)
	}
	mustContain(t, reg.Text(), `ragmux_rag_query_condense_total{outcome="error"} 1`)
	// The warn line names the connection, so the operator can tell which one.
	var warn string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "query condense unavailable") {
			warn = l
		}
	}
	if !strings.Contains(warn, "outcome=error") || !strings.Contains(warn, fmt.Sprintf("connection=%d", qconn.ID)) {
		t.Errorf("warn line = %q, want outcome=error and connection=%d:\n%s", warn, qconn.ID, logs.String())
	}
}

// The condense budget is the fixed one cut to the request's upstream budget.
func TestCondenseTimeoutIsBoundedByTheUpstreamBudget(t *testing.T) {
	cases := []struct {
		name     string
		gw       Gateway
		deadline time.Duration // left of the budget when the call runs; 0 = not started
		lo, hi   time.Duration
	}{
		{"default", Gateway{}, 0, 5 * time.Second, 5 * time.Second},
		{"set", Gateway{CondenseTimeout: 2 * time.Second}, 0, 2 * time.Second, 2 * time.Second},
		{"budget shorter than the default", Gateway{UpstreamTimeout: 3 * time.Second}, 0, 3 * time.Second, 3 * time.Second},
		{"budget longer", Gateway{UpstreamTimeout: time.Minute}, 0, 5 * time.Second, 5 * time.Second},
		{"started budget with 1s left", Gateway{UpstreamTimeout: time.Minute}, time.Second, 500 * time.Millisecond, time.Second},
		{"spent budget", Gateway{UpstreamTimeout: time.Minute}, -time.Second, time.Nanosecond, time.Nanosecond},
	}
	for _, tc := range cases {
		c := &call{}
		if tc.deadline != 0 {
			c.deadline = time.Now().Add(tc.deadline)
		}
		if got := tc.gw.condenseTimeout(c); got < tc.lo || got > tc.hi {
			t.Errorf("%s: timeout %s, want %s..%s", tc.name, got, tc.lo, tc.hi)
		}
	}
}

// Under the auto policy a bare confirmation is skipped as an ack on the
// default strategy and condensed on "condense" (ADR-024, 2026-10-28); a
// greeting is skipped on both.
func TestCondenseKeepsAcksUnderAutoPolicy(t *testing.T) {
	ack := msgsOf("user", "Tell me about the Apple plan", "assistant", "Shall I list the prices?", "user", "evet")

	t.Run("last_user skips the ack", func(t *testing.T) {
		e := newEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyAuto })
		e.attachRAG()
		e.up.resetEvents()
		e.up.scriptCondense(condenseReply("never", 1, 1), nil)
		resp, _ := e.chat(map[string]any{"messages": ack})
		if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-skip") != "ack" || fmt.Sprint(e.up.seen()) != "[chat:answer]" {
			t.Errorf("status %d skip %q events %q", resp.StatusCode, resp.Header.Get("x-ragmux-rag-skip"), e.up.seen())
		}
		if got := e.skipReasonOf(1); got != "ack" {
			t.Errorf("rag_skip_reason = %q", got)
		}
	})
	t.Run("condense condenses the ack", func(t *testing.T) {
		e := condenseEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyAuto })
		e.up.scriptCondense(condenseReply("prices of the Apple plan", 40, 6), nil)
		resp, _ := e.chat(map[string]any{"messages": ack})
		want := "[chat:condense embed:prices of the Apple plan chat:answer]"
		if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-skip") != "" || fmt.Sprint(e.up.seen()) != want {
			t.Errorf("status %d skip %q events %q, want %s", resp.StatusCode, resp.Header.Get("x-ragmux-rag-skip"), e.up.seen(), want)
		}
		if got := e.skipReasonOf(1); got != "" {
			t.Errorf("rag_skip_reason = %q, want NULL", got)
		}
	})
	t.Run("condense still skips a greeting", func(t *testing.T) {
		e := condenseEnv(t, func(p *store.Project) { p.RetrievalPolicy = store.RetrievalPolicyAuto })
		e.up.scriptCondense(condenseReply("never", 1, 1), nil)
		resp, _ := e.chat(map[string]any{"messages": msgsOf("user", "q", "assistant", "a", "user", "teşekkürler")})
		if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-skip") != "greeting" || fmt.Sprint(e.up.seen()) != "[chat:answer]" {
			t.Errorf("status %d skip %q events %q", resp.StatusCode, resp.Header.Get("x-ragmux-rag-skip"), e.up.seen())
		}
	})
}
