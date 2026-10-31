package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Models of the capability table: claude-sonnet-4-5 has a native schema and a
// forced tool_choice, claude-3-7-sonnet has a forced tool_choice only (and
// budget thinking), claude-opus-5-5 has a native schema and no forced
// tool_choice.
const (
	modelNative  = "claude-sonnet-4-5"
	modelToolish = "claude-3-7-sonnet-latest"
	modelNoForce = "claude-opus-5-5"
)

const objectSchema = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`

func schemaFormat(schema string) string {
	return `"response_format":{"type":"json_schema","json_schema":{"name":"x","strict":true,"schema":` + schema + `}}`
}

func translated(t *testing.T, model, reqJSON string) (anthropicRequest, map[string]json.RawMessage) {
	t.Helper()
	a, err := translateAnthropic(context.Background(), Config{Model: model}, parseReq(t, reqJSON))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return a, wire
}

func TestFormatNativeSchemaGoesToOutputConfig(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	a, wire := translated(t, modelNative, js)
	if a.OutputConfig == nil || a.OutputConfig.Format == nil || a.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("output_config = %+v", a.OutputConfig)
	}
	if string(a.OutputConfig.Format.Schema) != objectSchema {
		t.Errorf("schema = %s", a.OutputConfig.Format.Schema)
	}
	// Neither OpenAI's name/strict nor a tool come along.
	if got := string(wire["output_config"]); strings.Contains(got, "strict") || strings.Contains(got, `"json_schema":`) {
		t.Errorf("output_config = %s", got)
	}
	if len(a.Tools) != 0 || a.ToolChoice != nil || wire["response_format"] != nil {
		t.Errorf("tools=%v tool_choice=%s response_format=%s", a.Tools, a.ToolChoice, wire["response_format"])
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNative}, js); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

// The native schema leaves everything else alone: thinking, effort and the
// client's tools and tool_choice all go next to it.
func TestFormatNativeKeepsThinkingToolsAndToolChoice(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `,` + toolsJSON +
		`,"tool_choice":"required","reasoning_effort":"low"}`
	a, _ := translated(t, "claude-opus-4-7", js)
	if a.Thinking == nil || a.OutputConfig == nil || a.OutputConfig.Effort != "low" || a.OutputConfig.Format == nil {
		t.Errorf("thinking=%+v output_config=%+v", a.Thinking, a.OutputConfig)
	}
	if len(a.Tools) != 1 || a.Tools[0].Name != "get_weather" || string(a.ToolChoice) != `{"type":"any"}` {
		t.Errorf("tools=%v tool_choice=%s", a.Tools, a.ToolChoice)
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-7"}, js); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

func TestFormatToolPathForcesRagmuxResponse(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	a, _ := translated(t, modelToolish, js)
	if len(a.Tools) != 1 || a.Tools[0].Name != "ragmux_response" || string(a.Tools[0].InputSchema) != objectSchema {
		t.Fatalf("tools = %+v", a.Tools)
	}
	if string(a.ToolChoice) != `{"type":"tool","name":"ragmux_response"}` {
		t.Errorf("tool_choice = %s", a.ToolChoice)
	}
	if a.OutputConfig != nil {
		t.Errorf("output_config = %+v", a.OutputConfig)
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelToolish}, js); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

func TestFormatToolPathWrapsNonObjectRoots(t *testing.T) {
	for _, schema := range []string{`{"type":"array","items":{"type":"string"}}`, `{"type":"string"}`,
		`{"anyOf":[{"type":"string"},{"type":"null"}]}`, `{"type":["object","null"]}`} {
		js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(schema) + `}`
		a, _ := translated(t, modelToolish, js)
		var got struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(a.Tools[0].InputSchema, &got); err != nil {
			t.Fatal(err)
		}
		if got.Type != "object" || string(got.Properties["value"]) != schema || len(got.Required) != 1 {
			t.Errorf("%s wrapped as %s", schema, a.Tools[0].InputSchema)
		}
	}
	// A root without "type" but with properties is an object and is not wrapped.
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(`{"properties":{"a":{"type":"string"}}}`) + `}`
	if a, _ := translated(t, modelToolish, js); strings.Contains(string(a.Tools[0].InputSchema), `"value"`) {
		t.Errorf("object root wrapped: %s", a.Tools[0].InputSchema)
	}
}

func TestFormatSchemaMissingIs400(t *testing.T) {
	for _, rf := range []string{
		`{"type":"json_schema"}`,
		`{"type":"json_schema","json_schema":{"name":"x"}}`,
		`{"type":"json_schema","json_schema":{"name":"x","schema":"nope"}}`,
		`{"type":"json_schema","json_schema":{"name":"x","schema":null}}`,
	} {
		for _, model := range []string{modelNative, modelToolish} {
			_, err := translateAnthropic(context.Background(), Config{Model: model},
				parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"response_format":`+rf+`}`))
			if pe, ok := err.(*Error); !ok || pe.Status != http.StatusBadRequest {
				t.Errorf("%s on %s: err = %v", rf, model, err)
			}
		}
	}
}

