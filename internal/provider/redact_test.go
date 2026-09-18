package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"key sk-abcdefghijklmnop rejected":               "key [redacted] rejected",
		"anthropic sk-ant-api03-abcdefgh_ijk bad":        "anthropic [redacted] bad",
		"google AIzaSyA1234567890abcdefghijklmn invalid": "google [redacted] invalid",
		"header Bearer sk-proj-abcdefghijklmnop":         "header [redacted]",
		"header Bearer tok.en_value-123":                 "header [redacted]",
		"short sk-abc stays":                             "short sk-abc stays",
		"plain message":                                  "plain message",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpstreamErrorRedactsMessages(t *testing.T) {
	e := upstreamError(http.StatusUnauthorized, []byte(`{"error":{"message":"Incorrect API key provided: sk-abcdefghijklmnop1234","type":"invalid_request_error","code":"invalid_api_key"}}`))
	if strings.Contains(e.Message, "abcdefghijklmnop") || !strings.Contains(e.Message, "[redacted]") || e.Code != "invalid_api_key" {
		t.Errorf("error = %+v", e)
	}
	plain := upstreamError(http.StatusBadGateway, []byte("proxy refused Bearer AIzaSyA1234567890abcdefghijklmn"))
	if strings.Contains(plain.Message, "AIza") {
		t.Errorf("plain text not redacted: %q", plain.Message)
	}
}

func TestRedactMorePatterns(t *testing.T) {
	cases := map[string]string{
		"auth Basic dXNlcjpwYXNzd29yZA== failed":                   "auth [redacted] failed",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abc_def-123": "jwt [redacted]",
		"google ya29.a0AfH6SMBx-1234567890 expired":                "google [redacted] expired",
		"groq gsk_abcdefghijklmnop bad":                            "groq [redacted] bad",
		"hf hf_abcdefghijklmnop bad":                               "hf [redacted] bad",
		"xai xai-abcdefghijklmnop bad":                             "xai [redacted] bad",
		"dial https://user:p4ss@proxy.example:8080/x":              "dial https://[redacted]@proxy.example:8080/x",
		"plain http://proxy.example:8080/x stays":                  "plain http://proxy.example:8080/x stays",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactWith(t *testing.T) {
	got := RedactWith("key zzz-custom-key rejected, also sk-abcdefghijklmnop", "zzz-custom-key", "")
	if got != "key [redacted] rejected, also [redacted]" {
		t.Errorf("got %q", got)
	}
	if RedactWith("untouched") != "untouched" {
		t.Error("no secrets must be a no-op")
	}
}

func TestStreamErrorsRedacted(t *testing.T) {
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"key my-anthropic-key-1 sk-ant-api03-abcdefgh rejected\"}}\n\n"))
	}))
	defer anth.Close()
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: anth.URL, APIKey: "my-anthropic-key-1", Model: "m"})
	out := make(chan StreamChunk, 16)
	err := p.ChatStream(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: TextContent("q")}}}, out)
	var pe *Error
	if !errors.As(err, &pe) || pe.Message != "key [redacted] [redacted] rejected" || pe.Type != "overloaded_error" {
		t.Errorf("anthropic stream error = %v", err)
	}

	oll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"model my-ollama-key-1 not found"}` + "\n"))
	}))
	defer oll.Close()
	p, _ = New(Config{ProviderType: "ollama", BaseURL: oll.URL, APIKey: "my-ollama-key-1", Model: "m"})
	req := ChatRequest{Messages: []Message{{Role: "user", Content: TextContent("q")}}}
	if _, err := p.Chat(context.Background(), req); !errors.As(err, &pe) || pe.Message != "model [redacted] not found" {
		t.Errorf("ollama chat error = %v", err)
	}
	if err := p.ChatStream(context.Background(), req, out); !errors.As(err, &pe) || pe.Message != "model [redacted] not found" {
		t.Errorf("ollama stream error = %v", err)
	}
}
