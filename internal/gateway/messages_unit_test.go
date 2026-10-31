package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/tracing"
)

// These tests drive the Messages endpoint from the decoded request to the
// client's response, the way messages() does, but with the routing fixed by
// the test: the steps that need a database (the key lookup, the route table,
// the request log) are the ones the env-based tests in messages_test.go cover.

func TestGatewayCredential(t *testing.T) {
	hdr := func(kv ...string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		for i := 0; i+1 < len(kv); i += 2 {
			r.Header.Add(kv[i], kv[i+1])
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		key  string
		bad  string
	}{
		{"bearer", hdr("Authorization", "Bearer sk-user-1"), "sk-user-1", ""},
		{"x-api-key", hdr("x-api-key", "sk-user-1"), "sk-user-1", ""},
		{"both, same", hdr("Authorization", "Bearer sk-user-1", "x-api-key", "sk-user-1"), "sk-user-1", ""},
		{"both, different", hdr("Authorization", "Bearer sk-user-1", "x-api-key", "sk-user-2"), "", conflictingKeys},
		{"bearer sent twice, different", hdr("Authorization", "Bearer sk-user-1", "Authorization", "Bearer sk-user-2"), "", conflictingKeys},
		{"x-api-key sent twice, different", hdr("x-api-key", "sk-user-1", "x-api-key", "sk-user-2"), "", conflictingKeys},
		{"neither", hdr(), "", missingCredential},
		{"another scheme", hdr("Authorization", "Basic abc"), "", missingCredential},
		{"another scheme, x-api-key", hdr("Authorization", "Basic abc", "x-api-key", "sk-user-1"), "sk-user-1", ""},
		{"empty bearer", hdr("Authorization", "Bearer "), "", ""},
		{"empty bearer, x-api-key", hdr("Authorization", "Bearer ", "x-api-key", "sk-user-1"), "", conflictingKeys},
		{"management key is read, then refused by its prefix", hdr("x-api-key", "sk-mgmt-abc"), "sk-mgmt-abc", ""},
	}
	for _, tc := range cases {
		key, bad := gatewayCredential(tc.r)
		if key != tc.key || bad != tc.bad {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, key, bad, tc.key, tc.bad)
		}
	}
}

func TestIsMessagesPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/messages": true, "/v1/messages/": true, "/v1/messages/count_tokens": true, "/api/v1/messages": true,
		"/v1/chat/completions": false, "/v1/messagesx": false, "/v1/models": false, "/messages": false,
	} {
		if got := isMessagesPath(path); got != want {
			t.Errorf("isMessagesPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// envelopeServer serves handler behind the gateway's request id and envelope
// middleware, on the given path.
func envelopeServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	g := &Gateway{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(g.requestID(g.anthropicEnvelope(handler)))
	t.Cleanup(srv.Close)
	return srv
}

func getBody(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestEnvelopeRewritesGatewayErrors(t *testing.T) {
	cases := []struct {
		name       string
		write      func(w http.ResponseWriter)
		status     int
		typ, code  string
		wantNoCode bool
	}{
		{"scope", func(w http.ResponseWriter) {
			writeErrorCode(w, 403, "insufficient_scope", "insufficient_scope", "api key is not authorised for chat completions")
		}, 403, "permission_error", "insufficient_scope", false},
		{"model", func(w http.ResponseWriter) {
			writeErrorCode(w, 404, "invalid_request_error", "model_not_found", "no such model")
		},
			404, "not_found_error", "model_not_found", false},
		{"unknown key", func(w http.ResponseWriter) { invalidKey(w, "", "") }, 401, "authentication_error", "", true},
		{"retired key", func(w http.ResponseWriter) { invalidKey(w, "key_revoked", "this api key has been revoked") },
			401, "authentication_error", "key_revoked", false},
		{"limit", func(w http.ResponseWriter) {
			writeLimitError(w, limits.Decision{Reason: limits.ReasonRPM, LimitRequests: 5, RetryAfter: 3 * time.Second, Scope: limits.ScopeProject})
		}, 429, "rate_limit_error", limits.ReasonRPM, false},
		{"plain 400", func(w http.ResponseWriter) { writeError(w, 400, "invalid_request_error", "messages is required") },
			400, "invalid_request_error", "", true},
		{"too large", func(w http.ResponseWriter) { writeError(w, 413, "invalid_request_error", "request body too large") },
			413, "request_too_large", "", true},
		{"provider 503", func(w http.ResponseWriter) {
			writeProviderError(w, 503, &provider.Error{Status: 503, Type: "upstream_error", Message: "busy", RetryAfter: 4})
		}, 503, "overloaded_error", "", true},
		{"provider 502", func(w http.ResponseWriter) {
			writeProviderError(w, 502, &provider.Error{Status: 502, Type: "upstream_error", Message: "bad"})
		}, 502, "api_error", "", true},
		{"provider timeout", func(w http.ResponseWriter) {
			writeProviderError(w, 504, &provider.Error{Status: 504, Type: "timeout", Message: "slow"})
		}, 504, "timeout_error", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := envelopeServer(t, func(w http.ResponseWriter, r *http.Request) { tc.write(w) })
			resp, raw := getBody(t, srv.URL+"/v1/messages", map[string]string{"X-Request-Id": "req-42"})
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			var got struct {
				Type  string `json:"type"`
				Error struct {
					Type    string  `json:"type"`
					Message string  `json:"message"`
					Code    *string `json:"code"`
				} `json:"error"`
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if got.Type != "error" || got.Error.Type != tc.typ || got.Error.Message == "" || got.RequestID != "req-42" {
				t.Errorf("body = %s", raw)
			}
			if tc.wantNoCode {
				if bytes.Contains(raw, []byte(`"code"`)) {
					t.Errorf("a code was written: %s", raw)
				}
			} else if got.Error.Code == nil || *got.Error.Code != tc.code {
				t.Errorf("code = %v, want %q: %s", got.Error.Code, tc.code, raw)
			}
			if resp.Header.Get("request-id") != "req-42" || resp.Header.Get("X-Request-Id") != "req-42" {
				t.Errorf("ids: request-id=%q X-Request-Id=%q", resp.Header.Get("request-id"), resp.Header.Get("X-Request-Id"))
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
	// Retry-After set by the handler is kept.
	srv := envelopeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeProviderError(w, 503, &provider.Error{Status: 503, Type: "upstream_error", Message: "busy", RetryAfter: 4})
	})
	if resp, _ := getBody(t, srv.URL+"/v1/messages", nil); resp.Header.Get("Retry-After") != "4" {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
}

func TestEnvelopeLeavesOtherEndpointsAndSuccessAlone(t *testing.T) {
	srv := envelopeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		writeError(w, 400, "invalid_request_error", "messages is required")
	})
	resp, raw := getBody(t, srv.URL+"/v1/chat/completions", nil)
	if resp.StatusCode != 400 || !strings.Contains(string(raw), `"error":{"code":null,"message":"messages is required","type":"invalid_request_error"}`) ||
		resp.Header.Get("request-id") != "" {
		t.Errorf("chat error was changed: %d %s (request-id %q)", resp.StatusCode, raw, resp.Header.Get("request-id"))
	}
	resp, raw = getBody(t, srv.URL+"/v1/messages", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(string(raw)) != `{"ok":true}` {
		t.Errorf("success was changed: %d %s", resp.StatusCode, raw)
	}
}

func TestEnvelopeKeepsAnUpstreamErrorBody(t *testing.T) {
	const upstream = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Field required"},"request_id":"req_anthropic"}`
	srv := envelopeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeProviderError(w, 400, &provider.Error{Status: 400, Type: "invalid_request_error", Message: "max_tokens: Field required",
			UpstreamBody: []byte(upstream)})
	})
	resp, raw := getBody(t, srv.URL+"/v1/messages", nil)
	if resp.StatusCode != 400 || string(raw) != upstream {
		t.Errorf("body was changed: %d %s", resp.StatusCode, raw)
	}
}

// ---------------------------------------------------------------- harness

// scripted is a fake upstream that records what it was sent.
type scripted struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hdrs []http.Header
	body [][]byte
	hits atomic.Int64
}

func newScripted(t *testing.T, fn http.HandlerFunc) *scripted {
	t.Helper()
	s := &scripted{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hdrs, s.body = append(s.hdrs, r.Header.Clone()), append(s.body, raw)
		s.mu.Unlock()
		s.hits.Add(1)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		fn(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scripted) lastBody(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.body) == 0 {
		t.Fatal("the upstream was never called")
	}
	var m map[string]any
	if err := json.Unmarshal(s.body[len(s.body)-1], &m); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	return m
}

func (s *scripted) lastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hdrs[len(s.hdrs)-1]
}

// anthropicConn and compatConn are connections to scripted upstreams.
func anthropicConn(id int64, url, model string) *store.ModelConnection {
	return &store.ModelConnection{ID: id, Name: "anthropic", ProviderType: "anthropic", BaseURL: url, APIKey: "sk-ant-secretsecret", ModelName: model}
}

func compatConn(id int64, url string) *store.ModelConnection {
	return &store.ModelConnection{ID: id, Name: "compat", ProviderType: "custom_openai", BaseURL: url + "/v1", APIKey: "sk-compat-secret123", ModelName: "compat-model"}
}

// messagesRun is one request through the Messages endpoint.
type messagesRun struct {
	resp *http.Response
	body []byte
	rec  *store.RequestLog
	c    *call
}

// runMessages posts body to a Messages endpoint wired to project and targets.
func runMessages(t *testing.T, g *Gateway, project *store.Project, targets []*store.ModelConnection, body string, hdr map[string]string) *messagesRun {
	t.Helper()
	if g.Log == nil {
		g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	g.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
		return provider.New(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.ModelName,
			Timeout: 5 * time.Second})
	}
	run := &messagesRun{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := g.Tracer.Start(r.Context(), "test", tracing.KindInternal)
		c := &call{op: messagesOperation, start: time.Now(), w: w, r: r.WithContext(ctx), span: span, project: project,
			projectID: "1", requestID: requestIDFrom(r.Context()), obs: &requestObs{}, log: g.Log}
		run.c = c
		m, ok := g.readMessages(c)
		if !ok {
			return
		}
		parsed, req, opts := m.body, m.req, m.opts
		c.targets, c.conn = targets, targets[0]
		prov, err := g.Providers(c.conn)
		if err != nil {
			t.Error(err)
			return
		}
		if req.Model == "" {
			req.Model = c.conn.ModelName
		}
		prep := g.prepareContext(c, prov, &req, opts)
		c.rec = &store.RequestLog{ProjectID: project.ID, ModelName: c.conn.ModelName, Streamed: req.Stream}
		c.stamp(c.rec, 1)
		run.rec = c.rec
		g.invoke(c, g.newMessagesInvocation(c, r, parsed, req, m.warnings, prep, prov, nil))
	})
	srv := httptest.NewServer(g.requestID(g.anthropicEnvelope(handler)))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	run.resp = resp
	run.body, _ = io.ReadAll(resp.Body)
	return run
}

func testProject() *store.Project {
	return &store.Project{ID: 1, Name: "p", ContextPlacement: store.ContextPlacementSystem,
		RetrievalPolicy: store.RetrievalPolicyAlways, QueryStrategy: store.QueryStrategyLastUser}
}

func warnings(resp *http.Response) string { return strings.Join(resp.Header.Values("Warning"), "\n") }

const anthropicAnswer = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[` +
	`{"type":"thinking","thinking":"hm","signature":"sig-1"},{"type":"text","text":"Hello <3"},` +
	`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}}],"stop_reason":"tool_use","stop_sequence":null,` +
	`"usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":3}}`

func jsonAnswer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

const sdkRequest = `{"model":"claude-opus-5-5","max_tokens":256,"stream":false,"system":[{"type":"text","text":"You are Claude Code."}],` +
	`"tools":[{"name":"lookup","description":"find it","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}},` +
	`{"name":"shell","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"thinking":{"type":"adaptive"},` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"ragmux":{"sources":"inline"}}`

func TestMessagesNativeJSON(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	p := testProject()
	p.SystemPrompt = "Project prompt."
	p.ToolDeny = []string{"shell"}
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, sdkRequest,
		map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "context-management-2025-06-27,interleaved-thinking-2025-05-14"})
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	// The answer is the upstream's own, with the ragmux object the client asked for after it.
	if !strings.HasPrefix(string(run.body), anthropicAnswer[:len(anthropicAnswer)-1]) || !strings.Contains(string(run.body), `"ragmux":{"sources":[]}`) {
		t.Errorf("answer = %s", run.body)
	}
	h := up.lastHeader()
	if h.Get("anthropic-beta") != "context-management-2025-06-27,interleaved-thinking-2025-05-14" || h.Get("anthropic-version") != "2023-06-01" ||
		h.Get("x-api-key") != "sk-ant-secretsecret" {
		t.Errorf("upstream headers = %v", h)
	}
	b := up.lastBody(t)
	if _, ok := b["ragmux"]; ok {
		t.Error("ragmux reached the upstream")
	}
	if b["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", b["model"])
	}
	sys := b["system"].([]any)
	if len(sys) != 2 || sys[1].(map[string]any)["text"] != "Project prompt." || sys[0].(map[string]any)["text"] != "You are Claude Code." {
		t.Errorf("system = %v", sys)
	}
	tools := b["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "lookup" {
		t.Errorf("tools = %v", tools)
	}
	// claude-opus-5-5 takes no forced tool: any became auto, and said so.
	if tc, _ := json.Marshal(b["tool_choice"]); string(tc) != `{"type":"auto"}` {
		t.Errorf("tool_choice = %s", tc)
	}
	w := warnings(run.resp)
	if !strings.Contains(w, "auto is used") || !strings.Contains(w, "removed by this project's tool policy: shell") {
		t.Errorf("Warning = %s", w)
	}
	// The log row.
	if run.rec.StatusCode != 200 || run.rec.PromptTokens != 13 || run.rec.CompletionTokens != 4 || run.rec.CachedPromptTokens != 3 ||
		strings.Join(run.rec.ToolNames, ",") != "lookup" || run.rec.ToolsDenied != 1 || run.rec.Endpoint != "messages" {
		t.Errorf("row = %+v", run.rec)
	}
	if run.resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", run.resp.Header.Get("Content-Type"))
	}
}

func TestMessagesNativeStreamIsByteIdentical(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	up := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, ev := range strings.SplitAfter(stream, "\n\n") {
			_, _ = io.WriteString(w, ev)
			fl.Flush()
		}
	})
	body := `{"model":"claude-opus-5-5","max_tokens":9,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, body,
		map[string]string{"anthropic-beta": "x-beta"})
	if run.resp.StatusCode != 200 || string(run.body) != stream {
		t.Fatalf("status %d, stream changed:\n%q", run.resp.StatusCode, run.body)
	}
	if ct := run.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if run.rec.StatusCode != 200 || run.rec.PromptTokens != 25 || run.rec.CompletionTokens != 15 || !run.rec.Streamed {
		t.Errorf("row = %+v", run.rec)
	}
	if got := up.lastHeader().Get("anthropic-beta"); got != "x-beta" {
		t.Errorf("anthropic-beta = %q", got)
	}
}

// The client's model string is what the answer names, as on the translated
// path (ADR-018); the upstream gets the connection's model and every other
// byte of its answer reaches the client unchanged.
func TestMessagesNativeEchoesTheClientsModel(t *testing.T) {
	jsonUp := newScripted(t, jsonAnswer(anthropicAnswer))
	body := `{"model":"fast","max_tokens":9,"messages":[{"role":"user","content":"hi"}],"ragmux":{"sources":"inline"}}`
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, jsonUp.srv.URL, "claude-opus-5-5")}, body, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	want := strings.Replace(anthropicAnswer[:len(anthropicAnswer)-1], `"model":"claude-opus-5-5"`, `"model":"fast"`, 1) + `,"ragmux":{"sources":[]}}`
	if string(run.body) != want {
		t.Errorf("answer = %s\nwant     %s", run.body, want)
	}
	if got := jsonUp.lastBody(t)["model"]; got != "claude-opus-5-5" {
		t.Errorf("upstream model = %v", got)
	}

	const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n"
	const rest = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	streamUp := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, start+rest)
	})
	sbody := `{"model":"fast","max_tokens":9,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	run = runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, streamUp.srv.URL, "claude-opus-5-5")}, sbody, nil)
	wantStream := strings.Replace(start, `"model":"claude-opus-5-5"`, `"model":"fast"`, 1) + rest
	if run.resp.StatusCode != 200 || string(run.body) != wantStream {
		t.Errorf("stream changed beyond the model:\n%q\nwant\n%q", run.body, wantStream)
	}
	if run.rec.PromptTokens != 25 || run.rec.CompletionTokens != 15 {
		t.Errorf("row = %+v", run.rec)
	}
}

// A block this gateway does not translate is the provider's to judge on the
// native path: search_result carries a string source where an image carries an
// object, and the request must reach the upstream as the client wrote it.
func TestMessagesNativeForwardsSearchResultBlocks(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	body := `{"model":"claude-opus-5-5","max_tokens":9,"messages":[` +
		`{"role":"user","content":"find"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[` +
		`{"type":"search_result","source":"https://x.test","title":"T","content":[{"type":"text","text":"a"}],"citations":{"enabled":true}}]},` +
		`{"type":"search_result","source":"https://y.test","title":"U","content":[{"type":"text","text":"b"}]}]}]}`
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, body, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	got := string(up.body[0])
	for _, frag := range []string{`"source":"https://x.test"`, `"source":"https://y.test"`, `"type":"search_result"`} {
		if !strings.Contains(got, frag) {
			t.Errorf("upstream body lacks %s: %s", frag, got)
		}
	}
}

func TestMessagesNativeRelaysAnUpstream400Unchanged(t *testing.T) {
	const upstream = `{"type":"error","error":{"type":"invalid_request_error","message":"tools: Field required"},"request_id":"req_011"}`
	up := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstream)
	})
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":9,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
		run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, body, nil)
		if run.resp.StatusCode != 400 || string(run.body) != upstream {
			t.Errorf("stream=%v: %d %s", stream, run.resp.StatusCode, run.body)
		}
		if run.rec.StatusCode != 400 {
			t.Errorf("stream=%v: row status %d", stream, run.rec.StatusCode)
		}
	}
}

func TestMessagesNativeThinkingRoundTrip(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	history := `{"model":"claude-sonnet-4-5","max_tokens":9,"messages":[{"role":"user","content":"q"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig-1"},{"type":"redacted_thinking","data":"blob"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found"}]}],` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":2048}}`
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-sonnet-4-5")}, history, nil)
	if run.resp.StatusCode != 200 || warnings(run.resp) != "" {
		t.Fatalf("status %d warnings %q: %s", run.resp.StatusCode, warnings(run.resp), run.body)
	}
	asst := up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(asst) != 3 || asst[0].(map[string]any)["signature"] != "sig-1" || asst[1].(map[string]any)["data"] != "blob" {
		t.Errorf("the thinking blocks did not come back intact: %v", asst)
	}
	// Only the model differs: the client asked for claude-sonnet-4-5 (ADR-018).
	if want := strings.Replace(anthropicAnswer, `"model":"claude-opus-5-5"`, `"model":"claude-sonnet-4-5"`, 1); string(run.body) != want {
		t.Errorf("answer = %s", run.body)
	}
}

