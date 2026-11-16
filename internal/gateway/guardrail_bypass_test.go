package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/guardrail"
)

// Ways round a guardrail that review found, each pinned by a test: a field of
// the request or the answer that the scan did not read, or read differently
// from the way the same text was read elsewhere. A rule holds if it holds when
// the client picks the shape.

// ------------------------------------------------------------------ logprobs

// logprobsReply answers with the text and, whatever the request said, the token
// log-probabilities that spell it out.
func logprobsReply(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var toks []map[string]any
		for _, c := range text {
			toks = append(toks, map[string]any{"token": string(c), "logprob": -0.1})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "mock-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop", "logprobs": map[string]any{"content": toks}}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}})
	}
}

func logprobsStream(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rs := []rune(text)
		for i, c := range rs {
			ch := map[string]any{"index": 0, "delta": map[string]any{"content": string(c)},
				"logprobs": map[string]any{"content": []any{map[string]any{"token": string(c), "logprob": -0.1}}}}
			if i == len(rs)-1 {
				ch["finish_reason"] = "stop"
			}
			writeSSE(w, map[string]any{"id": "c", "choices": []any{ch}})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestLogprobsDoNotCarryAGuardedAnswer(t *testing.T) {
	ask := func() map[string]any {
		b := userBody("who am i")
		b["logprobs"], b["top_logprobs"] = true, 2
		return b
	}
	for _, action := range []string{guardrail.ActionRedact, guardrail.ActionBlock} {
		t.Run("JSON "+action, func(t *testing.T) {
			e := newEnv(t, nil)
			seen := e.recordUpstream(logprobsReply("id " + tckn))
			e.guardRules(guardrail.Rule{ID: "k", Type: guardrail.TypeTCKN, Action: action, Stages: []string{guardrail.StagePostResponse}})
			resp, out := e.chat(ask())
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), `"logprobs"`) || strings.Contains(string(raw), "1000") {
				t.Errorf("the answer carries logprobs or a part of the match: %s", raw)
			}
			// The upstream is not asked for them, and the client is told.
			sent := seen.last()
			if _, has := sent["logprobs"]; has {
				t.Errorf("the upstream was asked for logprobs: %v", sent)
			}
			if _, has := sent["top_logprobs"]; has {
				t.Errorf("the upstream was asked for top_logprobs: %v", sent)
			}
			if w := strings.Join(resp.Header.Values("Warning"), "|"); !strings.Contains(w, "logprobs was dropped") {
				t.Errorf("Warning = %q", w)
			}
		})
		t.Run("stream "+action, func(t *testing.T) {
			e := newEnv(t, nil)
			e.up.setChat(logprobsStream("id " + tckn))
			e.guardRules(guardrail.Rule{ID: "k", Type: guardrail.TypeTCKN, Action: action, Stages: []string{guardrail.StagePostResponse}})
			s := e.streamChat(ask())
			if strings.Contains(s.all(), `"logprobs"`) || strings.Contains(s.all(), "1000") {
				t.Errorf("a chunk carries logprobs or a part of the match: %s", s.all())
			}
			if len(s.header.Values("Warning")) == 0 {
				t.Error("a stream is told about it before its first byte: no Warning")
			}
		})
	}
	// A stage of flag rules hides nothing, so there is nothing to protect the
	// probabilities from: they stay, and the client is not told of the rule.
	t.Run("a flag-only project keeps them", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(logprobsReply("id " + tckn))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePostResponse))
		resp, out := e.chat(ask())
		raw, _ := json.Marshal(out)
		if !strings.Contains(string(raw), `"logprobs"`) || seen.last()["logprobs"] != true || len(resp.Header.Values("Warning")) != 0 {
			t.Errorf("logprobs were touched by a rule that only flags: %s", raw)
		}
	})
	t.Run("a project without a post_response rule keeps them", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(logprobsReply("id " + tckn))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, out := e.chat(ask())
		raw, _ := json.Marshal(out)
		if !strings.Contains(string(raw), `"logprobs"`) || seen.last()["logprobs"] != true || len(resp.Header.Values("Warning")) != 0 {
			t.Errorf("logprobs were touched on a project that does not guard the answer: %s", raw)
		}
	})
	t.Run("a request that did not ask is not warned", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "hello"}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		b := userBody("hi")
		b["logprobs"] = false
		if resp, _ := e.chat(b); len(resp.Header.Values("Warning")) != 0 {
			t.Errorf("Warning = %q", resp.Header.Values("Warning"))
		}
	})
}

