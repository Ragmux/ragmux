package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ollamaServer captures the request the adapter sends and answers with the
// scripted handler.
func ollamaServer(t *testing.T, handler func(w http.ResponseWriter, req ollamaRequest, raw map[string]json.RawMessage)) (*httptest.Server, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req ollamaRequest
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		_ = json.Unmarshal(body, &raw)
		handler(w, req, raw)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func parseReq(t *testing.T, s string) ChatRequest {
	t.Helper()
	var r ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOllamaChatJSON(t *testing.T) {
	var seen ollamaRequest
	srv, got := ollamaServer(t, func(w http.ResponseWriter, req ollamaRequest, _ map[string]json.RawMessage) {
		seen = req
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "llama3", "message": map[string]any{"role": "assistant", "content": "Hi there"},
			"done": true, "done_reason": "length", "prompt_eval_count": 12, "eval_count": 3})
	})
	// A pasted /v1 suffix is stripped and the key becomes a bearer header.
	p, err := New(Config{ProviderType: "ollama", BaseURL: srv.URL + "/v1", Model: "llama3", APIKey: "proxy-key"})
	if err != nil {
		t.Fatal(err)
	}
	req := parseReq(t, `{"model":"client-name","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
		"temperature":0.3,"top_p":0.9,"max_tokens":50,"stop":["END"],"keep_alive":"10m","num_ctx":8192,"options":{"seed":7}}`)
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Authorization") != "Bearer proxy-key" {
		t.Errorf("authorization = %q", got.Header.Get("Authorization"))
	}
	if seen.Model != "llama3" || seen.Stream || len(seen.Messages) != 2 || seen.Messages[0].Role != "system" || seen.Messages[1].Content != "hi" {
		t.Errorf("request = %+v", seen)
	}
	if seen.Options["temperature"] != 0.3 || seen.Options["top_p"] != 0.9 || seen.Options["num_predict"] != float64(50) ||
		seen.Options["num_ctx"] != float64(8192) || seen.Options["seed"] != float64(7) {
		t.Errorf("options = %v", seen.Options)
	}
	if stop, _ := seen.Options["stop"].([]any); len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", seen.Options["stop"])
	}
	if string(seen.KeepAlive) != `"10m"` {
		t.Errorf("keep_alive = %s", seen.KeepAlive)
	}
	if resp.Model != "client-name" || resp.Object != "chat.completion" || !strings.HasPrefix(resp.ID, "chatcmpl-") ||
		*resp.Choices[0].Message.Content != "Hi there" || *resp.Choices[0].FinishReason != "length" ||
		resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 15 {
		t.Errorf("response = %+v", resp)
	}
}

func TestOllamaToolCalls(t *testing.T) {
	var seen ollamaRequest
	srv, _ := ollamaServer(t, func(w http.ResponseWriter, req ollamaRequest, _ map[string]json.RawMessage) {
		seen = req
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"function": map[string]any{"name": "get_weather", "arguments": map[string]any{"city": "Ankara"}}}}},
			"done": true, "done_reason": "stop", "prompt_eval_count": 1, "eval_count": 1})
	})
	p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "llama3"})
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Ankara\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"},
		{"role":"user","content":"and now?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Tools pass through unchanged; assistant tool calls become objects; the
	// tool result keeps its name.
	if !strings.Contains(string(seen.Tools), `"name":"get_weather"`) {
		t.Errorf("tools = %s", seen.Tools)
	}
	if len(seen.Messages) != 4 || len(seen.Messages[1].ToolCalls) != 1 || string(seen.Messages[1].ToolCalls[0].Function.Arguments) != `{"city":"Ankara"}` ||
		seen.Messages[2].Role != "tool" || seen.Messages[2].Content != "sunny" || seen.Messages[2].ToolName != "get_weather" {
		t.Errorf("messages = %+v", seen.Messages)
	}
	if *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish = %v", *resp.Choices[0].FinishReason)
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(resp.Choices[0].Message.ToolCalls, &calls); err != nil || len(calls) != 1 {
		t.Fatalf("tool_calls = %s (%v)", resp.Choices[0].Message.ToolCalls, err)
	}
	if !strings.HasPrefix(calls[0].ID, "call_") || calls[0].Type != "function" || calls[0].Function.Name != "get_weather" ||
		calls[0].Function.Arguments != `{"city":"Ankara"}` {
		t.Errorf("call = %+v", calls[0])
	}
}

