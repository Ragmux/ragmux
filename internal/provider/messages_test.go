package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nmBody parses a request fixture.
func nmBody(t *testing.T, raw string) *MessagesBody {
	t.Helper()
	b, err := ParseMessagesBody([]byte(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return b
}

// nmAdapter is an Anthropic adapter on a model, pointing at base.
func nmAdapter(model, base string) *anthropic {
	return newAnthropic(Config{ProviderType: "anthropic", BaseURL: base, APIKey: "sk-ant-secretsecret", Model: model, Timeout: 5 * time.Second})
}

// nmPrepare prepares a body and decodes the result.
func nmPrepare(t *testing.T, model string, in MessagesInput) (map[string]any, MessagesPrepared) {
	t.Helper()
	prep, err := nmAdapter(model, "http://unused").PrepareMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(prep.Body, &out); err != nil {
		t.Fatalf("prepared body is not JSON: %v\n%s", err, prep.Body)
	}
	return out, prep
}

func TestMessagesBodyKeepsOrderAndBytes(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"a  <b> & \u00e9"}],  "zz":{"k" : 1},"stream":true}`
	b := nmBody(t, raw)
	if got := strings.Join(b.Keys(), ","); got != "model,messages,zz,stream" {
		t.Fatalf("keys = %s", got)
	}
	v, _ := b.Get("zz")
	if string(v) != `{"k" : 1}` {
		t.Fatalf("value was rewritten: %s", v)
	}
	b.Set("model", json.RawMessage(`"n"`))
	b.Delete("zz")
	b.Set("extra", json.RawMessage(`1`))
	want := `{"model":"n","messages":[{"role":"user","content":"a  <b> & \u00e9"}],"stream":true,"extra":1}`
	if got := string(b.Bytes()); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	for _, bad := range []string{`[]`, `{"a":1} x`, `{"a":`, ``} {
		if _, err := ParseMessagesBody([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestPrepareMessagesTouchesOnlyWhatADR028Lists(t *testing.T) {
	raw := `{"model":"client-name","max_tokens":64,"metadata":{"user_id":"u1"},"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi <there>"}]}],"ragmux":{"retrieval":"off"},"stream":false}`
	out, prep := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, raw), Version: "2023-06-01", Beta: "a-beta,b-beta"})
	if out["model"] != "claude-sonnet-4-5" {
		t.Errorf("model = %v", out["model"])
	}
	if _, ok := out["ragmux"]; ok {
		t.Error("ragmux reached the upstream body")
	}
	if _, ok := out["context_management"]; !ok {
		t.Error("context_management was dropped")
	}
	sys := out["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["text"] != "x-anthropic-billing-header: cc_version=1" {
		t.Errorf("system was changed: %v", sys)
	}
	if prep.Version != "2023-06-01" || prep.Beta != "a-beta,b-beta" || len(prep.Warnings) != 0 {
		t.Errorf("prepared = %+v", prep)
	}
	// The messages are the client's bytes, <, > and all.
	if !bytes.Contains(prep.Body, []byte(`"messages":[{"role":"user","content":[{"type":"text","text":"hi <there>"}]}]`)) {
		t.Errorf("messages were rewritten: %s", prep.Body)
	}
}

func TestPrepareMessagesDefaultsTheVersion(t *testing.T) {
	_, prep := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"x"}]}`)})
	if prep.Version != "2023-06-01" {
		t.Errorf("version = %q", prep.Version)
	}
}