func TestFormatJSONObject(t *testing.T) {
	js := `{"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`

	// A model that can be forced answers through the tool, with a free object.
	a, _ := translated(t, modelNative, js)
	if len(a.Tools) != 1 || a.Tools[0].Name != "ragmux_response" || string(a.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("tools = %+v", a.Tools)
	}
	if string(a.System) != `"Be brief."` {
		t.Errorf("system = %s", a.System)
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNative}, js); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}

	// One that cannot gets a sentence at the end of the system prompt, and says so.
	a, _ = translated(t, modelNoForce, js)
	if len(a.Tools) != 0 || a.ToolChoice != nil || a.OutputConfig != nil {
		t.Errorf("tools=%v tool_choice=%s output_config=%+v", a.Tools, a.ToolChoice, a.OutputConfig)
	}
	if string(a.System) != `"Be brief.\n\n`+anthropicJSONInstruction+`"` {
		t.Errorf("system = %s", a.System)
	}
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNoForce}, js)
	if len(w) != 1 || w[0] != "response_format json_object is approximated by an instruction on this model" {
		t.Errorf("warnings = %v", w)
	}

	// Without a system message the sentence is the system prompt.
	a, _ = translated(t, modelNoForce, `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	if string(a.System) != `"`+anthropicJSONInstruction+`"` {
		t.Errorf("system = %s", a.System)
	}

	// A cache_control on the system prompt stays on its block; the sentence follows.
	cached := `{"messages":[{"role":"system","content":[{"type":"text","text":"Rules","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	a, _ = translated(t, modelNoForce, cached)
	var blocks []anthropicContent
	if err := json.Unmarshal(a.System, &blocks); err != nil || len(blocks) != 2 || len(blocks[0].CacheControl) == 0 ||
		blocks[1].Text != anthropicJSONInstruction || len(blocks[1].CacheControl) != 0 {
		t.Errorf("system = %s (%v)", a.System, err)
	}
}

// Unknown models take the newest generation's restrictive defaults: no
// request the gateway builds for them can 400 on a forced tool.
func TestFormatUnknownModelNeverForcesATool(t *testing.T) {
	for _, rf := range []string{schemaFormat(objectSchema), `"response_format":{"type":"json_object"}`} {
		a, _ := translated(t, "claude-future-9", `{"messages":[{"role":"user","content":"hi"}],`+rf+`}`)
		if len(a.Tools) != 0 || a.ToolChoice != nil {
			t.Errorf("%s: tools=%v tool_choice=%s", rf, a.Tools, a.ToolChoice)
		}
	}
}

func TestFormatToolPathWithUserTools(t *testing.T) {
	const named = `{"type":"function","function":{"name":"get_weather"}}`
	cases := []struct {
		choice    string // the client's tool_choice, "" when absent
		want      string // the Messages tool_choice
		userTools bool   // whether get_weather is still sent
	}{
		{``, `{"type":"any","disable_parallel_tool_use":true}`, true},
		{`"auto"`, `{"type":"any","disable_parallel_tool_use":true}`, true},
		{`"required"`, `{"type":"any","disable_parallel_tool_use":true}`, true},
		{named, `{"name":"get_weather","type":"tool"}`, true},
		{`"none"`, `{"type":"tool","name":"ragmux_response"}`, false},
	}
	for _, c := range cases {
		js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `,` + toolsJSON
		if c.choice != "" {
			js += `,"tool_choice":` + c.choice
		}
		a, _ := translated(t, modelToolish, js+`}`)
		var names []string
		for _, tl := range a.Tools {
			names = append(names, tl.Name)
		}
		wantNames := "ragmux_response"
		if c.userTools {
			wantNames = "get_weather,ragmux_response"
		}
		if strings.Join(names, ",") != wantNames || string(a.ToolChoice) != c.want {
			t.Errorf("tool_choice %q: tools=%v tool_choice=%s", c.choice, names, a.ToolChoice)
		}
	}
}

func TestFormatUserToolNamedRagmuxResponseIs400(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) +
		`,"tools":[{"type":"function","function":{"name":"ragmux_response","parameters":{"type":"object"}}}]}`
	_, err := translateAnthropic(context.Background(), Config{Model: modelToolish}, parseReq(t, js))
	if pe, ok := err.(*Error); !ok || pe.Status != http.StatusBadRequest {
		t.Errorf("err = %v", err)
	}
	// The name is only reserved where the gateway uses it.
	if _, err := translateAnthropic(context.Background(), Config{Model: modelNative}, parseReq(t, js)); err != nil {
		t.Errorf("native path: %v", err)
	}
}

func TestFormatToolPathDropsThinking(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `,"temperature":0.2,"reasoning_effort":"high"}`
	a, _ := translated(t, modelToolish, js)
	if a.Thinking != nil {
		t.Errorf("thinking = %+v next to a forced tool", a.Thinking)
	}
	if a.MaxTokens != defaultMaxTokens || a.Temperature == nil {
		t.Errorf("max_tokens=%d temperature=%v, want the request as it is without thinking", a.MaxTokens, a.Temperature)
	}
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelToolish}, js)
	if len(w) != 1 || !strings.Contains(w[0], "thinking disabled") || !strings.Contains(w[0], "response_format") {
		t.Errorf("warnings = %v", w)
	}
	// Without thinking requested there is nothing to say.
	plain := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelToolish}, plain); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

// ADR-031: a model that cannot force a tool gets auto, with a Warning.
func TestToolChoiceDowngradedWithoutForcedToolChoice(t *testing.T) {
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"get_weather"}}`} {
		js := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":` + choice + `}`
		a, _ := translated(t, modelNoForce, js)
		if string(a.ToolChoice) != `{"type":"auto"}` || len(a.Tools) != 1 {
			t.Errorf("%s: tool_choice=%s tools=%v", choice, a.ToolChoice, a.Tools)
		}
		w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNoForce}, js)
		if len(w) != 1 || !strings.Contains(w[0], "tool_choice") || !strings.Contains(w[0], "auto") {
			t.Errorf("%s: warnings = %v", choice, w)
		}
		// The same request on a model that can force keeps the choice.
		b, _ := translated(t, modelNative, js)
		if string(b.ToolChoice) == `{"type":"auto"}` {
			t.Errorf("%s: downgraded on a model with forced tool choice", choice)
		}
		if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNative}, js); len(w) != 0 {
			t.Errorf("%s on %s: warnings = %v", choice, modelNative, w)
		}
	}
	for _, choice := range []string{`"auto"`, `"none"`} {
		js := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":` + choice + `}`
		if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNoForce}, js); len(w) != 0 {
			t.Errorf("%s: warnings = %v", choice, w)
		}
	}
	// No tools, nothing to force.
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: modelNoForce}, `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`); len(w) != 0 {
		t.Errorf("no tools: warnings = %v", w)
	}
}