// ragProject is a project with a RAG store attached, which is all the RAG x
// prefix rule looks at. The Retriever is present so the retrieval step is
// reached; a tool turn never runs it.
func ragProject() *store.Project {
	p := testProject()
	id := int64(7)
	p.RAGStoreID = &id
	return p
}

func TestMessagesNativeToolTurnOnARAGProject(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	g := &Gateway{Retriever: rag.NewRetriever(nil, nil), Metrics: nil}
	history := `{"model":"claude-opus-5-5","max_tokens":9,"messages":[{"role":"user","content":"q"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig-1"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found"},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}],` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
	run := runMessages(t, g, ragProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, history, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	// A turn that returns tool results is not retrieved for, whatever else the message holds.
	if got := run.resp.Header.Get("x-ragmux-rag-skip"); got != obs.RAGSkipToolTurn {
		t.Errorf("x-ragmux-rag-skip = %q", got)
	}
	// The earlier turns' thinking does not go back, and the client is told.
	asst := up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(asst) != 1 || asst[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant blocks = %v", asst)
	}
	if !strings.Contains(warnings(run.resp), "thinking blocks of earlier turns were dropped") {
		t.Errorf("Warning = %q", warnings(run.resp))
	}
	// The last user message is the client's, with no injected text.
	last := up.lastBody(t)["messages"].([]any)[2].(map[string]any)["content"].([]any)
	if len(last) != 2 || last[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("last user turn = %v", last)
	}
}

func TestMessagesNativeRAGOffClientStillDropsThinking(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	g := &Gateway{Retriever: rag.NewRetriever(nil, nil)}
	history := `{"model":"claude-opus-5-5","max_tokens":9,"ragmux":{"retrieval":"off"},"messages":[{"role":"user","content":"q"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig-1"},{"type":"text","text":"a"}]},{"role":"user","content":"next"}]}`
	run := runMessages(t, g, ragProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, history, nil)
	asst := up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if run.resp.StatusCode != 200 || len(asst) != 1 || run.resp.Header.Get("x-ragmux-rag-skip") != obs.RAGSkipClientOff {
		t.Errorf("status %d, skip %q, blocks %v", run.resp.StatusCode, run.resp.Header.Get("x-ragmux-rag-skip"), asst)
	}
	// A project without RAG keeps them.
	runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, history, nil)
	if asst = up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any); len(asst) != 2 {
		t.Errorf("a project without RAG lost its thinking blocks: %v", asst)
	}
}

