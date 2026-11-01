package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

func TestSessionFrom(t *testing.T) {
	long := strings.Repeat("a", 129)
	cases := []struct {
		name     string
		headers  [][2]string
		fallback bool
		wantID   string
		wantOK   bool
	}{
		{"absent", nil, false, "", true},
		{"valid", [][2]string{{"X-Ragmux-Session", "run-1.a:b_c"}}, false, "run-1.a:b_c", true},
		{"longest", [][2]string{{"X-Ragmux-Session", strings.Repeat("a", 128)}}, false, strings.Repeat("a", 128), true},
		{"too long", [][2]string{{"X-Ragmux-Session", long}}, false, "", false},
		{"empty value", [][2]string{{"X-Ragmux-Session", ""}}, false, "", false},
		{"space", [][2]string{{"X-Ragmux-Session", "a b"}}, false, "", false},
		{"quote", [][2]string{{"X-Ragmux-Session", `a"b`}}, false, "", false},
		{"non ascii", [][2]string{{"X-Ragmux-Session", "oturum-ş"}}, false, "", false},
		{"sent twice", [][2]string{{"X-Ragmux-Session", "a"}, {"X-Ragmux-Session", "b"}}, false, "", false},
		{"claude header ignored without opt-in", [][2]string{{"x-claude-code-session-id", "c1"}}, false, "", true},
		{"claude header is the fallback", [][2]string{{"x-claude-code-session-id", "c1"}}, true, "c1", true},
		{"ragmux header wins", [][2]string{{"X-Ragmux-Session", "r1"}, {"x-claude-code-session-id", "c1"}}, true, "r1", true},
		{"claude header is validated too", [][2]string{{"x-claude-code-session-id", "c 1"}}, true, "", false},
		{"bad ragmux header is not rescued by the fallback", [][2]string{{"X-Ragmux-Session", "a b"}, {"x-claude-code-session-id", "c1"}}, true, "", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		for _, h := range tc.headers {
			r.Header.Add(h[0], h[1])
		}
		id, ok := sessionFrom(r, tc.fallback)
		if id != tc.wantID || ok != tc.wantOK {
			t.Errorf("%s: sessionFrom = (%q, %v), want (%q, %v)", tc.name, id, ok, tc.wantID, tc.wantOK)
		}
	}
}

func TestToolPolicyPermits(t *testing.T) {
	cases := []struct {
		name        string
		allow, deny []string
		tool        string
		want        bool
	}{
		{"no lists", nil, nil, "anything", true},
		{"deny exact", nil, []string{"shell"}, "shell", false},
		{"deny leaves others", nil, []string{"shell"}, "search", true},
		{"deny glob", nil, []string{"mcp__github__*"}, "mcp__github__create_issue", false},
		{"deny glob is a prefix match", nil, []string{"mcp__github__*"}, "mcp__gitlab__create_issue", true},
		{"case sensitive", nil, []string{"Shell"}, "shell", true},
		{"allow only matches", []string{"search", "mcp__docs__*"}, nil, "search", true},
		{"allow glob", []string{"search", "mcp__docs__*"}, nil, "mcp__docs__read", true},
		{"allow rejects the rest", []string{"search"}, nil, "shell", false},
		{"deny beats allow", []string{"mcp__*"}, []string{"mcp__github__*"}, "mcp__github__x", false},
		{"allow still lets the rest of its glob through", []string{"mcp__*"}, []string{"mcp__github__*"}, "mcp__docs__x", true},
		{"star denies all", nil, []string{"*"}, "x", false},
		{"nameless tool under an allow list", []string{"search"}, nil, "", false},
	}
	for _, tc := range cases {
		if got := (toolPolicy{allow: tc.allow, deny: tc.deny}).permits(tc.tool); got != tc.want {
			t.Errorf("%s: permits(%q) = %v, want %v", tc.name, tc.tool, got, tc.want)
		}
	}
}

func TestToolName(t *testing.T) {
	cases := map[string]string{
		`{"type":"function","function":{"name":"get_weather"}}`: "get_weather",
		`{"type":"custom","custom":{"name":"grep"}}`:            "grep",
		`{"name":"get_weather","input_schema":{}}`:              "get_weather",
		`{"type":"web_search_20250305","name":"web_search"}`:    "web_search",
		`{"type":"code_interpreter"}`:                           "code_interpreter",
		`{"type":"function"}`:                                   "",
		`"nope"`:                                                "",
		`7`:                                                     "",
	}
	for in, want := range cases {
		if got := toolName(json.RawMessage(in)); got != want {
			t.Errorf("toolName(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestToolChoiceOf(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantName string
	}{
		{``, choiceAuto, ""},
		{`null`, choiceAuto, ""},
		{`"auto"`, choiceAuto, ""},
		{`"none"`, choiceNone, ""},
		{`"required"`, choiceRequired, ""},
		{`{"type":"function","function":{"name":"a"}}`, choiceNamed, "a"},
		{`{"type":"custom","custom":{"name":"b"}}`, choiceNamed, "b"},
		{`{"type":"tool","name":"c"}`, choiceNamed, "c"},
		{`{"type":"any"}`, choiceRequired, ""},
		{`{"type":"auto"}`, choiceAuto, ""},
		{`{"type":"none"}`, choiceNone, ""},
		{`{"type":"allowed_tools","allowed_tools":{}}`, choiceAuto, ""},
		{`[1]`, choiceAuto, ""},
	}
	for _, tc := range cases {
		kind, name := toolChoiceOf(json.RawMessage(tc.in))
		if kind != tc.wantKind || name != tc.wantName {
			t.Errorf("toolChoiceOf(%s) = (%s, %q), want (%s, %q)", tc.in, kind, name, tc.wantKind, tc.wantName)
		}
	}
}

func fn(name string) string {
	return `{"type":"function","function":{"name":"` + name + `","parameters":{"type":"object"}}}`
}

func TestGovernTools(t *testing.T) {
	deny := toolPolicy{deny: []string{"mcp__github__*", "shell"}}
	cases := []struct {
		name       string
		policy     toolPolicy
		tools      string
		choice     string
		wantTools  string // JSON, "" for dropped
		wantChoice string // "" for dropped
		wantDenied []string
		wantErr    string // error code, "" for none, "no code" for a refusal without one
		wantWarn   string // substring of the single warning, "" for none
	}{
		{name: "no lists leaves the request alone", policy: toolPolicy{}, tools: `[` + fn("a") + `]`, choice: `"auto"`,
			wantTools: `[` + fn("a") + `]`, wantChoice: `"auto"`},
		{name: "nothing denied leaves the bytes alone", policy: deny, tools: `[ ` + fn("a") + ` ]`,
			wantTools: `[ ` + fn("a") + ` ]`},
		{name: "strips the denied tool", policy: deny, tools: `[` + fn("a") + `,` + fn("shell") + `,` + fn("mcp__github__x") + `]`,
			wantTools: `[` + fn("a") + `]`, wantDenied: []string{"shell", "mcp__github__x"}, wantWarn: "2 tool(s) removed"},
		{name: "keeps a surviving tool_choice", policy: deny, tools: `[` + fn("a") + `,` + fn("shell") + `]`, choice: `"required"`,
			wantTools: `[` + fn("a") + `]`, wantChoice: `"required"`, wantDenied: []string{"shell"}, wantWarn: "shell"},
		{name: "names a denied tool", policy: deny, tools: `[` + fn("a") + `]`, choice: `{"type":"function","function":{"name":"shell"}}`,
			wantErr: "tool_not_allowed"},
		{name: "names a denied tool that was not even offered", policy: deny, choice: `{"type":"tool","name":"shell"}`,
			wantErr: "tool_not_allowed"},
		{name: "names an allowed tool", policy: deny, tools: `[` + fn("a") + `,` + fn("shell") + `]`, choice: `{"type":"function","function":{"name":"a"}}`,
			wantTools: `[` + fn("a") + `]`, wantChoice: `{"type":"function","function":{"name":"a"}}`, wantDenied: []string{"shell"}, wantWarn: "shell"},
		{name: "required with nothing left", policy: deny, tools: `[` + fn("shell") + `]`, choice: `"required"`, wantErr: "tool_not_allowed"},
		{name: "any with nothing left", policy: deny, tools: `[` + fn("shell") + `]`, choice: `{"type":"any"}`, wantErr: "tool_not_allowed"},
		{name: "auto with nothing left drops both", policy: deny, tools: `[` + fn("shell") + `]`, choice: `"auto"`,
			wantDenied: []string{"shell"}, wantWarn: "no tool is left"},
		{name: "absent choice with nothing left drops tools", policy: deny, tools: `[` + fn("shell") + `]`,
			wantDenied: []string{"shell"}, wantWarn: "no tool is left"},
		{name: "none with nothing left drops both", policy: deny, tools: `[` + fn("shell") + `]`, choice: `"none"`,
			wantDenied: []string{"shell"}, wantWarn: "no tool is left"},
		{name: "none with tools left stays none", policy: deny, tools: `[` + fn("a") + `,` + fn("shell") + `]`, choice: `"none"`,
			wantTools: `[` + fn("a") + `]`, wantChoice: `"none"`, wantDenied: []string{"shell"}, wantWarn: "shell"},
		{name: "allow list strips what it does not name", policy: toolPolicy{allow: []string{"a"}}, tools: `[` + fn("a") + `,` + fn("b") + `]`,
			wantTools: `[` + fn("a") + `]`, wantDenied: []string{"b"}, wantWarn: "b"},
		{name: "tools that is not an array is refused", policy: deny, tools: `{"a":1}`, wantErr: "no code"},
		{name: "a name that is unsafe to echo is counted, not named", policy: toolPolicy{allow: []string{"a"}},
			tools: `[` + fn("a") + `,` + fn(`x\"y`) + `]`, wantTools: `[` + fn("a") + `]`, wantDenied: []string{`x"y`}, wantWarn: "1 tool(s) removed by this project's tool policy"},
	}
	for _, tc := range cases {
		req := provider.ChatRequest{Tools: json.RawMessage(tc.tools), ToolChoice: json.RawMessage(tc.choice)}
		if tc.tools == "" {
			req.Tools = nil
		}
		v, terr := governTools(tc.policy, &req)
		if tc.wantErr != "" {
			wantCode := tc.wantErr
			if wantCode == "no code" {
				wantCode = ""
			}
			if terr == nil || terr.code != wantCode {
				t.Errorf("%s: error = %+v, want code %q", tc.name, terr, tc.wantErr)
			}
			continue
		}
		if terr != nil {
			t.Errorf("%s: unexpected error %+v", tc.name, terr)
			continue
		}
		if string(req.Tools) != tc.wantTools {
			t.Errorf("%s: tools = %s, want %s", tc.name, req.Tools, tc.wantTools)
		}
		if string(req.ToolChoice) != tc.wantChoice {
			t.Errorf("%s: tool_choice = %s, want %s", tc.name, req.ToolChoice, tc.wantChoice)
		}
		if strings.Join(v.denied, ",") != strings.Join(tc.wantDenied, ",") {
			t.Errorf("%s: denied = %v, want %v", tc.name, v.denied, tc.wantDenied)
		}
		switch {
		case tc.wantWarn == "" && len(v.warnings) != 0:
			t.Errorf("%s: warnings = %v, want none", tc.name, v.warnings)
		case tc.wantWarn != "" && (len(v.warnings) != 1 || !strings.Contains(v.warnings[0], tc.wantWarn)):
			t.Errorf("%s: warnings = %v, want one holding %q", tc.name, v.warnings, tc.wantWarn)
		}
	}
}

// A refused request leaves the caller's request untouched.
func TestGovernToolsRefusalKeepsTheRequest(t *testing.T) {
	req := provider.ChatRequest{Tools: json.RawMessage(`[` + fn("shell") + `]`), ToolChoice: json.RawMessage(`"required"`)}
	if _, terr := governTools(toolPolicy{deny: []string{"shell"}}, &req); terr == nil {
		t.Fatal("want a refusal")
	}
	if string(req.Tools) != `[`+fn("shell")+`]` || string(req.ToolChoice) != `"required"` {
		t.Errorf("request changed on refusal: %s %s", req.Tools, req.ToolChoice)
	}
}

func TestToolNameSet(t *testing.T) {
	var s toolNameSet
	if s.names() != nil {
		t.Error("an empty set must give nil")
	}
	// A full message, then stream deltas: the name arrives once, later deltas
	// carry only argument fragments.
	s.add(json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"b","arguments":"{\"secret\":1}"}},{"function":{"name":"a","arguments":""}}]`))
	s.add(json.RawMessage(`[{"index":0,"function":{"arguments":"{\"x\""}}]`))
	s.add(json.RawMessage(`[{"function":{"name":"a"}}]`))
	s.add(json.RawMessage(`not json`))
	s.add(nil)
	got := s.names()
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("names = %v, want [a b]", got)
	}
	for i := 0; i < 10*store.MaxToolNames; i++ {
		s.add(json.RawMessage(`[{"function":{"name":"t` + strings.Repeat("x", i%7) + string(rune('A'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + `"}}]`))
	}
	if n := len(s.seen); n > maxCollectedTools {
		t.Errorf("set grew to %d, bound is %d", n, maxCollectedTools)
	}
	long := strings.Repeat("n", 500)
	var l toolNameSet
	l.add(json.RawMessage(`[{"function":{"name":"` + long + `"}}]`))
	if n := l.names(); len(n) != 1 || len(n[0]) != store.MaxToolNameBytes {
		t.Errorf("long name = %d bytes, want %d", len(n[0]), store.MaxToolNameBytes)
	}
}

func TestWriteLimitErrorSessionCap(t *testing.T) {
	w := httptest.NewRecorder()
	writeLimitError(w, limits.Decision{Reason: limits.ReasonSessionTokenCap, Scope: limits.ScopeSession,
		SessionUsed: 52000, SessionLimit: 50000})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none: a session does not reset", got)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"message": "Session token cap reached (52000 of 50000 tokens used).",
		"type": "insufficient_quota", "code": "session_token_cap", "scope": "session"}
	for k, v := range want {
		if body.Error[k] != v {
			t.Errorf("error.%s = %v, want %v", k, body.Error[k], v)
		}
	}
	if len(body.Error) != len(want) {
		t.Errorf("error = %v, want exactly %v", body.Error, want)
	}
}

func TestGovernToolsDropsParallelToolCallsWithTheTools(t *testing.T) {
	policy := toolPolicy{deny: []string{"shell"}}
	parse := func(body string) provider.ChatRequest {
		var req provider.ChatRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		return req
	}

	// Nothing left: tools, tool_choice and parallel_tool_calls all go.
	req := parse(`{"messages":[],"tools":[` + fn("shell") + `],"tool_choice":"auto","parallel_tool_calls":false,"user":"u"}`)
	if _, terr := governTools(policy, &req); terr != nil {
		t.Fatal(terr)
	}
	if _, ok := req.Extra["parallel_tool_calls"]; ok || req.Tools != nil || req.ToolChoice != nil {
		t.Errorf("tools %s choice %s extra %v, want none of the three", req.Tools, req.ToolChoice, req.Extra)
	}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "parallel_tool_calls") || !strings.Contains(string(b), `"user":"u"`) {
		t.Errorf("wire body = %s", b)
	}

	// A tool survives: the field stays.
	req = parse(`{"messages":[],"tools":[` + fn("shell") + `,` + fn("a") + `],"parallel_tool_calls":false}`)
	if _, terr := governTools(policy, &req); terr != nil {
		t.Fatal(terr)
	}
	if _, ok := req.Extra["parallel_tool_calls"]; !ok {
		t.Error("parallel_tool_calls was dropped although a tool is left")
	}
}

func allowedTools(mode string, names ...string) string {
	var entries []string
	for _, n := range names {
		entries = append(entries, `{"type":"function","function":{"name":"`+n+`"}}`)
	}
	return `{"type":"allowed_tools","allowed_tools":{"mode":"` + mode + `","tools":[` + strings.Join(entries, ",") + `]}}`
}

func TestGovernToolsAllowedToolsChoice(t *testing.T) {
	policy := toolPolicy{deny: []string{"shell"}}
	cases := []struct {
		name       string
		tools      string
		choice     string
		wantTools  string
		wantChoice string
		wantErr    string
		wantWarn   string
	}{
		{name: "entries the policy removes leave the list", tools: `[` + fn("a") + `,` + fn("shell") + `]`,
			choice:    allowedTools("required", "shell", "a"),
			wantTools: `[` + fn("a") + `]`, wantChoice: allowedTools("required", "a"), wantWarn: "1 tool(s) removed from allowed_tools"},
		{name: "untouched list stays byte for byte", tools: `[` + fn("a") + `,` + fn("shell") + `]`,
			choice: allowedTools("auto", "a"), wantTools: `[` + fn("a") + `]`, wantChoice: allowedTools("auto", "a"), wantWarn: "shell"},
		{name: "required list emptied is refused", tools: `[` + fn("a") + `,` + fn("shell") + `]`,
			choice: allowedTools("required", "shell"), wantErr: "tool_not_allowed"},
		{name: "required with every tool stripped is refused", tools: `[` + fn("shell") + `]`,
			choice: allowedTools("required", "shell"), wantErr: "tool_not_allowed"},
		{name: "required with every tool stripped and an allowed entry is refused", tools: `[` + fn("shell") + `]`,
			choice: allowedTools("required", "a"), wantErr: "tool_not_allowed"},
		{name: "auto list emptied drops tool_choice", tools: `[` + fn("a") + `,` + fn("shell") + `]`,
			choice: allowedTools("auto", "shell"), wantTools: `[` + fn("a") + `]`, wantChoice: `"none"`, wantWarn: "removed from allowed_tools"},
		{name: "auto list emptied with nothing denied still forbids the rest", tools: `[` + fn("a") + `]`,
			choice: allowedTools("auto", "shell"), wantTools: `[` + fn("a") + `]`, wantChoice: `"none"`, wantWarn: "removed from allowed_tools"},
		{name: "auto with every tool stripped drops both", tools: `[` + fn("shell") + `]`,
			choice: allowedTools("auto", "shell"), wantWarn: "no tool is left"},
		{name: "auto list emptied without tools in the request drops tool_choice", tools: ``,
			choice: allowedTools("auto", "shell"), wantWarn: "removed from allowed_tools"},
		{name: "list narrowed without tools in the request", tools: ``,
			choice: allowedTools("auto", "shell", "a"), wantChoice: allowedTools("auto", "a"), wantWarn: "removed from allowed_tools"},
	}
	for _, tc := range cases {
		req := provider.ChatRequest{ToolChoice: json.RawMessage(tc.choice)}
		if tc.tools != "" {
			req.Tools = json.RawMessage(tc.tools)
		}
		before := string(req.ToolChoice)
		v, terr := governTools(policy, &req)
		if tc.wantErr != "" {
			if terr == nil || terr.code != tc.wantErr {
				t.Errorf("%s: error = %+v, want %q", tc.name, terr, tc.wantErr)
			}
			if string(req.ToolChoice) != before {
				t.Errorf("%s: a refused request was changed", tc.name)
			}
			continue
		}
		if terr != nil {
			t.Errorf("%s: unexpected error %+v", tc.name, terr)
			continue
		}
		if string(req.Tools) != tc.wantTools || !sameJSON(req.ToolChoice, json.RawMessage(tc.wantChoice)) {
			t.Errorf("%s: tools %s choice %s, want %s and %s", tc.name, req.Tools, req.ToolChoice, tc.wantTools, tc.wantChoice)
		}
		joined := strings.Join(v.warnings, "|")
		if (tc.wantWarn == "") != (joined == "") || !strings.Contains(joined, tc.wantWarn) {
			t.Errorf("%s: warnings = %v, want one holding %q", tc.name, v.warnings, tc.wantWarn)
		}
	}
}

// sameJSON compares two JSON values by content; empty means absent.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