func TestPrepareMessagesAppendsToTheEndOfSystem(t *testing.T) {
	cases := []struct {
		name, system string
		want         []string // texts of the system blocks, or one for a string
	}{
		{"absent", ``, []string{"PROJECT\n\nCONTEXT"}},
		{"string", `"client"`, []string{"client", "PROJECT", "CONTEXT"}},
		{"blocks", `[{"type":"text","text":"b1"},{"type":"text","text":"b2"}]`, []string{"b1", "b2", "PROJECT", "CONTEXT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"messages":[{"role":"user","content":"x"}]`
			if tc.system != "" {
				raw += `,"system":` + tc.system
			}
			raw += `}`
			out, _ := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, raw), SystemAppend: []string{"PROJECT", "CONTEXT"}})
			var got []string
			switch s := out["system"].(type) {
			case string:
				got = []string{s}
			case []any:
				for _, b := range s {
					got = append(got, b.(map[string]any)["text"].(string))
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("system = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrepareMessagesLeadsTheLastUserTurnAfterToolResults(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"reminder"}]}]}`
	out, _ := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, raw), BeforeLastUser: "CONTEXT <x>"})
	msgs := out["messages"].([]any)
	content := msgs[2].(map[string]any)["content"].([]any)
	var types []string
	for _, b := range content {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "tool_result,text,text" {
		t.Fatalf("block order = %v", types)
	}
	if content[1].(map[string]any)["text"] != "CONTEXT <x>" || content[2].(map[string]any)["text"] != "reminder" {
		t.Errorf("content = %v", content)
	}
	// The earlier turns are untouched.
	if msgs[0].(map[string]any)["content"] != "q" {
		t.Errorf("earlier turn changed: %v", msgs[0])
	}

	// A string content becomes the block followed by the original text.
	out, _ = nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"orig"}]}`), BeforeLastUser: "CTX"})
	c := out["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(c) != 2 || c[0].(map[string]any)["text"] != "CTX" || c[1].(map[string]any)["text"] != "orig" {
		t.Errorf("content = %v", c)
	}
}

// thinking round fixture: a tool round whose assistant turn carries (or not) a
// signed thinking block.
func nmToolRound(withThinking bool) string {
	think := ``
	if withThinking {
		think = `{"type":"thinking","thinking":"hmm","signature":"sig-1"},{"type":"redacted_thinking","data":"blob"},`
	}
	return `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[` + think +
		`{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`
}

func nmBlockTypes(t *testing.T, out map[string]any, msg int) []string {
	t.Helper()
	var types []string
	for _, b := range out["messages"].([]any)[msg].(map[string]any)["content"].([]any) {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	return types
}

func TestPrepareMessagesDropsReplayedThinkingUnderRAG(t *testing.T) {
	body := nmToolRound(true) + `}`
	// A model that binds thinking to the prefix, on a RAG project: dropped, with a Warning.
	out, prep := nmPrepare(t, "claude-opus-5-5", MessagesInput{Body: nmBody(t, body), RAGActive: true})
	if got := nmBlockTypes(t, out, 1); strings.Join(got, ",") != "tool_use" {
		t.Errorf("assistant blocks = %v", got)
	}
	if len(prep.Warnings) != 1 || prep.Warnings[0] != warnThinkingPrefixDropped {
		t.Errorf("warnings = %q", prep.Warnings)
	}
	// Not RAG: replayed as the client sent it.
	out, prep = nmPrepare(t, "claude-opus-5-5", MessagesInput{Body: nmBody(t, body)})
	if got := nmBlockTypes(t, out, 1); strings.Join(got, ",") != "thinking,redacted_thinking,tool_use" || len(prep.Warnings) != 0 {
		t.Errorf("blocks = %v, warnings = %q", got, prep.Warnings)
	}
	// A legacy model is not bound: kept even on a RAG project.
	out, prep = nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, body), RAGActive: true})
	if got := nmBlockTypes(t, out, 1); len(got) != 3 || len(prep.Warnings) != 0 {
		t.Errorf("legacy model: blocks = %v, warnings = %q", got, prep.Warnings)
	}
	// An assistant turn of nothing but thinking is not a turn.
	only := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"}]},{"role":"user","content":"r"}]}`
	out, _ = nmPrepare(t, "claude-opus-5-5", MessagesInput{Body: nmBody(t, only), RAGActive: true})
	if n := len(out["messages"].([]any)); n != 2 {
		t.Errorf("%d messages left", n)
	}
}

