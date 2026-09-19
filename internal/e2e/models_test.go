package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/testdb"
)

// keyedUpstream is a chat endpoint that only answers with the expected
// bearer key and echoes the received credential in its error otherwise, so
// the tests can check both key reuse and redaction.
func keyedUpstream(key string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad credential " + got, "type": "auth"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	}))
}

func TestConnectionTests(t *testing.T) {
	const key = "topsecret-key-1234567890"
	up := keyedUpstream(key)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "keyed", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": key, "model_name": "m"}, "")
	if conn["_status"] != float64(201) || conn["last_test_at"] != nil || conn["last_test_ok"] != nil || conn["last_test_error"] != "" {
		t.Fatalf("create: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	// Unsaved test with the key given inline.
	body := map[string]any{"provider_type": "custom_openai", "base_url": up.URL + "/v1", "api_key": key, "model_name": "m", "mode": "chat"}
	if r := e.call("POST", "/admin/api/models/test", body, ""); r["_status"] != float64(200) || r["ok"] != true || r["reply"] != "pong" {
		t.Fatalf("unsaved test: %v", r)
	}
	// Without a key the upstream refuses; the echoed credential is redacted.
	body["api_key"] = "sk-wrongkey12345678"
	r := e.call("POST", "/admin/api/models/test", body, "")
	if r["_status"] != float64(200) || r["ok"] != false || !strings.Contains(r["error"].(string), "[redacted]") || strings.Contains(r["error"].(string), "wrongkey") {
		t.Fatalf("unsaved test with bad key: %v", r)
	}
	// connection_id with an empty key reuses the stored one for a changed model.
	body["api_key"], body["connection_id"], body["model_name"] = "", connID, "other-model"
	if r := e.call("POST", "/admin/api/models/test", body, ""); r["ok"] != true {
		t.Fatalf("unsaved test via connection_id: %v", r)
	}
	// Validation matches create.
	if r := e.call("POST", "/admin/api/models/test", map[string]any{"provider_type": "nope", "model_name": "m"}, ""); r["_status"] != float64(400) {
		t.Errorf("invalid provider_type: %v", r)
	}
	if r := e.call("POST", "/admin/api/models/test", map[string]any{"provider_type": "custom_openai", "base_url": "ftp://x/v1", "model_name": "m"}, ""); r["_status"] != float64(400) {
		t.Errorf("bad scheme: %v", r)
	}
	// Viewers cannot test.
	e.call("POST", "/admin/api/users", map[string]any{"username": "viewer", "password": "password123", "role": "viewer"}, "")
	if r := e.call("POST", "/admin/api/models/test", body, e.login("viewer", "password123")); r["_status"] != float64(403) {
		t.Errorf("viewer test: %v", r)
	}

	// Nothing was persisted by the unsaved tests.
	if g := e.call("GET", fmt.Sprintf("/admin/api/models/%d", connID), nil, ""); g["last_test_at"] != nil {
		t.Fatalf("unsaved test must not be recorded: %v", g)
	}
	// The saved test records its outcome.
	if r := e.call("POST", fmt.Sprintf("/admin/api/models/%d/test", connID), map[string]any{"mode": "chat"}, ""); r["ok"] != true {
		t.Fatalf("saved test: %v", r)
	}
	g := e.call("GET", fmt.Sprintf("/admin/api/models/%d", connID), nil, "")
	if g["last_test_ok"] != true || g["last_test_at"] == nil || g["last_test_error"] != "" {
		t.Fatalf("recorded outcome: %v", g)
	}
	if _, ok := g["last_test_latency_ms"].(float64); !ok {
		t.Fatalf("last_test_latency_ms: %v", g)
	}
	// A failing test is recorded too, without the credential.
	bad := e.call("POST", "/admin/api/models", map[string]any{"name": "bad", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "sk-anotherwrong1234567", "model_name": "m"}, "")
	badID := int64(bad["id"].(float64))
	if r := e.call("POST", fmt.Sprintf("/admin/api/models/%d/test", badID), nil, ""); r["ok"] != false {
		t.Fatalf("failing test: %v", r)
	}
	list := e.call("GET", "/admin/api/models", nil, "")["_list"].([]any)
	var found bool
	for _, it := range list {
		m := it.(map[string]any)
		if int64(m["id"].(float64)) != badID {
			continue
		}
		found = true
		msg, _ := m["last_test_error"].(string)
		if m["last_test_ok"] != false || msg == "" || strings.Contains(msg, "anotherwrong") {
			t.Errorf("recorded failure: %v", m)
		}
	}
	if !found {
		t.Error("connection missing from list")
	}
}

func TestUnsavedConnectionTestBlocksPrivateHosts(t *testing.T) {
	e := newEnvWith(t, testdb.Config(t), true, func(a *admin.Admin) { a.AllowPrivateUpstreams = false })
	for _, u := range []string{"http://169.254.169.254/latest", "http://127.0.0.1:11434/v1", "http://[::1]:8000/v1"} {
		r := e.call("POST", "/admin/api/models/test", map[string]any{"provider_type": "custom_openai", "base_url": u,
			"api_key": "k", "model_name": "m"}, "")
		if r["_status"] != float64(400) || !strings.Contains(r["error"].(map[string]any)["message"].(string), "private") {
			t.Errorf("%s: %v", u, r)
		}
	}
}

func TestPrivateUpstreamFlag(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	create := func(baseURL string) map[string]any {
		t.Helper()
		r := e.call("POST", "/admin/api/models", map[string]any{"name": "c-" + baseURL, "provider_type": "openai",
			"base_url": baseURL, "api_key": "sk-test12345678", "model_name": "gpt-4o"}, "")
		if r["_status"] != float64(201) {
			t.Fatalf("create %q: %v", baseURL, r)
		}
		return r
	}
	cases := map[string]bool{
		"":                          false, // provider default URL
		"http://127.0.0.1:11434/v1": true,
		"http://localhost:8000/v1":  true,
		"http://[::1]:8000/v1":      true,
		"http://10.1.2.3/v1":        true,
		"http://169.254.169.254/v1": true,
		"http://8.8.8.8/v1":         false,
	}
	ids := map[string]int64{}
	for u, want := range cases {
		r := create(u)
		if r["private_upstream"] != want {
			t.Errorf("create %q: private_upstream = %v, want %v", u, r["private_upstream"], want)
		}
		ids[u] = int64(r["id"].(float64))
	}
	for _, it := range e.call("GET", "/admin/api/models", nil, "")["_list"].([]any) {
		m := it.(map[string]any)
		if want, ok := cases[m["base_url"].(string)]; ok && m["private_upstream"] != want {
			t.Errorf("list %q: private_upstream = %v, want %v", m["base_url"], m["private_upstream"], want)
		}
	}
	// Recomputed on update, in both directions.
	id := ids["http://127.0.0.1:11434/v1"]
	upd := e.call("PUT", fmt.Sprintf("/admin/api/models/%d", id), map[string]any{"name": "c", "provider_type": "openai",
		"base_url": "", "model_name": "gpt-4o"}, "")
	if upd["_status"] != float64(200) || upd["private_upstream"] != false {
		t.Errorf("update to default url: %v", upd)
	}
	upd = e.call("PUT", fmt.Sprintf("/admin/api/models/%d", id), map[string]any{"name": "c", "provider_type": "openai",
		"base_url": "http://192.168.1.10/v1", "model_name": "gpt-4o"}, "")
	if upd["private_upstream"] != true {
		t.Errorf("update to private url: %v", upd)
	}
	if g := e.call("GET", fmt.Sprintf("/admin/api/models/%d", id), nil, ""); g["private_upstream"] != true {
		t.Errorf("get after update: %v", g)
	}
}

func TestProviderTypeCapabilities(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	list := e.call("GET", "/admin/api/provider-types", nil, "")["_list"].([]any)
	// Every adapter now translates tools, Gemini included.
	wantTools := map[string]bool{"openai": true, "anthropic": true, "gemini": true, "deepseek": true, "ollama": true, "custom_openai": true}
	// Gemini sends a function call whole inside one chunk rather than as
	// argument deltas, which is the one capability that separates it here.
	wantToolStreaming := map[string]bool{"openai": true, "anthropic": true, "gemini": false, "deepseek": true, "ollama": false, "custom_openai": true}
	seen := 0
	for _, it := range list {
		m := it.(map[string]any)
		typ := m["type"].(string)
		want, ok := wantTools[typ]
		if !ok {
			t.Errorf("unexpected type %q", typ)
			continue
		}
		seen++
		if m["supports_tools"] != want || m["supports_streaming"] != true {
			t.Errorf("%s: %v", typ, m)
		}
		// The flat fields are kept for older clients, so they must keep
		// agreeing with the capabilities object they are taken from.
		caps, _ := m["capabilities"].(map[string]any)
		if caps == nil {
			t.Errorf("%s: no capabilities object", typ)
			continue
		}
		if caps["tools"] != want || caps["streaming"] != true ||
			caps["embeddings"] != m["supports_embeddings"] {
			t.Errorf("%s: capabilities disagree with the flat fields: %v", typ, m)
		}
		if caps["tool_streaming"] != wantToolStreaming[typ] {
			t.Errorf("%s: tool_streaming = %v, want %v", typ, caps["tool_streaming"], wantToolStreaming[typ])
		}
	}
	if seen != len(wantTools) {
		t.Errorf("saw %d types, want %d", seen, len(wantTools))
	}
}
