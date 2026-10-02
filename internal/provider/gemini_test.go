package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
	g, err := translateGemini(context.Background(), Config{ProviderType: "gemini"}, req)
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
			g, err := translateGemini(context.Background(), Config{}, parseReq(t, `{"messages":[{"role":"user","content":"hi"}],`+tc.choice+tools+`}`))
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

func TestTranslateGeminiImages(t *testing.T) {
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("JPEG"))
	}))
	defer img.Close()
	content := func(url string) ChatRequest {
		return parseReq(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+url+`"}}]}]}`)
	}
	// fileData only takes what Gemini can resolve itself: a Files API object
	// or a Cloud Storage path. Anything else used to be sent here too and was
	// rejected upstream.
	for _, uri := range []string{geminiFilesPrefix + "abc123", "gs://bucket/a.png"} {
		g, err := translateGemini(context.Background(), Config{}, content(uri))
		if err != nil {
			t.Fatal(err)
		}
		if fd := g.Contents[0].Parts[0].FileData; fd == nil || fd.FileURI != uri {
			t.Errorf("%s: parts = %+v", uri, g.Contents[0].Parts)
		}
	}
	// Every other URL is fetched and inlined.
	g, err := translateGemini(context.Background(), Config{Images: &ImageFetcher{Client: img.Client()}}, content(img.URL+"/a.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if in := g.Contents[0].Parts[0].InlineData; in == nil || in.MimeType != "image/jpeg" ||
		in.Data != base64.StdEncoding.EncodeToString([]byte("JPEG")) {
		t.Errorf("parts = %+v", g.Contents[0].Parts)
	}
	// With fetching disabled there is nothing honest left to send.
	_, err = translateGemini(context.Background(), Config{}, content(img.URL+"/a.jpg"))
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Errorf("err = %v", err)
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

// geminiFixtureSignature is the thoughtSignature on the first functionCall in
// testdata/gemini_thought_signature.{json,sse}.
const geminiFixtureSignature = "CiQB0e2Kb7xQm1v3fJ0yR8nA4sL2pT6uW9eZ5cH3kD1gV7oN0qYSWgHR7YpuX2q9eL4mC8vB1nK6sT3wJ0aF5dG7hE2iU9oP4rQ1yZ8xC3vN6bM0lK2jH5gF8dS1aW4eR7tY0uI3oP6=="

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// geminiTwoTurnServer answers the first request with first and records every
// request after it, which is the turn that has to carry the signature back.
func geminiTwoTurnServer(t *testing.T, stream bool, first []byte) (*httptest.Server, func() []geminiRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []geminiRequest
	srv := geminiServer(t, func(w http.ResponseWriter, req geminiRequest) {
		mu.Lock()
		seen = append(seen, req)
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			_, _ = w.Write(first)
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"sunny, 14:00"}]},"finishReason":"STOP"}]}`))
	})
	return srv, func() []geminiRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]geminiRequest(nil), seen...)
	}
}

// relayedCall is a tool call as an OpenAI client holds it, extension included.
type relayedCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	Signature string `json:"ragmux_signature,omitempty"`
}

// secondTurn is what a client sends after running the calls: the assistant
// message exactly as it came back, then one result per call.
func secondTurn(t *testing.T, assistantToolCalls json.RawMessage, calls []relayedCall) ChatRequest {
	t.Helper()
	msgs := []map[string]any{
		{"role": "user", "content": "weather and time in Ankara?"},
		{"role": "assistant", "content": "", "tool_calls": assistantToolCalls},
	}
	for _, c := range calls {
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": "ok:" + c.Function.Name})
	}
	b, err := json.Marshal(map[string]any{"model": "client", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return parseReq(t, string(b))
}

// assertSignedFunctionCalls checks the model turn of the follow-up request:
// the signed call carries its signature on its own functionCall part, and
// the unsigned one carries none.
func assertSignedFunctionCalls(t *testing.T, req geminiRequest) {
	t.Helper()
	var model *geminiContent
	for i := range req.Contents {
		if req.Contents[i].Role == "model" {
			model = &req.Contents[i]
		}
	}
	if model == nil || len(model.Parts) != 2 {
		t.Fatalf("model turn = %+v", req.Contents)
	}
	p0, p1 := model.Parts[0], model.Parts[1]
	if p0.FunctionCall == nil || p0.FunctionCall.Name != "get_weather" || p0.ThoughtSignature != geminiFixtureSignature {
		t.Errorf("first part = %+v", p0)
	}
	if p1.FunctionCall == nil || p1.FunctionCall.Name != "get_time" || p1.ThoughtSignature != "" {
		t.Errorf("second part = %+v", p1)
	}
}

func TestGeminiThoughtSignatureRoundTrip(t *testing.T) {
	// The split fixture carries the signature on a part of its own, which
	// must still reach the turn's first call.
	for _, fixture := range []string{"gemini_thought_signature.json", "gemini_thought_signature_split.json"} {
		t.Run(fixture, func(t *testing.T) {
			srv, seen := geminiTwoTurnServer(t, false, readFixture(t, fixture))
			p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, Model: "gemini-3-pro-preview"})
			resp, err := p.Chat(context.Background(), parseReq(t, `{"model":"client","messages":[{"role":"user","content":"weather and time in Ankara?"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			toolCalls := resp.Choices[0].Message.ToolCalls
			var calls []relayedCall
			if err := json.Unmarshal(toolCalls, &calls); err != nil || len(calls) != 2 {
				t.Fatalf("tool_calls = %s (%v)", toolCalls, err)
			}
			if calls[0].Signature != geminiFixtureSignature || calls[1].Signature != "" {
				t.Errorf("signatures = %q, %q", calls[0].Signature, calls[1].Signature)
			}
			// The extension sits beside the standard fields, which keep their shape.
			// Arguments are Gemini's args object relayed as-is, so compare as JSON:
			// the fixture is pretty-printed.
			var args map[string]string
			if calls[0].Type != "function" || calls[0].Function.Name != "get_weather" ||
				json.Unmarshal([]byte(calls[0].Function.Arguments), &args) != nil || args["city"] != "Ankara" {
				t.Errorf("call = %+v", calls[0])
			}
			if strings.Count(string(toolCalls), toolCallSignatureField) != 1 {
				t.Errorf("unsigned call carries the field: %s", toolCalls)
			}

			if _, err := p.Chat(context.Background(), secondTurn(t, toolCalls, calls)); err != nil {
				t.Fatal(err)
			}
			reqs := seen()
			if len(reqs) != 2 {
				t.Fatalf("got %d requests", len(reqs))
			}
			assertSignedFunctionCalls(t, reqs[1])
		})
	}
}

