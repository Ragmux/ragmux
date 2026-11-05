package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/guardrail"
)

// The text-carrying fields of the chat and Messages shapes are inventoried in
// the engineer report (Düzeltme turu 2). Each field the inventory marks as read
// has a case here that plants a match in it; each marked exempt has one that
// shows the exemption is the narrow one it claims.

func regexRule(id, action, pattern string, stages ...string) guardrail.Rule {
	return guardrail.Rule{ID: id, Type: guardrail.TypeRegex, Action: action, Stages: stages, Pattern: pattern}
}

// chatMsgs is a request body with one extra message after a plain user turn.
func chatMsgs(extra ...any) map[string]any {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	return map[string]any{"messages": append(msgs, extra...)}
}

func TestPreRequestReadsEveryChatField(t *testing.T) {
	secret := "id " + tckn
	call := func(name, args string) map[string]any {
		return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}
	}
	bodies := map[string]map[string]any{
		"a refusal part": chatMsgs(map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "refusal", "refusal": secret}}}),
		"an input_text part": chatMsgs(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": secret}}}),
		"a part of a type that does not exist yet": chatMsgs(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "frobnicate", "payload": map[string]any{"words": secret}}}}),
		"a tool message with parts": chatMsgs(call("lookup", "{}"), map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{
			map[string]any{"type": "text", "text": secret}}}),
		"reasoning_content":       chatMsgs(map[string]any{"role": "assistant", "content": "x", "reasoning_content": secret}),
		"reasoning_content parts": chatMsgs(map[string]any{"role": "assistant", "content": "x", "reasoning_content": []any{map[string]any{"type": "text", "text": secret}}}),
		"a prediction string":     mergeBody(chatMsgs(), "prediction", map[string]any{"type": "content", "content": secret}),
		"a prediction with parts": mergeBody(chatMsgs(), "prediction", map[string]any{"type": "content", "content": []any{map[string]any{"type": "text", "text": secret}}}),
		"a participant name":      chatMsgs(map[string]any{"role": "user", "content": "x", "name": tckn}),
		"a tool call name":        chatMsgs(call(tckn, "{}")),
		"a key of tool arguments": chatMsgs(call("lookup", `{"`+tckn+`":"x"}`)),
		"a number of tool args":   chatMsgs(call("lookup", `{"id":`+tckn+`}`)),
		"plain tool arguments":    chatMsgs(call("lookup", "id "+tckn)),
		"a nested tool argument":  chatMsgs(call("lookup", `{"a":[{"b":"`+secret+`"}]}`)),
		"a string content":        chatMsgs(map[string]any{"role": "user", "content": secret}),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			_, calls := e.chatCalls()
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
			resp, out := e.chat(body)
			if _, code, _ := errOf(t, out); resp.StatusCode != http.StatusBadRequest || code != "guardrail_blocked" || calls() != 0 {
				t.Errorf("%d %v (upstream calls %d): the match went through", resp.StatusCode, out, calls())
			}
		})
	}

	t.Run("an image, audio, a file and identifiers are not text", func(t *testing.T) {
		e := newEnv(t, nil)
		_, calls := e.chatCalls()
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		body := chatMsgs(
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://img.example/" + tckn + ".png", "detail": "low"}},
				map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": tckn, "format": "wav"}},
				map[string]any{"type": "file", "file": map[string]any{"file_data": tckn, "filename": tckn + ".pdf"}}}},
			call("lookup", "{}"),
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"})
		body["user"] = tckn // the end-user id the operator chose
		body["stop"] = []string{tckn}
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "schema": map[string]any{"description": secret}}}
		if resp, out := e.chat(body); resp.StatusCode != http.StatusOK || calls() != 1 {
			t.Errorf("%d %v", resp.StatusCode, out)
		}
	})

	t.Run("redact: reasoning_content, prediction, parts and keys keep their shape", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		body := chatMsgs(
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": secret}}, "reasoning_content": secret},
			call("lookup", `{"`+tckn+`":"x","n":3}`))
		body["prediction"] = map[string]any{"type": "content", "content": secret}
		resp, out := e.chat(body)
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		sent := seen.last()
		raw, _ := json.Marshal(sent)
		if strings.Contains(string(raw), tckn) {
			t.Fatalf("the match reached the upstream: %s", raw)
		}
		msgs := sent["messages"].([]any)
		asst := msgs[1].(map[string]any)
		part := asst["content"].([]any)[0].(map[string]any)
		if part["type"] != "refusal" || part["refusal"] != "id [REDACTED:k]" || asst["reasoning_content"] != "id [REDACTED:k]" {
			t.Errorf("assistant message = %v", asst)
		}
		if p := sent["prediction"].(map[string]any); p["type"] != "content" || p["content"] != "id [REDACTED:k]" {
			t.Errorf("prediction = %v", p)
		}
		args := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
		var v map[string]any
		if err := json.Unmarshal([]byte(args), &v); err != nil || v["[REDACTED:k]"] != "x" || v["n"] != float64(3) {
			t.Errorf("arguments = %s (%v)", args, err)
		}
	})
}

