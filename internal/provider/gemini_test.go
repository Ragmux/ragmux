package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// geminiServer captures the request the adapter sends and answers with the
// scripted handler.
func geminiServer(t *testing.T, handler func(w http.ResponseWriter, req geminiRequest)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/models/") {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTranslateGeminiTools(t *testing.T) {
	req := parseReq(t, `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Ankara\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"tool","tool_call_id":"call_gone","content":"nobody asked"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}}]}`)
	g, err := translateGemini(Config{ProviderType: "gemini"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Tools) != 1 || len(g.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v", g.Tools)
	}
	d := g.Tools[0].FunctionDeclarations[0]
	if d.Name != "get_weather" || d.Description != "d" || strings.Contains(string(d.Parameters), "additionalProperties") ||
		!strings.Contains(string(d.Parameters), `"city"`) {
		t.Errorf("declaration = %+v", d)
	}
	// The assistant turn keeps its functionCall, and the tool result becomes a
	// functionResponse under the name recovered from the call id. The result
	// whose call is missing is dropped rather than relayed.
	if len(g.Contents) != 3 || g.Contents[1].Role != "model" || g.Contents[1].Parts[0].FunctionCall == nil ||
		g.Contents[1].Parts[0].FunctionCall.Name != "get_weather" ||
		string(g.Contents[1].Parts[0].FunctionCall.Args) != `{"city":"Ankara"}` {
		t.Fatalf("contents = %+v", g.Contents)
	}
	fr := g.Contents[2].Parts[0].FunctionResponse
	if g.Contents[2].Role != "user" || len(g.Contents[2].Parts) != 1 || fr == nil || fr.Name != "get_weather" ||
		string(fr.Response) != `{"result":"sunny"}` {
		t.Errorf("tool result = %+v", g.Contents[2])
	}
}

func TestTranslateGeminiToolChoice(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}]`
	cases := []struct {
		name    string
		choice  string
		mode    string
		allowed []string
	}{
		{name: "absent"},
		{name: "auto", choice: `"tool_choice":"auto",`, mode: "AUTO"},
		{name: "required", choice: `"tool_choice":"required",`, mode: "ANY"},
		// "none" keeps the declarations: Gemini has an exact equivalent, so
		// unlike the Anthropic path the tools are not removed.
		{name: "none", choice: `"tool_choice":"none",`, mode: "NONE"},
		{name: "named", choice: `"tool_choice":{"type":"function","function":{"name":"a"}},`, mode: "ANY", allowed: []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := translateGemini(Config{}, parseReq(t, `{"messages":[{"role":"user","content":"hi"}],`+tc.choice+tools+`}`))
			if err != nil {
				t.Fatal(err)
			}
			if len(g.Tools) != 1 {
				t.Fatalf("tools = %+v", g.Tools)
			}
			if tc.mode == "" {
				if g.ToolConfig != nil {
					t.Fatalf("toolConfig = %+v", g.ToolConfig)
				}
				return
			}
			if g.ToolConfig == nil || g.ToolConfig.FunctionCallingConfig.Mode != tc.mode {
				t.Fatalf("toolConfig = %+v", g.ToolConfig)
			}
			if strings.Join(g.ToolConfig.FunctionCallingConfig.AllowedFunctionNames, ",") != strings.Join(tc.allowed, ",") {
				t.Errorf("allowed = %v", g.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
			}
		})
	}
}

func TestGeminiChatToolCalls(t *testing.T) {
	srv := geminiServer(t, func(w http.ResponseWriter, _ geminiRequest) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[
			{"text":"looking it up"},
			{"functionCall":{"name":"get_weather","args":{"city":"Ankara"}}},
			{"functionCall":{"name":"get_time","args":{}}}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}`))
	})
	p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, APIKey: "k", Model: "gemini-2.0-flash"})
	resp, err := p.Chat(context.Background(), parseReq(t, `{"model":"client","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ch := resp.Choices[0]
	if *ch.Message.Content != "looking it up" {
		t.Errorf("content = %q", *ch.Message.Content)
	}
	// Gemini reports STOP even when the turn only called tools.
	if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
		t.Errorf("finish = %v", ch.FinishReason)
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(ch.Message.ToolCalls, &calls); err != nil || len(calls) != 2 {
		t.Fatalf("tool_calls = %s (%v)", ch.Message.ToolCalls, err)
	}
	// Gemini sends no call ids, so the gateway generates them.
	if !strings.HasPrefix(calls[0].ID, "call_") || calls[0].ID == calls[1].ID || calls[0].Type != "function" ||
		calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"Ankara"}` ||
		calls[1].Function.Arguments != `{}` {
		t.Errorf("calls = %+v", calls)
	}
}

func TestGeminiFinishReasonToolCalls(t *testing.T) {
	for _, reason := range []string{"STOP", "MAX_TOKENS", ""} {
		srv := geminiServer(t, func(w http.ResponseWriter, _ geminiRequest) {
			_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}}}]},"finishReason":%q}]}`, reason)
		})
		p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, Model: "m"})
		resp, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if f := resp.Choices[0].FinishReason; f == nil || *f != "tool_calls" {
			t.Errorf("%s: finish = %v", reason, f)
		}
	}
}

func TestGeminiStreamToolCalls(t *testing.T) {
	srv := geminiServer(t, func(w http.ResponseWriter, _ geminiRequest) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`data: {"candidates":[{"content":{"parts":[{"text":"one sec"}]}}]}`,
			`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Ankara"}}}]}}]}`,
			`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
		} {
			_, _ = w.Write([]byte(ev + "\n\n"))
		}
	})
	p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, Model: "m"})
	out := make(chan StreamChunk, 16)
	if err := p.ChatStream(context.Background(), parseReq(t, `{"model":"client","messages":[{"role":"user","content":"hi"}]}`), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var text, finish string
	var deltas []string
	var usage *Usage
	for c := range out {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
			if len(ch.Delta.ToolCalls) > 0 {
				deltas = append(deltas, string(ch.Delta.ToolCalls))
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if text != "one sec" || finish != "tool_calls" || usage == nil || usage.TotalTokens != 5 {
		t.Errorf("text=%q finish=%q usage=%+v", text, finish, usage)
	}
	// Each call arrives complete, so each is one whole delta with its own
	// index; no argument fragments are invented.
	if len(deltas) != 2 {
		t.Fatalf("got %d tool deltas: %v", len(deltas), deltas)
	}
	if !strings.Contains(deltas[0], `"index":0`) || !strings.Contains(deltas[0], `"name":"get_weather"`) ||
		!strings.Contains(deltas[0], `{\"city\":\"Ankara\"}`) {
		t.Errorf("first delta = %s", deltas[0])
	}
	if !strings.Contains(deltas[1], `"index":1`) || !strings.Contains(deltas[1], `"name":"get_time"`) {
		t.Errorf("second delta = %s", deltas[1])
	}
}
