package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// The policy for request fields the gateway does not read itself (ADR-041).
// The tests without a database drive decodeChat and the Messages endpoint
// directly; the ones that go through the whole stack need the test database.

func TestExtraWarnings(t *testing.T) {
	if got := extraWarnings(nil); got != nil {
		t.Errorf("none removed: %v", got)
	}
	if got := extraWarnings([]string{"seed", "store"}); !reflect.DeepEqual(got,
		[]string{"field seed removed by project policy", "field store removed by project policy"}) {
		t.Errorf("two fields: %v", got)
	}
	// A name that is not safe to echo is counted, not named; so are the names
	// past the bound.
	got := extraWarnings([]string{`a"b`, "x\r\ny", "ok"})
	if !reflect.DeepEqual(got, []string{"field ok removed by project policy", "2 more field(s) removed by project policy"}) {
		t.Errorf("unsafe names: %v", got)
	}
	if got := extraWarnings([]string{"a b"}); !reflect.DeepEqual(got, []string{"1 field(s) removed by project policy"}) {
		t.Errorf("only unsafe: %v", got)
	}
	many := []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"}
	got = extraWarnings(many)
	if len(got) != maxWarnedExtra+1 || got[maxWarnedExtra] != "2 more field(s) removed by project policy" {
		t.Errorf("bounded: %v", got)
	}
}

