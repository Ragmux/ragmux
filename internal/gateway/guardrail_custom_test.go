package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/guardrail"
)

// An OpenAI freeform custom tool call carries its text in custom.input, where
// a function call has arguments. Both are the text of what the model wrote for
// a tool, and a rule reads both.
func TestCustomToolCallsAreRead(t *testing.T) {
	input := "SELECT * FROM people WHERE tckn='" + tckn + "'"
	customCall := func(name, input string) map[string]any {
		return map[string]any{"id": "c1", "type": "custom", "custom": map[string]any{"name": name, "input": input}}
	}
	customOf := func(out map[string]any) map[string]any {
		return messageOf(out)["tool_calls"].([]any)[0].(map[string]any)["custom"].(map[string]any)
	}
	history := func(call map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"}}}
	}

	t.Run("answer, redact", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{customCall("sql", input)}}, "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		c := customOf(out)
		if got := c["input"].(string); strings.Contains(got, tckn) || !strings.Contains(got, "[REDACTED:k]") || c["name"] != "sql" {
			t.Errorf("custom = %v", c)
		}
	})
	t.Run("answer, the name is read", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{customCall("t"+tckn, "x")}}, "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		if raw, _ := json.Marshal(out); strings.Contains(string(raw), tckn) {
			t.Errorf("%s", raw)
		}
	})
	t.Run("answer, block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{customCall("sql", input)}}, "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if finishOf(out) != "content_filter" || strings.Contains(string(raw), tckn) {
			t.Errorf("%s", raw)
		}
	})
	t.Run("answer, a match that is not there changes nothing", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{customCall("sql", "SELECT 1")}}, "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		if c := customOf(out); c["input"] != "SELECT 1" || c["name"] != "sql" {
			t.Errorf("custom = %v", c)
		}
	})
	t.Run("stream: the match is not written, redact and block", func(t *testing.T) {
		for _, action := range []string{guardrail.ActionRedact, guardrail.ActionBlock} {
			e := newEnv(t, nil)
			var deltas []map[string]any
			for i, r := range input {
				c := map[string]any{"index": 0, "custom": map[string]any{"input": string(r)}}
				if i == 0 {
					c = map[string]any{"index": 0, "id": "c1", "type": "custom", "custom": map[string]any{"name": "sql", "input": string(r)}}
				}
				deltas = append(deltas, map[string]any{"tool_calls": []any{c}})
			}
			e.up.setChat(streamDeltas(deltas...))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			s := e.streamChat(userBody("hi"))
			if strings.Contains(s.all(), tckn) || strings.Contains(s.args, "1000000") {
				t.Errorf("%s: the match is in the stream: %q", action, s.args)
			}
			if action == guardrail.ActionBlock && s.finish != "content_filter" {
				t.Errorf("block: finish %q", s.finish)
			}
			if action == guardrail.ActionRedact && !strings.Contains(s.args, "SELECT * FROM people") {
				t.Errorf("redact: the rest of the input is lost: %q", s.args)
			}
		}
	})
	t.Run("stream: a call that is a custom one keeps its delta after a flush", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(
			map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "custom", "custom": map[string]any{"name": "sql", "input": "SELECT 1"}}}},
		))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if !strings.Contains(s.args, "SELECT 1") || strings.Contains(s.all(), `"function"`) {
			t.Errorf("args %q, events %q", s.args, s.all())
		}
	})
	t.Run("request, redact", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		resp, out := e.chat(history(customCall("sql", input)))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		c := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["custom"].(map[string]any)
		if got := c["input"].(string); strings.Contains(got, tckn) || !strings.Contains(got, "[REDACTED:k]") || c["name"] != "sql" {
			t.Errorf("custom sent upstream = %v", c)
		}
	})
	t.Run("request, block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, _ := e.chat(history(customCall("sql", input)))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
}