// compatChat answers an OpenAI chat call: with a tool call, or text, streamed or not.
func compatChat(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &req)
	if !req.Stream {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-9", "model": "compat-model",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "Looking.",
				"tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}}}}}},
			"usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send(map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Look"}}}})
	time.Sleep(120 * time.Millisecond) // quiet long enough for a ping
	send(map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ing."}}}})
	send(map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}}}}, "finish_reason": "tool_calls"}}})
	send(map[string]any{"id": "c", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 5}})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

const canonicalRequest = `{"model":"my-route","max_tokens":128,"top_k":4,"stream":%v,"system":"Be brief.","metadata":{"user_id":"u-1"},` +
	`"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],"tool_choice":{"type":"any"},` +
	`"messages":[{"role":"user","content":"find x"}]}`

func TestMessagesCanonicalJSON(t *testing.T) {
	up := newScripted(t, compatChat)
	p := testProject()
	p.SystemPrompt = "Project prompt."
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{compatConn(1, up.srv.URL)}, fmt.Sprintf(canonicalRequest, false),
		map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "context-management-2025-06-27"})
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	// The upstream got a chat request.
	b := up.lastBody(t)
	msgs := b["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || b["user"] != "u-1" || b["tool_choice"] != "required" {
		t.Errorf("chat request = %v", b)
	}
	if sys := msgs[0].(map[string]any)["content"].(string); !strings.HasPrefix(sys, "Project prompt.") {
		t.Errorf("system = %q", sys)
	}
	if fn := b["tools"].([]any)[0].(map[string]any)["function"].(map[string]any); fn["name"] != "lookup" {
		t.Errorf("tools = %v", b["tools"])
	}
	// The client got a Messages answer.
	var got struct {
		ID, Type, Role, Model string
		Content               []map[string]any
		StopReason            string         `json:"stop_reason"`
		StopSequence          *string        `json:"stop_sequence"`
		Usage                 map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(run.body, &got); err != nil {
		t.Fatalf("%s: %v", run.body, err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.Model != "my-route" || got.StopReason != "tool_use" || got.StopSequence != nil ||
		len(got.Content) != 2 || got.Content[0]["text"] != "Looking." || got.Content[1]["type"] != "tool_use" ||
		got.Content[1]["name"] != "lookup" || got.Usage["input_tokens"] != 12 || got.Usage["output_tokens"] != 5 {
		t.Errorf("answer = %s", run.body)
	}
	// What the connection could not carry is said.
	w := warnings(run.resp)
	if !strings.Contains(w, "top_k") || !strings.Contains(w, "anthropic-beta values are not applied by this connection: context-management-2025-06-27") {
		t.Errorf("Warning = %s", w)
	}
	if strings.Contains(w, "anthropic-version") {
		t.Errorf("the default version earned a Warning: %s", w)
	}
	if run.rec.StatusCode != 200 || run.rec.PromptTokens != 12 || strings.Join(run.rec.ToolNames, ",") != "lookup" {
		t.Errorf("row = %+v", run.rec)
	}
}

func TestMessagesCanonicalVersionWarning(t *testing.T) {
	up := newScripted(t, compatChat)
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{compatConn(1, up.srv.URL)}, fmt.Sprintf(canonicalRequest, false),
		map[string]string{"anthropic-version": "2024-10-22"})
	if !strings.Contains(warnings(run.resp), "anthropic-version 2024-10-22 is not applied by this connection") {
		t.Errorf("Warning = %q", warnings(run.resp))
	}
}

func TestMessagesCanonicalStreamEventsAndPing(t *testing.T) {
	old := messagesPingInterval
	messagesPingInterval = 40 * time.Millisecond
	t.Cleanup(func() { messagesPingInterval = old })
	up := newScripted(t, compatChat)
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{compatConn(1, up.srv.URL)}, fmt.Sprintf(canonicalRequest, true), nil)
	if run.resp.StatusCode != 200 || run.resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q: %s", run.resp.StatusCode, run.resp.Header.Get("Content-Type"), run.body)
	}
	var names []string
	for _, line := range strings.Split(string(run.body), "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	// ping events sit among the content events and never before message_start.
	if names[0] != "message_start" || names[len(names)-2] != "message_delta" || names[len(names)-1] != "message_stop" {
		t.Fatalf("events = %v", names)
	}
	pings := 0
	for _, n := range names {
		if n == "ping" {
			pings++
		}
	}
	if pings == 0 {
		t.Errorf("no ping while the upstream was quiet: %v", names)
	}
	var content []string
	for _, n := range names {
		if n != "ping" {
			content = append(content, n)
		}
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if strings.Join(content, ",") != want {
		t.Errorf("events\n%s\nwant\n%s", strings.Join(content, ","), want)
	}
	if !strings.Contains(string(run.body), `"stop_reason":"tool_use"`) || !strings.Contains(string(run.body), `"output_tokens":5`) {
		t.Errorf("message_delta missing stop reason or usage:\n%s", run.body)
	}
	if run.rec.StatusCode != 200 || run.rec.PromptTokens != 12 || run.rec.CompletionTokens != 5 || strings.Join(run.rec.ToolNames, ",") != "lookup" {
		t.Errorf("row = %+v", run.rec)
	}
}

func TestMessagesFallsBackFromAnthropicToACanonicalTarget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		anth := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		})
		compat := newScripted(t, compatChat)
		body := fmt.Sprintf(`{"model":"my-route","max_tokens":9,"stream":%v,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],`+
			`"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"find x"}]}`, stream)
		run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, anth.srv.URL, "claude-opus-5-5"), compatConn(2, compat.srv.URL)},
			body, nil)
		if run.resp.StatusCode != 200 || anth.hits.Load() != 1 || compat.hits.Load() != 1 {
			t.Fatalf("stream=%v: status %d, anthropic hits %d, compat hits %d: %s", stream, run.resp.StatusCode, anth.hits.Load(), compat.hits.Load(), run.body)
		}
		if stream {
			if !strings.HasPrefix(string(run.body), "event: message_start\n") || !strings.Contains(string(run.body), "event: message_stop\n") {
				t.Errorf("not a Messages stream: %s", run.body)
			}
		} else if !strings.Contains(string(run.body), `"type":"message"`) {
			t.Errorf("not a Messages answer: %s", run.body)
		}
		// The first target's warnings (the forced tool it could not take) do not describe the answer of the second.
		if strings.Contains(warnings(run.resp), "auto is used") {
			t.Errorf("stream=%v: Warning from the failed attempt: %s", stream, warnings(run.resp))
		}
		if run.rec.Attempt != 2 || run.c.conn.ID != 2 {
			t.Errorf("stream=%v: attempt = %d, conn = %d", stream, run.rec.Attempt, run.c.conn.ID)
		}
	}
}