// ---------------------------------------------------------------- responses

func messagesServer(t *testing.T, body string, sent *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if sent != nil {
			*sent = b
		}
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func toolResponse(blocks ...string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","content":[` + strings.Join(blocks, ",") +
		`],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`
}

func ragmuxUse(input string) string {
	return `{"type":"tool_use","id":"toolu_1","name":"ragmux_response","input":` + input + `}`
}

func chatVia(t *testing.T, model, upstream, reqJSON string) *ChatResponse {
	t.Helper()
	srv := messagesServer(t, upstream, nil)
	p, err := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), parseReq(t, reqJSON))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFormatToolAnswerBecomesContent(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	resp := chatVia(t, modelToolish, toolResponse(ragmuxUse(`{"name":"Ada"}`)), js)
	ch := resp.Choices[0]
	if ch.Message.Content == nil || *ch.Message.Content != `{"name":"Ada"}` || !json.Valid([]byte(*ch.Message.Content)) {
		t.Errorf("content = %v", ch.Message.Content)
	}
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %v", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 0 {
		t.Errorf("tool_calls = %s", ch.Message.ToolCalls)
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "ragmux_response") {
		t.Errorf("response mentions the synthetic tool: %s", raw)
	}
}

func TestFormatToolAnswerUnwrapsValue(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(`{"type":"array","items":{"type":"integer"}}`) + `}`
	resp := chatVia(t, modelToolish, toolResponse(ragmuxUse(`{"value": [1, 2, 3]}`)), js)
	if got := *resp.Choices[0].Message.Content; got != `[1, 2, 3]` {
		t.Errorf("content = %q", got)
	}
	// A string root stays a JSON string, quotes included.
	js = `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(`{"type":"string"}`) + `}`
	resp = chatVia(t, modelToolish, toolResponse(ragmuxUse(`{"value":"ok"}`)), js)
	if got := *resp.Choices[0].Message.Content; got != `"ok"` {
		t.Errorf("content = %q", got)
	}
}

func TestFormatJSONObjectToolAnswer(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	resp := chatVia(t, modelNative, toolResponse(ragmuxUse(`{"a":1}`)), js)
	if got := *resp.Choices[0].Message.Content; got != `{"a":1}` || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("content=%q finish=%v", got, resp.Choices[0].FinishReason)
	}
}

// The model may call one of the client's tools instead; that turn is an
// ordinary tool_calls turn.
func TestFormatClientToolCallStaysACall(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `,` + toolsJSON + `}`
	use := `{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"Ankara"}}`
	resp := chatVia(t, modelToolish, toolResponse(use), js)
	ch := resp.Choices[0]
	if *ch.FinishReason != "tool_calls" || !strings.Contains(string(ch.Message.ToolCalls), "get_weather") {
		t.Errorf("finish=%v tool_calls=%s", *ch.FinishReason, ch.Message.ToolCalls)
	}
}

// A request without a JSON response_format reads tool calls as before, even
// when the model names a tool ragmux_response of its own.
func TestFormatAbsentLeavesToolCallsAlone(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `}`
	resp := chatVia(t, modelToolish, toolResponse(ragmuxUse(`{}`)), js)
	if !strings.Contains(string(resp.Choices[0].Message.ToolCalls), "ragmux_response") || *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("choice = %+v", resp.Choices[0])
	}
}

func TestFormatNativeAnswerIsPlainText(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	text := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"{\"name\":\"Ada\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`
	resp := chatVia(t, modelNative, text, js)
	if got := *resp.Choices[0].Message.Content; got != `{"name":"Ada"}` || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("content=%q finish=%v", got, resp.Choices[0].FinishReason)
	}
}

