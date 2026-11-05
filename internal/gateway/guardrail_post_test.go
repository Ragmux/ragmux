package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/guardrail"
)

// post_response (ADR-034): the answer as the client reads it, whole and
// streamed, on the three shapes the gateway speaks (chat completions, the
// canonical Messages endpoint and the native one).

// chatReply is an upstream that answers with one fixed message.
func chatReply(msg map[string]any, finish string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		msg["role"] = "assistant"
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "mock-model",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}})
	}
}

// streamDeltas is an upstream that streams the given deltas, the last with
// finish_reason stop, then a usage trailer.
func streamDeltas(deltas ...map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, d := range deltas {
			ch := map[string]any{"index": 0, "delta": d}
			if i == len(deltas)-1 {
				ch["finish_reason"] = "stop"
			}
			writeSSE(w, map[string]any{"id": "c", "choices": []any{ch}})
		}
		writeSSE(w, map[string]any{"id": "c", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 2}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// charDeltas splits text into one content delta per character: the worst case
// for a match that straddles chunks.
func charDeltas(text string) []map[string]any {
	var out []map[string]any
	for _, r := range text {
		out = append(out, map[string]any{"content": string(r)})
	}
	return out
}

// streamed is what a client read from a chat completions stream.
type streamed struct {
	status    int
	header    http.Header
	events    []string
	text      string
	reasoning string
	args      string
	finish    string
	done      bool
}

func (e *env) streamChat(body map[string]any) streamed {
	e.t.Helper()
	body["stream"] = true
	resp := e.post(context.Background(), "/v1/chat/completions", body, e.key)
	defer resp.Body.Close()
	s := streamed{status: resp.StatusCode, header: resp.Header}
	for _, ev := range readSSE(e.t, resp.Body) {
		s.events = append(s.events, ev)
		if ev == "[DONE]" {
			s.done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning_content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != nil {
				s.text += *ch.Delta.Content
			}
			if ch.Delta.Reasoning != nil {
				s.reasoning += *ch.Delta.Reasoning
			}
			for _, tc := range ch.Delta.ToolCalls {
				s.args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				s.finish = *ch.FinishReason
			}
		}
	}
	return s
}

func (s streamed) all() string { return strings.Join(s.events, "\n") }

func messageOf(out map[string]any) map[string]any {
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	ch, _ := choices[0].(map[string]any)
	m, _ := ch["message"].(map[string]any)
	return m
}

func finishOf(out map[string]any) string {
	choices, _ := out["choices"].([]any)
	ch, _ := choices[0].(map[string]any)
	s, _ := ch["finish_reason"].(string)
	return s
}

// -------------------------------------------------------- chat, whole answers

func TestPostResponseChatJSON(t *testing.T) {
	answer := "Your id is " + tckn + " and the iban " + iban + "."
	t.Run("redact", func(t *testing.T) {
		e := newEnv(t, nil)
		reg := e.withMetrics()
		e.up.setChat(chatReply(map[string]any{"content": answer}, "stop"))
		e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse),
			guardRule("hesap", guardrail.TypeIBAN, guardrail.ActionRedact, guardrail.StagePostResponse))
		resp, out := e.chat(userBody("who am i"))
		got, _ := messageOf(out)["content"].(string)
		if resp.StatusCode != 200 || got != "Your id is [REDACTED:kimlik] and the iban [REDACTED:hesap]." {
			t.Fatalf("status %d content %q", resp.StatusCode, got)
		}
		if row := e.guardRowOf(resp); row.Redacted != 2 || row.Stage != "" || strings.Join(row.Rules, ",") != "kimlik,hesap" {
			t.Errorf("row = %+v", row)
		}
		mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="post_response",action="redact"} 2`, e.proj.ID))
	})
	t.Run("block empties the answer", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": answer, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"}}}}, "tool_calls"))
		e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		resp, out := e.chat(userBody("who am i"))
		m := messageOf(out)
		if resp.StatusCode != 200 || m["content"] != "" || m["tool_calls"] != nil || finishOf(out) != "content_filter" ||
			resp.Header.Get("x-ragmux-guardrail") != "blocked" {
			t.Errorf("status %d message %v finish %q header %q", resp.StatusCode, m, finishOf(out), resp.Header.Get("x-ragmux-guardrail"))
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), tckn) {
			t.Errorf("the match reached the client: %s", raw)
		}
		if row := e.guardRowOf(resp); row.Stage != "post_response" || row.Status != 200 {
			t.Errorf("row = %+v", row)
		}
		// The tokens were spent; the usage is the upstream's.
		if rec := e.lastLog(); rec.CompletionTokens != 3 {
			t.Errorf("completion tokens = %d", rec.CompletionTokens)
		}
	})
	t.Run("flag leaves the answer", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": answer}, "stop"))
		e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePostResponse))
		resp, out := e.chat(userBody("who am i"))
		if got, _ := messageOf(out)["content"].(string); got != answer || len(resp.Header.Values("Warning")) != 0 {
			t.Errorf("content %q warnings %v", got, resp.Header.Values("Warning"))
		}
		if row := e.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("tool call arguments", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": `{"id":"` + tckn + `"}`}}}}, "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("who am i"))
		raw, _ := json.Marshal(messageOf(out)["tool_calls"])
		if strings.Contains(string(raw), tckn) || !strings.Contains(string(raw), `[REDACTED:k]`) {
			t.Errorf("tool_calls = %s", raw)
		}
	})
	t.Run("a rule of another stage does not read the answer", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": answer}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		_, out := e.chat(userBody("who am i"))
		if got, _ := messageOf(out)["content"].(string); got != answer {
			t.Errorf("content %q", got)
		}
	})
}

// Thinking is scanned by post_response, but what the model thought is signed
// by its provider: a block rule still blocks, and a redact rule can only flag.
func TestPostResponseThinking(t *testing.T) {
	think := "the id is " + tckn
	msg := func() map[string]any {
		return map[string]any{"content": "fine", "reasoning_content": think}
	}
	t.Run("block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(msg(), "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		resp, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if m := messageOf(out); finishOf(out) != "content_filter" || m["content"] != "" || strings.Contains(string(raw), tckn) {
			t.Errorf("message %v finish %q", m, finishOf(out))
		}
		if row := e.guardRowOf(resp); row.Stage != "post_response" {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("redact counts as a flag and changes nothing", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(msg(), "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		resp, out := e.chat(userBody("hi"))
		var rc string
		_ = json.Unmarshal(mustJSON(t, messageOf(out)["reasoning_content"]), &rc)
		if rc != think || messageOf(out)["content"] != "fine" {
			t.Errorf("reasoning %q: the thinking text must not be edited", rc)
		}
		if row := e.guardRowOf(resp); row.Flags != 1 || row.Redacted != 0 {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("flag", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(msg(), "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePostResponse))
		resp, _ := e.chat(userBody("hi"))
		if row := e.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// -------------------------------------------------------------- chat, streams

func TestPostResponseChatStream(t *testing.T) {
	answer := "Id: " + tckn + " ok"
	t.Run("redact across one-character deltas", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(charDeltas(answer)...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.status != 200 || s.text != "Id: [REDACTED:k] ok" || s.finish != "stop" || !s.done {
			t.Fatalf("status %d text %q finish %q done %v", s.status, s.text, s.finish, s.done)
		}
		if strings.Contains(s.all(), "1000000") {
			t.Errorf("a part of the match was written: %s", s.all())
		}
		// The row is written when the stream ends.
		resp := e.lastLog()
		if resp.StatusCode != 200 {
			t.Errorf("row = %+v", resp)
		}
	})
	t.Run("block writes none of the match and ends the stream", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(charDeltas(strings.Repeat("x", 300) + answer + " and more")...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.status != 200 || s.finish != "content_filter" || !s.done {
			t.Fatalf("status %d finish %q done %v", s.status, s.finish, s.done)
		}
		if strings.Contains(s.all(), "1000") || strings.Contains(s.text, "ok") || strings.Contains(s.text, "more") {
			t.Errorf("text after or at the match reached the client: %q", s.text)
		}
		if !strings.HasPrefix(strings.Repeat("x", 300), s.text) || len(s.text) < 300-200 {
			t.Errorf("the text before the match is a prefix of what was written, minus the held tail: got %d characters", len(s.text))
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var stage *string
			_ = e.st.DB().QueryRow(context.Background(), `SELECT guardrail_blocked_stage FROM request_logs ORDER BY id DESC LIMIT 1`).Scan(&stage)
			if stage != nil && *stage == "post_response" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("the row never learned of the block")
	})
	t.Run("flag", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(charDeltas(answer)...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.text != answer || s.finish != "stop" {
			t.Errorf("text %q finish %q: a flag changes nothing", s.text, s.finish)
		}
	})
	t.Run("tool call arguments", func(t *testing.T) {
		e := newEnv(t, nil)
		var deltas []map[string]any
		args := `{"id":"` + tckn + `"}`
		for i, r := range args {
			call := map[string]any{"index": 0, "function": map[string]any{"arguments": string(r)}}
			if i == 0 {
				call = map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(r)}}
			}
			deltas = append(deltas, map[string]any{"tool_calls": []any{call}})
		}
		e.up.setChat(streamDeltas(deltas...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.args != `{"id":"[REDACTED:k]"}` {
			t.Errorf("arguments = %q", s.args)
		}
	})
	t.Run("thinking: block", func(t *testing.T) {
		e := newEnv(t, nil)
		var deltas []map[string]any
		for _, r := range "the id is " + tckn {
			deltas = append(deltas, map[string]any{"reasoning_content": string(r)})
		}
		deltas = append(deltas, map[string]any{"content": "fine"})
		e.up.setChat(streamDeltas(deltas...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.finish != "content_filter" || strings.Contains(s.all(), "1000") || s.text != "" {
			t.Errorf("finish %q text %q events %s", s.finish, s.text, s.all())
		}
	})
	t.Run("thinking: redact is a flag", func(t *testing.T) {
		e := newEnv(t, nil)
		var deltas []map[string]any
		think := "the id is " + tckn
		for _, r := range think {
			deltas = append(deltas, map[string]any{"reasoning_content": string(r)})
		}
		deltas = append(deltas, map[string]any{"content": "fine"})
		e.up.setChat(streamDeltas(deltas...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		s := e.streamChat(userBody("hi"))
		if s.reasoning != think || s.text != "fine" || s.finish != "stop" {
			t.Errorf("reasoning %q text %q finish %q", s.reasoning, s.text, s.finish)
		}
	})
	t.Run("a stream with the stage's rules matching nothing is the same text", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(streamDeltas(charDeltas("nothing to see here")...))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		if s := e.streamChat(userBody("hi")); s.text != "nothing to see here" || s.finish != "stop" || !s.done {
			t.Errorf("text %q finish %q", s.text, s.finish)
		}
	})
}

// Without a rule for the stage the stream is relayed as it always was: a
// chunk is on the client's side before the upstream's next one is written. A
// window would hold the first chunk back until the end.
func TestNoWindowWithoutAPostResponseRule(t *testing.T) {
	for name, rules := range map[string][]guardrail.Rule{
		"no rules":                  nil,
		"rules of the other stages": {guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest, guardrail.StagePreRetrieval)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			release := make(chan struct{})
			e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hel"}}}})
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
				writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "lo"}, "finish_reason": "stop"}}})
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			})
			if rules != nil {
				e.guardRules(rules...)
			}
			resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userBody("hi")["messages"], "stream": true}, e.key)
			defer resp.Body.Close()
			first := make(chan string, 1)
			go func() {
				buf := make([]byte, 4096)
				var got strings.Builder
				for !strings.Contains(got.String(), "Hel") {
					n, err := resp.Body.Read(buf)
					got.Write(buf[:n])
					if err != nil {
						break
					}
				}
				first <- got.String()
			}()
			select {
			case got := <-first:
				if !strings.Contains(got, `"content":"Hel"`) {
					t.Errorf("first read = %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the first chunk did not reach the client while the upstream was still streaming: a window is holding it")
			}
			close(release)
		})
	}
}

// ------------------------------------------------------------ canonical Messages

func TestPostResponseCanonicalMessages(t *testing.T) {
	post := func(e *env, stream bool) (*http.Response, []byte) {
		return e.messages(map[string]any{"model": "claude-x", "max_tokens": 32, "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, map[string]string{"x-api-key": e.key})
	}
	t.Run("JSON redact", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "id " + tckn}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		resp, raw := post(e, false)
		if resp.StatusCode != 200 || strings.Contains(string(raw), tckn) || !strings.Contains(string(raw), "id [REDACTED:k]") {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
	})
	t.Run("JSON block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "id " + tckn}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		resp, raw := post(e, false)
		var out struct {
			StopReason string `json:"stop_reason"`
			Content    []any  `json:"content"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 || out.StopReason != "refusal" || strings.Contains(string(raw), tckn) {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
		for _, b := range out.Content {
			if m, _ := b.(map[string]any); m["text"] != "" && m["text"] != nil {
				t.Errorf("content = %v", out.Content)
			}
		}
	})
	t.Run("stream redact and block", func(t *testing.T) {
		for action, want := range map[string]string{guardrail.ActionRedact: "id [REDACTED:k] ok", guardrail.ActionBlock: ""} {
			e := newEnv(t, nil)
			e.up.setChat(streamDeltas(charDeltas("id " + tckn + " ok")...))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			resp, raw := post(e, true)
			text, stop := messagesStreamText(t, raw)
			if resp.StatusCode != 200 || text != want || strings.Contains(string(raw), "1000000") {
				t.Errorf("%s: %d text %q: %s", action, resp.StatusCode, text, raw)
			}
			if action == guardrail.ActionBlock && stop != "refusal" {
				t.Errorf("block: stop_reason %q", stop)
			}
			if action == guardrail.ActionRedact && stop != "end_turn" {
				t.Errorf("redact: stop_reason %q", stop)
			}
		}
	})
}

