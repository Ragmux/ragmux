package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
)

// The Messages endpoint behind the real key lookup, route table and request
// log. messages_unit_test.go covers the request handling with the routing
// fixed; these cover what needs the database.

// messages posts to /v1/messages with exactly the headers given: unlike call,
// it adds no Authorization of its own.
func (e *env) messages(body any, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/v1/messages", rdr)
	if err != nil {
		e.t.Fatal(err)
	}
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
	return resp, raw
}

var messagesBody = map[string]any{"model": "claude-x", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

// envelopeOf decodes an Anthropic error envelope.
func envelopeOf(t *testing.T, raw []byte) (typ, code, requestID string) {
	t.Helper()
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Type != "error" || got.Error.Message == "" {
		t.Fatalf("not an Anthropic error envelope: %s", raw)
	}
	return got.Error.Type, got.Error.Code, got.RequestID
}

func TestMessagesAcceptsBothKeyHeaders(t *testing.T) {
	e := newEnv(t, nil)
	_, userKey := e.userKey("sdk", []int64{e.proj.ID}, nil)
	for name, hdr := range map[string]map[string]string{
		"x-api-key, project key": {"x-api-key": e.key},
		"x-api-key, user key":    {"x-api-key": userKey},
		"bearer":                 {"Authorization": "Bearer " + e.key},
		"both, the same key":     {"Authorization": "Bearer " + e.key, "x-api-key": e.key},
	} {
		resp, raw := e.messages(messagesBody, hdr)
		if resp.StatusCode != 200 || !strings.Contains(string(raw), `"type":"message"`) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, raw)
		}
	}
	// The other /v1 endpoints take it too: Claude Code lists models with it.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/v1/models with x-api-key: %d", resp.StatusCode)
	}
}

func TestMessagesRefusesWhatItCannotAuthenticate(t *testing.T) {
	e := newEnv(t, nil)
	_, userKey := e.userKey("sdk", []int64{e.proj.ID}, nil)
	_, mgmtRaw, err := e.st.CreateAPIKey(context.Background(), &store.APIKey{Kind: store.KindManagement, Name: "m", UserID: e.owner.ID,
		Scopes: []string{store.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		hdr  map[string]string
		code string
	}{
		"no key":                                {nil, ""},
		"unknown key":                           {map[string]string{"x-api-key": "sk-proj-doesnotexist"}, ""},
		"management key, x-api-key":             {map[string]string{"x-api-key": mgmtRaw}, ""},
		"management key, bearer":                {map[string]string{"Authorization": "Bearer " + mgmtRaw}, ""},
		"two different keys":                    {map[string]string{"Authorization": "Bearer " + e.key, "x-api-key": userKey}, ""},
		"a good bearer, a bad x-api-key":        {map[string]string{"Authorization": "Bearer " + e.key, "x-api-key": "sk-proj-nope"}, ""},
		"a good x-api-key, a management bearer": {map[string]string{"x-api-key": e.key, "Authorization": "Bearer " + mgmtRaw}, ""},
	}
	for name, tc := range cases {
		resp, raw := e.messages(messagesBody, tc.hdr)
		if resp.StatusCode != 401 {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, raw)
			continue
		}
		typ, code, id := envelopeOf(t, raw)
		if typ != "authentication_error" || code != tc.code || id == "" || id != resp.Header.Get("X-Request-Id") || resp.Header.Get("request-id") != id {
			t.Errorf("%s: type %q code %q id %q: %s", name, typ, code, id, raw)
		}
	}
	// The same management key is no better on the other /v1 endpoints.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", mgmtRaw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("/v1/models with a management key in x-api-key: %d", resp.StatusCode)
	}
}