func TestPrepareMessagesForcedToolOnAModelWithoutIt(t *testing.T) {
	for _, tc := range []struct{ choice, want string }{
		{`{"type":"any"}`, `{"type":"auto"}`},
		{`{"type":"tool","name":"f"}`, `{"type":"auto"}`},
		{`{"type":"any","disable_parallel_tool_use":true}`, `{"disable_parallel_tool_use":true,"type":"auto"}`},
	} {
		body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":` + tc.choice + `}`
		out, prep := nmPrepare(t, "claude-opus-5-5", MessagesInput{Body: nmBody(t, body)})
		got, _ := json.Marshal(out["tool_choice"])
		if string(got) != tc.want {
			t.Errorf("%s: tool_choice = %s, want %s", tc.choice, got, tc.want)
		}
		if len(prep.Warnings) != 1 || prep.Warnings[0] != warnForcedToolAuto {
			t.Errorf("%s: warnings = %q", tc.choice, prep.Warnings)
		}
	}
	// A model that takes a forced tool keeps it, and an auto choice is left alone.
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}`
	out, prep := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, body)})
	if got, _ := json.Marshal(out["tool_choice"]); string(got) != `{"type":"any"}` || len(prep.Warnings) != 0 {
		t.Errorf("sonnet 4.5: %s %q", got, prep.Warnings)
	}
	body = `{"messages":[{"role":"user","content":"x"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}}`
	if _, prep = nmPrepare(t, "claude-opus-5-5", MessagesInput{Body: nmBody(t, body)}); len(prep.Warnings) != 0 {
		t.Errorf("auto earned %q", prep.Warnings)
	}
}

func TestPrepareMessagesBudgetThinkingRules(t *testing.T) {
	const enabled = `"thinking":{"type":"enabled","budget_tokens":2048}`
	tool := `"tools":[{"name":"f","input_schema":{"type":"object"}}]`

	// A forced tool_choice next to budget thinking: thinking gives way.
	body := `{"messages":[{"role":"user","content":"x"}],` + tool + `,"tool_choice":{"type":"tool","name":"f"},` + enabled + `}`
	out, prep := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, body)})
	if _, ok := out["thinking"]; ok || len(prep.Warnings) != 1 || prep.Warnings[0] != warnThinkingForcedTool {
		t.Errorf("forced tool: thinking=%v warnings=%q", out["thinking"], prep.Warnings)
	}

	// A tool round whose assistant turn has no thinking block: thinking gives way.
	out, prep = nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, nmToolRound(false)+`,`+tool+`,`+enabled+`}`)})
	if _, ok := out["thinking"]; ok || len(prep.Warnings) != 1 || prep.Warnings[0] != warnThinkingToolRound {
		t.Errorf("tool round: thinking=%v warnings=%q", out["thinking"], prep.Warnings)
	}

	// The same round with its thinking block sent back keeps thinking.
	out, prep = nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, nmToolRound(true)+`,`+tool+`,`+enabled+`}`)})
	if _, ok := out["thinking"]; !ok || len(prep.Warnings) != 0 {
		t.Errorf("signed round: thinking=%v warnings=%q", out["thinking"], prep.Warnings)
	}

	// Under the RAG x prefix rule on a legacy budget model nothing is dropped,
	// so the round keeps thinking; the rule does not touch it.
	// Adaptive thinking never needs the block back.
	out, prep = nmPrepare(t, "claude-opus-4-8", MessagesInput{Body: nmBody(t, nmToolRound(false)+`,`+tool+`,"thinking":{"type":"adaptive"}}`)})
	if _, ok := out["thinking"]; !ok || len(prep.Warnings) != 0 {
		t.Errorf("adaptive: thinking=%v warnings=%q", out["thinking"], prep.Warnings)
	}
}

func TestPrepareMessagesTopPWithThinking(t *testing.T) {
	for _, thinking := range []string{`{"type":"enabled","budget_tokens":2048}`, `{"type":"adaptive"}`} {
		model := "claude-sonnet-4-5"
		if strings.Contains(thinking, "adaptive") {
			model = "claude-opus-4-8"
		}
		body := `{"messages":[{"role":"user","content":"x"}],"top_p":0.9,"thinking":` + thinking + `}`
		out, prep := nmPrepare(t, model, MessagesInput{Body: nmBody(t, body)})
		if _, ok := out["top_p"]; ok || len(prep.Warnings) != 1 || prep.Warnings[0] != warnTopPThinking {
			t.Errorf("%s: top_p=%v warnings=%q", thinking, out["top_p"], prep.Warnings)
		}
		body = `{"messages":[{"role":"user","content":"x"}],"top_p":0.97,"thinking":` + thinking + `}`
		if out, prep = nmPrepare(t, model, MessagesInput{Body: nmBody(t, body)}); out["top_p"] != 0.97 || len(prep.Warnings) != 0 {
			t.Errorf("%s: top_p 0.97 = %v, warnings=%q", thinking, out["top_p"], prep.Warnings)
		}
	}
	// Without thinking the client's top_p stands.
	out, _ := nmPrepare(t, "claude-sonnet-4-5", MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"x"}],"top_p":0.5}`)})
	if out["top_p"] != 0.5 {
		t.Errorf("top_p = %v", out["top_p"])
	}
}

// nmUpstream records what the adapter sent and answers with fn.
type nmUpstream struct {
	srv  *httptest.Server
	hdr  http.Header
	body []byte
	path string
}