// decodeChatFor runs the chat decode step for a body against a project, with no
// database.
func decodeChatFor(t *testing.T, p *store.Project, body string) (provider.ChatRequest, ragmuxRequest, *call, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c := &call{op: chatOperation, w: rec, r: r, project: p, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g := &Gateway{Log: c.log}
	req, opts, ok := g.decodeChat(c)
	if !ok {
		t.Logf("answered %d: %s", rec.Code, rec.Body)
	}
	return req, opts, c, ok
}

const chatWithExtras = `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"seed":9,"logprobs":true,` +
	`"parallel_tool_calls":false,"reasoning_effort":"low","store":true,"service_tier":"flex","keep_alive":"5m","num_ctx":4096,` +
	`"options":{"num_predict":8},"safety_identifier":"s-1"}`

func extraKeys(req provider.ChatRequest) []string {
	var keys []string
	for k := range req.Extra {
		keys = append(keys, k)
	}
	return keys
}

func TestDecodeChatExtraPolicy(t *testing.T) {
	withLists := func(allow, deny []string) *store.Project {
		p := testProject()
		p.ExtraAllow, p.ExtraDeny = allow, deny
		return p
	}

	// Empty lists: every field is kept, and nothing is warned.
	req, opts, _, ok := decodeChatFor(t, testProject(), chatWithExtras)
	if !ok || len(req.Extra) != 10 || len(opts.Warnings) != 0 {
		t.Fatalf("empty lists: ok %v, %d extra fields (%v), warnings %v", ok, len(req.Extra), extraKeys(req), opts.Warnings)
	}
	req, opts, _, ok = decodeChatFor(t, withLists([]string{}, []string{}), chatWithExtras)
	if !ok || len(req.Extra) != 10 || len(opts.Warnings) != 0 {
		t.Fatalf("empty non-nil lists: ok %v, %d extra fields, warnings %v", ok, len(req.Extra), opts.Warnings)
	}

	// Deny, with a glob: the field is gone and named in a Warning.
	req, opts, _, ok = decodeChatFor(t, withLists(nil, []string{"store", "service_*"}), chatWithExtras)
	if !ok {
		t.Fatal("refused")
	}
	for _, gone := range []string{"store", "service_tier"} {
		if _, has := req.Extra[gone]; has {
			t.Errorf("%s was kept", gone)
		}
	}
	for _, kept := range []string{"seed", "logprobs", "parallel_tool_calls", "reasoning_effort", "keep_alive", "num_ctx", "options"} {
		if _, has := req.Extra[kept]; !has {
			t.Errorf("%s was removed", kept)
		}
	}
	if !reflect.DeepEqual(opts.Warnings, []string{"field service_tier removed by project policy", "field store removed by project policy"}) {
		t.Errorf("warnings = %v", opts.Warnings)
	}
	// The fields the gateway reads are the client's.
	if req.Model != "m" || req.Temperature == nil || *req.Temperature != 0.3 || len(req.Messages) != 1 {
		t.Errorf("known fields changed: %+v", req)
	}

	// Deny wins over allow; an allow list removes what it does not match,
	// including what Ollama reads.
	req, opts, _, _ = decodeChatFor(t, withLists([]string{"seed", "store", "options"}, []string{"store"}), chatWithExtras)
	if got := extraKeys(req); len(got) != 2 || req.Extra["seed"] == nil || req.Extra["options"] == nil {
		t.Errorf("allow + deny kept %v", got)
	}
	if len(opts.Warnings) != maxWarnedExtra+1 || opts.Warnings[maxWarnedExtra] != "3 more field(s) removed by project policy" {
		t.Errorf("warnings = %v, want five named and the rest counted", opts.Warnings)
	}
	req, _, _, _ = decodeChatFor(t, withLists(nil, []string{"keep_alive", "num_ctx", "options"}), chatWithExtras)
	for _, k := range []string{"keep_alive", "num_ctx", "options"} {
		if _, has := req.Extra[k]; has {
			t.Errorf("%s was kept: Ollama would read it", k)
		}
	}

	// The ragmux object is the gateway's own and is not a field a list governs.
	req, opts, _, ok = decodeChatFor(t, withLists([]string{"seed"}, nil),
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":1,"ragmux":{"sources":"inline"}}`)
	if !ok || !opts.InlineSources || len(opts.Warnings) != 0 || req.Extra["seed"] == nil {
		t.Errorf("ragmux: ok %v, opts %+v, extra %v", ok, opts, extraKeys(req))
	}
}

// A safety_identifier the policy removed does not go to a translated provider
// request; the end user of record is still the client's own.
func TestDecodeChatExtraPolicyAndTheEndUser(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"hi"}],"safety_identifier":"s-1","user":"u-1"}`
	req, _, c, _ := decodeChatFor(t, testProject(), body)
	if req.EndUser != "s-1" || c.providerEndUser != "s-1" || c.endUser != "s-1" || req.Extra["safety_identifier"] == nil {
		t.Errorf("no policy: EndUser %q, providerEndUser %q, endUser %q", req.EndUser, c.providerEndUser, c.endUser)
	}

	p := testProject()
	p.ExtraDeny = []string{"safety_identifier"}
	req, opts, c, _ := decodeChatFor(t, p, body)
	if _, has := req.Extra["safety_identifier"]; has {
		t.Error("safety_identifier stayed in the request")
	}
	if req.EndUser != "u-1" || c.providerEndUser != "u-1" {
		t.Errorf("with safety_identifier stripped the provider gets user: EndUser %q, providerEndUser %q, want u-1", req.EndUser, c.providerEndUser)
	}
	if c.endUser != "s-1" {
		t.Errorf("end user of record = %q, want the client's value kept for Ragmux's own record", c.endUser)
	}
	if !reflect.DeepEqual(opts.Warnings, []string{"field safety_identifier removed by project policy"}) {
		t.Errorf("warnings = %v", opts.Warnings)
	}

	// Nothing else to fall back on: nothing is set aside for the provider; an
	// unusable user is not forwarded either.
	for _, only := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"safety_identifier":"s-1"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"safety_identifier":"s-1","user":"a b"}`,
	} {
		p.ExtraDeny = []string{"safety_identifier"}
		req, _, c, _ = decodeChatFor(t, p, only)
		if req.EndUser != "" || c.providerEndUser != "" || c.endUser != "s-1" {
			t.Errorf("%s: EndUser %q, providerEndUser %q, endUser %q", only, req.EndUser, c.providerEndUser, c.endUser)
		}
	}

	// A policy that strips something else leaves it alone.
	p.ExtraDeny = []string{"store"}
	req, _, c, _ = decodeChatFor(t, p, body)
	if req.EndUser != "s-1" || c.providerEndUser != "s-1" {
		t.Errorf("other field stripped: EndUser %q, providerEndUser %q", req.EndUser, c.providerEndUser)
	}
}