func mergeBody(b map[string]any, key string, v any) map[string]any {
	b[key] = v
	return b
}

// Arguments are read as JSON, and as the text they also are: a pattern that
// spans a key and its value is a pattern of the text.
func TestToolArgumentsAreAlsoProbedAsText(t *testing.T) {
	rule := regexRule("pw", guardrail.ActionBlock, `"password"\s*:\s*"[^"]+"`, guardrail.StagePreRequest, guardrail.StagePostResponse)
	args := `{"user":"a","password":"hunter2"}`
	t.Run("request", func(t *testing.T) {
		e := newEnv(t, nil)
		e.guardRules(rule)
		resp, _ := e.chat(chatMsgs(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "login", "arguments": args}}}}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
	t.Run("JSON answer and stream decide alike", func(t *testing.T) {
		call := map[string]any{"content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
			"function": map[string]any{"name": "login", "arguments": args}}}}
		ej := newEnv(t, nil)
		ej.up.setChat(chatReply(call, "tool_calls"))
		ej.guardRules(rule)
		_, out := ej.chat(userBody("hi"))
		if finishOf(out) != "content_filter" {
			t.Errorf("JSON finish %q", finishOf(out))
		}
		es := newEnv(t, nil)
		var deltas []map[string]any
		for i, r := range args {
			c := map[string]any{"index": 0, "function": map[string]any{"arguments": string(r)}}
			if i == 0 {
				c = map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "login", "arguments": string(r)}}
			}
			deltas = append(deltas, map[string]any{"tool_calls": []any{c}})
		}
		es.up.setChat(streamDeltas(deltas...))
		es.guardRules(rule)
		if s := es.streamChat(userBody("hi")); s.finish != "content_filter" || strings.Contains(s.args, "hunter2") {
			t.Errorf("stream finish %q args %q", s.finish, s.args)
		}
	})
	t.Run("native input", func(t *testing.T) {
		body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"login","input":{"user":"a","password":"hunter2"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":4}}`
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(body))
		e.guardRules(rule)
		_, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if !strings.Contains(string(raw), `"stop_reason":"refusal"`) || strings.Contains(string(raw), "hunter2") {
			t.Errorf("%s", raw)
		}
	})
}

// A client can type "[REDACTED:<id>]" as easily as a rule can write it. Only a
// placeholder the scan wrote itself is let alone.
func TestTypedPlaceholdersDoNotGetAMatchPast(t *testing.T) {
	rule := regexRule("pw", guardrail.ActionBlock, `(?i)password\s*=\s*\S+`, guardrail.StagePreRequest, guardrail.StagePostResponse)
	for text, want := range map[string]int{
		"password=hunter2":                 http.StatusBadRequest,
		"password=hunter2[REDACTED:pw]":    http.StatusBadRequest,
		"password=[REDACTED:pw]hunter2":    http.StatusBadRequest,
		"password=hunter2[REDACTED:other]": http.StatusBadRequest,
		"see [REDACTED:pw] for details":    http.StatusOK,
	} {
		e := newEnv(t, nil)
		e.guardRules(rule)
		if resp, _ := e.chat(userBody(text)); resp.StatusCode != want {
			t.Errorf("request %q: status %d, want %d", text, resp.StatusCode, want)
		}
	}
	// The same in an answer, whole and streamed.
	for text, blocked := range map[string]bool{"password=hunter2[REDACTED:pw]": true, "see [REDACTED:pw] for details": false} {
		ej := newEnv(t, nil)
		ej.up.setChat(chatReply(map[string]any{"content": text}, "stop"))
		ej.guardRules(rule)
		_, out := ej.chat(userBody("hi"))
		es := newEnv(t, nil)
		es.up.setChat(streamDeltas(charDeltas(text)...))
		es.guardRules(rule)
		s := es.streamChat(userBody("hi"))
		if (finishOf(out) == "content_filter") != blocked || (s.finish == "content_filter") != blocked {
			t.Errorf("answer %q: JSON finish %q, stream finish %q, want blocked=%v", text, finishOf(out), s.finish, blocked)
		}
	}
}

