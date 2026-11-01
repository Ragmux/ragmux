package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
)

// anthropicUpstream is a fake Anthropic API: it records the headers and body
// of each call and answers the Messages request with a JSON message or, when
// the request streams, with the event sequence.
type anthropicUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	hdr  http.Header
	body []byte
}

const e2eMessageJSON = `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Hello"}],` +
	`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":2}}`

const e2eMessageStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e2e\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func newAnthropicUpstream(t *testing.T) *anthropicUpstream {
	u := &anthropicUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hdr, u.body = r.Header.Clone(), raw
		u.mu.Unlock()
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, e2eMessageStream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, e2eMessageJSON)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *anthropicUpstream) seen() (http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hdr, string(u.body)
}

// postMessages posts to /v1/messages with exactly the headers given.
func (e *env) postMessages(body string, headers map[string]string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// The Anthropic SDKs and Claude Code, pointed at Ragmux with a base URL and
// nothing else: x-api-key, the anthropic-* headers, a Messages body in and a
// Messages answer out, the upstream's own bytes (ADR-028).
func TestMessagesEndpointForAnthropicClients(t *testing.T) {
	up := newAnthropicUpstream(t)
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "claude", "provider_type": "anthropic",
		"base_url": up.URL, "api_key": "sk-ant-e2e-secret", "model_name": "claude-sonnet-4-5"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	created := e.call("POST", "/admin/api/projects", map[string]any{"name": "claude-code", "model_connection_id": conn["id"],
		"system_prompt": "Project prompt."}, "")
	if created["_status"] != float64(201) {
		t.Fatalf("create project: %v", created)
	}
	key := created["api_key"].(string)

	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "anthropic-beta": "prompt-caching-2024-07-31,context-management-2025-06-27",
		"x-claude-code-session-id": "cc-e2e"}
	req := `{"model":"claude-sonnet-4-5","max_tokens":64,"system":[{"type":"text","text":"You are Claude Code."}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]%s}`

	resp, raw := e.postMessages(strings.Replace(req, "%s", "", 1), hdr)
	if resp.StatusCode != 200 || raw != e2eMessageJSON {
		t.Fatalf("status %d, answer changed:\n%s", resp.StatusCode, raw)
	}
	got, body := up.seen()
	if got.Get("anthropic-beta") != hdr["anthropic-beta"] || got.Get("anthropic-version") != "2023-06-01" || got.Get("x-api-key") != "sk-ant-e2e-secret" {
		t.Errorf("upstream headers = %v", got)
	}
	if !strings.Contains(body, `"text":"You are Claude Code."`) || !strings.Contains(body, `"text":"Project prompt."`) ||
		strings.Index(body, "You are Claude Code.") > strings.Index(body, "Project prompt.") {
		t.Errorf("the project prompt does not follow the client's system blocks: %s", body)
	}
	if resp.Header.Get("request-id") == "" || resp.Header.Get("request-id") != resp.Header.Get("X-Request-Id") {
		t.Errorf("request-id %q, X-Request-Id %q", resp.Header.Get("request-id"), resp.Header.Get("X-Request-Id"))
	}

	// The same call, streamed.
	resp, raw = e.postMessages(strings.Replace(req, "%s", `,"stream":true`, 1), hdr)
	if resp.StatusCode != 200 || raw != e2eMessageStream || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, stream changed:\n%q", resp.StatusCode, raw)
	}

	// The calls are in the request log under the Messages endpoint and the
	// session Claude Code named. The rows are written once the response is out.
	found := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		logs := e.call("GET", "/admin/api/metrics/requests?limit=10", nil, "")
		rows, _ := logs["_list"].([]any)
		found = 0
		for _, r := range rows {
			row, _ := r.(map[string]any)
			if row["endpoint"] == "messages" && row["session_id"] == "cc-e2e" {
				found++
			}
		}
		if found == 2 {
			break
		}
	}
	if found != 2 {
		t.Errorf("%d request log rows for the Messages calls, want 2", found)
	}
}

// A management key is no gateway key under either header, and neither header
// opens the admin API (PRD rule 2, read with ADR-028).
func TestManagementKeyIsRefusedInXAPIKey(t *testing.T) {
	up := newAnthropicUpstream(t)
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "claude", "provider_type": "anthropic",
		"base_url": up.URL, "api_key": "sk-ant-e2e-secret", "model_name": "claude-sonnet-4-5"}, "")
	created := e.call("POST", "/admin/api/projects", map[string]any{"name": "p", "model_connection_id": conn["id"]}, "")
	gateway := created["api_key"].(string)
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "management", "name": "m", "scopes": []string{"read"}}, "")
	if k["_status"] != float64(201) {
		t.Fatalf("management key: %v", k)
	}
	mgmt := k["api_key"].(string)

	get := func(path string, hdr map[string]string) int {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// The control: the management key in Authorization is what the admin API takes.
	if got := get("/admin/api/projects", map[string]string{"Authorization": "Bearer " + mgmt}); got != 200 {
		t.Fatalf("management key in Authorization: %d", got)
	}
	if got := get("/admin/api/projects", map[string]string{"x-api-key": mgmt}); got != 401 {
		t.Errorf("management key in x-api-key on /admin/api: %d, want 401", got)
	}
	if got := get("/admin/api/projects", map[string]string{"x-api-key": gateway}); got != 401 {
		t.Errorf("gateway key in x-api-key on /admin/api: %d, want 401", got)
	}
	for name, hdr := range map[string]map[string]string{
		"x-api-key":     {"x-api-key": mgmt},
		"bearer":        {"Authorization": "Bearer " + mgmt},
		"different two": {"Authorization": "Bearer " + gateway, "x-api-key": mgmt},
	} {
		resp, raw := e.postMessages(`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`, hdr)
		if resp.StatusCode != 401 || !strings.Contains(raw, `"type":"error"`) || !strings.Contains(raw, `"authentication_error"`) {
			t.Errorf("%s on /v1/messages: %d %s", name, resp.StatusCode, raw)
		}
	}
	if got := get("/v1/models", map[string]string{"x-api-key": mgmt}); got != 401 {
		t.Errorf("management key in x-api-key on /v1/models: %d, want 401", got)
	}
}