func newNMUpstream(t *testing.T, fn http.HandlerFunc) *nmUpstream {
	t.Helper()
	u := &nmUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hdr, u.path = r.Header.Clone(), r.URL.Path
		u.body, _ = io.ReadAll(r.Body)
		fn(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

const nmAnswer = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Hi <3"},` +
	`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}}],"stop_reason":"tool_use","stop_sequence":null,` +
	`"usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"service_tier":"standard"}}`

func TestMessagesCallSendsHeadersAndBodyAsTheyAre(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nmAnswer)
	})
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	prep, err := a.PrepareMessages(MessagesInput{Body: nmBody(t, `{"model":"x","max_tokens":9,"messages":[{"role":"user","content":"hi"}],"ragmux":{}}`),
		Version: "2023-06-01", Beta: "prompt-caching-2024-07-31, extended-cache-ttl-2025-04-11"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := a.MessagesCall(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if up.path != "/v1/messages" {
		t.Errorf("path = %s", up.path)
	}
	if got := up.hdr.Get("anthropic-beta"); got != "prompt-caching-2024-07-31, extended-cache-ttl-2025-04-11" {
		t.Errorf("anthropic-beta = %q", got)
	}
	if got := up.hdr.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := up.hdr.Get("x-api-key"); got != "sk-ant-secretsecret" {
		t.Errorf("x-api-key = %q", got)
	}
	if bytes.Contains(up.body, []byte("ragmux")) || !bytes.Contains(up.body, []byte(`"model":"claude-sonnet-4-5"`)) {
		t.Errorf("upstream body = %s", up.body)
	}
	if string(reply.Body) != nmAnswer {
		t.Errorf("answer was rewritten:\n%s", reply.Body)
	}
	if reply.Usage.PromptTokens != 15 || reply.Usage.CompletionTokens != 4 || reply.Usage.CachedTokens() != 3 {
		t.Errorf("usage = %+v", reply.Usage)
	}
	if len(reply.ToolNames) != 1 || reply.ToolNames[0] != "lookup" || reply.TextChars != 5 || reply.Finish == nil || *reply.Finish != "tool_calls" {
		t.Errorf("summary = %+v", reply.MessagesSummary)
	}

	// No beta header at all when the client sent none.
	prep, _ = a.PrepareMessages(MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"hi"}]}`)})
	if _, err := a.MessagesCall(context.Background(), prep); err != nil {
		t.Fatal(err)
	}
	if _, ok := up.hdr["Anthropic-Beta"]; ok {
		t.Errorf("an anthropic-beta header appeared: %v", up.hdr["Anthropic-Beta"])
	}
	if up.hdr.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("default version = %q", up.hdr.Get("anthropic-version"))
	}
}

func TestMessagesCallAddsTheRagmuxObjectToTheAnswer(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, nmAnswer) })
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	prep, _ := a.PrepareMessages(MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"hi"}]}`), Inline: []byte(`{"sources":[]}`)})
	reply, err := a.MessagesCall(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(reply.Body), nmAnswer[:len(nmAnswer)-1]) || !strings.HasSuffix(string(reply.Body), `,"ragmux":{"sources":[]}}`) {
		t.Errorf("answer = %s", reply.Body)
	}
}

func TestMessagesCallRelaysAClientErrorBodyUnchanged(t *testing.T) {
	const body = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Field required"},"request_id":"req_011"}`
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	})
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	prep, _ := a.PrepareMessages(MessagesInput{Body: nmBody(t, `{"messages":[{"role":"user","content":"hi"}]}`)})
	_, err := a.MessagesCall(context.Background(), prep)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 400 || string(pe.UpstreamBody) != body {
		t.Fatalf("err = %#v", err)
	}
	// A 5xx is the adapter's normalised error: no body is kept for relay.
	up2 := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	})
	a = nmAdapter("claude-sonnet-4-5", up2.srv.URL)
	_, err = a.MessagesCall(context.Background(), prep)
	if !errors.As(err, &pe) || pe.Status != 502 || pe.UpstreamBody != nil {
		t.Fatalf("5xx err = %#v", err)
	}
	// An upstream that echoes the connection's key does not leak it.
	up3 := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key sk-ant-secretsecret"}}`)
	})
	a = nmAdapter("claude-sonnet-4-5", up3.srv.URL)
	_, err = a.MessagesCall(context.Background(), prep)
	if !errors.As(err, &pe) || bytes.Contains(pe.UpstreamBody, []byte("secretsecret")) {
		t.Fatalf("key leaked: %#v", err)
	}
}

// nmStream is a streamed answer with the events Anthropic sends, ping included.
const nmStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func nmRelay(t *testing.T, a *anthropic, in MessagesInput) (string, MessagesSummary, error, bool) {
	t.Helper()
	prep, err := a.PrepareMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.MessagesStream(context.Background(), prep)
	if err != nil {
		return "", MessagesSummary{}, err, false
	}
	defer st.Close()
	var out bytes.Buffer
	begun := false
	sum, err := st.Relay(&out, func() {}, func() { begun = true }, nil)
	return out.String(), sum, err, begun
}