// A block rule that reaches over what a redact rule wrote reads the same text
// whole and streamed.
func TestJSONAndStreamAgreeAroundARedaction(t *testing.T) {
	redact := guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse)
	block := regexRule("line", guardrail.ActionBlock, `id: .*`, guardrail.StagePostResponse)
	text := "id: " + tckn + " secret stuff"

	ej := newEnv(t, nil)
	ej.up.setChat(chatReply(map[string]any{"content": text}, "stop"))
	ej.guardRules(redact, block)
	_, out := ej.chat(userBody("hi"))

	es := newEnv(t, nil)
	es.up.setChat(streamDeltas(charDeltas(text)...))
	es.guardRules(redact, block)
	s := es.streamChat(userBody("hi"))

	if finishOf(out) != "content_filter" || s.finish != "content_filter" || strings.Contains(s.all(), "secret stuff") {
		t.Errorf("JSON finish %q, stream finish %q, stream text %q", finishOf(out), s.finish, s.text)
	}
}

// ------------------------------------------------------------------ the rest

func TestPreRequestNativeURLsAndNamesAreRead(t *testing.T) {
	blocks := map[string]any{
		"a document url": map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": "https://a.example/doc.pdf?id=" + tckn}},
		"a search_result source": map[string]any{"type": "search_result", "source": "https://a.example/" + tckn, "title": "t",
			"content": []any{map[string]any{"type": "text", "text": "x"}}},
		"a name in a block of a type that does not exist yet": map[string]any{"type": "frobnicate", "name": "id " + tckn},
	}
	for name, block := range blocks {
		t.Run(name, func(t *testing.T) {
			e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
			resp, raw := e.messages(nativeUserBody(map[string]any{"type": "text", "text": "hi"}, block), nativeHeaders(e))
			if resp.StatusCode != http.StatusBadRequest || up.hits.Load() != 0 {
				t.Errorf("%d %s", resp.StatusCode, raw)
			}
		})
	}
	// What is still left alone is an identifier, a signature or binary data.
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
	resp, raw := e.messages(nativeUserBody(map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": tckn}},
		map[string]any{"type": "tool_result", "tool_use_id": tckn, "content": "fine"},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": tckn, "content": []any{
			map[string]any{"type": "web_search_result", "encrypted_content": tckn, "title": "t"}}}), nativeHeaders(e))
	if resp.StatusCode != 200 || up.hits.Load() != 1 {
		t.Errorf("%d %s", resp.StatusCode, raw)
	}
}

// The address of a citation is part of the answer.
func TestPostResponseReadsCitationURLs(t *testing.T) {
	annotation := []any{map[string]any{"type": "url_citation", "url_citation": map[string]any{
		"start_index": 0, "end_index": 4, "title": "t", "url": "https://a.example/?id=" + tckn}}}
	for _, action := range []string{guardrail.ActionBlock, guardrail.ActionRedact} {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "see it", "annotations": annotation}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), tckn) {
			t.Errorf("%s: the address carries the match to the client: %s", action, raw)
		}
	}
}

