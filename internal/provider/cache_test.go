package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// drain collects a stream's text and its final usage block.
func drain(t *testing.T, p Provider, req ChatRequest) (string, *Usage) {
	t.Helper()
	out := make(chan StreamChunk, 32)
	if err := p.ChatStream(context.Background(), req, out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var text string
	var usage *Usage
	for c := range out {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	return text, usage
}

func userRequest(t *testing.T, body string) ChatRequest {
	t.Helper()
	var req ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// TestAnthropicUsageCacheTokens covers the accounting shift: Anthropic's
// input_tokens excludes both cache counters, so the adapter adds them in to
// keep prompt_tokens comparable with OpenAI's and prompt + completion equal
// to total. It is checked on the JSON reply, on message_start and on a
// message_delta that repeats the cache fields.
func TestAnthropicUsageCacheTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":200}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				// A newer API version repeats the cache counters here; the
				// output count is the only thing that is new.
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cache_creation_input_tokens":40,"cache_read_input_tokens":200}}`,
				`data: {"type":"message_stop"}`,
			} {
				_, _ = w.Write([]byte(ev + "\n\n"))
			}
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":200,"output_tokens":5}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "ak", Model: "claude"})
	req := userRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	check := func(what string, u *Usage) {
		t.Helper()
		if u == nil {
			t.Fatalf("%s: no usage", what)
		}
		if u.PromptTokens != 250 {
			t.Errorf("%s: prompt tokens = %d, want 10+40+200", what, u.PromptTokens)
		}
		if u.CachedTokens() != 200 || u.CacheWriteTokens() != 40 {
			t.Errorf("%s: cached = %d write = %d, want 200/40", what, u.CachedTokens(), u.CacheWriteTokens())
		}
		if u.TotalTokens != u.PromptTokens+u.CompletionTokens {
			t.Errorf("%s: total %d != prompt %d + completion %d", what, u.TotalTokens, u.PromptTokens, u.CompletionTokens)
		}
	}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	check("chat", resp.Usage)

	req.Stream = true
	_, usage := drain(t, p, req)
	check("stream", usage)
}

// TestAnthropicUsageCacheDeltaKeepsStartValues guards the merge rule: an
// omitted cache counter in message_delta must not erase message_start's.
func TestAnthropicUsageCacheDeltaKeepsStartValues(t *testing.T) {
	var u anthropicUsageBlock
	u.merge(anthropicUsageBlock{InputTokens: 10, CacheReadInputTokens: 200})
	u.merge(anthropicUsageBlock{OutputTokens: 5})
	got := u.usage()
	if got.PromptTokens != 210 || got.CachedTokens() != 200 || got.CompletionTokens != 5 {
		t.Errorf("merged usage = %+v (cached %d)", got, got.CachedTokens())
	}
}

// TestAnthropicCacheControlPlacements checks the three places a breakpoint
// can ride along: content parts, tools and the system block array.
func TestAnthropicCacheControlPlacements(t *testing.T) {
	req := userRequest(t, `{"model":"m","messages":[
		{"role":"system","content":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"big doc","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"type":"function","cache_control":{"type":"ephemeral"},"function":{"name":"t1","parameters":{"type":"object"}}},
		         {"type":"function","function":{"name":"t2","cache_control":{"type":"persistent"}}}]}`)
	ar, err := translateAnthropic(req, "claude")
	if err != nil {
		t.Fatal(err)
	}
	// System becomes a block array carrying the marker.
	var sysBlocks []anthropicContent
	if err := json.Unmarshal(ar.System, &sysBlocks); err != nil {
		t.Fatalf("system is not a block array: %s", ar.System)
	}
	if len(sysBlocks) != 1 || sysBlocks[0].Text != "rules" || string(sysBlocks[0].CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("system blocks = %+v", sysBlocks)
	}
	// The marker stays on the content part it was written on.
	parts := ar.Messages[0].Content
	if len(parts) != 2 || len(parts[0].CacheControl) != 0 || string(parts[1].CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("content parts = %+v", parts)
	}
	// Tools accept it at the top level or inside function.
	if len(ar.Tools) != 2 || string(ar.Tools[0].CacheControl) != `{"type":"ephemeral"}` ||
		string(ar.Tools[1].CacheControl) != `{"type":"persistent"}` {
		t.Errorf("tools = %+v", ar.Tools)
	}
}

// TestAnthropicSystemShapeUnchangedWithoutCacheControl pins the one wire
// change on a working path: with no breakpoint anywhere the system field is
// still the plain joined string, byte for byte.
func TestAnthropicSystemShapeUnchangedWithoutCacheControl(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"system","content":"be terse"},{"role":"developer","content":"really"},{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"be terse"}]},{"role":"developer","content":"really"},{"role":"user","content":"hi"}]}`,
	} {
		ar, err := translateAnthropic(userRequest(t, body), "claude")
		if err != nil {
			t.Fatal(err)
		}
		if string(ar.System) != `"be terse\n\nreally"` {
			t.Errorf("system = %s, want the joined JSON string", ar.System)
		}
		raw, err := json.Marshal(ar)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"system":"be terse\n\nreally"`) {
			t.Errorf("wire shape changed: %s", raw)
		}
		if strings.Contains(string(raw), "cache_control") {
			t.Errorf("cache_control leaked into a request that has none: %s", raw)
		}
	}
}

