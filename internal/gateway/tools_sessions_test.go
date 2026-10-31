package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// seenBodies records the request bodies the scripted upstream received.
type seenBodies struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *seenBodies) add(b map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, b)
}

func (s *seenBodies) last() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

func (s *seenBodies) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// recordUpstream makes the upstream note every chat body and answer with fn,
// or with the default answer when fn is nil.
func (e *env) recordUpstream(fn http.HandlerFunc) *seenBodies {
	seen := &seenBodies{}
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		seen.add(b)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		if fn != nil {
			fn(w, r)
			return
		}
		okChat(w, r)
	})
	return seen
}

func toolDef(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{"type": "object"}}}
}

func toolNamesOf(body map[string]any) []string {
	var out []string
	tools, _ := body["tools"].([]any)
	for _, t := range tools {
		fn, _ := t.(map[string]any)["function"].(map[string]any)
		out = append(out, fn["name"].(string))
	}
	return out
}

// toolCallingChat answers with a tool call to each of names; streamed, the
// name arrives in the first delta and the arguments in later ones.
func toolCallingChat(names ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		calls := make([]any, len(names))
		for i, n := range names {
			calls[i] = map[string]any{"index": i, "id": "call_" + n, "type": "function",
				"function": map[string]any{"name": n, "arguments": `{"secret":"do-not-store"}`}}
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": calls}}}})
			frag := make([]any, len(names))
			for i := range names {
				frag[i] = map[string]any{"index": i, "function": map[string]any{"arguments": " "}}
			}
			writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": frag}, "finish_reason": "tool_calls"}}})
			writeSSE(w, map[string]any{"id": "c", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 2}})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "mock-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, "finish_reason": "tool_calls"}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 1}})
	}
}