func TestNativeStreamReadsWhatItDoesNotKnow(t *testing.T) {
	events := func(ev ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range ev {
				_, _ = fmt.Fprint(w, e)
			}
		}
	}
	start := func(content string) string {
		return sseEvent2("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":`+content+`,"usage":{"input_tokens":25,"output_tokens":1}}}`)
	}
	tail := sseEvent2("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`) +
		sseEvent2("message_stop", `{"type":"message_stop"}`)
	cases := map[string]http.HandlerFunc{
		"an unknown delta": events(start(`[]`),
			sseEvent2("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sseEvent2("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta","payload":"id `+tckn+`"}}`),
			sseEvent2("content_block_stop", `{"type":"content_block_stop","index":0}`), tail),
		"content in message_start": events(start(`[{"type":"text","text":"id `+tckn+`"}]`), tail),
	}
	for name, up := range cases {
		for _, action := range []string{guardrail.ActionBlock, guardrail.ActionRedact} {
			t.Run(name+", "+action, func(t *testing.T) {
				e, _ := anthropicEnv(t, "claude-opus-5-5", up)
				e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
				_, raw := e.messages(nativeBody(true), nativeHeaders(e))
				if strings.Contains(string(raw), tckn) {
					t.Errorf("the match reached the client: %s", raw)
				}
				if _, _, _, stop := nativeParts(t, raw); action == guardrail.ActionBlock && stop != "refusal" {
					t.Errorf("stop %q: %s", stop, raw)
				}
			})
		}
	}
}

// The name of a tool call is a string the model chose.
func TestPostResponseReadsToolCallNames(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
		"function": map[string]any{"name": "t" + tckn, "arguments": "{}"}}}}, "tool_calls"))
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
	_, out := e.chat(userBody("hi"))
	raw, _ := json.Marshal(out)
	if finishOf(out) != "content_filter" || strings.Contains(string(raw), tckn) {
		t.Errorf("%s", raw)
	}
}