// What the upstream gets, for an OpenAI-compatible connection.
func TestChatExtraPolicyUpstreamBody(t *testing.T) {
	up := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	send := func(p *store.Project) map[string]any {
		t.Helper()
		req, _, _, ok := decodeChatFor(t, p, chatWithExtras)
		if !ok {
			t.Fatal("refused")
		}
		conn := compatConn(1, up.srv.URL)
		prov, err := provider.New(provider.Config{ProviderType: conn.ProviderType, BaseURL: conn.BaseURL, APIKey: conn.APIKey,
			Model: conn.ModelName, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prov.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return up.lastBody(t)
	}

	// Empty lists: the body is what it was before the lists existed.
	b := send(testProject())
	for k, want := range map[string]any{"seed": 9.0, "logprobs": true, "parallel_tool_calls": false, "reasoning_effort": "low",
		"store": true, "service_tier": "flex", "temperature": 0.3} {
		if !reflect.DeepEqual(b[k], want) {
			t.Errorf("empty lists: %s = %v, want %v", k, b[k], want)
		}
	}

	// Denied fields do not reach the upstream; the others do.
	p := testProject()
	p.ExtraDeny = []string{"store", "service_tier", "safety_identifier"}
	b = send(p)
	for _, gone := range []string{"store", "service_tier", "safety_identifier"} {
		if _, has := b[gone]; has {
			t.Errorf("%s reached the upstream", gone)
		}
	}
	if b["seed"] != 9.0 || b["reasoning_effort"] != "low" || b["model"] != "compat-model" || b["temperature"] != 0.3 {
		t.Errorf("body = %v", b)
	}
}

// ---------------------------------------------------------------- Messages

const nativeWithExtras = `{"model":"claude-opus-5-5","max_tokens":64,"top_k":4,"mcp_servers":[{"type":"url","url":"https://mcp.example","name":"m"}],` +
	`"container":"c1","context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},"thinking":{"type":"adaptive"},` +
	`"metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"hi"}]}`

func TestMessagesNativeWithEmptyListsIsUnchanged(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	run := runMessages(t, &Gateway{}, testProject(), []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, nativeWithExtras, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	var sent, want map[string]any
	_ = json.Unmarshal([]byte(nativeWithExtras), &want)
	sent = up.lastBody(t)
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("upstream body = %v\nwant the client's body %v", sent, want)
	}
	if w := warnings(run.resp); strings.Contains(w, "project policy") {
		t.Errorf("Warning = %q", w)
	}
}

func TestMessagesNativeExtraPolicy(t *testing.T) {
	// Deny: the two fields that cost money are cut, the answer is served, and
	// the Warning names them. The fields with a handled name are not touched.
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	p := testProject()
	p.ExtraDeny = []string{"mcp_servers", "container"}
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, nativeWithExtras, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	b := up.lastBody(t)
	for _, gone := range []string{"mcp_servers", "container"} {
		if _, has := b[gone]; has {
			t.Errorf("%s reached the upstream", gone)
		}
	}
	for _, kept := range []string{"top_k", "context_management", "thinking", "metadata", "messages", "max_tokens"} {
		if _, has := b[kept]; !has {
			t.Errorf("%s was removed", kept)
		}
	}
	w := warnings(run.resp)
	if !strings.Contains(w, `299 ragmux "field container removed by project policy"`) || !strings.Contains(w, `299 ragmux "field mcp_servers removed by project policy"`) {
		t.Errorf("Warning = %q", w)
	}

	// Allow: what it does not name is cut, handled fields included in "kept".
	up = newScripted(t, jsonAnswer(anthropicAnswer))
	p = testProject()
	p.ExtraAllow = []string{"top_k"}
	run = runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, nativeWithExtras, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	b = up.lastBody(t)
	for _, gone := range []string{"mcp_servers", "container", "context_management"} {
		if _, has := b[gone]; has {
			t.Errorf("allow list: %s reached the upstream", gone)
		}
	}
	if b["top_k"] != 4.0 || b["thinking"] == nil || b["metadata"] == nil {
		t.Errorf("allow list: body = %v", b)
	}

	// Deny wins over allow.
	up = newScripted(t, jsonAnswer(anthropicAnswer))
	p = testProject()
	p.ExtraAllow, p.ExtraDeny = []string{"*"}, []string{"mcp_*"}
	run = runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, nativeWithExtras, nil)
	if _, has := up.lastBody(t)["mcp_servers"]; has || run.resp.StatusCode != 200 {
		t.Errorf("deny did not win: %d %v", run.resp.StatusCode, up.lastBody(t))
	}
}

// The policy is applied to the parsed body, so a client cannot get a field
// past it by repeating the key or writing it with an escape.
func TestMessagesNativeExtraPolicyCannotBeSidestepped(t *testing.T) {
	up := newScripted(t, jsonAnswer(anthropicAnswer))
	p := testProject()
	p.ExtraDeny = []string{"mcp_servers", "container"}
	body := `{"model":"claude-opus-5-5","max_tokens":64,"mcp_servers":[{"url":"https://a"}],"mcp_servers":[{"url":"https://b"}],` +
		`"container":"c1","messages":[{"role":"user","content":"hi"}]}`
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, up.srv.URL, "claude-opus-5-5")}, body, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	raw := string(up.body[len(up.body)-1])
	if strings.Contains(raw, "mcp") || strings.Contains(raw, "container") || strings.Contains(raw, "example") {
		t.Errorf("a denied field reached the upstream: %s", raw)
	}
}

