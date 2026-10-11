package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/netguard"
)

func TestTransportErrorClassification(t *testing.T) {
	var logged strings.Builder
	cfg := Config{APIKey: "topsecretkey123", Logger: slog.New(slog.NewTextHandler(&logged, nil))}
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{&url.Error{Op: "Post", URL: "https://x", Err: &netguard.Error{Host: "internal.example"}}, 502,
			`upstream host "internal.example" resolves to a private or local address; set ALLOW_PRIVATE_UPSTREAMS=true or add it to PRIVATE_UPSTREAM_ALLOWLIST`},
		{&url.Error{Op: "Get", URL: "https://x", Err: &netguard.RedirectError{Reason: "target host differs from the request host"}}, 502,
			"upstream redirect rejected: target host differs from the request host"},
		{&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: &timeoutErr{}}}, 504, "upstream request timed out"},
		{&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}}}, 502, "upstream unreachable"},
		{&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("connect: connection refused topsecretkey123")}}, 502, "upstream request failed"},
		{&url.Error{Op: "Post", URL: "https://x", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, 502, "upstream TLS handshake failed"},
		{&url.Error{Op: "Post", URL: "https://x", Err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}}, 502, "upstream TLS handshake failed"},
		{&url.Error{Op: "Post", URL: "http://x", Err: errors.New(`malformed HTTP response "\x15\x03\x01"`)}, 502, "upstream returned a non-HTTP response"},
		{errors.New("something odd with topsecretkey123"), 502, "upstream request failed"},
	}
	for _, c := range cases {
		e := transportError(cfg, "https://api.example.com/v1/chat", c.err)
		if e.Status != c.status || e.Message != c.msg {
			t.Errorf("%v -> %d %q, want %d %q", c.err, e.Status, e.Message, c.status, c.msg)
		}
	}
	log := logged.String()
	if strings.Contains(log, "topsecretkey123") || !strings.Contains(log, "[redacted]") || !strings.Contains(log, "host=api.example.com") {
		t.Errorf("log leaked or lacks host:\n%s", log)
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

func TestDoRequestConnectionRefused(t *testing.T) {
	// Grab a free port, close it and connect: guaranteed refusal.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := Config{APIKey: "k", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Client: &http.Client{Timeout: 5 * time.Second}}
	_, _, err = doRequest(context.Background(), cfg, opChat, "http://"+addr+"/v1", nil, map[string]any{})
	var pe *Error
	if !errors.As(err, &pe) || pe.Message != "upstream unreachable" {
		t.Errorf("err = %v", err)
	}
}

func TestDoRequestNetguardRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: netguard.SafeDialContext(nil, false, nil)}}
	cfg := Config{Client: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, err := doRequest(context.Background(), cfg, opChat, srv.URL, nil, nil)
	var pe *Error
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "PRIVATE_UPSTREAM_ALLOWLIST") {
		t.Errorf("err = %v", err)
	}
}

func TestUpstreamErrorCapAndSecrets(t *testing.T) {
	long := strings.Repeat("x", 3000)
	e := upstreamError(http.StatusBadRequest, []byte(long))
	if len(e.Message) != maxUpstreamMessage {
		t.Errorf("message length = %d", len(e.Message))
	}
	e = upstreamError(http.StatusUnauthorized, []byte(`{"error":{"message":"key my-custom-key-9 rejected"}}`), "my-custom-key-9")
	if e.Message != "key [redacted] rejected" {
		t.Errorf("message = %q", e.Message)
	}
	e = upstreamError(http.StatusBadGateway, []byte(""), "")
	if e.Message != "Bad Gateway" || e.Status != http.StatusBadGateway {
		t.Errorf("empty body: %+v", e)
	}
}

func TestDoStreamLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := w.Write([]byte("data: " + strings.Repeat("a", 100) + "\n\n")); err != nil {
				return
			}
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	// Byte cap.
	cfg := Config{StreamMaxBytes: 500, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	resp, err := doStream(context.Background(), cfg, srv.URL, nil, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var pe *Error
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "maximum size") {
		t.Errorf("byte cap: err = %v", err)
	}

	// Duration cap.
	cfg = Config{StreamMaxDuration: 50 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	resp, err = doStream(context.Background(), cfg, srv.URL, nil, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.As(err, &pe) || pe.Status != http.StatusGatewayTimeout || !strings.Contains(pe.Message, "maximum duration") {
		t.Errorf("duration cap: err = %v", err)
	}
}

func TestGeminiModelPathEscaped(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if strings.Contains(r.URL.Path, "batchEmbedContents") {
			_, _ = w.Write([]byte(`{"embeddings":[{"values":[1,2]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()
	cfg := Config{ProviderType: "gemini", BaseURL: srv.URL, APIKey: "k", Model: "models/../../evil/x?y=1"}
	p := newGemini(cfg)
	req := ChatRequest{Messages: []Message{{Role: "user", Content: TextContent("q")}}}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	emb, _ := NewEmbedder(cfg)
	if _, _, err := emb.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/models/..%2F..%2Fevil%2Fx%3Fy=1:generateContent", "/models/..%2F..%2Fevil%2Fx%3Fy=1:batchEmbedContents"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("paths = %q, want %q", paths, want)
	}
	if geminiModelPath("gemini-2.0-flash") != "gemini-2.0-flash" || geminiModelPath("models/text-embedding-004") != "text-embedding-004" {
		t.Error("plain names must pass through unchanged")
	}
}

// TestTransportErrorRetryClass pins the typed classification the gateway's
// retry loop reads (ADR-020): a request that never left is NotSent and
// Retryable; a connection reset after the connection was made is neither, even
// though the client-visible message is the same "upstream unreachable".
func TestTransportErrorRetryClass(t *testing.T) {
	cfg := Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dial := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: err}}
	}
	cases := []struct {
		name      string
		err       error
		notSent   bool
		retryable bool
		message   string
	}{
		{"dns", dial(&net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}), true, true, "upstream unreachable"},
		{"refused", dial(syscall.ECONNREFUSED), true, true, "upstream unreachable"},
		{"host unreachable", dial(syscall.EHOSTUNREACH), true, true, "upstream unreachable"},
		{"tls handshake", &url.Error{Op: "Post", URL: "https://x", Err: tls.RecordHeaderError{Msg: "bad"}}, true, true, "upstream TLS handshake failed"},
		{"reset after send", &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, false, false, "upstream unreachable"},
		{"unexpected eof", &url.Error{Op: "Post", URL: "https://x", Err: io.ErrUnexpectedEOF}, false, false, "upstream unreachable"},
		{"timeout", dial(&timeoutErr{}), false, false, "upstream request timed out"},
		{"guard", &url.Error{Op: "Post", URL: "https://x", Err: &netguard.Error{Host: "internal.example"}}, true, false, ""},
	}
	for _, c := range cases {
		e := transportError(cfg, "https://api.example.com/v1/chat", c.err)
		if e.NotSent != c.notSent || e.Retryable != c.retryable {
			t.Errorf("%s: NotSent=%v Retryable=%v, want %v %v", c.name, e.NotSent, e.Retryable, c.notSent, c.retryable)
		}
		if c.message != "" && e.Message != c.message {
			t.Errorf("%s: message = %q, want %q", c.name, e.Message, c.message)
		}
		if e.UpstreamStatus != 0 {
			t.Errorf("%s: UpstreamStatus = %d, want 0 (no response)", c.name, e.UpstreamStatus)
		}
	}
}

func TestUpstreamStatusErrorRetryClass(t *testing.T) {
	cases := []struct {
		provider   string
		status     int
		retryAfter string
		wantStatus int
		retryable  bool
		delay      time.Duration
	}{
		{"openai", 429, "", 429, true, 0},
		{"openai", 429, "1", 429, true, time.Second},
		{"openai", 503, "30", 503, true, 30 * time.Second},
		{"anthropic", 529, "", 503, true, 0},
		{"openai", 529, "", 502, false, 0},
		{"openai", 500, "", 502, false, 0},
		{"openai", 502, "", 502, false, 0},
		{"openai", 504, "", 504, false, 0},
		{"openai", 401, "", 401, false, 0},
		{"openai", 400, "", 400, false, 0},
		{"openai", 429, "garbage", 429, true, 0},
	}
	for _, c := range cases {
		resp := &http.Response{StatusCode: c.status, Header: http.Header{}}
		if c.retryAfter != "" {
			resp.Header.Set("Retry-After", c.retryAfter)
		}
		e := upstreamStatusError(Config{ProviderType: c.provider}, resp, []byte("x"))
		if e.UpstreamStatus != c.status || e.Status != c.wantStatus || e.Retryable != c.retryable || e.UpstreamRetryDelay != c.delay {
			t.Errorf("%s %d Retry-After=%q: got status=%d upstream=%d retryable=%v delay=%v, want status=%d retryable=%v delay=%v",
				c.provider, c.status, c.retryAfter, e.Status, e.UpstreamStatus, e.Retryable, e.UpstreamRetryDelay, c.wantStatus, c.retryable, c.delay)
		}
		if e.NotSent {
			t.Errorf("%d: an answered request is never NotSent", c.status)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"7", 7 * time.Second},
		{"99999999999999999999", maxRetryDelay},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"soon", 0},
	}
	for _, c := range cases {
		if got := retryDelay(c.in, now); got != c.want {
			t.Errorf("retryDelay(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