func TestGeminiStreamThoughtSignatureRoundTrip(t *testing.T) {
	// The split fixture carries the signature on a part of its own, which
	// must still reach the turn's first call.
	for _, fixture := range []string{"gemini_thought_signature.sse", "gemini_thought_signature_split.sse"} {
		t.Run(fixture, func(t *testing.T) {
			srv, seen := geminiTwoTurnServer(t, true, readFixture(t, fixture))
			p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, Model: "gemini-3-pro-preview"})
			out := make(chan StreamChunk, 16)
			if err := p.ChatStream(context.Background(), parseReq(t, `{"model":"client","messages":[{"role":"user","content":"weather and time in Ankara?"}]}`), out); err != nil {
				t.Fatal(err)
			}
			close(out)
			// Accumulate the deltas the way an OpenAI client does: by index, with
			// every field of a whole call arriving in its one delta.
			var calls []relayedCall
			for c := range out {
				for _, ch := range c.Choices {
					if len(ch.Delta.ToolCalls) == 0 {
						continue
					}
					var deltas []struct {
						Index int `json:"index"`
						relayedCall
					}
					if err := json.Unmarshal(ch.Delta.ToolCalls, &deltas); err != nil {
						t.Fatal(err)
					}
					for _, d := range deltas {
						for len(calls) <= d.Index {
							calls = append(calls, relayedCall{})
						}
						calls[d.Index] = d.relayedCall
					}
				}
			}
			if len(calls) != 2 || calls[0].Signature != geminiFixtureSignature || calls[1].Signature != "" {
				t.Fatalf("calls = %+v", calls)
			}
			toolCalls, _ := json.Marshal(calls)

			if _, err := p.Chat(context.Background(), secondTurn(t, toolCalls, calls)); err != nil {
				t.Fatal(err)
			}
			reqs := seen()
			if len(reqs) != 2 {
				t.Fatalf("got %d requests", len(reqs))
			}
			assertSignedFunctionCalls(t, reqs[1])
		})
	}
}

// signedHistory is a follow-up turn whose assistant tool call carries a
// Gemini signature, as it would after a fallback or a model switch.
const signedHistory = `{"model":"client","messages":[
	{"role":"user","content":"weather?"},
	{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Ankara\"}"},"ragmux_signature":"c2lnbmF0dXJl"}]},
	{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`