func TestMessagesStreamRelaysEveryByte(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nmStream)
	})
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	out, sum, err, begun := nmRelay(t, a, MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`), Beta: "x-beta"})
	if err != nil || !begun {
		t.Fatalf("err = %v, begun = %v", err, begun)
	}
	if out != nmStream {
		t.Errorf("stream was changed:\n%q\nwant\n%q", out, nmStream)
	}
	if up.hdr.Get("anthropic-beta") != "x-beta" {
		t.Errorf("anthropic-beta = %q", up.hdr.Get("anthropic-beta"))
	}
	if sum.Usage == nil || sum.Usage.PromptTokens != 25 || sum.Usage.CompletionTokens != 15 {
		t.Errorf("usage = %+v", sum.Usage)
	}
	if sum.Model != "claude-sonnet-4-5" || len(sum.ToolNames) != 1 || sum.ToolNames[0] != "lookup" || sum.TextChars != 5 ||
		sum.Finish == nil || *sum.Finish != "tool_calls" {
		t.Errorf("summary = %+v", sum)
	}
	if strings.Count(out, "event: ping") != 2 {
		t.Error("ping events were not kept")
	}
}

func TestMessagesStreamAddsTheRagmuxObjectToMessageStart(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, nmStream) })
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	out, _, err, _ := nmRelay(t, a, MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`), Inline: []byte(`{"sources":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	first, rest, _ := strings.Cut(out, "\n\n")
	if !strings.Contains(first, `"ragmux":{"sources":[]}`) || !strings.HasPrefix(first, "event: message_start\ndata: ") {
		t.Errorf("message_start = %s", first)
	}
	var ev struct {
		Message struct {
			ID     string          `json:"id"`
			Ragmux json.RawMessage `json:"ragmux"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.SplitN(first, "\n", 2)[1], "data: ")), &ev); err != nil || ev.Message.ID != "msg_01" {
		t.Errorf("message_start is not intact: %v %+v", err, ev)
	}
	if !strings.HasSuffix(out, rest) || !strings.Contains(nmStream, rest) {
		t.Error("events after message_start changed")
	}
}

func TestMessagesStreamFailsBeforeTheFirstEvent(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`)
	})
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	_, _, err, begun := nmRelay(t, a, MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 400 || !strings.Contains(string(pe.UpstreamBody), `"nope"`) || begun {
		t.Fatalf("err = %#v begun=%v", err, begun)
	}

	// A 200 that carries no events is the upstream failing, before anything is written.
	up = newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "text/event-stream") })
	a = nmAdapter("claude-sonnet-4-5", up.srv.URL)
	_, _, err, begun = nmRelay(t, a, MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
	if !errors.As(err, &pe) || pe.Status != 502 || begun {
		t.Fatalf("empty stream: err = %#v begun=%v", err, begun)
	}
}

func TestMessagesStreamReportsAnErrorEvent(t *testing.T) {
	const ev = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, nmStream[:strings.Index(nmStream, "event: ping")]+ev)
	})
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	out, sum, err, _ := nmRelay(t, a, MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil || !strings.HasSuffix(out, ev) || sum.FailedType != "overloaded_error" || sum.FailedMessage != "Overloaded" {
		t.Errorf("err=%v sum=%+v out=%q", err, sum, out)
	}
}

type nmFailWriter struct{}

func (nmFailWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestMessagesStreamReportsAClientThatWentAway(t *testing.T) {
	up := newNMUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, nmStream) })
	a := nmAdapter("claude-sonnet-4-5", up.srv.URL)
	prep, _ := a.PrepareMessages(MessagesInput{Body: nmBody(t, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
	st, err := a.MessagesStream(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Relay(nmFailWriter{}, func() {}, func() {}, nil); !errors.Is(err, ErrClientWrite) {
		t.Errorf("err = %v", err)
	}
}

// nmTranslate reads a request into canonical form.
func nmTranslate(t *testing.T, raw string) *MessagesTranslation {
	t.Helper()
	tr, err := MessagesToChat(nmBody(t, raw))
	if err != nil {
		t.Fatalf("MessagesToChat: %v", err)
	}
	return tr
}

func TestMessagesToChatTranslatesAConversation(t *testing.T) {
	tr := nmTranslate(t, `{"model":"claude-x","max_tokens":128,"temperature":0.2,"top_p":0.9,"stop_sequences":["END"],"stream":true,`+
		`"metadata":{"user_id":"user-9"},"system":[{"type":"text","text":"S1"},{"type":"text","text":"S2","cache_control":{"type":"ephemeral"}}],`+
		`"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object","properties":{"q":{"type":"string"}}},"cache_control":{"type":"ephemeral"}}],`+
		`"tool_choice":{"type":"any","disable_parallel_tool_use":true},"thinking":{"type":"enabled","budget_tokens":9000},`+
		`"messages":[`+
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},`+
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig"},{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"found"}],"is_error":true},{"type":"text","text":"<system-reminder>"}]}]}`)
	req := tr.Request
	if req.Model != "claude-x" || !req.Stream || *req.MaxTokens != 128 || *req.Temperature != 0.2 || *req.TopP != 0.9 || req.User != "user-9" {
		t.Errorf("scalars: %+v", req)
	}
	if string(req.Stop) != `["END"]` {
		t.Errorf("stop = %s", req.Stop)
	}
	if e, _ := req.ReasoningEffort(); e != "high" {
		t.Errorf("reasoning_effort = %q", e)
	}
	if string(req.Extra["parallel_tool_calls"]) != "false" || string(req.ToolChoice) != `"required"` {
		t.Errorf("tool_choice = %s extra = %v", req.ToolChoice, req.Extra)
	}
	if !strings.Contains(string(req.Tools), `"name":"lookup"`) || !strings.Contains(string(req.Tools), `"cache_control"`) ||
		!strings.Contains(string(req.Tools), `"parameters":{"type":"object"`) {
		t.Errorf("tools = %s", req.Tools)
	}
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "system,system,user,assistant,tool,user" {
		t.Fatalf("roles = %s", got)
	}
	if !strings.Contains(string(req.Messages[1].Content), `"cache_control"`) || req.Messages[0].Text() != "S1" {
		t.Errorf("system blocks: %s | %s", req.Messages[0].Content, req.Messages[1].Content)
	}
	if !strings.Contains(string(req.Messages[2].Content), "data:image/png;base64,AAAA") {
		t.Errorf("image part = %s", req.Messages[2].Content)
	}
	asst := req.Messages[3]
	if asst.Text() != "ok" || !strings.Contains(string(asst.ToolCalls), `"arguments":"{\"q\":\"x\"}"`) ||
		!strings.Contains(string(asst.Thinking), `"signature":"sig"`) {
		t.Errorf("assistant = %+v", asst)
	}
	tool := req.Messages[4]
	if tool.ToolCallID != "toolu_1" || !tool.IsError || tool.Text() != "found" {
		t.Errorf("tool message = %+v", tool)
	}
	if req.Messages[5].Text() != "<system-reminder>" {
		t.Errorf("user message = %s", req.Messages[5].Content)
	}
	// The tool results answered, retrieval is skipped by the original message.
	if !tr.ToolTurn {
		t.Error("ToolTurn is false")
	}
	if len(tr.Warnings) != 1 || !strings.Contains(tr.Warnings[0], "budget_tokens") {
		t.Errorf("warnings = %q", tr.Warnings)
	}
}