func TestOllamaStreamNDJSON(t *testing.T) {
	srv, _ := ollamaServer(t, func(w http.ResponseWriter, req ollamaRequest, _ map[string]json.RawMessage) {
		if !req.Stream {
			t.Error("stream not requested")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, line := range []string{
			`{"model":"llama3","message":{"role":"assistant","content":"Hel"},"done":false}`,
			`{"model":"llama3","message":{"role":"assistant","content":"lo"},"done":false}`,
			`{"model":"llama3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":9,"eval_count":2}`,
		} {
			_, _ = fmt.Fprintln(w, line)
		}
	})
	p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "llama3"})
	out := make(chan StreamChunk, 16)
	err := p.ChatStream(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`), out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []StreamChunk
	for c := range out {
		chunks = append(chunks, c)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks: %+v", len(chunks), chunks)
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" || *chunks[0].Choices[0].Delta.Content != "Hel" || chunks[0].Object != "chat.completion.chunk" || chunks[0].Model != "x" {
		t.Errorf("first = %+v", chunks[0])
	}
	if chunks[1].Choices[0].Delta.Role != "" || *chunks[1].Choices[0].Delta.Content != "lo" {
		t.Errorf("second = %+v", chunks[1])
	}
	if chunks[2].Choices[0].FinishReason == nil || *chunks[2].Choices[0].FinishReason != "stop" {
		t.Errorf("finish chunk = %+v", chunks[2])
	}
	if len(chunks[3].Choices) != 0 || chunks[3].Usage == nil || chunks[3].Usage.PromptTokens != 9 || chunks[3].Usage.CompletionTokens != 2 {
		t.Errorf("usage chunk = %+v", chunks[3])
	}
	for _, c := range chunks[:3] {
		if c.ID != chunks[0].ID {
			t.Errorf("chunk ids differ: %s vs %s", c.ID, chunks[0].ID)
		}
	}
}

// Ollama can spread tool calls over several NDJSON lines. Each line used to
// index its calls from zero, so a client accumulating by index merged them
// into one call with concatenated arguments.
func TestOllamaStreamToolCallIndexes(t *testing.T) {
	srv, _ := ollamaServer(t, func(w http.ResponseWriter, _ ollamaRequest, _ map[string]json.RawMessage) {
		for _, line := range []string{
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Ankara"}}}]},"done":false}`,
			`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_time","arguments":{"tz":"UTC"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":6}`,
		} {
			_, _ = fmt.Fprintln(w, line)
		}
	})
	p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "llama3"})
	out := make(chan StreamChunk, 16)
	if err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var deltas []string
	var finish string
	for c := range out {
		for _, ch := range c.Choices {
			if len(ch.Delta.ToolCalls) > 0 {
				deltas = append(deltas, string(ch.Delta.ToolCalls))
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("got %d tool deltas: %v", len(deltas), deltas)
	}
	for i, want := range []string{
		`"index":0`, `"index":1`,
	} {
		if !strings.Contains(deltas[i], want) {
			t.Errorf("delta %d = %s, want %s", i, deltas[i], want)
		}
	}
	if !strings.Contains(deltas[0], `"name":"get_weather"`) || !strings.Contains(deltas[1], `"name":"get_time"`) ||
		!strings.Contains(deltas[1], `{\"tz\":\"UTC\"}`) {
		t.Errorf("deltas = %v", deltas)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
}

func TestOllamaErrors(t *testing.T) {
	t.Run("http error body", func(t *testing.T) {
		srv, _ := ollamaServer(t, func(w http.ResponseWriter, _ ollamaRequest, _ map[string]json.RawMessage) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model 'nope' not found, try pulling it first"}`))
		})
		p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "nope"})
		_, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
		var pe *Error
		if !errors.As(err, &pe) || pe.Status != 404 || !strings.Contains(pe.Message, "model 'nope' not found") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("error inside stream", func(t *testing.T) {
		srv, _ := ollamaServer(t, func(w http.ResponseWriter, _ ollamaRequest, _ map[string]json.RawMessage) {
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"a"},"done":false}`)
			_, _ = fmt.Fprintln(w, `{"error":"out of memory"}`)
		})
		p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "m"})
		out := make(chan StreamChunk, 16)
		err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out)
		var pe *Error
		if !errors.As(err, &pe) || pe.Message != "out of memory" || pe.Status != http.StatusBadGateway {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("truncated stream", func(t *testing.T) {
		srv, _ := ollamaServer(t, func(w http.ResponseWriter, _ ollamaRequest, _ map[string]json.RawMessage) {
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"a"},"done":false}`)
		})
		p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "m"})
		out := make(chan StreamChunk, 16)
		if err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out); err == nil {
			t.Error("expected an error for a stream without a done line")
		}
	})
}

