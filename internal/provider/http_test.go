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
	_, err = doRequest(context.Background(), cfg, http.MethodPost, "http://"+addr+"/v1", nil, map[string]any{})
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
	_, err := doRequest(context.Background(), cfg, http.MethodPost, srv.URL, nil, nil)
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
	if _, err := emb.Embed(context.Background(), []string{"a"}); err != nil {
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