// TestOpenAICachedTokensPassthrough: OpenAI reports its automatic caching in
// prompt_tokens_details, which unmarshals straight into Usage.
func TestOpenAICachedTokensPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		if strings.Contains(body, "cache_control") {
			// The gateway sends nothing of its own; a client's marker rides
			// along inside the raw content, which OpenAI ignores.
			if !strings.Contains(body, `"cache_control":{"type":"ephemeral"}`) {
				t.Errorf("client cache_control was rewritten: %s", body)
			}
		}
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"id":"c","choices":[],"usage":{"prompt_tokens":900,"completion_tokens":10,"total_tokens":910,"prompt_tokens_details":{"cached_tokens":768}}}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":900,"completion_tokens":10,"total_tokens":910,
			         "prompt_tokens_details":{"cached_tokens":768},"completion_tokens_details":{"reasoning_tokens":4}}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "openai", BaseURL: srv.URL, APIKey: "k", Model: "gpt-4o"})
	req := userRequest(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)

	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.CachedTokens() != 768 || u.PromptTokens != 900 || u.CacheWriteTokens() != 0 {
		t.Errorf("chat usage = %+v (cached %d)", u, u.CachedTokens())
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 4 {
		t.Errorf("reasoning tokens missing: %+v", u.CompletionTokensDetails)
	}

	req.Stream = true
	if _, usage := drain(t, p, req); usage.CachedTokens() != 768 {
		t.Errorf("stream usage = %+v", usage)
	}
}

// TestDeepSeekCacheHitNormalises: DeepSeek reports the split at the top
// level; normalize folds it into the OpenAI details object.
func TestDeepSeekCacheHitNormalises(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No total_tokens either: normalize computes it.
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_cache_hit_tokens":896,"prompt_cache_miss_tokens":104}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "deepseek", BaseURL: srv.URL, APIKey: "k", Model: "deepseek-chat"})
	resp, err := p.Chat(context.Background(), userRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.CachedTokens() != 896 {
		t.Errorf("cached tokens = %d, want 896", u.CachedTokens())
	}
	if u.PromptTokens != 1000 || u.TotalTokens != 1020 {
		t.Errorf("usage = %+v", u)
	}
}

// TestGeminiCachedContentTokens: promptTokenCount already includes the
// cached prefix, so only the breakdown changes; thoughts stay folded into
// the completion count and are reported again as reasoning tokens.
func TestGeminiCachedContentTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":1200,"cachedContentTokenCount":1000,"candidatesTokenCount":20,
			                 "thoughtsTokenCount":30,"totalTokenCount":1250}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, APIKey: "k", Model: "gemini-2.5-flash"})
	resp, err := p.Chat(context.Background(), userRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.PromptTokens != 1200 || u.CachedTokens() != 1000 {
		t.Errorf("prompt = %d cached = %d, want 1200/1000", u.PromptTokens, u.CachedTokens())
	}
	if u.CompletionTokens != 50 || u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 30 {
		t.Errorf("completion = %d details = %+v", u.CompletionTokens, u.CompletionTokensDetails)
	}
	if u.TotalTokens != 1250 || u.CacheWriteTokens() != 0 {
		t.Errorf("usage = %+v", u)
	}
}

// TestUsageAccessorsAreNilSafe: the gateway reads these off an optional
// usage block on paths where the upstream sent none.
func TestUsageAccessorsAreNilSafe(t *testing.T) {
	var u *Usage
	u.normalize()
	if u.CachedTokens() != 0 || u.CacheWriteTokens() != 0 {
		t.Error("nil usage reported tokens")
	}
	empty := &Usage{PromptTokens: 3}
	if empty.CachedTokens() != 0 || empty.CacheWriteTokens() != 0 {
		t.Error("usage without details reported tokens")
	}
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