// The project's own rules read the numbers in tool arguments; the webhook is
// sent keys and strings, never a number, and never the arguments as one text.
func TestWebhookIsNotSentTheNumbersOfToolArguments(t *testing.T) {
	args := `{"id":` + tckn + `,"n":7,"note":"hello"}`
	history := func() map[string]any {
		return chatMsgs(
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": args}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"})
	}
	texts := func(h *hook) []string {
		calls := h.seen()
		if len(calls) != 1 {
			t.Fatalf("%d webhook calls, want 1", len(calls))
		}
		var out []string
		for _, x := range calls[0].Body["texts"].([]any) {
			out = append(out, x.(map[string]any)["text"].(string))
		}
		return out
	}
	check := func(t *testing.T, got []string) {
		t.Helper()
		for _, s := range got {
			if s == tckn || s == "7" || strings.HasPrefix(s, "{") {
				t.Errorf("the webhook was sent %q: a number or the raw arguments", s)
			}
		}
		have := strings.Join(got, "|")
		for _, want := range []string{"id", "n", "note", "hello", "lookup"} {
			if !strings.Contains(have, want) {
				t.Errorf("the webhook was not sent %q (texts: %q)", want, got)
			}
		}
	}

	t.Run("a flag rule still counts the number", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		e.withHook(2000, "", guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRequest))
		resp, out := e.chat(history())
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		check(t, texts(h))
		if row := e.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v: the local rule must read the number", row)
		}
	})
	t.Run("a redact rule still redacts the number", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		h := e.newHook()
		e.withHook(2000, "", guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		if resp, out := e.chat(history()); resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		sent := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
		if strings.Contains(sent, tckn) {
			t.Errorf("the number reached the upstream: %s", sent)
		}
		for _, s := range texts(h) {
			if s == tckn {
				t.Errorf("the webhook was sent the number")
			}
		}
	})
	t.Run("a block rule still blocks on the number", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		e.withHook(2000, "", guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		if resp, _ := e.chat(history()); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d", resp.StatusCode)
		}
		if len(h.seen()) != 0 {
			t.Error("a blocked request was sent to the webhook")
		}
	})
	t.Run("the webhook's redaction lands on the right texts", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		h := e.newHook()
		h.set(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Texts []struct {
					Index int    `json:"index"`
					Text  string `json:"text"`
				} `json:"texts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i := range body.Texts {
				if body.Texts[i].Text == "hello" {
					body.Texts[i].Text = "[X]"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"action": "redact", "reason": "r", "texts": body.Texts})
		})
		e.withHook(2000, "")
		if resp, out := e.chat(history()); resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		sent := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
		var v map[string]any
		if err := json.Unmarshal([]byte(sent), &v); err != nil || v["note"] != "[X]" || v["n"] != float64(7) || v["id"] != float64(10000000146) {
			t.Errorf("arguments = %s (%v)", sent, err)
		}
	})
	t.Run("native tool input", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		h := e.newHook()
		e.withHook(2000, "", guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRequest))
		body := map[string]any{"model": "claude-opus-5-5", "max_tokens": 64, "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "lookup",
				"input": map[string]any{"id": 10000000146, "n": 7, "note": "hello"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "done"}}}}}
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		check2 := texts(h)
		for _, s := range check2 {
			if s == tckn || s == "7" || strings.HasPrefix(s, "{") {
				t.Errorf("the webhook was sent %q", s)
			}
		}
		if row := e.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
}

// ------------------------------------------------------------------- names

var legalName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func keywordRule(id, action string, words []string, stages ...string) guardrail.Rule {
	return guardrail.Rule{ID: id, Type: guardrail.TypeKeywords, Action: action, Stages: stages, Keywords: words}
}

// The name of a native tool call is read, in the request and in the answer,
// whole and streamed, as the name of a chat tool call is.
func TestNativeToolUseNameIsRead(t *testing.T) {
	toolUse := map[string]any{"type": "tool_use", "id": "t1", "name": tckn, "input": map[string]any{}}
	t.Run("request, block", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		body := map[string]any{"model": "claude-opus-5-5", "max_tokens": 64, "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{toolUse}}}}
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != http.StatusBadRequest || up.hits.Load() != 0 {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
	})
	answer := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t","name":"` + tckn + `","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":4}}`
	t.Run("answer, block", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(answer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if strings.Contains(string(raw), tckn) || !strings.Contains(string(raw), `"stop_reason":"refusal"`) {
			t.Errorf("%s", raw)
		}
	})
	t.Run("answer, redact leaves a legal name", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(answer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, raw := e.messages(nativeBody(false), nativeHeaders(e))
		var out struct {
			Content []map[string]any `json:"content"`
		}
		_ = json.Unmarshal(raw, &out)
		if name, _ := out.Content[0]["name"].(string); name != "REDACTED-k" || !legalName.MatchString(name) {
			t.Errorf("name = %q: %s", name, raw)
		}
	})
	t.Run("stream", func(t *testing.T) {
		up := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseEvent2("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
			_, _ = fmt.Fprint(w, sseEvent2("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"`+tckn+`","input":{}}}`))
			_, _ = fmt.Fprint(w, sseEvent2("content_block_stop", `{"type":"content_block_stop","index":0}`))
			_, _ = fmt.Fprint(w, sseEvent2("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`))
			_, _ = fmt.Fprint(w, sseEvent2("message_stop", `{"type":"message_stop"}`))
		}
		for _, action := range []string{guardrail.ActionBlock, guardrail.ActionRedact} {
			e, _ := anthropicEnv(t, "claude-opus-5-5", up)
			e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			_, raw := e.messages(nativeBody(true), nativeHeaders(e))
			if strings.Contains(string(raw), tckn) {
				t.Errorf("%s: the name reached the client: %s", action, raw)
			}
			if _, _, _, stop := nativeParts(t, raw); action == guardrail.ActionBlock && stop != "refusal" {
				t.Errorf("stop %q", stop)
			}
		}
	})
}

// A redacted name is still a name: the tool call stays well formed, the
// request stays acceptable. It no longer matches the tool table of the client,
// which is the price of hiding it.
func TestRedactedNamesStayLegal(t *testing.T) {
	rules := []guardrail.Rule{keywordRule("tok", guardrail.ActionRedact, []string{"token"}, guardrail.StagePreRequest, guardrail.StagePostResponse)}
	t.Run("a tool call in an answer", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
			"function": map[string]any{"name": "refresh-token", "arguments": "{}"}}}}, "tool_calls"))
		e.guardRules(rules...)
		_, out := e.chat(userBody("hi"))
		name := messageOf(out)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		if name != "refresh-REDACTED-tok" || !legalName.MatchString(name) {
			t.Errorf("name = %q", name)
		}
	})
	t.Run("a tool call in a stream", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": "refresh-token", "arguments": "{}"}}}}))
		e.guardRules(rules...)
		s := e.streamChat(userBody("hi"))
		if !strings.Contains(s.all(), `"name":"refresh-REDACTED-tok"`) || strings.Contains(s.all(), "[REDACTED") {
			t.Errorf("%s", s.all())
		}
	})
	t.Run("names in the history of a request", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(rules...)
		resp, out := e.chat(chatMsgs(
			map[string]any{"role": "user", "content": "x", "name": "my-token"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "refresh-token", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"}))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		msgs := seen.last()["messages"].([]any)
		n1 := msgs[1].(map[string]any)["name"].(string)
		n2 := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
		if n1 != "my-REDACTED-tok" || n2 != "refresh-REDACTED-tok" || !legalName.MatchString(n1) || !legalName.MatchString(n2) {
			t.Errorf("names sent upstream = %q, %q", n1, n2)
		}
	})
	t.Run("text next to a name is still the usual placeholder", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(rules...)
		e.chat(chatMsgs(map[string]any{"role": "user", "content": "a token", "name": "my-token"}))
		m := seen.last()["messages"].([]any)[1].(map[string]any)
		if m["content"] != "a [REDACTED:tok]" || m["name"] != "my-REDACTED-tok" {
			t.Errorf("message = %v", m)
		}
	})
	t.Run("block and flag act on a name as they do on text", func(t *testing.T) {
		e := newEnv(t, nil)
		e.guardRules(keywordRule("b", guardrail.ActionBlock, []string{"token"}, guardrail.StagePreRequest))
		if resp, _ := e.chat(chatMsgs(map[string]any{"role": "user", "content": "x", "name": "my-token"})); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("block: status %d", resp.StatusCode)
		}
		e2 := newEnv(t, nil)
		seen := e2.recordUpstream(nil)
		e2.guardRules(keywordRule("f", guardrail.ActionFlag, []string{"token"}, guardrail.StagePreRequest))
		resp, _ := e2.chat(chatMsgs(map[string]any{"role": "user", "content": "x", "name": "my-token"}))
		if m := seen.last()["messages"].([]any)[1].(map[string]any); m["name"] != "my-token" {
			t.Errorf("flag changed the name: %v", m)
		}
		if row := e2.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("the webhook is sent a name with the role of its message", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		e.withHook(2000, "")
		e.chat(chatMsgs(map[string]any{"role": "user", "content": "x", "name": "alice"}))
		var found bool
		for _, x := range h.seen()[0].Body["texts"].([]any) {
			tx := x.(map[string]any)
			if tx["text"] == "alice" && tx["role"] == "user" {
				found = true
			}
		}
		if !found {
			t.Errorf("webhook body = %s", h.seen()[0].Raw)
		}
	})
}