func TestTranslateOllamaImagesAndFormat(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}],
		"response_format":{"type":"json_object"}}`)
	out, err := translateOllama(context.Background(), Config{Model: "llava"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Content != "what is this?" || len(out.Messages[0].Images) != 1 || out.Messages[0].Images[0] != "QUJD" {
		t.Errorf("message = %+v", out.Messages[0])
	}
	if string(out.Format) != `"json"` {
		t.Errorf("format = %s", out.Format)
	}
	if out.Options != nil {
		t.Errorf("options should be omitted when empty: %v", out.Options)
	}
	remote := parseReq(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`)
	if _, err := translateOllama(context.Background(), Config{Model: "m"}, remote); err == nil {
		t.Error("remote image URLs should be rejected without an image fetcher")
	}
	if _, err := translateOllama(context.Background(), Config{Model: "m"}, ChatRequest{}); err == nil {
		t.Error("empty messages should be rejected")
	}
}

// With a fetcher configured the same remote URL is downloaded and inlined,
// because Ollama takes nothing else.
func TestTranslateOllamaInlinesRemoteImages(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGBYTES"))
	}))
	defer srv.Close()
	cfg := Config{Model: "llava", Images: &ImageFetcher{Client: srv.Client()}}
	req := parseReq(t, `{"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"`+srv.URL+`/a.png"}}]}]}`)
	out, err := translateOllama(context.Background(), cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages[0].Images) != 1 || out.Messages[0].Images[0] != base64.StdEncoding.EncodeToString([]byte("PNGBYTES")) {
		t.Errorf("images = %v", out.Messages[0].Images)
	}
	if got != "image/*" {
		t.Errorf("accept = %q", got)
	}
	// The per-request cap bounds the fan-out of one chat request.
	cfg.Images.MaxPerRequest = 1
	two := parseReq(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+srv.URL+`/a.png"}},
		{"type":"image_url","image_url":{"url":"`+srv.URL+`/b.png"}}]}]}`)
	_, err = translateOllama(context.Background(), cfg, two)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 400 || !strings.Contains(pe.Message, "at most 1 remote images") {
		t.Errorf("err = %v", err)
	}
}

func TestNewEmbedderOllamaStripsV1(t *testing.T) {
	e, err := NewEmbedder(Config{ProviderType: "ollama", BaseURL: "http://ollama:11434/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if oe := e.(*ollamaEmbedder); oe.base != "http://ollama:11434" {
		t.Errorf("base = %q", oe.base)
	}
}