func TestFormatBodyReachesTheWire(t *testing.T) {
	var sent []byte
	srv := messagesServer(t, toolResponse(ragmuxUse(`{"name":"Ada"}`)), &sent)
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: modelToolish})
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	if _, err := p.Chat(context.Background(), parseReq(t, js)); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tools      []struct{ Name string }     `json:"tools"`
		ToolChoice struct{ Type, Name string } `json:"tool_choice"`
	}
	if err := json.Unmarshal(sent, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Name != "ragmux_response" || wire.ToolChoice.Type != "tool" || wire.ToolChoice.Name != "ragmux_response" {
		t.Errorf("wire = %s", sent)
	}
	if strings.Contains(string(sent), "response_format") {
		t.Errorf("response_format on the wire: %s", sent)
	}
}

// ------------------------------------------------------------------ streams

// sseOf renders Messages API events. fragments are the partial_json pieces of
// one ragmux_response call at content index idx.
func sseOf(idx int, fragments []string, stop string, extra ...string) string {
	var b strings.Builder
	ev := func(data string) { b.WriteString("event: x\ndata: " + data + "\n\n") }
	ev(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`)
	ev(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"toolu_1","name":"ragmux_response","input":{}}}`, idx))
	for _, f := range fragments {
		fb, _ := json.Marshal(f)
		ev(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, idx, fb))
	}
	ev(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
	for _, e := range extra {
		ev(e)
	}
	ev(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, stop))
	ev(`{"type":"message_stop"}`)
	return b.String()
}

type streamed struct {
	content   string
	toolCalls []string
	finish    string
	deltas    []string
}