func TestMessagesGatewayErrorsAreInTheAnthropicEnvelope(t *testing.T) {
	e := newEnv(t, nil)
	_, noChat := e.userKey("models-only", []int64{e.proj.ID}, func(k *store.APIKey) { k.Scopes = []string{store.ScopeModels} })
	resp, raw := e.messages(messagesBody, map[string]string{"x-api-key": noChat})
	typ, code, _ := envelopeOf(t, raw)
	if resp.StatusCode != 403 || typ != "permission_error" || code != "insufficient_scope" {
		t.Errorf("scope: %d %s", resp.StatusCode, raw)
	}

	// A project with chat routes answers an unknown model with 404.
	e.addRoute("fast", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})
	resp, raw = e.messages(map[string]any{"model": "nope", "max_tokens": 9, "messages": messagesBody["messages"]}, map[string]string{"x-api-key": e.key})
	typ, code, _ = envelopeOf(t, raw)
	if resp.StatusCode != 404 || typ != "not_found_error" || code != "model_not_found" {
		t.Errorf("unknown model: %d %s", resp.StatusCode, raw)
	}

	// The second request in a minute is over a limit of one.
	e.proj.RateLimitRPM = 1
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	good := map[string]any{"model": "fast", "max_tokens": 9, "messages": messagesBody["messages"]}
	if resp, raw = e.messages(good, map[string]string{"x-api-key": e.key}); resp.StatusCode != 200 {
		t.Fatalf("first request: %d %s", resp.StatusCode, raw)
	}
	resp, raw = e.messages(good, map[string]string{"x-api-key": e.key})
	typ, code, _ = envelopeOf(t, raw)
	if resp.StatusCode != 429 || typ != "rate_limit_error" || code == "" || resp.Header.Get("Retry-After") == "" {
		t.Errorf("rate limit: %d %s (Retry-After %q)", resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
}

func TestMessagesReadsTheClaudeCodeSession(t *testing.T) {
	e := newEnv(t, nil)
	hdr := func(kv ...string) map[string]string {
		h := map[string]string{"x-api-key": e.key}
		for i := 0; i+1 < len(kv); i += 2 {
			h[kv[i]] = kv[i+1]
		}
		return h
	}
	if resp, raw := e.messages(messagesBody, hdr("x-claude-code-session-id", "cc-session-1")); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if row := e.lastLog(); row.SessionID != "cc-session-1" || row.Endpoint != "messages" || row.RequestedModel != "claude-x" {
		t.Errorf("row = %+v", row)
	}
	// X-Ragmux-Session wins when both are there.
	if resp, _ := e.messages(messagesBody, hdr("x-claude-code-session-id", "cc-session-2", "X-Ragmux-Session", "mine")); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if s := e.waitSession("mine", 1); s.Requests != 1 {
		t.Errorf("session mine = %+v", s)
	}
	// A bad value is refused, in the envelope.
	resp, raw := e.messages(messagesBody, hdr("x-claude-code-session-id", "bad id!"))
	typ, code, _ := envelopeOf(t, raw)
	if resp.StatusCode != 400 || typ != "invalid_request_error" || code != errInvalidSession {
		t.Errorf("bad session: %d %s", resp.StatusCode, raw)
	}
	// The chat endpoint does not read the Claude Code header.
	resp, out := e.chatWith(map[string]string{"x-claude-code-session-id": "bad id!"}, map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 {
		t.Errorf("chat with a Claude Code header: %d %v", resp.StatusCode, out)
	}
}

// anthropicEnv is an env whose project is served by an Anthropic connection
// that points at a scripted upstream.
func anthropicEnv(t *testing.T, model string, fn http.HandlerFunc) (*env, *scripted) {
	t.Helper()
	e := newEnv(t, nil)
	up := newScripted(t, fn)
	conn, err := e.st.CreateConnection(context.Background(), &store.ModelConnection{Name: "claude", ProviderType: "anthropic",
		BaseURL: up.srv.URL, APIKey: "sk-ant-secretsecret", ModelName: model})
	if err != nil {
		t.Fatal(err)
	}
	e.proj.ModelConnectionID = conn.ID
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	return e, up
}

func TestMessagesNativeThroughTheStack(t *testing.T) {
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
	e.proj.SystemPrompt = "Project prompt."
	e.proj.ToolDeny = []string{"shell"}
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	resp, raw := e.messages(sdkRequest, map[string]string{"x-api-key": e.key, "anthropic-version": "2023-06-01",
		"anthropic-beta": "interleaved-thinking-2025-05-14", "x-claude-code-session-id": "cc-1"})
	if resp.StatusCode != 200 || !strings.HasPrefix(string(raw), anthropicAnswer[:len(anthropicAnswer)-1]) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if got := up.lastHeader().Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q", got)
	}
	b := up.lastBody(t)
	if _, ok := b["ragmux"]; ok || len(b["tools"].([]any)) != 1 {
		t.Errorf("upstream body = %v", b)
	}
	row := e.lastLog()
	if row.Endpoint != "messages" || row.SessionID != "cc-1" || row.ToolsDenied != 1 || strings.Join(row.ToolNames, ",") != "lookup" ||
		row.PromptTokens != 13 || row.CompletionTokens != 4 || row.ModelName != "claude-opus-5-5" {
		t.Errorf("row = %+v", row)
	}
}

func TestMessagesNativeRAGOnAToolTurn(t *testing.T) {
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
	e.proj.ContextPlacement = store.ContextPlacementBeforeLastUser
	// The project answers through the Anthropic connection while its store
	// embeds through the mock one.
	e.attachRAG()
	embedded := e.up.embeds.Load() // the fixture document's ingestion

	toolTurn := `{"model":"claude-opus-5-5","max_tokens":9,"messages":[{"role":"user","content":"tell me about apples"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig-1"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found"},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}],` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
	resp, raw := e.messages(toolTurn, map[string]string{"x-api-key": e.key})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("x-ragmux-rag-skip"); got != obs.RAGSkipToolTurn {
		t.Errorf("x-ragmux-rag-skip = %q", got)
	}
	if got := e.up.embeds.Load(); got != embedded {
		t.Errorf("a tool turn embedded a query (%d calls after the fixture's %d)", got, embedded)
	}
	msgs := up.lastBody(t)["messages"].([]any)
	if asst := msgs[1].(map[string]any)["content"].([]any); len(asst) != 1 || asst[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("thinking was sent back on a RAG project: %v", asst)
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Warning"), "\n"), "thinking blocks of earlier turns were dropped") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}

	// A turn that asks something is retrieved for: the context leads the last
	// user message, behind the client's cached prefix.
	ask := `{"model":"claude-opus-5-5","max_tokens":9,"system":"Be brief.","messages":[{"role":"user","content":"tell me about apples"}]}`
	resp, raw = e.messages(ask, map[string]string{"x-api-key": e.key})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	b := up.lastBody(t)
	last := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(last) != 2 || !strings.Contains(last[0].(map[string]any)["text"].(string), "apples are red") || last[1].(map[string]any)["text"] != "tell me about apples" {
		t.Errorf("last user turn = %v", last)
	}
	if b["system"] != "Be brief." {
		t.Errorf("system = %v", b["system"])
	}

	// With the system placement the context is the end of the system prompt.
	e.proj.ContextPlacement = store.ContextPlacementSystem
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	if resp, raw = e.messages(ask, map[string]string{"x-api-key": e.key}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	sys := up.lastBody(t)["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["text"] != "Be brief." || !strings.Contains(sys[1].(map[string]any)["text"].(string), "apples are red") {
		t.Errorf("system = %v", sys)
	}
}

// T4 through the gateway on the chat endpoint (ADR-031): a RAG project's
// request to an Anthropic model that binds thinking to the prompt prefix does
// not carry the earlier turns' thinking blocks upstream.
func TestChatOnARAGProjectDropsReplayedThinkingUpstream(t *testing.T) {
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	e.attachRAG()
	body := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "assistant", "content": "a", "ragmux_thinking": []any{map[string]any{"type": "thinking", "thinking": "hm", "signature": "sig-1"}}},
		map[string]any{"role": "user", "content": "tell me about apples"}}}
	resp, out := e.chat(body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	for _, blk := range up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any) {
		if blk.(map[string]any)["type"] == "thinking" {
			t.Errorf("the thinking block went upstream: %v", blk)
		}
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Warning"), "\n"), "thinking blocks of earlier turns were dropped") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}

	// Retrieval switched off by the project: the prompt is never changed, nothing is dropped.
	e.proj.RetrievalPolicy = store.RetrievalPolicyOff
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	if resp, out = e.chat(body); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if blocks := up.lastBody(t)["messages"].([]any)[1].(map[string]any)["content"].([]any); len(blocks) != 2 {
		t.Errorf("thinking was dropped on a project without retrieval: %v", blocks)
	}
}
