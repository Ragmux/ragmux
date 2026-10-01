package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An upstream 5xx is flattened to 502, except 503 and 504: those tell the
// client the upstream is overloaded or slow, which is worth a retry.
func TestUpstreamErrorStatusMapping(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{http.StatusInternalServerError, http.StatusBadGateway},
		{http.StatusNotImplemented, http.StatusBadGateway},
		{http.StatusBadGateway, http.StatusBadGateway},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, http.StatusGatewayTimeout},
		{599, http.StatusBadGateway},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
	} {
		if got := upstreamError(c.in, []byte("x")).Status; got != c.want {
			t.Errorf("upstream %d -> %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUpstreamRetryAfterShapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"7", "7"},
		{" 120 ", "120"},
		{"Wed, 21 Oct 2026 07:28:00 GMT", "Wed, 21 Oct 2026 07:28:00 GMT"},
		{"", ""},
		{"-1", ""},
		{"1.5", ""},
		{"soon", ""},
		{strings.Repeat("9", maxRetryAfter+1), ""},
	} {
		h := http.Header{}
		if c.in != "" {
			h.Set("Retry-After", c.in)
		}
		if got := upstreamRetryAfter(h); got != c.want {
			t.Errorf("Retry-After %q -> %q, want %q", c.in, got, c.want)
		}
	}
}

// The upstream's Retry-After survives both request paths into *Error, and
// the status mapping holds on both.
func TestUpstreamErrorCarriesRetryAfter(t *testing.T) {
	for _, c := range []struct {
		status     int
		retryAfter string
		want       int
	}{
		{http.StatusTooManyRequests, "7", http.StatusTooManyRequests},
		{http.StatusServiceUnavailable, "Wed, 21 Oct 2026 07:28:00 GMT", http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, "", http.StatusGatewayTimeout},
		{http.StatusInternalServerError, "", http.StatusBadGateway},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"busy","type":"overloaded_error"}}`))
		}))
		cfg := Config{ProviderType: "openai", BaseURL: srv.URL, APIKey: "k"}

		jsonErr := doJSON(context.Background(), cfg, opChat, srv.URL, nil, map[string]any{}, nil)
		_, streamErr := doStream(context.Background(), cfg, srv.URL, nil, map[string]any{})
		srv.Close()

		for name, err := range map[string]error{"json": jsonErr, "stream": streamErr} {
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("%d %s: err = %v", c.status, name, err)
			}
			if pe.Status != c.want || pe.UpstreamRetryAfter != c.retryAfter || pe.RetryAfter != 0 {
				t.Errorf("%d %s: status=%d retry=%q gwRetry=%d", c.status, name, pe.Status, pe.UpstreamRetryAfter, pe.RetryAfter)
			}
		}
	}
}

// Anthropic's 529 "overloaded" is a 503 to the client, with the upstream's
// Retry-After, on both request paths. Another provider's 529 is just an
// unknown 5xx.
func TestAnthropicOverloadedBecomes503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusAnthropicOverloaded)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	}))
	defer srv.Close()
	for providerType, want := range map[string]int{"anthropic": http.StatusServiceUnavailable, "openai": http.StatusBadGateway} {
		cfg := Config{ProviderType: providerType, BaseURL: srv.URL, APIKey: "k"}
		jsonErr := doJSON(context.Background(), cfg, opChat, srv.URL, nil, map[string]any{}, nil)
		_, streamErr := doStream(context.Background(), cfg, srv.URL, nil, map[string]any{})
		for name, err := range map[string]error{"json": jsonErr, "stream": streamErr} {
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("%s %s: err = %v", providerType, name, err)
			}
			if pe.Status != want || pe.UpstreamRetryAfter != "30" {
				t.Errorf("%s %s: status=%d retry=%q, want %d", providerType, name, pe.Status, pe.UpstreamRetryAfter, want)
			}
		}
	}
}

// An unknown stop_reason becomes "stop" -- OpenAI clients switch on a closed
// set -- and is logged at Warn with the value alone.
func TestAnthropicFinishUnknownReason(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	if got := anthropicFinish(log, "some_new_reason"); got == nil || *got != "stop" {
		t.Fatalf("finish = %v, want stop", got)
	}
	line := logged.String()
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "stop_reason=some_new_reason") {
		t.Errorf("log = %q", line)
	}

	logged.Reset()
	for in, want := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls",
		"refusal": "content_filter", "model_context_window_exceeded": "length"} {
		if got := anthropicFinish(log, in); got == nil || *got != want {
			t.Errorf("%s -> %v, want %s", in, got, want)
		}
	}
	if anthropicFinish(log, "") != nil {
		t.Error("empty stop_reason should map to nil")
	}
	if logged.Len() != 0 {
		t.Errorf("known reasons must not log: %q", logged.String())
	}
}

// A json_schema / json_object request to an Anthropic connection is still
// forwarded (without the field, which the Messages API does not have),
// logged at Info, and reported through RequestWarnings.
func TestAnthropicResponseFormatIsForwardedWithWarning(t *testing.T) {
	for _, rfType := range []string{"json_schema", "json_object"} {
		t.Run(rfType, func(t *testing.T) {
			var sent map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&sent)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
			}))
			defer srv.Close()
			var logged strings.Builder
			p, err := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "ak", Model: "claude",
				Logger: slog.New(slog.NewTextHandler(&logged, nil))})
			if err != nil {
				t.Fatal(err)
			}
			var req ChatRequest
			body := `{"model":"c","messages":[{"role":"user","content":"secret prompt"}],"response_format":{"type":"` + rfType + `","json_schema":{"name":"x","schema":{"type":"object"}}}}`
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}

			rw, ok := p.(RequestWarner)
			if !ok {
				t.Fatal("anthropic adapter does not implement RequestWarner")
			}
			if w := rw.RequestWarnings(req); len(w) != 1 || w[0] != anthropicResponseFormatWarning {
				t.Errorf("warnings = %q", w)
			}

			resp, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatalf("chat: %v", err)
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("resp = %+v", resp)
			}
			if _, has := sent["response_format"]; has || sent == nil {
				t.Errorf("upstream body = %v", sent)
			}
			line := logged.String()
			if !strings.Contains(line, "level=INFO") || !strings.Contains(line, "response_format="+rfType) {
				t.Errorf("log = %q", line)
			}
			if strings.Contains(line, "secret prompt") {
				t.Errorf("log leaks content: %q", line)
			}
		})
	}
}

func TestAnthropicRequestWarningsOnlyForJSONModes(t *testing.T) {
	p := newAnthropic(Config{})
	for _, rf := range []string{``, `{"type":"text"}`, `"json_object"`, `{"type":"other"}`} {
		req := ChatRequest{ResponseFormat: json.RawMessage(rf)}
		if w := p.RequestWarnings(req); len(w) != 0 {
			t.Errorf("response_format %s: warnings = %q", rf, w)
		}
	}
}
