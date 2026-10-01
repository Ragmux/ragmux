package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

const anthropicWarning = `299 ragmux "response_format is not supported for anthropic connections; ignored"`

// fakeAnthropic is a scripted Messages API: JSON or SSE, depending on the
// request's stream flag. It records whether response_format reached it.
func fakeAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, has := req["response_format"]; has {
			t.Errorf("response_format reached the Messages API: %v", req)
		}
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{}"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`data: {"type":"message_stop"}`,
			} {
				_, _ = fmt.Fprint(w, ev+"\n\n")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAnthropicProvider(t *testing.T, baseURL string) provider.Provider {
	t.Helper()
	p, err := provider.New(provider.Config{ProviderType: "anthropic", BaseURL: baseURL, APIKey: "ak", Model: "claude",
		Timeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonSchemaRequest(stream bool) map[string]any {
	return map[string]any{"messages": userMsg, "stream": stream,
		"response_format": map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "x", "schema": map[string]any{"type": "object"}}}}
}

// --- Database-free coverage of the helpers and the stream path ---

func TestWriteProviderErrorRetryAfter(t *testing.T) {
	for _, c := range []struct {
		name string
		pe   *provider.Error
		want string
	}{
		{"upstream seconds", &provider.Error{Status: 429, UpstreamRetryAfter: "7"}, "7"},
		{"upstream date", &provider.Error{Status: 503, UpstreamRetryAfter: "Wed, 21 Oct 2026 07:28:00 GMT"}, "Wed, 21 Oct 2026 07:28:00 GMT"},
		{"gateway wins", &provider.Error{Status: 429, RetryAfter: 2, UpstreamRetryAfter: "7"}, "2"},
		{"none", &provider.Error{Status: 502}, ""},
	} {
		w := httptest.NewRecorder()
		writeProviderError(w, c.pe.Status, c.pe)
		if got := w.Header().Get("Retry-After"); got != c.want {
			t.Errorf("%s: Retry-After = %q, want %q", c.name, got, c.want)
		}
		if w.Code != c.pe.Status || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %v", c.name, w.Code, w.Header())
		}
	}
}

type plainProvider struct{ saturatedProvider }

type warningProvider struct {
	saturatedProvider
	texts []string
}

func (p warningProvider) RequestWarnings(provider.ChatRequest) []string { return p.texts }

func TestSetRequestWarnings(t *testing.T) {
	h := http.Header{}
	setRequestWarnings(h, plainProvider{}, provider.ChatRequest{})
	if len(h.Values("Warning")) != 0 {
		t.Errorf("adapter without warnings set %v", h)
	}
	h = http.Header{}
	setRequestWarnings(h, warningProvider{texts: []string{"a", `b "quoted"`}}, provider.ChatRequest{})
	if got := h.Values("Warning"); len(got) != 2 || got[0] != `299 ragmux "a"` || got[1] != `299 ragmux "b \"quoted\""` {
		t.Errorf("Warning = %q", got)
	}
}

// The stream path writes the Warning header before its first byte, with
// the real Anthropic adapter behind it. This runs without a database.
func TestAnthropicStreamWarningBeforeFirstByte(t *testing.T) {
	prov := newAnthropicProvider(t, fakeAnthropic(t).URL)
	g := &Gateway{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var req provider.ChatRequest
	raw, _ := json.Marshal(jsonSchemaRequest(true))
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setRequestWarnings(w.Header(), prov, req)
	g.stream(w, r, prov, &store.ModelConnection{ProviderType: "anthropic"}, req, &store.RequestLog{}, &requestObs{}, 0)

	res := w.Result()
	if res.StatusCode != http.StatusOK || res.Header.Get("Warning") != anthropicWarning {
		t.Fatalf("status=%d Warning=%q", res.StatusCode, res.Header.Get("Warning"))
	}
	if body := w.Body.String(); !strings.Contains(body, `"content":"{}"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("body = %q", body)
	}
}

// --- End to end through the router (needs TEST_DATABASE_URL) ---

func TestUpstreamRetryAfterIsRelayed(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	})
	for _, stream := range []bool{false, true} {
		resp, out := e.chat(map[string]any{"messages": userMsg, "stream": stream})
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "7" {
			t.Errorf("stream=%v: %d Retry-After=%q %v", stream, resp.StatusCode, resp.Header.Get("Retry-After"), out)
		}
	}
}

func TestUpstream5xxStatusMapping(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []struct{ upstream, want int }{
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, http.StatusGatewayTimeout},
		{http.StatusInternalServerError, http.StatusBadGateway},
	} {
		status := c.upstream
		e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", status)
		})
		for _, stream := range []bool{false, true} {
			resp, out := e.chat(map[string]any{"messages": userMsg, "stream": stream})
			if resp.StatusCode != c.want || errorField(t, out, "message") != "down" {
				t.Errorf("upstream %d stream=%v: %d %v", c.upstream, stream, resp.StatusCode, out)
			}
		}
	}
}

func TestAnthropicResponseFormatWarning(t *testing.T) {
	e := newEnv(t, nil)
	base := fakeAnthropic(t).URL
	e.gw.Providers = func(*store.ModelConnection) (provider.Provider, error) {
		return newAnthropicProvider(t, base), nil
	}
	for _, stream := range []bool{false, true} {
		resp := e.post(context.Background(), "/v1/chat/completions", jsonSchemaRequest(stream), e.key)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Warning") != anthropicWarning {
			t.Errorf("stream=%v: %d Warning=%q %s", stream, resp.StatusCode, resp.Header.Get("Warning"), body)
		}
	}
	// Without response_format there is nothing to warn about.
	resp, _ := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Warning") != "" {
		t.Errorf("plain request: %d Warning=%q", resp.StatusCode, resp.Header.Get("Warning"))
	}
}