func TestMessagesAnthropic503WithoutAFallbackIsAnEnvelope(t *testing.T) {
	anth := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, anth.srv.URL, "claude-opus-5-5")},
		`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var got struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(run.body, &got)
	if run.resp.StatusCode != 503 || got.Type != "error" || got.Error.Type != "overloaded_error" || run.resp.Header.Get("Retry-After") != "7" {
		t.Errorf("%d %s (Retry-After %q)", run.resp.StatusCode, run.body, run.resp.Header.Get("Retry-After"))
	}
}

func TestMessagesRejectsABadRequestInTheEnvelope(t *testing.T) {
	up := newScripted(t, compatChat)
	for name, body := range map[string]string{
		"not json":       `{"model":`,
		"no messages":    `{"model":"m","max_tokens":9}`,
		"empty messages": `{"model":"m","max_tokens":9,"messages":[]}`,
		"bad role":       `{"model":"m","max_tokens":9,"messages":[{"role":"system","content":"x"}]}`,
		"bad ragmux":     `{"model":"m","max_tokens":9,"ragmux":5,"messages":[{"role":"user","content":"x"}]}`,
	} {
		run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{compatConn(1, up.srv.URL)}, body, nil)
		if run.resp.StatusCode != 400 || !strings.Contains(string(run.body), `"type":"error"`) || !strings.Contains(string(run.body), `"invalid_request_error"`) {
			t.Errorf("%s: %d %s", name, run.resp.StatusCode, run.body)
		}
	}
	if up.hits.Load() != 0 {
		t.Error("a bad request reached the upstream")
	}
}

func TestMessagesToolPolicyRefusalsKeepTheirCodes(t *testing.T) {
	up := newScripted(t, compatChat)
	p := testProject()
	p.ToolDeny = []string{"shell"}
	body := `{"model":"m","max_tokens":9,"tools":[{"name":"shell","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"shell"},"messages":[{"role":"user","content":"x"}]}`
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{compatConn(1, up.srv.URL)}, body, nil)
	if run.resp.StatusCode != 400 || !strings.Contains(string(run.body), `"code":"tool_not_allowed"`) || up.hits.Load() != 0 {
		t.Errorf("%d %s", run.resp.StatusCode, run.body)
	}
}

// T4 through the gateway, on the chat endpoint: the project has a RAG store, so
// the gateway marks the request and the Anthropic adapter does not send back
// the thinking blocks an earlier turn produced.
func TestChatRequestOnARAGProjectDropsReplayedThinking(t *testing.T) {
	up := newScripted(t, jsonAnswer(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	for _, tc := range []struct {
		name string
		p    *store.Project
		kept bool
	}{
		{"rag project", ragProject(), false},
		{"rag project, retrieval off", func() *store.Project { p := ragProject(); p.RetrievalPolicy = store.RetrievalPolicyOff; return p }(), true},
		{"no store", testProject(), true},
	} {
		g := &Gateway{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		conn := anthropicConn(1, up.srv.URL, "claude-opus-5-5")
		prov, err := provider.New(provider.Config{ProviderType: "anthropic", BaseURL: conn.BaseURL, APIKey: conn.APIKey, Model: conn.ModelName, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		var req provider.ChatRequest
		if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"q"},`+
			`{"role":"assistant","content":"a","ragmux_thinking":[{"type":"thinking","thinking":"hm","signature":"sig-1"}]},{"role":"user","content":"next"}]}`), &req); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		hr := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		_, span := g.Tracer.Start(hr.Context(), "test", tracing.KindInternal)
		c := &call{op: chatOperation, w: rr, r: hr, span: span, project: tc.p, projectID: "1", obs: &requestObs{}, log: g.Log,
			conn: conn, targets: []*store.ModelConnection{conn}}
		g.prepareChat(c, prov, &req, ragmuxRequest{})
		if req.RAGActive != !tc.kept {
			t.Errorf("%s: RAGActive = %v", tc.name, req.RAGActive)
		}
		if _, err := prov.Chat(hr.Context(), req); err != nil {
			t.Fatal(err)
		}
		content := up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any)
		hasThinking := false
		for _, blk := range content {
			hasThinking = hasThinking || blk.(map[string]any)["type"] == "thinking"
		}
		if hasThinking != tc.kept {
			t.Errorf("%s: thinking sent = %v, want %v", tc.name, hasThinking, tc.kept)
		}
		gotWarning := strings.Contains(strings.Join(rr.Header().Values("Warning"), "\n"), "thinking blocks of earlier turns were dropped")
		if gotWarning == tc.kept {
			t.Errorf("%s: Warning present = %v", tc.name, gotWarning)
		}
	}
}

func TestRAGActive(t *testing.T) {
	id := int64(3)
	for _, tc := range []struct {
		name string
		p    store.Project
		want bool
	}{
		{"no store", store.Project{RetrievalPolicy: store.RetrievalPolicyAlways}, false},
		{"store, always", store.Project{RAGStoreID: &id, RetrievalPolicy: store.RetrievalPolicyAlways}, true},
		{"store, auto", store.Project{RAGStoreID: &id, RetrievalPolicy: store.RetrievalPolicyAuto}, true},
		{"store, off", store.Project{RAGStoreID: &id, RetrievalPolicy: store.RetrievalPolicyOff}, false},
	} {
		if got := ragActive(&tc.p); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}