func TestMessagesToChatNamesWhatItDrops(t *testing.T) {
	tr := nmTranslate(t, `{"model":"m","max_tokens":9,"top_k":5,"context_management":{},"container":"c","mcp_servers":[],"thinking":{"type":"disabled"},`+
		`"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"f","input_schema":{"type":"object"},"strict":true}],`+
		`"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}},{"type":"text","text":"q","citations":{"enabled":true}}]}]}`)
	joined := strings.Join(tr.Warnings, "\n")
	for _, want := range []string{"container", "context_management", "mcp_servers", "top_k", "document", "web_search_20250305", "strict", "citations"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %q:\n%s", want, joined)
		}
	}
	if e, _ := tr.Request.ReasoningEffort(); e != "none" {
		t.Errorf("disabled thinking = %q", e)
	}
	if strings.Contains(string(tr.Request.Tools), "web_search") || !strings.Contains(string(tr.Request.Tools), `"f"`) {
		t.Errorf("tools = %s", tr.Request.Tools)
	}
	if tr.ToolTurn {
		t.Error("ToolTurn without a tool_result")
	}
}

// A block whose fields the translation does not model (a search_result has a
// string source, an image an object) is dropped by its type with a Warning;
// it never fails the request, and the tool turn is still recognised.
func TestMessagesToChatToleratesBlocksItDoesNotModel(t *testing.T) {
	tr := nmTranslate(t, `{"model":"m","max_tokens":9,"messages":[`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[`+
		`{"type":"text","text":"kept"},{"type":"search_result","source":"https://x","title":"T","content":[{"type":"text","text":"a"}]}]},`+
		`{"type":"search_result","source":"https://y","title":"U","content":[{"type":"text","text":"b"}]},`+
		`{"type":"image","source":"not-an-object"}]}]}`)
	if !tr.ToolTurn {
		t.Error("the tool turn was not recognised")
	}
	joined := strings.Join(tr.Warnings, "\n")
	if !strings.Contains(joined, "search_result") {
		t.Errorf("no warning names search_result:\n%s", joined)
	}
	var kept bool
	for _, m := range tr.Request.Messages {
		if m.Role == "tool" && strings.Contains(string(m.Content), "kept") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the text of the tool result was lost: %+v", tr.Request.Messages)
	}
	for name, raw := range map[string]string{
		"non-object block":      `{"role":"user","content":["text"]}`,
		"malformed tool_result": `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x","is_error":"yes"}]}`,
	} {
		if _, err := MessagesToChat(nmBody(t, `{"model":"m","max_tokens":9,"messages":[`+raw+`]}`)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMessagesToChatChoices(t *testing.T) {
	base := `"messages":[{"role":"user","content":"x"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]`
	for _, tc := range []struct{ choice, want string }{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"f"}`, `{"function":{"name":"f"},"type":"function"}`},
	} {
		tr := nmTranslate(t, `{`+base+`,"tool_choice":`+tc.choice+`}`)
		if got := string(tr.Request.ToolChoice); got != tc.want {
			t.Errorf("%s -> %s, want %s", tc.choice, got, tc.want)
		}
	}
	// Adaptive thinking takes its effort from output_config.
	tr := nmTranslate(t, `{`+base+`,"thinking":{"type":"adaptive"},"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	if e, _ := tr.Request.ReasoningEffort(); e != "low" || !strings.Contains(string(tr.Request.ResponseFormat), `"schema":{"type":"object"}`) {
		t.Errorf("effort = %q, format = %s", e, tr.Request.ResponseFormat)
	}
	for _, bad := range []string{`{"messages":[]}`, `{"messages":[{"role":"system","content":"x"}]}`, `{"messages":[{"role":"user","content":5}]}`,
		`{"messages":[{"role":"user","content":"x"}],"max_tokens":"many"}`} {
		if _, err := MessagesToChat(nmBody(t, bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestChatToMessages(t *testing.T) {
	text, finish := "Hello", "tool_calls"
	resp := &ChatResponse{ID: "chatcmpl-abc", Choices: []Choice{{Message: ResponseMessage{Role: "assistant", Content: &text,
		ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]`),
		Thinking:  []ThinkingBlock{{Type: "thinking", Thinking: "hm", Signature: "sig"}, {Type: "redacted_thinking", Data: "blob"}}},
		FinishReason: &finish}},
		Usage: &Usage{PromptTokens: 20, CompletionTokens: 7, PromptTokensDetails: &PromptTokensDetails{CachedTokens: 5, CacheWriteTokens: 2}}}
	raw, err := json.Marshal(ChatToMessages(resp, "my-route", []byte(`{"sources":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID, Type, Role, Model string
		Content               []map[string]any
		StopReason            string          `json:"stop_reason"`
		StopSequence          *string         `json:"stop_sequence"`
		Usage                 map[string]int  `json:"usage"`
		Ragmux                json.RawMessage `json:"ragmux"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "msg_abc" || got.Type != "message" || got.Role != "assistant" || got.Model != "my-route" || got.StopReason != "tool_use" || got.StopSequence != nil {
		t.Errorf("envelope = %s", raw)
	}
	var types []string
	for _, b := range got.Content {
		types = append(types, b["type"].(string))
	}
	if strings.Join(types, ",") != "thinking,redacted_thinking,text,tool_use" {
		t.Errorf("blocks = %v", types)
	}
	if got.Content[0]["signature"] != "sig" || got.Content[3]["name"] != "f" || got.Content[3]["input"].(map[string]any)["a"] != float64(1) {
		t.Errorf("content = %v", got.Content)
	}
	if got.Usage["input_tokens"] != 13 || got.Usage["output_tokens"] != 7 || got.Usage["cache_read_input_tokens"] != 5 || got.Usage["cache_creation_input_tokens"] != 2 {
		t.Errorf("usage = %v", got.Usage)
	}
	if string(got.Ragmux) != `{"sources":[]}` {
		t.Errorf("ragmux = %s", got.Ragmux)
	}
	for finish, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "refusal", "weird": "end_turn"} {
		if MessagesStopReason(finish) != want {
			t.Errorf("%s -> %s", finish, MessagesStopReason(finish))
		}
	}
}

// nmEvents splits an encoded stream into its events.
func nmEvents(t *testing.T, b []byte) (names []string, data []map[string]any) {
	t.Helper()
	for _, block := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		lines := strings.SplitN(block, "\n", 2)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("not an event: %q", block)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &m); err != nil {
			t.Fatalf("event data: %v", err)
		}
		names = append(names, strings.TrimPrefix(lines[0], "event: "))
		data = append(data, m)
	}
	return names, data
}

func TestMessagesEncoderEventSequence(t *testing.T) {
	zero := 0
	s := func(v string) *string { return &v }
	enc := NewMessagesEncoder("my-route", []byte(`{"sources":[]}`))
	var out []byte
	for _, c := range []StreamChunk{
		{Choices: []StreamChoice{{Delta: Delta{Role: "assistant", Content: s("")}}}},
		{Choices: []StreamChoice{{Delta: Delta{Thinking: []ThinkingBlock{{Index: &zero, Type: "thinking", Thinking: "hm"}}}}}},
		{Choices: []StreamChoice{{Delta: Delta{Thinking: []ThinkingBlock{{Index: &zero, Type: "thinking", Signature: "sig"}}}}}},
		{Choices: []StreamChoice{{Delta: Delta{Content: s("Hel")}}}},
		{Choices: []StreamChoice{{Delta: Delta{Content: s("lo")}}}},
		{Choices: []StreamChoice{{Delta: Delta{ToolCalls: json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]`)}}}},
		{Choices: []StreamChoice{{Delta: Delta{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"{\"a\""}}]`)}}}},
		{Choices: []StreamChoice{{Delta: Delta{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":":1}"}}]`)}}}},
		{Choices: []StreamChoice{{Delta: Delta{ToolCalls: json.RawMessage(`[{"index":1,"id":"call_2","function":{"name":"g","arguments":"{}"}}]`)}, FinishReason: s("tool_calls")}}},
		{Choices: []StreamChoice{}, Usage: &Usage{PromptTokens: 11, CompletionTokens: 6}},
	} {
		out = append(out, enc.Chunk(c)...)
	}
	out = append(out, enc.Finish()...)
	names, data := nmEvents(t, out)
	want := "message_start," +
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // thinking
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // text
		"content_block_start,content_block_delta,content_block_delta,content_block_stop," + // tool f
		"content_block_start,content_block_delta,content_block_stop," + // tool g
		"message_delta,message_stop"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("events\n%s\nwant\n%s", got, want)
	}
	msg := data[0]["message"].(map[string]any)
	if msg["model"] != "my-route" || msg["stop_reason"] != nil || msg["ragmux"] == nil || !strings.HasPrefix(msg["id"].(string), "msg_") {
		t.Errorf("message_start = %v", msg)
	}
	// Block indexes count up, one block open at a time.
	idx := []float64{}
	for i, n := range names {
		if n == "content_block_start" {
			idx = append(idx, data[i]["index"].(float64))
		}
	}
	for i, v := range idx {
		if int(v) != i {
			t.Errorf("block %d has index %v", i, v)
		}
	}
	if d := data[2]["delta"].(map[string]any); d["type"] != "thinking_delta" || d["thinking"] != "hm" {
		t.Errorf("thinking delta = %v", d)
	}
	if d := data[3]["delta"].(map[string]any); d["type"] != "signature_delta" || d["signature"] != "sig" {
		t.Errorf("signature delta = %v", d)
	}
	if d := data[10]["delta"].(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != `{"a"` {
		t.Errorf("input delta = %v", d)
	}
	last := data[len(data)-2]
	if d := last["delta"].(map[string]any); d["stop_reason"] != "tool_use" {
		t.Errorf("message_delta = %v", last)
	}
	if u := last["usage"].(map[string]any); u["input_tokens"] != float64(11) || u["output_tokens"] != float64(6) {
		t.Errorf("usage = %v", u)
	}
}

