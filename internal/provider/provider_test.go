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

func TestChatRequestExtraRoundTrip(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.5,"seed":42,"logprobs":true}`
	var r ChatRequest
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if string(r.Extra["seed"]) != "42" {
		t.Errorf("extra seed = %s", r.Extra["seed"])
	}
	out, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["seed"] != float64(42) || m["logprobs"] != true || m["temperature"] != 0.5 {
		t.Errorf("round trip lost fields: %s", out)
	}
	if _, ok := m["Extra"]; ok {
		t.Errorf("Extra must not be serialised as a field: %s", out)
	}
	plain, _ := json.Marshal(ChatRequest{Model: "m"})
	if strings.Contains(string(plain), "Extra") {
		t.Errorf("empty Extra leaked into wire format: %s", plain)
	}
	if r.Messages[0].Text() != "hi" {
		t.Errorf("text = %q", r.Messages[0].Text())
	}
}

// TestIncludeUsage: the gateway holds the usage-only trailer back unless the
// client asked for it, so "asked for it" has to be read the way OpenAI reads
// it — and anything unreadable has to mean no.
func TestIncludeUsage(t *testing.T) {
	cases := []struct {
		streamOptions string
		want          bool
	}{
		{``, false},
		{`null`, false},
		{`{}`, false},
		{`{"include_usage":true}`, true},
		{`{"include_usage":false}`, false},
		{`{"include_usage":true,"something_else":1}`, true},
		// Neither a wrong type nor a broken object is a yes.
		{`{"include_usage":"yes"}`, false},
		{`{"include_usage":1}`, false},
		{`["include_usage"]`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		r := ChatRequest{StreamOptions: json.RawMessage(c.streamOptions)}
		if got := r.IncludeUsage(); got != c.want {
			t.Errorf("IncludeUsage(%q) = %v, want %v", c.streamOptions, got, c.want)
		}
	}
}

func TestTranslateAnthropic(t *testing.T) {
	var r ChatRequest
	json.Unmarshal([]byte(`{"model":"x","messages":[
		{"role":"system","content":"be terse"},
		{"role":"system","content":"really"},
		{"role":"user","content":"a"},
		{"role":"user","content":[{"type":"text","text":"b"}]},
		{"role":"assistant","content":"c"},
		{"role":"user","content":"d"}],"max_tokens":10,"stop":"END"}`), &r)
	ar, err := translateAnthropic(context.Background(), Config{Model: "claude-x"}, r)
	if err != nil {
		t.Fatal(err)
	}
	// Without a cache_control the system field stays a plain JSON string.
	if string(ar.System) != `"be terse\n\nreally"` || ar.MaxTokens != 10 || ar.Model != "claude-x" {
		t.Errorf("bad translation: %+v", ar)
	}
	if len(ar.Messages) != 3 || len(ar.Messages[0].Content) != 2 || ar.Messages[1].Role != "assistant" {
		t.Errorf("messages not merged: %+v", ar.Messages)
	}
	if len(ar.StopSequences) != 1 || ar.StopSequences[0] != "END" {
		t.Errorf("stop = %v", ar.StopSequences)
	}
}

func TestTranslateGemini(t *testing.T) {
	var r ChatRequest
	json.Unmarshal([]byte(`{"model":"x","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"user","content":"u2"}],"temperature":0.2}`), &r)
	g, err := translateGemini(context.Background(), Config{ProviderType: "gemini"}, r)
	if err != nil {
		t.Fatal(err)
	}
	if g.SystemInstruction == nil || g.SystemInstruction.Parts[0].Text != "s" {
		t.Errorf("system missing")
	}
	if len(g.Contents) != 3 || g.Contents[1].Role != "model" {
		t.Errorf("contents = %+v", g.Contents)
	}
	if g.GenerationConfig["temperature"] != 0.2 {
		t.Errorf("gen config = %v", g.GenerationConfig)
	}
}