func TestGeminiSignatureNotSentToOpenAI(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "openai", BaseURL: srv.URL, APIKey: "k", Model: "gpt-x"})
	req := parseReq(t, signedHistory)
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	out := make(chan StreamChunk, 8)
	if err := p.ChatStream(context.Background(), req, out); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests", len(bodies))
	}
	for i, b := range bodies {
		if strings.Contains(b, toolCallSignatureField) || strings.Contains(b, "c2lnbmF0dXJl") {
			t.Errorf("request %d leaked the signature: %s", i, b)
		}
		var sent struct {
			Messages []struct {
				ToolCalls []relayedCall `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(b), &sent); err != nil || len(sent.Messages) != 3 || len(sent.Messages[1].ToolCalls) != 1 {
			t.Fatalf("request %d = %s (%v)", i, b, err)
		}
		if c := sent.Messages[1].ToolCalls[0]; c.ID != "call_1" || c.Type != "function" || c.Function.Name != "get_weather" ||
			c.Function.Arguments != `{"city":"Ankara"}` {
			t.Errorf("request %d call = %+v", i, c)
		}
	}
	// The caller's request is untouched: a fallback to a Gemini connection
	// still has the signature to send.
	if !strings.Contains(string(req.Messages[1].ToolCalls), "c2lnbmF0dXJl") {
		t.Errorf("caller's messages were rewritten: %s", req.Messages[1].ToolCalls)
	}
	g, err := translateGemini(context.Background(), Config{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if g.Contents[1].Parts[0].ThoughtSignature != "c2lnbmF0dXJl" {
		t.Errorf("gemini part = %+v", g.Contents[1].Parts[0])
	}
}

func TestGeminiSignatureNotSentToAnthropicOrOllama(t *testing.T) {
	req := parseReq(t, signedHistory)
	ar, err := translateAnthropic(context.Background(), Config{Model: "claude-x"}, req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ar)
	if strings.Contains(string(b), toolCallSignatureField) || strings.Contains(string(b), "c2lnbmF0dXJl") {
		t.Errorf("anthropic request leaked the signature: %s", b)
	}
	if !strings.Contains(string(b), `"tool_use"`) {
		t.Errorf("anthropic request lost the call: %s", b)
	}
	or, err := translateOllama(context.Background(), Config{Model: "llama"}, req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(or)
	if strings.Contains(string(b), toolCallSignatureField) || strings.Contains(string(b), "c2lnbmF0dXJl") {
		t.Errorf("ollama request leaked the signature: %s", b)
	}
}

func TestStripToolCallSignatures(t *testing.T) {
	plain := parseReq(t, `{"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	// Nothing to strip: the very same slice comes back, bytes untouched.
	if got := stripToolCallSignatures(plain.Messages); &got[0] != &plain.Messages[0] {
		t.Error("unsigned history was copied")
	}
	// A key spelled with a JSON escape is the same key to every decoder.
	escaped := parseReq(t, `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","ragmux\u005fsignature":"s","function":{"name":"f","arguments":"{}"}}]}]}`)
	if got := stripToolCallSignatures(escaped.Messages); strings.Contains(string(got[0].ToolCalls), `"s"`) {
		t.Errorf("escaped key survived: %s", got[0].ToolCalls)
	}
	// Malformed tool_calls pass through for the upstream to judge.
	bad := []Message{{Role: "assistant", ToolCalls: json.RawMessage(`"oops"`)}}
	if got := stripToolCallSignatures(bad); string(got[0].ToolCalls) != `"oops"` {
		t.Errorf("malformed = %s", got[0].ToolCalls)
	}
}

func TestTranslateGeminiResponseFormat(t *testing.T) {
	cases := []struct {
		name       string
		format     string
		wantSchema bool
		check      func(t *testing.T, schema string)
	}{
		{name: "json_object", format: `{"type":"json_object"}`},
		{name: "text", format: `{"type":"text"}`},
		{name: "json_schema", format: `{"type":"json_schema","json_schema":{"name":"weather","strict":true,"schema":{
			"type":"object","additionalProperties":false,"$defs":{"Unit":{"type":"string","enum":["c","f"]}},
			"properties":{"city":{"type":"string"},"unit":{"$ref":"#/$defs/Unit"}},"required":["city","unit"]}}}`,
			wantSchema: true,
			check: func(t *testing.T, schema string) {
				for _, gone := range []string{"additionalProperties", "$defs", "$ref", "strict"} {
					if strings.Contains(schema, gone) {
						t.Errorf("%s survived: %s", gone, schema)
					}
				}
				for _, kept := range []string{`"city"`, `"enum":["c","f"]`, `"required":["city","unit"]`} {
					if !strings.Contains(schema, kept) {
						t.Errorf("%s missing: %s", kept, schema)
					}
				}
			}},
		// A response may be rooted at an array; that is a real constraint.
		{name: "array root", format: `{"type":"json_schema","json_schema":{"name":"list","schema":{"type":"array","items":{"type":"string"}}}}`,
			wantSchema: true,
			check: func(t *testing.T, schema string) {
				if !strings.Contains(schema, `"type":"array"`) {
					t.Errorf("schema = %s", schema)
				}
			}},
		// Nothing left to constrain: JSON mode alone, not an object Gemini rejects.
		{name: "empty object", format: `{"type":"json_schema","json_schema":{"name":"any","schema":{"type":"object","additionalProperties":true}}}`},
		{name: "no schema", format: `{"type":"json_schema","json_schema":{"name":"x"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"response_format":`+c.format+`}`)
			g, err := translateGemini(context.Background(), Config{}, req)
			if err != nil {
				t.Fatal(err)
			}
			wantMime := c.name != "text"
			if got := g.GenerationConfig["responseMimeType"]; (got == "application/json") != wantMime {
				t.Errorf("responseMimeType = %v", got)
			}
			schema, ok := g.GenerationConfig["responseSchema"]
			if ok != c.wantSchema {
				t.Fatalf("responseSchema = %v", schema)
			}
			if !ok {
				return
			}
			// Assert on the wire bytes, which is what Gemini judges.
			b, _ := json.Marshal(g)
			var wire struct {
				GenerationConfig struct {
					ResponseSchema json.RawMessage `json:"responseSchema"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			c.check(t, string(wire.GenerationConfig.ResponseSchema))
		})
	}
}

func TestGeminiSignaturesCollect(t *testing.T) {
	call := func(name, sig string) geminiPart {
		return geminiPart{FunctionCall: &geminiFunctionCall{Name: name, Args: json.RawMessage(`{}`)}, ThoughtSignature: sig}
	}
	signed := func(text, sig string) geminiPart { return geminiPart{Text: text, ThoughtSignature: sig} }
	cases := []struct {
		name   string
		chunks [][]geminiPart
		want   []string // signature per call, in order
	}{
		{name: "on the call", chunks: [][]geminiPart{{call("a", "s1"), call("b", "")}}, want: []string{"s1", ""}},
		{name: "part before", chunks: [][]geminiPart{{signed("", "s1"), call("a", ""), call("b", "")}}, want: []string{"s1", ""}},
		{name: "part after", chunks: [][]geminiPart{{call("a", ""), call("b", ""), signed("", "s1")}}, want: []string{"s1", ""}},
		{name: "earlier chunk", chunks: [][]geminiPart{{signed("", "s1")}, {call("a", ""), call("b", "")}}, want: []string{"s1", ""}},
		{name: "own wins", chunks: [][]geminiPart{{signed("", "other"), call("a", "s1"), signed("", "late")}}, want: []string{"s1"}},
		{name: "first call only", chunks: [][]geminiPart{{call("a", "s1")}, {signed("", "late"), call("b", "")}}, want: []string{"s1", ""}},
		{name: "text only", chunks: [][]geminiPart{{signed("hi", "s1")}}, want: nil},
		// A signature on a part with content belongs to that content.
		{name: "text before", chunks: [][]geminiPart{{signed("thinking aloud", "t1"), call("a", ""), call("b", "")}}, want: []string{"", ""}},
		{name: "text after", chunks: [][]geminiPart{{call("a", ""), signed("done", "t1")}}, want: []string{""}},
		{name: "text chunk before", chunks: [][]geminiPart{{signed("thinking aloud", "t1")}, {call("a", "")}}, want: []string{""}},
		{name: "text then empty", chunks: [][]geminiPart{{signed("thinking aloud", "t1"), signed("", "s1"), call("a", "")}}, want: []string{"s1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sigs geminiSignatures
			var got []string
			for _, parts := range c.chunks {
				for _, tc := range sigs.collect(geminiContent{Parts: parts}) {
					got = append(got, tc.Signature)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("signatures = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGeminiResponseFormatLogsDroppedKeywords(t *testing.T) {
	var logged bytes.Buffer
	cfg := Config{Logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	req := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",
		"json_schema":{"name":"weather","schema":{"type":"object","additionalProperties":false,"properties":{"city":{"type":"string"}}}}}}`)
	if _, err := translateGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	line := logged.String()
	for _, want := range []string{"response schema keywords dropped for gemini", "provider=gemini", "schema=weather", "additionalProperties"} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}

	// A schema the sanitiser keeps whole logs nothing.
	logged.Reset()
	req = parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",
		"json_schema":{"name":"weather","schema":{"type":"object","properties":{"city":{"type":"string"}}}}}}`)
	if _, err := translateGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if logged.Len() != 0 {
		t.Errorf("unexpected log: %s", logged.String())
	}
}