// A route whose targets take different paths is governed once, on the body,
// so the translated target and the native one see the same fields.
func TestMessagesExtraPolicyOnBothPaths(t *testing.T) {
	anth := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	})
	compat := newScripted(t, compatChat)
	p := testProject()
	p.ExtraDeny = []string{"mcp_servers", "container", "top_k"}
	run := runMessages(t, &Gateway{}, p, []*store.ModelConnection{anthropicConn(1, anth.srv.URL, "claude-opus-5-5"), compatConn(2, compat.srv.URL)},
		nativeWithExtras, nil)
	if run.resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", run.resp.StatusCode, run.body)
	}
	if raw := string(anth.body[0]); strings.Contains(raw, "mcp_servers") || strings.Contains(raw, "container") || strings.Contains(raw, `"top_k"`) {
		t.Errorf("the native attempt carried a denied field: %s", raw)
	}
	cb := compat.lastBody(t)
	for _, k := range []string{"mcp_servers", "container", "top_k"} {
		if _, has := cb[k]; has {
			t.Errorf("the translated attempt carried %s", k)
		}
	}
	w := warnings(run.resp)
	for _, k := range []string{"mcp_servers", "container", "top_k"} {
		if !strings.Contains(w, "field "+k+" removed by project policy") {
			t.Errorf("Warning = %q, want %s named", w, k)
		}
	}
}

// ---------------------------------------------------------- full stack (DB)

func (e *env) setExtraLists(allow, deny []string) {
	e.t.Helper()
	e.proj.ExtraAllow, e.proj.ExtraDeny = allow, deny
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		e.t.Fatal(err)
	}
}

func TestExtraPolicyThroughTheStack(t *testing.T) {
	e := newEnv(t, nil)
	seen := e.recordUpstream(nil)
	body := func() map[string]any {
		return map[string]any{"messages": userMsg, "seed": 5, "logprobs": true, "parallel_tool_calls": false,
			"reasoning_effort": "low", "store": true, "service_tier": "flex"}
	}

	// A project that never set the lists forwards every field, as before.
	resp, out := e.chat(body())
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	b := seen.last()
	for _, k := range []string{"seed", "logprobs", "parallel_tool_calls", "reasoning_effort", "store", "service_tier"} {
		if _, has := b[k]; !has {
			t.Errorf("no lists: %s did not reach the upstream", k)
		}
	}
	if len(resp.Header.Values("Warning")) != 0 {
		t.Errorf("no lists: Warning = %v", resp.Header.Values("Warning"))
	}

	// Deny: the request is served, the fields are cut and named.
	e.setExtraLists(nil, []string{"store", "service_*"})
	resp, out = e.chat(body())
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	b = seen.last()
	if _, has := b["store"]; has {
		t.Error("store reached the upstream")
	}
	if _, has := b["service_tier"]; has {
		t.Error("service_tier reached the upstream")
	}
	if b["seed"] != 5.0 || b["reasoning_effort"] != "low" {
		t.Errorf("body = %v", b)
	}
	w := strings.Join(resp.Header.Values("Warning"), "\n")
	if !strings.Contains(w, `field store removed by project policy`) || !strings.Contains(w, `field service_tier removed by project policy`) {
		t.Errorf("Warning = %q", w)
	}

	// Allow: only what it names is forwarded.
	e.setExtraLists([]string{"seed", "reasoning_effort"}, nil)
	if resp, out = e.chat(body()); resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	b = seen.last()
	for _, gone := range []string{"logprobs", "parallel_tool_calls", "store", "service_tier"} {
		if _, has := b[gone]; has {
			t.Errorf("allow list: %s reached the upstream", gone)
		}
	}
	if b["seed"] != 5.0 || b["reasoning_effort"] != "low" {
		t.Errorf("allow list: body = %v", b)
	}

	// A list that is cleared forwards everything again.
	e.setExtraLists([]string{}, []string{})
	if resp, out = e.chat(body()); resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	if _, has := seen.last()["store"]; !has {
		t.Error("cleared lists: store did not reach the upstream")
	}
}

func TestExtraPolicyOnNativeMessagesThroughTheStack(t *testing.T) {
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
	e.setExtraLists(nil, []string{"mcp_servers", "container"})
	resp, raw := e.messages(nativeWithExtras, map[string]string{"x-api-key": e.key})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	b := up.lastBody(t)
	if _, has := b["mcp_servers"]; has {
		t.Error("mcp_servers reached the upstream")
	}
	if _, has := b["container"]; has {
		t.Error("container reached the upstream")
	}
	if b["top_k"] != 4.0 {
		t.Errorf("body = %v", b)
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Warning"), "\n"), "field mcp_servers removed by project policy") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}
}