// waitSession waits for the deferred session upsert to reach tokens.
func (e *env) waitSession(sessionID string, requests int64) store.SessionUsage {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u, err := e.st.GetSessionUsage(context.Background(), e.proj.ID, sessionID)
		if err != nil {
			e.t.Fatal(err)
		}
		if u.Requests >= requests || time.Now().After(deadline) {
			return u
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) setToolLists(allow, deny []string) {
	e.t.Helper()
	e.proj.ToolAllow, e.proj.ToolDeny = allow, deny
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) chatWith(headers map[string]string, body map[string]any) (*http.Response, map[string]any) {
	e.t.Helper()
	return e.call(http.MethodPost, "/v1/chat/completions", body, e.key, headers)
}

func TestToolDenyStripsTheToolAndWarns(t *testing.T) {
	e := newEnv(t, nil)
	e.setToolLists(nil, []string{"mcp__github__*", "shell"})
	seen := e.recordUpstream(nil)

	resp, out := e.chat(map[string]any{"messages": userMsg, "tool_choice": "auto",
		"tools": []any{toolDef("get_weather"), toolDef("shell"), toolDef("mcp__github__create_issue")}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	if got := toolNamesOf(seen.last()); !reflect.DeepEqual(got, []string{"get_weather"}) {
		t.Errorf("the upstream saw tools %v, want only get_weather", got)
	}
	warned := false
	for _, w := range resp.Header.Values("Warning") {
		warned = warned || (strings.Contains(w, "2 tool(s) removed") && strings.Contains(w, "shell"))
	}
	if !warned {
		t.Errorf("Warning headers = %v, want one naming the removal", resp.Header.Values("Warning"))
	}
	if rec := e.lastLog(); rec.ToolsDenied != 2 {
		t.Errorf("tools_denied = %d, want 2", rec.ToolsDenied)
	}
}

func TestToolDenyLeavesHistoryAndOtherRequestsAlone(t *testing.T) {
	e := newEnv(t, nil)
	e.setToolLists(nil, []string{"shell"})
	seen := e.recordUpstream(nil)
	history := []map[string]any{
		{"role": "user", "content": "run it"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "done"},
	}
	resp, out := e.chat(map[string]any{"messages": history, "tools": []any{toolDef("shell"), toolDef("search")}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	msgs, _ := seen.last()["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("upstream saw %d messages, want the 3 of the history", len(msgs))
	}
	if _, ok := msgs[1].(map[string]any)["tool_calls"]; !ok {
		t.Error("the past tool_calls were touched")
	}
	if rec := e.lastLog(); rec.ToolsDenied != 1 {
		t.Errorf("tools_denied = %d, want 1", rec.ToolsDenied)
	}

	// A request with no tools, and a project with no lists, are untouched.
	if resp, _ := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 || len(resp.Header.Values("Warning")) != 0 {
		t.Errorf("plain request: %d warnings %v", resp.StatusCode, resp.Header.Values("Warning"))
	}
}

func TestToolAllowListKeepsOnlyMatches(t *testing.T) {
	e := newEnv(t, nil)
	e.setToolLists([]string{"search", "mcp__docs__*"}, []string{"mcp__docs__write"})
	seen := e.recordUpstream(nil)
	resp, out := e.chat(map[string]any{"messages": userMsg,
		"tools": []any{toolDef("search"), toolDef("mcp__docs__read"), toolDef("mcp__docs__write"), toolDef("shell")}})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	if got := toolNamesOf(seen.last()); !reflect.DeepEqual(got, []string{"search", "mcp__docs__read"}) {
		t.Errorf("the upstream saw tools %v, want search and mcp__docs__read (deny beats allow)", got)
	}
}

func TestToolChoiceConflictsAre400(t *testing.T) {
	e := newEnv(t, nil)
	e.setToolLists(nil, []string{"shell"})
	seen := e.recordUpstream(nil)
	cases := map[string]map[string]any{
		"names a denied tool": {"messages": userMsg, "tools": []any{toolDef("shell"), toolDef("search")},
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "shell"}}},
		"required with nothing left": {"messages": userMsg, "tools": []any{toolDef("shell")}, "tool_choice": "required"},
	}
	for name, body := range cases {
		resp, out := e.chat(body)
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "tool_not_allowed" ||
			errorField(t, out, "type") != "invalid_request_error" {
			t.Errorf("%s: %d %v, want 400 tool_not_allowed", name, resp.StatusCode, out)
		}
	}
	if seen.count() != 0 {
		t.Errorf("a refused request reached the upstream %d times", seen.count())
	}

	// Auto with nothing left drops tools, tool_choice and parallel_tool_calls,
	// and the request is served.
	resp, out := e.chat(map[string]any{"messages": userMsg, "tools": []any{toolDef("shell")}, "tool_choice": "auto",
		"parallel_tool_calls": false})
	if resp.StatusCode != 200 {
		t.Fatalf("auto with nothing left: %d %v", resp.StatusCode, out)
	}
	if b := seen.last(); b["tools"] != nil || b["tool_choice"] != nil || b["parallel_tool_calls"] != nil {
		t.Errorf("the upstream saw tools %v tool_choice %v parallel_tool_calls %v, want none", b["tools"], b["tool_choice"], b["parallel_tool_calls"])
	}
	// "none" with nothing left behaves the same way.
	resp, out = e.chat(map[string]any{"messages": userMsg, "tools": []any{toolDef("shell")}, "tool_choice": "none"})
	if resp.StatusCode != 200 {
		t.Fatalf("none with nothing left: %d %v", resp.StatusCode, out)
	}
	if b := seen.last(); b["tools"] != nil || b["tool_choice"] != nil {
		t.Errorf("the upstream saw tools %v tool_choice %v, want neither", b["tools"], b["tool_choice"])
	}
}

func TestCalledToolNamesAreLogged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e := newEnv(t, nil)
		e.recordUpstream(toolCallingChat("b_tool", "a_tool", "b_tool"))
		resp := e.post(context.Background(), "/v1/chat/completions",
			map[string]any{"messages": userMsg, "stream": stream, "tools": []any{toolDef("a_tool"), toolDef("b_tool")}}, e.key)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stream=%v: %d %s", stream, resp.StatusCode, raw)
		}
		rec := e.lastLog()
		if !reflect.DeepEqual(rec.ToolNames, []string{"a_tool", "b_tool"}) {
			t.Errorf("stream=%v: tool_names = %v, want [a_tool b_tool]", stream, rec.ToolNames)
		}
		// Names only: the arguments the model produced are nowhere on any column
		// of any row.
		var leaked int
		if err := e.st.DB().QueryRow(context.Background(),
			`SELECT count(*) FROM request_logs l WHERE row_to_json(l)::text LIKE '%do-not-store%'`).Scan(&leaked); err != nil || leaked != 0 {
			t.Errorf("stream=%v: tool arguments reached the log (%d, %v)", stream, leaked, err)
		}
	}

	e := newEnv(t, nil)
	if resp, _ := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatal("plain chat failed")
	}
	if rec := e.lastLog(); len(rec.ToolNames) != 0 {
		t.Errorf("a request whose answer called no tool logged %v", rec.ToolNames)
	}
}