func TestMessagesEncoderWithoutAFinishReason(t *testing.T) {
	names, data := nmEvents(t, NewMessagesEncoder("m", nil).Finish())
	if strings.Join(names, ",") != "message_start,message_delta,message_stop" {
		t.Fatalf("events = %v", names)
	}
	if d := data[1]["delta"].(map[string]any); d["stop_reason"] != "end_turn" {
		t.Errorf("delta = %v", d)
	}
	if string(MessagesPing()) != "event: ping\ndata: {\"type\": \"ping\"}\n\n" {
		t.Errorf("ping = %q", MessagesPing())
	}
}

func TestMessagesErrorEnvelope(t *testing.T) {
	var got struct {
		Type  string
		Error struct{ Type, Message, Code string }
		ReqID string `json:"request_id"`
	}
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
		404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 500: "api_error", 502: "api_error",
		503: "overloaded_error", 504: "timeout_error"} {
		raw := MessagesError(status, "model_not_found", "no such model", "req-1")
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Type != "error" || got.Error.Type != want || got.Error.Code != "model_not_found" || got.ReqID != "req-1" {
			t.Errorf("%d: %s", status, raw)
		}
	}
	if strings.Contains(string(MessagesError(400, "", "x", "r")), `"code"`) {
		t.Error("an empty code was written")
	}
}