// -------------------------------------------- native blocks of unnamed types

func nativeUserBody(content ...any) map[string]any {
	return map[string]any{"model": "claude-opus-5-5", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": content}}}
}

// A client picks the type of a block. Whatever type it picks, the text in it is
// read.
func TestPreRequestReadsEveryNativeBlock(t *testing.T) {
	secret := "id " + tckn
	blocks := map[string]any{
		"document, text source": map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": secret}},
		"document, content source": map[string]any{"type": "document", "source": map[string]any{"type": "content",
			"content": []any{map[string]any{"type": "text", "text": secret}}}},
		"document, content source as a string": map[string]any{"type": "document", "source": map[string]any{"type": "content", "content": secret}},
		"document title":                       map[string]any{"type": "document", "title": secret, "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "x"}},
		"document context":                     map[string]any{"type": "document", "context": secret, "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "x"}},
		"search_result": map[string]any{"type": "search_result", "source": "https://a.example", "title": "t",
			"content": []any{map[string]any{"type": "text", "text": secret}}},
		"search_result title": map[string]any{"type": "search_result", "source": "https://a.example", "title": secret,
			"content": []any{map[string]any{"type": "text", "text": "x"}}},
		"tool_result with a document": map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": secret}}}},
		"tool_result with a search_result": map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
			map[string]any{"type": "search_result", "source": "s", "title": "t", "content": []any{map[string]any{"type": "text", "text": secret}}}}},
		"a type that does not exist yet": map[string]any{"type": "frobnicate", "payload": map[string]any{"words": secret}},
		"a text block with a citation": map[string]any{"type": "text", "text": "hi", "citations": []any{
			map[string]any{"type": "char_location", "cited_text": secret, "document_index": 0}}},
		"a number in a tool input": map[string]any{"type": "tool_use", "id": "t1", "name": "lookup", "input": map[string]any{"id": 10000000146}},
	}
	for name, block := range blocks {
		t.Run(name, func(t *testing.T) {
			e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
			resp, raw := e.messages(nativeUserBody(map[string]any{"type": "text", "text": "hi"}, block), nativeHeaders(e))
			_, code, _ := envelopeOf(t, raw)
			if resp.StatusCode != http.StatusBadRequest || code != "guardrail_blocked" || up.hits.Load() != 0 {
				t.Errorf("%d %s (upstream hits %d): the match went through", resp.StatusCode, raw, up.hits.Load())
			}
		})
	}

	t.Run("redact keeps the structure of the block", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		resp, raw := e.messages(nativeUserBody(
			map[string]any{"type": "document", "title": secret, "citations": map[string]any{"enabled": true},
				"source": map[string]any{"type": "text", "media_type": "text/plain", "data": secret}},
			map[string]any{"type": "search_result", "source": "https://a.example/" + tckn, "title": "t",
				"content": []any{map[string]any{"type": "text", "text": secret}}},
			map[string]any{"type": "tool_use", "id": "t1", "name": "lookup", "input": map[string]any{"id": 10000000146, "n": 7}},
		), nativeHeaders(e))
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
		sent := up.lastBody(t)
		msgs := sent["messages"].([]any)[0].(map[string]any)["content"].([]any)
		doc, sr, tu := msgs[0].(map[string]any), msgs[1].(map[string]any), msgs[2].(map[string]any)
		src := doc["source"].(map[string]any)
		if doc["title"] != "id [REDACTED:k]" || src["data"] != "id [REDACTED:k]" || src["type"] != "text" || src["media_type"] != "text/plain" ||
			doc["type"] != "document" || doc["citations"].(map[string]any)["enabled"] != true {
			t.Errorf("document = %v", doc)
		}
		// A search result's source is a label the client chose, and it is read;
		// the "url" of a source object is an address and is not.
		if sr["source"] != "https://a.example/[REDACTED:k]" || sr["content"].([]any)[0].(map[string]any)["text"] != "id [REDACTED:k]" {
			t.Errorf("search_result = %v", sr)
		}
		in := tu["input"].(map[string]any)
		if in["id"] != "[REDACTED:k]" || in["n"] != float64(7) {
			t.Errorf("tool input = %v", in)
		}
	})

	t.Run("binary data and ids are not text", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, raw := e.messages(nativeUserBody(
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": tckn}},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": tckn}},
			map[string]any{"type": "tool_result", "tool_use_id": tckn, "content": "fine"},
			map[string]any{"type": "text", "text": "ok", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
		), nativeHeaders(e))
		if resp.StatusCode != 200 || up.hits.Load() != 1 {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
	})

	t.Run("the system prompt as blocks of any type", func(t *testing.T) {
		e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		b := nativeUserBody(map[string]any{"type": "text", "text": "hi"})
		b["system"] = []any{map[string]any{"type": "text", "text": "x", "citations": []any{map[string]any{"cited_text": secret}}}}
		resp, raw := e.messages(b, nativeHeaders(e))
		if resp.StatusCode != http.StatusBadRequest || up.hits.Load() != 0 {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
	})
}