// messagesStreamText reads a Messages SSE body: the text deltas joined and the
// stop reason.
func messagesStreamText(t *testing.T, raw []byte) (text, stop string) {
	t.Helper()
	for _, ev := range strings.Split(string(raw), "\n\n") {
		_, data, ok := strings.Cut(ev, "data: ")
		if !ok {
			continue
		}
		var m struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		switch m.Type {
		case "content_block_delta":
			text += m.Delta.Text
		case "message_delta":
			stop = m.Delta.StopReason
		}
	}
	return text, stop
}

// ---------------------------------------------------------------- native Messages

func sseEvent2(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

// nativeAnswer is a Messages JSON answer: a thinking block, a text block and a
// tool call.
func nativeAnswer(text, thinking, toolID string) string {
	return fmt.Sprintf(`{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[`+
		`{"type":"thinking","thinking":%q,"signature":"sig-1"},{"type":"text","text":%q},`+
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":%q,"n":1}}],"stop_reason":"tool_use","stop_sequence":null,`+
		`"usage":{"input_tokens":10,"output_tokens":4}}`, thinking, text, toolID)
}

// scriptedNativeStream is a Messages stream with a thinking block (thought, then its
// signature) and a text block, the text one character per delta.
func scriptedNativeStream(thought, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		send := func(name, data string) {
			_, _ = fmt.Fprint(w, sseEvent2(name, data))
			fl.Flush()
		}
		send("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}`)
		send("ping", `{"type": "ping"}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		for _, r := range thought {
			send("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, string(r)))
		}
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`)
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		for _, r := range text {
			send("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`, string(r)))
		}
		send("content_block_stop", `{"type":"content_block_stop","index":1}`)
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`)
		send("message_stop", `{"type":"message_stop"}`)
	}
}

// nativeParts reads a Messages SSE body into the text, the thinking, the
// signature and the stop reason.
func nativeParts(t *testing.T, raw []byte) (text, thinking, sig, stop string) {
	t.Helper()
	for _, ev := range strings.Split(string(raw), "\n\n") {
		_, data, ok := strings.Cut(ev, "data: ")
		if !ok {
			continue
		}
		var m struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				Signature  string `json:"signature"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &m) != nil {
			t.Fatalf("event is not JSON: %q", ev)
		}
		switch m.Type {
		case "content_block_delta":
			text += m.Delta.Text
			thinking += m.Delta.Thinking
			sig += m.Delta.Signature
		case "message_delta":
			stop = m.Delta.StopReason
		}
	}
	return text, thinking, sig, stop
}

var nativeHeaders = func(e *env) map[string]string {
	return map[string]string{"x-api-key": e.key, "anthropic-version": "2023-06-01"}
}

func nativeBody(stream bool) map[string]any {
	return map[string]any{"model": "claude-opus-5-5", "max_tokens": 64, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
}

func TestPostResponseNativeJSON(t *testing.T) {
	answer := nativeAnswer("id "+tckn, "I think "+tckn, tckn)
	t.Run("redact", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(answer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		resp, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		var out struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Content) != 3 {
			t.Fatalf("%v: %s", err, raw)
		}
		if out.Content[1]["text"] != "id [REDACTED:k]" {
			t.Errorf("text = %v", out.Content[1]["text"])
		}
		in, _ := json.Marshal(out.Content[2]["input"])
		if strings.Contains(string(in), tckn) || !strings.Contains(string(in), `"n":1`) {
			t.Errorf("tool input = %s", in)
		}
		// The thinking text and its signature are what the provider signed.
		if out.Content[0]["thinking"] != "I think "+tckn || out.Content[0]["signature"] != "sig-1" {
			t.Errorf("thinking block = %v", out.Content[0])
		}
		if row := e.guardRowOf(resp); row.Redacted != 2 || row.Flags != 1 {
			t.Errorf("row = %+v: two redactions (text, tool input) and the thinking flagged", row)
		}
	})
	t.Run("block", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(answer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		resp, raw := e.messages(nativeBody(false), nativeHeaders(e))
		var out struct {
			Content    []any          `json:"content"`
			StopReason string         `json:"stop_reason"`
			Usage      map[string]int `json:"usage"`
			ID         string         `json:"id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 || len(out.Content) != 0 ||
			out.StopReason != "refusal" || out.ID != "msg_01" || out.Usage["output_tokens"] != 4 || strings.Contains(string(raw), tckn) {
			t.Errorf("%d %v %s", resp.StatusCode, err, raw)
		}
		if resp.Header.Get("x-ragmux-guardrail") != "blocked" {
			t.Errorf("header = %q", resp.Header.Get("x-ragmux-guardrail"))
		}
	})
	t.Run("a thinking block alone blocks too", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(nativeAnswer("fine", "I think "+tckn, "x")))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if !strings.Contains(string(raw), `"stop_reason":"refusal"`) || strings.Contains(string(raw), tckn) {
			t.Errorf("%s", raw)
		}
	})
}

func TestPostResponseNativeStream(t *testing.T) {
	thought, text := "I think "+tckn, "id "+tckn+" ok"
	t.Run("redact", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", scriptedNativeStream(thought, text))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		resp, raw := e.messages(nativeBody(true), nativeHeaders(e))
		gotText, gotThinking, sig, stop := nativeParts(t, raw)
		if resp.StatusCode != 200 || gotText != "id [REDACTED:k] ok" || stop != "end_turn" {
			t.Fatalf("%d text %q stop %q: %s", resp.StatusCode, gotText, stop, raw)
		}
		// The thinking text is flagged, never edited, and keeps its signature.
		if gotThinking != thought || sig != "sig-1" {
			t.Errorf("thinking %q signature %q", gotThinking, sig)
		}
		if row := e.guardRowOf(resp); row.Redacted != 1 || row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("block closes the message", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", scriptedNativeStream("calm thoughts", "id "+tckn+" ok and more"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		resp, raw := e.messages(nativeBody(true), nativeHeaders(e))
		gotText, _, _, stop := nativeParts(t, raw)
		s := string(raw)
		if resp.StatusCode != 200 || stop != "refusal" || strings.Contains(s, "1000000") || strings.Contains(gotText, "more") {
			t.Fatalf("%d text %q stop %q: %s", resp.StatusCode, gotText, stop, raw)
		}
		if !strings.HasSuffix(s, sseEvent2("message_stop", `{"type":"message_stop"}`)) || strings.Count(s, "content_block_stop") != 4 {
			t.Errorf("the message is not closed in order: %s", s)
		}
		_ = up
		if row := e.guardRowOf(resp); row.Stage != "post_response" {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("thinking block", func(t *testing.T) {
		e, _ := anthropicEnv(t, "claude-opus-5-5", scriptedNativeStream("the id is "+tckn, "fine"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, raw := e.messages(nativeBody(true), nativeHeaders(e))
		_, _, _, stop := nativeParts(t, raw)
		if stop != "refusal" || strings.Contains(string(raw), "1000000") {
			t.Errorf("stop %q: %s", stop, raw)
		}
	})
}

// With nothing to do the guard is invisible: the same bytes, for the JSON
// answer under a post_response rule that does not match, and for the stream
// under rules that are not post_response ones.
func TestNativeIsByteIdenticalWithAGuardrailOn(t *testing.T) {
	type run struct {
		name   string
		answer http.HandlerFunc
		stream bool
		rules  []guardrail.Rule
	}
	for _, tc := range []run{
		{"JSON, a pre_request rule", jsonAnswer(anthropicAnswer), false, []guardrail.Rule{guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest)}},
		{"JSON, a post_response rule nothing matches", jsonAnswer(anthropicAnswer), false, []guardrail.Rule{guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse)}},
		{"stream, a pre_request rule", scriptedNativeStream("calm", "all well"), true, []guardrail.Rule{guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest, guardrail.StagePreRetrieval)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := anthropicEnv(t, "claude-opus-5-5", tc.answer)
			before, rawBefore := e.messages(nativeBody(tc.stream), nativeHeaders(e))
			e.guardRules(tc.rules...)
			after, rawAfter := e.messages(nativeBody(tc.stream), nativeHeaders(e))
			if before.StatusCode != 200 || after.StatusCode != 200 || string(rawBefore) != string(rawAfter) {
				t.Errorf("the answer changed:\n%d %q\n%d %q", before.StatusCode, rawBefore, after.StatusCode, rawAfter)
			}
			if after.Header.Get("x-ragmux-guardrail") != "" || len(after.Header.Values("Warning")) != len(before.Header.Values("Warning")) {
				t.Errorf("headers: %v", after.Header)
			}
		})
	}
}

// pre_request on the native endpoint: the body is read in the Messages shape
// and the request that goes upstream is the edited one.
func TestPreRequestNativeMessages(t *testing.T) {
	history := func() map[string]any {
		return map[string]any{"model": "claude-opus-5-5", "max_tokens": 64,
			"system": []any{map[string]any{"type": "text", "text": "System id " + tckn}},
			"messages": []any{
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "user id " + tckn}}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "thinking", "thinking": "private " + tckn, "signature": "sig-9"},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "who is " + tckn, "n": 7}}}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "result " + tckn},
					map[string]any{"type": "text", "text": "and plain"}}},
			}}
	}
	t.Run("redact", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		resp, raw := e.messages(history(), nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		sent := up.lastBody(t)
		text := string(mustJSON(t, sent))
		// Every text but the signed thinking block lost the number.
		if strings.Count(text, tckn) != 1 || !strings.Contains(text, "private "+tckn) {
			t.Errorf("the number is in %d places: %s", strings.Count(text, tckn), text)
		}
		mustContain(t, text, "System id [REDACTED:k]", "user id [REDACTED:k]", "who is [REDACTED:k]", `"n":7`, "result [REDACTED:k]", "and plain", `"signature":"sig-9"`)
		if w := resp.Header.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "redacted 4 match(es)") {
			t.Errorf("Warning = %q", w)
		}
		if row := e.guardRowOf(resp); row.Redacted != 4 {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("block is an Anthropic error", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, raw := e.messages(history(), nativeHeaders(e))
		typ, code, _ := envelopeOf(t, raw)
		if resp.StatusCode != 400 || typ != "invalid_request_error" || code != "guardrail_blocked" || up.hits.Load() != 0 {
			t.Errorf("%d %s %s upstream hits %d: %s", resp.StatusCode, typ, code, up.hits.Load(), raw)
		}
		if strings.Contains(string(raw), tckn) {
			t.Errorf("the envelope echoes the match: %s", raw)
		}
	})
	t.Run("a thinking block alone is not read", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		body := map[string]any{"model": "claude-opus-5-5", "max_tokens": 64, "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "id " + tckn, "signature": "s"},
				map[string]any{"type": "text", "text": "hello"}}},
			map[string]any{"role": "user", "content": "go on"}}}
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != 200 || up.hits.Load() != 1 {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
	})
	t.Run("the canonical endpoint reads the same body", func(t *testing.T) {
		e := newEnv(t, nil)
		_, calls := e.chatCalls()
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, raw := e.messages(history(), nativeHeaders(e))
		_, code, _ := envelopeOf(t, raw)
		if resp.StatusCode != 400 || code != "guardrail_blocked" || calls() != 0 {
			t.Errorf("%d %s: %s", resp.StatusCode, code, raw)
		}
	})
	t.Run("redact on the canonical endpoint translates the edited body", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		body := map[string]any{"model": "claude-x", "max_tokens": 64, "system": "System id " + tckn,
			"messages": []any{map[string]any{"role": "user", "content": "user id " + tckn}}}
		resp, raw := e.messages(body, nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		sent := string(mustJSON(t, seen.last()))
		if strings.Contains(sent, tckn) || !strings.Contains(sent, "System id [REDACTED:k]") || !strings.Contains(sent, "user id [REDACTED:k]") {
			t.Errorf("upstream body: %s", sent)
		}
	})
}

// The project's rules and the request's own data: whatever the guard saw is
// not kept. The request rows, the log and the metrics hold counts and rule
// identifiers, never a match, a hash or a position (PRD rule 10).
func TestNoMatchedContentIsStored(t *testing.T) {
	e := newEnv(t, nil)
	logs := e.captureLog()
	reg := e.withMetrics()
	h := e.newHook()
	h.set(answer(`{"action":"flag","reason":"seen"}`))
	e.guard(guardrail.Config{
		Rules: []guardrail.Rule{
			guardRule("redactor", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest, guardrail.StagePreRetrieval),
			guardRule("flagger", guardrail.TypeIBAN, guardrail.ActionFlag, guardrail.StagePreRequest),
			guardRule("answer", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse),
		},
		Webhook: &guardrail.Webhook{Enabled: true, TimeoutMS: 2000},
	})
	e.up.setChat(chatReply(map[string]any{"content": "your id is " + tckn}, "stop"))

	for i := 0; i < 3; i++ {
		resp, _ := e.chat(userBody("id " + tckn + " iban " + iban))
		_ = e.guardRowOf(resp)
	}
	// A streamed answer with a match in it, a blocked request and a moderation call.
	e.up.setChat(streamDeltas(charDeltas("your id is " + tckn)...))
	streamedRow := e.streamChat(userBody("id " + tckn + " iban " + iban))
	if strings.Contains(streamedRow.all(), tckn) {
		t.Errorf("the stream carries the match: %s", streamedRow.all())
	}
	e.guardRules(guardRule("blocker", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
	resp, _ := e.chat(userBody("id " + tckn))
	_ = e.guardRowOf(resp)
	e.post(context.Background(), "/v1/moderations", map[string]any{"input": "id " + tckn}, e.key).Body.Close()

	var rows []string
	r, err := e.st.DB().Query(context.Background(), `SELECT row_to_json(r)::text FROM request_logs r`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, s)
	}
	r.Close()
	if len(rows) < 4 {
		t.Fatalf("%d rows", len(rows))
	}
	var project string
	if err := e.st.DB().QueryRow(context.Background(), `SELECT row_to_json(p)::text FROM projects p WHERE id = $1`, e.proj.ID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	iban0 := strings.ReplaceAll(iban, " ", "")
	places := map[string]string{"request_logs": strings.Join(rows, "\n"), "log": logs.String(), "metrics": reg.Text()}
	for name, text := range places {
		for _, secret := range []string{tckn, iban, iban0, iban0[4:]} {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds a matched value %q", name, secret)
			}
		}
	}
}