// ------------------------------------------------------------------ shapes

// A server that does not validate takes what the schema forbids. A shape the
// code did not expect is read as text, not passed.
func TestPreRequestReadsShapesTheSchemaForbids(t *testing.T) {
	secret := "id " + tckn
	call := func(args any) map[string]any {
		return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": args}}}}
	}
	bodies := map[string]map[string]any{
		"content as one object":     chatMsgs(map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": secret}}),
		"content as a list of text": chatMsgs(map[string]any{"role": "user", "content": []any{secret}}),
		"arguments as an object":    chatMsgs(call(map[string]any{"id": "x " + tckn})),
		"arguments as a list":       chatMsgs(call([]any{secret})),
		"reasoning_content as an object": chatMsgs(map[string]any{"role": "assistant", "content": "x",
			"reasoning_content": map[string]any{"text": secret}}),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			_, calls := e.chatCalls()
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
			if resp, out := e.chat(body); resp.StatusCode != http.StatusBadRequest || calls() != 0 {
				t.Errorf("%d %v (upstream calls %d)", resp.StatusCode, out, calls())
			}
		})
	}
	t.Run("native content as one block or a list of strings", func(t *testing.T) {
		for name, content := range map[string]any{
			"one block":       map[string]any{"type": "text", "text": secret},
			"a list of texts": []any{secret},
		} {
			e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
			body := map[string]any{"model": "claude-opus-5-5", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": content}}}
			if resp, raw := e.messages(body, nativeHeaders(e)); resp.StatusCode != http.StatusBadRequest || up.hits.Load() != 0 {
				t.Errorf("%s: %d %s", name, resp.StatusCode, raw)
			}
		}
	})
	t.Run("redact keeps the shape it found", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		e.chat(chatMsgs(map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": secret}}))
		c := seen.last()["messages"].([]any)[1].(map[string]any)["content"].(map[string]any)
		if c["text"] != "id [REDACTED:k]" || c["type"] != "text" {
			t.Errorf("content = %v", c)
		}
	})
}

// ----------------------------------------------------------- duplicate keys

// A key that appears twice is read by one parser as the first value and by
// another as the last. The rules read the last, and the arguments go on with
// that one alone.
func TestDuplicateJSONKeysDoNotHideAValue(t *testing.T) {
	t.Run("tool arguments, redact", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		resp, out := e.chat(chatMsgs(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": `{"a":"id ` + tckn + `","a":"x"}`}}}}))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		raw, _ := json.Marshal(seen.last())
		if strings.Contains(string(raw), tckn) {
			t.Errorf("the first value reached the upstream: %s", raw)
		}
		args := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
		if !json.Valid([]byte(args)) {
			t.Errorf("arguments = %s", args)
		}
	})
	t.Run("tool arguments, block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, _ := e.chat(chatMsgs(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": `{"a":"id ` + tckn + `","a":"x"}`}}}}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
	t.Run("native tool input, redact", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		body := `{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"a":"id ` + tckn + `","a":"x"}}]}]}`
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		up.mu.Lock()
		sent := string(up.body[len(up.body)-1]) // the bytes, not a decoded copy that keeps one value
		up.mu.Unlock()
		if strings.Contains(sent, tckn) {
			t.Errorf("the first value reached the upstream: %s", sent)
		}
	})
	// The translated Messages endpoint reads the canonical request from the body
	// the walk left, so a repeated key must not survive in it either.
	t.Run("canonical Messages tool input, redact", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		body := `{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"a":"id ` + tckn + `","a":"x"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}]}`
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		sent, _ := json.Marshal(seen.last())
		if strings.Contains(string(sent), tckn) {
			t.Errorf("the first value reached the upstream: %s", sent)
		}
		// And what went up is the last value, as the native path sends it.
		if !strings.Contains(string(sent), `x`) || !strings.Contains(string(sent), "lookup") {
			t.Errorf("the tool call is gone: %s", sent)
		}
	})
	t.Run("canonical Messages tool input, flag-only project", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRequest))
		body := `{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"a":"id ` + tckn + `","a":"x"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}]}`
		if resp, raw := e.messages(body, nativeHeaders(e)); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		sent, _ := json.Marshal(seen.last())
		if strings.Contains(string(sent), tckn) {
			t.Errorf("a repeated key carried a value the rules did not read upstream: %s", sent)
		}
	})
	t.Run("canonical Messages tool input, block", func(t *testing.T) {
		e := newEnv(t, nil)
		_, calls := e.chatCalls()
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		body := `{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"a":"id ` + tckn + `","a":"x"}}]}]}`
		if resp, _ := e.messages(body, nativeHeaders(e)); resp.StatusCode != http.StatusBadRequest || calls() != 0 {
			t.Errorf("status %d, upstream calls %d", resp.StatusCode, calls())
		}
	})
	t.Run("a document with no duplicates is not rewritten", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		args := `{"b": 1,   "a": "x"}`
		e.chat(chatMsgs(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": args}}}}))
		got := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
		if got != args {
			t.Errorf("arguments = %q, want them untouched", got)
		}
	})
}

// A redact rule whose placeholder a block rule matches stops the conversation
// that carries the redacted answer in its history; the API warns about it (see
// the admin test) and the gateway does what the rules say.
func TestAPlaceholderInTheHistoryIsPlainText(t *testing.T) {
	e := newEnv(t, nil)
	e.guardRules(guardRule("iban", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse),
		keywordRule("w", guardrail.ActionBlock, []string{"iban"}, guardrail.StagePreRequest))
	e.up.setChat(chatReply(map[string]any{"content": "id " + tckn}, "stop"))
	_, out := e.chat(userBody("hi"))
	answer, _ := messageOf(out)["content"].(string)
	if answer != "id [REDACTED:iban]" {
		t.Fatalf("answer = %q", answer)
	}
	resp, _ := e.chat(chatMsgs(map[string]any{"role": "assistant", "content": answer}, map[string]any{"role": "user", "content": "and?"}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d: the placeholder is text the client wrote", resp.StatusCode)
	}
}