// ---------------------------------------------------- citations in an answer

func TestPostResponseReadsCitationsAndServerToolResults(t *testing.T) {
	answer := func(action string) *env {
		body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[` +
			`{"type":"text","text":"see the document","citations":[{"type":"char_location","cited_text":"id ` + tckn + `","document_index":0,"document_title":"t","start_char_index":0,"end_char_index":5}]},` +
			`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://a.example","title":"page ` + tckn + `","encrypted_content":"abc","page_age":null}]}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`
		e, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(body))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
		return e
	}
	t.Run("JSON redact", func(t *testing.T) {
		e := answer(guardrail.ActionRedact)
		resp, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if resp.StatusCode != 200 || strings.Contains(string(raw), tckn) {
			t.Fatalf("%d the match is in the answer: %s", resp.StatusCode, raw)
		}
		var out struct {
			Content []map[string]any `json:"content"`
		}
		_ = json.Unmarshal(raw, &out)
		cit := out.Content[0]["citations"].([]any)[0].(map[string]any)
		res := out.Content[1]["content"].([]any)[0].(map[string]any)
		if cit["cited_text"] != "id [REDACTED:k]" || cit["type"] != "char_location" || cit["start_char_index"] != float64(5)-5 || cit["document_index"] != float64(0) {
			t.Errorf("citation = %v", cit)
		}
		if res["title"] != "page [REDACTED:k]" || res["encrypted_content"] != "abc" || res["url"] != "https://a.example" || out.Content[1]["tool_use_id"] != "srvtoolu_1" {
			t.Errorf("search result = %v", res)
		}
	})
	t.Run("JSON block", func(t *testing.T) {
		e := answer(guardrail.ActionBlock)
		_, raw := e.messages(nativeBody(false), nativeHeaders(e))
		if strings.Contains(string(raw), tckn) || !strings.Contains(string(raw), `"stop_reason":"refusal"`) {
			t.Errorf("%s", raw)
		}
	})

	stream := func(events ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseEvent2("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}`))
			for _, ev := range events {
				_, _ = fmt.Fprint(w, ev)
			}
			_, _ = fmt.Fprint(w, sseEvent2("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`))
			_, _ = fmt.Fprint(w, sseEvent2("message_stop", `{"type":"message_stop"}`))
		}
	}
	citation := stream(
		sseEvent2("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent2("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"see it"}}`),
		sseEvent2("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"id `+tckn+`","document_index":0,"document_title":"t","start_char_index":0,"end_char_index":5}}}`),
		sseEvent2("content_block_stop", `{"type":"content_block_stop","index":0}`))
	serverTool := stream(
		sseEvent2("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://a.example","title":"page `+tckn+`","encrypted_content":"abc","page_age":null}]}}`),
		sseEvent2("content_block_stop", `{"type":"content_block_stop","index":0}`))

	for name, up := range map[string]http.HandlerFunc{"citations_delta": citation, "a server tool result in a block start": serverTool} {
		t.Run("stream redact, "+name, func(t *testing.T) {
			e, _ := anthropicEnv(t, "claude-opus-5-5", up)
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
			resp, raw := e.messages(nativeBody(true), nativeHeaders(e))
			if resp.StatusCode != 200 || strings.Contains(string(raw), tckn) || !strings.Contains(string(raw), "[REDACTED:k]") {
				t.Errorf("%d: %s", resp.StatusCode, raw)
			}
			if !strings.Contains(string(raw), `"end_char_index":5`) && name == "citations_delta" {
				t.Errorf("the citation lost its positions: %s", raw)
			}
			if !strings.Contains(string(raw), "encrypted_content") && name != "citations_delta" {
				t.Errorf("the encrypted payload was lost: %s", raw)
			}
		})
		t.Run("stream block, "+name, func(t *testing.T) {
			e, _ := anthropicEnv(t, "claude-opus-5-5", up)
			e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
			_, raw := e.messages(nativeBody(true), nativeHeaders(e))
			_, _, _, stop := nativeParts(t, raw)
			if stop != "refusal" || strings.Contains(string(raw), tckn) {
				t.Errorf("stop %q: %s", stop, raw)
			}
		})
	}
}

func TestPostResponseReadsAnnotations(t *testing.T) {
	annotation := func() []any {
		return []any{map[string]any{"type": "url_citation", "url_citation": map[string]any{
			"start_index": 0, "end_index": 4, "title": "page " + tckn, "url": "https://a.example"}}}
	}
	t.Run("JSON redact", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "see it", "annotations": annotation()}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), tckn) {
			t.Fatalf("the match is in the answer: %s", raw)
		}
		an := messageOf(out)["annotations"].([]any)[0].(map[string]any)["url_citation"].(map[string]any)
		if an["title"] != "page [REDACTED:k]" || an["url"] != "https://a.example" || an["start_index"] != float64(0) || an["end_index"] != float64(4) {
			t.Errorf("annotation = %v", an)
		}
	})
	t.Run("JSON block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(map[string]any{"content": "see it", "annotations": annotation()}, "stop"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), tckn) || finishOf(out) != "content_filter" {
			t.Errorf("finish %q: %s", finishOf(out), raw)
		}
	})
	for _, action := range []string{guardrail.ActionRedact, guardrail.ActionBlock} {
		t.Run("stream "+action, func(t *testing.T) {
			e := newEnv(t, nil)
			e.up.setChat(streamDeltas(map[string]any{"content": "see it"}, map[string]any{"annotations": annotation()}))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			s := e.streamChat(userBody("hi"))
			if strings.Contains(s.all(), tckn) {
				t.Errorf("the match is in the stream: %s", s.all())
			}
			if action == guardrail.ActionBlock && s.finish != "content_filter" {
				t.Errorf("finish %q", s.finish)
			}
		})
	}
}

// ------------------------------------------------------- numbers in arguments

func TestToolArgumentsAreReadAsJSON(t *testing.T) {
	args := `{"id":10000000146,"n":3,"nested":{"ids":[10000000146,5]},"s":"plain"}`
	call := func(args string) map[string]any {
		return map[string]any{"content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": args}}}}
	}
	argsOf := func(out map[string]any) string {
		tc := messageOf(out)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
		return tc["arguments"].(string)
	}

	t.Run("answer, redact: a number match becomes a string and the JSON stays valid", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(call(args), "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		got := argsOf(out)
		var v map[string]any
		if err := json.Unmarshal([]byte(got), &v); err != nil || strings.Contains(got, tckn) {
			t.Fatalf("arguments = %s (%v)", got, err)
		}
		if v["id"] != "[REDACTED:k]" || v["n"] != float64(3) || v["s"] != "plain" ||
			v["nested"].(map[string]any)["ids"].([]any)[0] != "[REDACTED:k]" || v["nested"].(map[string]any)["ids"].([]any)[1] != float64(5) {
			t.Errorf("arguments = %v", v)
		}
	})
	t.Run("answer, block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(call(args), "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		raw, _ := json.Marshal(out)
		if finishOf(out) != "content_filter" || strings.Contains(string(raw), tckn) {
			t.Errorf("%s", raw)
		}
	})
	t.Run("arguments that are not JSON are read as text", func(t *testing.T) {
		e := newEnv(t, nil)
		e.up.setChat(chatReply(call(`{"id": `+tckn), "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		if got := argsOf(out); strings.Contains(got, tckn) {
			t.Errorf("arguments = %s", got)
		}
	})
	t.Run("stream: the match is not written", func(t *testing.T) {
		for _, action := range []string{guardrail.ActionRedact, guardrail.ActionBlock} {
			e := newEnv(t, nil)
			var deltas []map[string]any
			for i, r := range args {
				c := map[string]any{"index": 0, "function": map[string]any{"arguments": string(r)}}
				if i == 0 {
					c = map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(r)}}
				}
				deltas = append(deltas, map[string]any{"tool_calls": []any{c}})
			}
			e.up.setChat(streamDeltas(deltas...))
			e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			s := e.streamChat(userBody("hi"))
			if strings.Contains(s.all(), tckn) || strings.Contains(s.args, "1000000") {
				t.Errorf("%s: the match is in the stream: %q", action, s.args)
			}
		}
	})
	t.Run("native JSON and native stream decide alike", func(t *testing.T) {
		input := `{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"id":10000000146}}`
		body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[` + input + `],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":4}}`
		sse := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseEvent2("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}`))
			_, _ = fmt.Fprint(w, sseEvent2("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`))
			for _, part := range []string{`{"id":`, `1000000`, `0146}`} {
				_, _ = fmt.Fprint(w, sseEvent2("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, part)))
			}
			_, _ = fmt.Fprint(w, sseEvent2("content_block_stop", `{"type":"content_block_stop","index":0}`))
			_, _ = fmt.Fprint(w, sseEvent2("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`))
			_, _ = fmt.Fprint(w, sseEvent2("message_stop", `{"type":"message_stop"}`))
		}
		for _, action := range []string{guardrail.ActionBlock, guardrail.ActionRedact} {
			ej, _ := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(body))
			ej.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			_, rawJSON := ej.messages(nativeBody(false), nativeHeaders(ej))
			es, _ := anthropicEnv(t, "claude-opus-5-5", sse)
			es.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePostResponse))
			_, rawSSE := es.messages(nativeBody(true), nativeHeaders(es))
			if strings.Contains(string(rawJSON), tckn) || strings.Contains(string(rawSSE), tckn) {
				t.Errorf("%s: a match got through\njson: %s\nsse: %s", action, rawJSON, rawSSE)
			}
			if action == guardrail.ActionBlock {
				_, _, _, stop := nativeParts(t, rawSSE)
				if stop != "refusal" || !strings.Contains(string(rawJSON), `"stop_reason":"refusal"`) {
					t.Errorf("block: json %s / stream stop %q", rawJSON, stop)
				}
			}
		}
	})
	t.Run("request: a number in an earlier tool call is read, and the arguments stay JSON", func(t *testing.T) {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
		resp, out := e.chat(map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": args}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"}}})
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
		sent := seen.last()["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
		var v map[string]any
		if err := json.Unmarshal([]byte(sent), &v); err != nil || v["id"] != "[REDACTED:k]" || v["n"] != float64(3) {
			t.Errorf("arguments sent upstream = %s (%v)", sent, err)
		}
	})
	t.Run("request: block", func(t *testing.T) {
		e := newEnv(t, nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		resp, _ := e.chat(map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": args}}}}}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
	t.Run("a number nothing matches is left exactly as it was", func(t *testing.T) {
		e := newEnv(t, nil)
		plain := `{"n":12345,"f":1.50,"big":123456789012345678901234567890}`
		e.up.setChat(chatReply(call(plain), "tool_calls"))
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePostResponse))
		_, out := e.chat(userBody("hi"))
		if got := argsOf(out); got != plain {
			t.Errorf("arguments = %s, want %s", got, plain)
		}
	})
}

// A moderation input that is null is a bad input, not an empty one.
func TestModerationRefusesANullInput(t *testing.T) {
	e := newEnv(t, nil)
	for name, body := range map[string]string{"null entry": `{"input":["a",null]}`, "only null": `{"input":[null]}`, "number entry": `{"input":["a",1]}`} {
		resp := e.post(context.Background(), "/v1/moderations", body, e.key)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if resp := e.post(context.Background(), "/v1/moderations", `{"input":["a",""]}`, e.key); resp.StatusCode != 200 {
		t.Errorf("an empty string is an input: %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
}

// A post_response block that follows a skipped retrieval is still the row's
// stage, and the counter counts it.
func TestPostResponseBlockAfterASkippedRetrieval(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	reg := e.withMetrics()
	e.up.setChat(chatReply(map[string]any{"content": "id " + tckn}, "stop"))
	e.guardRules(guardRule("q", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRetrieval),
		guardRule("a", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse))
	resp, out := e.chat(userBody("tell me " + tckn))
	if resp.StatusCode != 200 || finishOf(out) != "content_filter" {
		t.Fatalf("status %d finish %q", resp.StatusCode, finishOf(out))
	}
	if row := e.guardRowOf(resp); row.Stage != "post_response" || row.SkipCause != "guardrail" {
		t.Errorf("row = %+v", row)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="post_response",action="block"} 1`, e.proj.ID),
		fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="pre_retrieval",action="block"} 1`, e.proj.ID))
}
