package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureServer answers every request with testdata/<name> and records the
// body of the last one.
func fixtureServer(t *testing.T, contentType, name string) (*httptest.Server, *string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

// passthroughConnections are the adapters that relay an OpenAI-shaped reply.
func passthroughConnections(srvURL string) map[string]Config {
	return map[string]Config{
		"custom_openai": {ProviderType: "custom_openai", BaseURL: srvURL, APIKey: "k", Model: "m"},
		"openai":        {ProviderType: "openai", BaseURL: srvURL, APIKey: "k", Model: "m"},
		"azure_openai":  {ProviderType: AzureProviderType, BaseURL: srvURL, APIKey: "k", Model: "dep"},
	}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenAICompatRelaysResponseFields(t *testing.T) {
	srv, _ := fixtureServer(t, "application/json", "openai_passthrough.json")
	for name, cfg := range passthroughConnections(srv.URL) {
		t.Run(name, func(t *testing.T) {
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			got := asMap(t, resp)
			if got["system_fingerprint"] != "fp_44709d6fcb" {
				t.Errorf("system_fingerprint = %v", got["system_fingerprint"])
			}
			choice := got["choices"].([]any)[0].(map[string]any)
			msg := choice["message"].(map[string]any)
			if msg["refusal"] != "I can't help with that." {
				t.Errorf("refusal = %v", msg["refusal"])
			}
			ann, _ := msg["annotations"].([]any)
			if len(ann) != 1 || ann[0].(map[string]any)["type"] != "url_citation" {
				t.Errorf("annotations = %v", msg["annotations"])
			}
			lp, _ := choice["logprobs"].(map[string]any)
			content, _ := lp["content"].([]any)
			if len(content) != 1 || content[0].(map[string]any)["token"] != "Hi" {
				t.Errorf("logprobs = %v", choice["logprobs"])
			}
			// Nothing was invented around the relayed fields.
			if msg["content"] != nil {
				t.Errorf("content = %v, want null", msg["content"])
			}
		})
	}
}

func TestOpenAICompatRelaysStreamFields(t *testing.T) {
	srv, _ := fixtureServer(t, "text/event-stream", "openai_passthrough.sse")
	for name, cfg := range passthroughConnections(srv.URL) {
		t.Run(name, func(t *testing.T) {
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			out := make(chan StreamChunk, 16)
			if err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out); err != nil {
				t.Fatal(err)
			}
			close(out)
			var fingerprints, logprobs int
			var refusal string
			var annotations []any
			var sawNullLogprobs bool
			for c := range out {
				m := asMap(t, c)
				if m["system_fingerprint"] == "fp_44709d6fcb" {
					fingerprints++
				}
				for _, ch := range m["choices"].([]any) {
					ch := ch.(map[string]any)
					if lp, ok := ch["logprobs"]; ok {
						if lp == nil {
							sawNullLogprobs = true
						} else {
							logprobs++
						}
					}
					d := ch["delta"].(map[string]any)
					if r, ok := d["refusal"].(string); ok {
						refusal += r
					}
					if a, ok := d["annotations"].([]any); ok {
						annotations = append(annotations, a...)
					}
				}
			}
			// Four chunks carry choices plus the usage trailer: all five name
			// the backend.
			if fingerprints != 5 {
				t.Errorf("system_fingerprint on %d chunks, want 5", fingerprints)
			}
			if logprobs != 1 || !sawNullLogprobs {
				t.Errorf("logprobs objects = %d, null seen = %v", logprobs, sawNullLogprobs)
			}
			if refusal != "I can't" {
				t.Errorf("refusal = %q", refusal)
			}
			if len(annotations) != 1 {
				t.Errorf("annotations = %v", annotations)
			}
		})
	}
}

// logprobs and top_logprobs are request fields the gateway does not know by
// name; they must reach the upstream through the passthrough of unknown
// fields, on every OpenAI-shaped adapter.
func TestOpenAICompatForwardsLogprobsRequest(t *testing.T) {
	srv, body := fixtureServer(t, "application/json", "openai_passthrough.json")
	for name, cfg := range passthroughConnections(srv.URL) {
		t.Run(name, func(t *testing.T) {
			p, _ := New(cfg)
			req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"logprobs":true,"top_logprobs":2}`)
			if _, err := p.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(*body), &sent); err != nil {
				t.Fatal(err)
			}
			if sent["logprobs"] != true || sent["top_logprobs"] != float64(2) {
				t.Errorf("upstream saw logprobs=%v top_logprobs=%v", sent["logprobs"], sent["top_logprobs"])
			}
		})
	}
}

// ragmux_is_error is for the Anthropic adapter; an OpenAI-shaped upstream
// never sees it, and the caller's request is not modified (a fallback may
// still need the flag).
func TestOpenAICompatStripsToolErrorFlag(t *testing.T) {
	srv, body := fixtureServer(t, "application/json", "openai_passthrough.json")
	for name, cfg := range passthroughConnections(srv.URL) {
		t.Run(name, func(t *testing.T) {
			p, _ := New(cfg)
			req := parseReq(t, `{"messages":[{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"boom","ragmux_is_error":true}]}`)
			if !req.Messages[2].IsError {
				t.Fatal("ragmux_is_error not decoded")
			}
			if _, err := p.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(*body, "ragmux_is_error") {
				t.Errorf("upstream body carries the extension: %s", *body)
			}
			if !req.Messages[2].IsError {
				t.Error("the caller's request was modified")
			}
		})
	}
}

func TestTranslateAnthropicToolResult(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgo="
	req := parseReq(t, `{"model":"x","messages":[
		{"role":"user","content":"look"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"shot","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"run","arguments":"{}"}},
			{"id":"c3","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[
			{"type":"text","text":"screenshot"},
			{"type":"image_url","image_url":{"url":"`+png+`"}}]},
		{"role":"tool","tool_call_id":"c2","content":"permission denied","ragmux_is_error":true},
		{"role":"tool","tool_call_id":"c3","content":[{"type":"text","text":"a.txt"},{"type":"text","text":"b.txt"}]}]}`)
	ar, err := translateAnthropic(context.Background(), Config{Model: "claude-x"}, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   *bool           `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	last := wire.Messages[len(wire.Messages)-1]
	if last.Role != "user" || len(last.Content) != 3 {
		t.Fatalf("tool results = %s", raw)
	}
	img, failed, plain := last.Content[0], last.Content[1], last.Content[2]

	// An image part: the content becomes a block array holding an image block.
	if img.Type != "tool_result" || img.ToolUseID != "c1" || img.IsError != nil {
		t.Errorf("image result = %+v", img)
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(img.Content, &blocks); err != nil || len(blocks) != 2 {
		t.Fatalf("image content = %s (%v)", img.Content, err)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "screenshot" ||
		blocks[1].Type != "image" || blocks[1].Source.Type != "base64" ||
		blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != "iVBORw0KGgo=" {
		t.Errorf("blocks = %+v", blocks)
	}

	// The error flag: is_error true, content still the plain string.
	if failed.ToolUseID != "c2" || failed.IsError == nil || !*failed.IsError || string(failed.Content) != `"permission denied"` {
		t.Errorf("failed result = %+v content=%s", failed, failed.Content)
	}

	// No image and no flag: the wire shape it always had.
	if plain.ToolUseID != "c3" || plain.IsError != nil || string(plain.Content) != `"a.txt\nb.txt"` {
		t.Errorf("plain result = %+v content=%s", plain, plain.Content)
	}
}

func TestAnthropicToolResultReachesUpstream(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	p, err := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-x"})
	if err != nil {
		t.Fatal(err)
	}
	req := parseReq(t, `{"messages":[{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","ragmux_is_error":true,"content":[
			{"type":"text","text":"failed, see picture"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`)
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, `"is_error":true`) || !strings.Contains(sent, `"type":"image"`) {
		t.Errorf("anthropic request lacks is_error or the image block: %s", sent)
	}
	if strings.Contains(sent, "ragmux_is_error") {
		t.Errorf("the extension leaked upstream: %s", sent)
	}
}