func TestSSEParser(t *testing.T) {
	src := "event: a\ndata: 1\n\n: comment\ndata: x\ndata: y\n\ndata: last"
	var got []sseEvent
	readSSE(strings.NewReader(src), func(e sseEvent) bool { got = append(got, e); return true })
	if len(got) != 3 || got[0].Event != "a" || got[0].Data != "1" || got[1].Data != "x\ny" || got[2].Data != "last" {
		t.Errorf("events = %+v", got)
	}
}

func TestOpenAICompatStreamAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var req map[string]any
		json.Unmarshal(body, &req)
		if req["model"] != "target-model" {
			t.Errorf("model not overridden: %v", req["model"])
		}
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"He\"}}]}\n\n"))
			w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"))
			w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		if strings.Contains(string(body), "fail") {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit","code":"rl"}}`))
			return
		}
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	p, _ := New(Config{ProviderType: "custom_openai", BaseURL: srv.URL, APIKey: "k", Model: "target-model"})
	var req ChatRequest
	json.Unmarshal([]byte(`{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`), &req)
	resp, err := p.Chat(context.Background(), req)
	if err != nil || *resp.Choices[0].Message.Content != "Hello" {
		t.Fatalf("chat: %v %+v", err, resp)
	}

	out := make(chan StreamChunk, 10)
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
	if text != "Hello" || usage == nil || usage.TotalTokens != 5 {
		t.Errorf("stream text=%q usage=%+v", text, usage)
	}

	json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"fail"}]}`), &req)
	_, err = p.Chat(context.Background(), req)
	pe, ok := err.(*Error)
	if !ok || pe.Status != 429 || pe.Message != "slow down" || pe.Code != "rl" {
		t.Errorf("error relay: %v", err)
	}
}

func TestAnthropicStreamNormalisation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "ak" || r.URL.Path != "/v1/messages" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			w.Write([]byte(ev + "\n\n"))
		}
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "ak", Model: "claude"})
	var req ChatRequest
	json.Unmarshal([]byte(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`), &req)
	out := make(chan StreamChunk, 20)
	if err := p.ChatStream(context.Background(), req, out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var text, finish string
	var usage *Usage
	for c := range out {
		if c.ID != "msg_1" || c.Model != "client" {
			t.Errorf("chunk id/model: %s %s", c.ID, c.Model)
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if text != "Hi there" || finish != "stop" || usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 2 {
		t.Errorf("text=%q finish=%q usage=%+v", text, finish, usage)
	}
}

// TestAnthropicStreamToolCallBytes pins the exact tool_calls deltas the
// Anthropic adapter emits, down to the key order json.Marshal gives a map.
// OpenAI clients accumulate these deltas by index, so the sequence is a wire
// contract: the shared normaliser must reproduce it byte for byte.
func TestAnthropicStreamToolCallBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":4}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one sec"}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Ankara\"}"}}`,
			`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"get_time"}}`,
			`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			// An argument delta for a block that never opened is ignored.
			`data: {"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(ev + "\n\n"))
		}
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "ak", Model: "claude"})
	var req ChatRequest
	_ = json.Unmarshal([]byte(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`), &req)
	out := make(chan StreamChunk, 32)
	if err := p.ChatStream(context.Background(), req, out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var got []string
	var finish string
	for c := range out {
		for _, ch := range c.Choices {
			if len(ch.Delta.ToolCalls) > 0 {
				got = append(got, string(ch.Delta.ToolCalls))
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	want := []string{
		`[{"function":{"arguments":"","name":"get_weather"},"id":"toolu_1","index":0,"type":"function"}]`,
		`[{"function":{"arguments":"{\"city\":"},"index":0}]`,
		`[{"function":{"arguments":"\"Ankara\"}"},"index":0}]`,
		`[{"function":{"arguments":"","name":"get_time"},"id":"toolu_2","index":1,"type":"function"}]`,
		`[{"function":{"arguments":"{}"},"index":1}]`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tool deltas: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delta %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
}