func streamVia(t *testing.T, model, upstream, reqJSON string) streamed {
	t.Helper()
	srv := messagesServer(t, upstream, nil)
	p, err := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan StreamChunk, 256)
	req := parseReq(t, reqJSON)
	req.Stream = true
	err = p.ChatStream(context.Background(), req, out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	var s streamed
	for c := range out {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				s.content += *ch.Delta.Content
				s.deltas = append(s.deltas, *ch.Delta.Content)
			}
			if len(ch.Delta.ToolCalls) > 0 {
				s.toolCalls = append(s.toolCalls, string(ch.Delta.ToolCalls))
			}
			if ch.FinishReason != nil {
				s.finish = *ch.FinishReason
			}
		}
	}
	return s
}

func TestFormatStreamToolDeltasBecomeContent(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	up := sseOf(0, []string{`{"na`, `me":`, `"Ada`, `"}`}, "tool_use")
	s := streamVia(t, modelToolish, up, js)
	if s.content != `{"name":"Ada"}` || !json.Valid([]byte(s.content)) {
		t.Errorf("content = %q (deltas %q)", s.content, s.deltas)
	}
	if s.finish != "stop" || len(s.toolCalls) != 0 {
		t.Errorf("finish=%q tool_calls=%v", s.finish, s.toolCalls)
	}
}

func TestFormatStreamUnwrapsValueAtEveryCut(t *testing.T) {
	const whole = `{"value":{"a":[1,{"b":"}"}],"c":"x"}}`
	const want = `{"a":[1,{"b":"}"}],"c":"x"}`
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(`{"type":["object","null"]}`) + `}`
	// Cut the stream in two at every byte: the opener and the closing brace
	// must come off wherever the cut falls.
	for i := 0; i <= len(whole); i++ {
		s := streamVia(t, modelToolish, sseOf(0, []string{whole[:i], whole[i:]}, "tool_use"), js)
		if s.content != want || s.finish != "stop" {
			t.Fatalf("cut at %d: content=%q finish=%q", i, s.content, s.finish)
		}
	}
	// One byte at a time.
	var frags []string
	for _, r := range whole {
		frags = append(frags, string(r))
	}
	if s := streamVia(t, modelToolish, sseOf(0, frags, "tool_use"), js); s.content != want {
		t.Errorf("byte by byte: %q", s.content)
	}
}

func TestFormatStreamUnwrapDropsTrailingWhitespaceAndKeepsInnerBraces(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{ "value" : [1, 2] }`, `[1, 2]`},
		{"{\"value\":\"a}\"}\n", `"a}"`},
		{`{"value":{"x":{}}}`, `{"x":{}}`},
		{`{"value":7}`, `7`},
	} {
		s := newFormatStream(true)
		var got string
		for i := 0; i < len(c.in); i++ {
			got += s.Feed(c.in[i : i+1])
		}
		got += s.Flush()
		if strings.TrimSpace(got) != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
	// An opener that is not the wrapper is not lost.
	s := newFormatStream(true)
	got := s.Feed(`{"other":1}`) + s.Flush()
	if got != `{"other":1}` {
		t.Errorf("unrecognised input -> %q", got)
	}
}

func TestFormatStreamJSONObject(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	s := streamVia(t, modelNative, sseOf(0, []string{`{"a":`, `1}`}, "tool_use"), js)
	if s.content != `{"a":1}` || s.finish != "stop" {
		t.Errorf("content=%q finish=%q", s.content, s.finish)
	}
}

func TestFormatStreamClientToolCallStaysACall(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `,` + toolsJSON + `}`
	var b strings.Builder
	ev := func(data string) { b.WriteString("data: " + data + "\n\n") }
	ev(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`)
	ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{}}}`)
	ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Ankara\"}"}}`)
	ev(`{"type":"content_block_stop","index":0}`)
	ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
	ev(`{"type":"message_stop"}`)
	s := streamVia(t, modelToolish, b.String(), js)
	if s.finish != "tool_calls" || len(s.toolCalls) != 2 || s.content != "" {
		t.Errorf("finish=%q tool_calls=%v content=%q", s.finish, s.toolCalls, s.content)
	}
}