func TestSessionHeaderIsRecordedAndCounted(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 2; i++ {
		resp, out := e.chatWith(map[string]string{"X-Ragmux-Session": "agent-run.42:a"}, map[string]any{"messages": userMsg})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d %v", resp.StatusCode, out)
		}
	}
	if rec := e.lastLog(); rec.SessionID != "agent-run.42:a" {
		t.Errorf("session_id = %q", rec.SessionID)
	}
	u := e.waitSession("agent-run.42:a", 2)
	if u.Requests != 2 || u.PromptTokens != 10 || u.CompletionTokens != 2 {
		t.Errorf("session usage = %+v, want 2 requests, 10 prompt and 2 completion tokens", u)
	}

	// Without the header there is no session row.
	e2 := newEnv(t, nil)
	if resp, _ := e2.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatal("plain chat failed")
	}
	if rec := e2.lastLog(); rec.SessionID != "" {
		t.Errorf("session_id = %q, want none", rec.SessionID)
	}
	if list, _ := e2.st.ListSessions(context.Background(), e2.proj.ID, 0, 0); len(list) != 0 {
		t.Errorf("sessions = %d, want 0", len(list))
	}
}

func TestInvalidSessionIsRefused(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.SessionTokenCap = 1 })
	seen := e.recordUpstream(nil)
	for name, v := range map[string]string{"space": "a b", "quote": `a"b`, "too long": strings.Repeat("a", 129), "empty": "", "non ascii": "ş"} {
		resp, out := e.chatWith(map[string]string{"X-Ragmux-Session": v}, map[string]any{"messages": userMsg})
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_session_id" {
			t.Errorf("%s: %d %v, want 400 invalid_session_id", name, resp.StatusCode, out)
		}
	}
	if seen.count() != 0 {
		t.Errorf("a refused request reached the upstream %d times", seen.count())
	}
	rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 10)
	if err != nil || len(rows) != 0 {
		t.Errorf("refused requests wrote %d rows (%v)", len(rows), err)
	}
}

func TestSessionTokenCapAnswers429(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.SessionTokenCap = 10 })
	hdr := map[string]string{"X-Ragmux-Session": "loop-1"}

	// The first request is served (nothing spent yet) and spends 5+1 tokens; the
	// second is served too (6 < 10) and takes the session to 12.
	for i := 0; i < 2; i++ {
		resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %v", i+1, resp.StatusCode, out)
		}
		e.waitSession("loop-1", int64(i+1))
	}
	resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the cap: %d %v, want 429", resp.StatusCode, out)
	}
	if errorField(t, out, "code") != "session_token_cap" || errorField(t, out, "type") != "insufficient_quota" ||
		errorField(t, out, "scope") != "session" {
		t.Errorf("error = %v", out["error"])
	}
	if msg, _ := errorField(t, out, "message").(string); msg != "Session token cap reached (12 of 10 tokens used)." {
		t.Errorf("message = %q", msg)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}

	// The denial is a 429 row of that session; other sessions and requests
	// without one are served.
	deadline := time.Now().Add(5 * time.Second)
	var denied *store.RequestLog
	for denied == nil && time.Now().Before(deadline) {
		rows, _ := e.st.SessionRequests(context.Background(), e.proj.ID, "loop-1", 10)
		for _, r := range rows {
			if r.StatusCode == http.StatusTooManyRequests {
				denied = r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if denied == nil || denied.Error != "session_token_cap" || denied.SessionID != "loop-1" {
		t.Errorf("denial row = %+v", denied)
	}
	if resp, out := e.chatWith(map[string]string{"X-Ragmux-Session": "loop-2"}, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Errorf("another session: %d %v", resp.StatusCode, out)
	}
	if resp, out := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Errorf("no session: %d %v", resp.StatusCode, out)
	}
	// A denied request spends nothing: the session total stays at two requests.
	if u := e.waitSession("loop-1", 2); u.Requests != 2 {
		t.Errorf("session requests = %d, want 2", u.Requests)
	}
}

// The session is a column and nothing else: never a metric label (PRD rule 10).
// A session-cap denial uses the existing limits_denied series with its own
// closed reason value.
func TestSessionIsNotAMetricLabel(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.SessionTokenCap = 1 })
	reg := e.withMetrics()
	hdr := map[string]string{"X-Ragmux-Session": "never-a-label"}
	if resp, _ := e.chatWith(hdr, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatal("chat failed")
	}
	e.waitSession("never-a-label", 1)
	if resp, _ := e.chatWith(hdr, map[string]any{"messages": userMsg}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("the second request should be over the cap")
	}
	text := reg.Text()
	if strings.Contains(text, "never-a-label") {
		t.Error("the session id reached the metrics output")
	}
	mustContain(t, text, `ragmux_limits_denied_total{project="`+strconv.FormatInt(e.proj.ID, 10)+`",reason="session_token_cap"} 1`)
}