// A length cut-off is not a clean stop: it is reported as the upstream said.
func TestFormatStreamMaxTokensStaysLength(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	s := streamVia(t, modelToolish, sseOf(0, []string{`{"na`}, "max_tokens"), js)
	if s.finish != "length" {
		t.Errorf("finish = %q", s.finish)
	}
}

// Native and plain requests stream text as before.
func TestFormatStreamNativeIsText(t *testing.T) {
	var b strings.Builder
	ev := func(data string) { b.WriteString("data: " + data + "\n\n") }
	ev(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`)
	ev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	ev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"name\":"}}`)
	ev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\"Ada\"}"}}`)
	ev(`{"type":"content_block_stop","index":0}`)
	ev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
	ev(`{"type":"message_stop"}`)
	js := `{"messages":[{"role":"user","content":"hi"}],` + schemaFormat(objectSchema) + `}`
	s := streamVia(t, modelNative, b.String(), js)
	if s.content != `{"name":"Ada"}` || s.finish != "stop" {
		t.Errorf("content=%q finish=%q", s.content, s.finish)
	}
}

// ADR-029 sends a json_schema through a forced tool on a model without a
// native schema, so such a model must be able to take a forced tool. The path
// in planAnthropicFormat for the other combination is unreachable on purpose;
// a table row that makes it reachable fails here.
func TestAnthropicModelsCanEnforceJSONSchema(t *testing.T) {
	for _, r := range modelRules {
		if r.providerType == "anthropic" && !r.flags.nativeJSONSchema && !r.flags.forcedToolChoice {
			t.Errorf("row %q has neither native_json_schema nor forced_tool_choice", r.pattern)
		}
	}
	// The rows are not the only source: the defaults for a model no row
	// matches (and for none given) must hold too.
	for _, model := range []string{"", "claude-future-9"} {
		if c := CapabilitiesFor("anthropic", model); !c.NativeJSONSchema && !c.ForcedToolChoice {
			t.Errorf("anthropic %q: neither native_json_schema nor forced_tool_choice", model)
		}
	}
}

// The call may have no input to stream: Anthropic sends no fragment, or one
// empty fragment, for `{}`. Chat answers `{}` and so does the stream.
func TestFormatStreamEmptyToolInputIsEmptyObject(t *testing.T) {
	cases := map[string][]string{"no fragment": nil, "empty fragment": {""}}
	rfs := map[string]string{
		"json_object":    `"response_format":{"type":"json_object"}`,
		"object schema":  schemaFormat(objectSchema),
		"wrapped schema": schemaFormat(`{"type":"array","items":{"type":"string"}}`),
	}
	for name, frags := range cases {
		for rname, rf := range rfs {
			model := modelToolish
			if rname == "json_object" {
				model = modelNative
			}
			js := `{"messages":[{"role":"user","content":"hi"}],` + rf + `}`
			s := streamVia(t, model, sseOf(0, frags, "tool_use"), js)
			if s.content != "{}" || s.finish != "stop" {
				t.Errorf("%s / %s: content=%q finish=%q", name, rname, s.content, s.finish)
			}
		}
	}
	// Content that did stream is not followed by a made-up object.
	s := streamVia(t, modelNative, sseOf(0, []string{`{"a":1}`}, "tool_use"),
		`{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	if s.content != `{"a":1}` {
		t.Errorf("content = %q", s.content)
	}
}

// reasoning_effort none on a model that switches thinking off with
// "disabled" is applied as sent, next to the forced tool: nothing to warn of.
func TestFormatToolPathKeepsDisabledThinkingSilently(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"},"reasoning_effort":"none"}`
	a, _ := translated(t, "claude-opus-4-6", js)
	if len(a.Tools) != 1 || a.Tools[0].Name != "ragmux_response" {
		t.Fatalf("tools = %+v", a.Tools)
	}
	if a.Thinking == nil || a.Thinking.Type != "disabled" {
		t.Errorf("thinking = %+v, want the disabled marker kept", a.Thinking)
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-6"}, js); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
	// Thinking that was on is still dropped, with its Warning.
	on := strings.Replace(js, `"none"`, `"high"`, 1)
	a, _ = translated(t, "claude-opus-4-6", on)
	if a.Thinking != nil {
		t.Errorf("thinking = %+v next to a forced tool", a.Thinking)
	}
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-6"}, on); !hasWarning(w, "thinking disabled") {
		t.Errorf("warnings = %v", w)
	}
}
