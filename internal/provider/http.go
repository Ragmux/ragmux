package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"syscall"
	"time"

	"github.com/ragmux/ragmux/internal/netguard"
	"github.com/ragmux/ragmux/internal/tracing"
)

// Outbound call kinds, the suffix of the provider.* client span and the only
// values that reach a span name from this package.
const (
	opChat   = "chat"
	opEmbed  = "embed"
	opRerank = "rerank"
)

// Config is the per-connection information adapters need.
type Config struct {
	ProviderType string
	BaseURL      string
	APIKey       string
	Model        string
	// Timeout bounds non-streaming calls. Streaming calls only use it for the
	// connect/header phase.
	Timeout time.Duration
	Client  *http.Client
	// StreamMaxDuration bounds a streaming response end to end (default 30m).
	StreamMaxDuration time.Duration
	// StreamMaxBytes caps the bytes read from a streaming response body
	// (default 256 MiB).
	StreamMaxBytes int64
	// Logger receives transport failures with their raw (redacted) cause;
	// nil means slog.Default().
	Logger *slog.Logger
	// Images resolves remote image URLs for adapters whose upstream cannot
	// fetch one. Nil keeps the plain rejection those adapters answered with
	// before, so switching image fetching off changes nothing else.
	Images *ImageFetcher
	// Tracer records one client span per upstream call and is the single
	// place traceparent is written outbound: every chat, embed, stream and
	// rerank call in this package funnels through doRequest. Nil is a
	// disabled tracer and costs nothing.
	Tracer *tracing.Tracer
}

func (c Config) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 5 * time.Minute
}

func (c Config) streamMaxDuration() time.Duration {
	if c.StreamMaxDuration > 0 {
		return c.StreamMaxDuration
	}
	return 30 * time.Minute
}

func (c Config) streamMaxBytes() int64 {
	if c.StreamMaxBytes > 0 {
		return c.StreamMaxBytes
	}
	return 256 << 20
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) baseURL(def string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return def
}

// doJSON posts a JSON body and decodes a JSON response, mapping non-2xx
// statuses to *Error.
func doJSON(ctx context.Context, cfg Config, op, url string, headers map[string]string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	resp, span, err := doRequest(ctx, cfg, op, url, headers, body)
	if err != nil {
		return err
	}
	defer span.End()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		e := &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "read upstream response: " + RedactWith(err.Error(), cfg.APIKey)}
		spanFailed(span, "read_failed")
		return e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := upstreamError(resp.StatusCode, raw, cfg.APIKey)
		spanFailed(span, e.Type)
		return e
	}
	span.SetStatusOK()
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		e := &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "decode upstream response: " + err.Error()}
		spanFailed(span, "decode_failed")
		return e
	}
	return nil
}

// spanFailed marks an upstream span failed with a bounded reason.
//
// It deliberately does not record the provider's own message. That message
// is relayed to the client and written to the request log, but a provider
// that quotes the offending request back inside a 400 would otherwise
// export prompt text to a third-party collector, and a span carries
// metadata, never payload.
func spanFailed(span *tracing.Span, reason string) {
	if !span.IsRecording() {
		return
	}
	span.RecordError(errors.New(reason))
}

// doStream posts a JSON body and returns the raw response for SSE reading.
// The body is bounded in bytes and wall time (Config.StreamMaxBytes /
// StreamMaxDuration); closing it releases the timeout.
func doStream(ctx context.Context, cfg Config, url string, headers map[string]string, body any) (*http.Response, error) {
	sctx, cancel := context.WithTimeout(ctx, cfg.streamMaxDuration())
	resp, span, err := doRequest(sctx, cfg, opChat, url, headers, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		e := upstreamError(resp.StatusCode, raw, cfg.APIKey)
		spanFailed(span, e.Type)
		span.End()
		return nil, e
	}
	// The span is handed to the body rather than ended here, so it covers
	// the whole stream instead of only the header phase: a chat span that
	// stopped at the response headers would report a 40-second completion
	// as a 200-millisecond call.
	resp.Body = &streamBody{body: resp.Body, remaining: cfg.streamMaxBytes(), limit: cfg.streamMaxBytes(),
		ctx: sctx, parent: ctx, cancel: cancel, span: span}
	return resp, nil
}

// streamBody enforces the byte and duration limits of a streaming response
// and holds the provider span open until the stream is closed.
type streamBody struct {
	body      io.ReadCloser
	remaining int64
	limit     int64
	ctx       context.Context
	parent    context.Context
	cancel    context.CancelFunc
	span      *tracing.Span
}

func (s *streamBody) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "upstream stream exceeded the maximum size"}
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
	}
	n, err := s.body.Read(p)
	s.remaining -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) && s.ctx.Err() != nil && s.parent.Err() == nil {
		return n, &Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream stream exceeded the maximum duration"}
	}
	return n, err
}

func (s *streamBody) Close() error {
	if s.span.IsRecording() {
		s.span.SetAttributes(tracing.Int64("ragmux.stream.bytes", s.limit-s.remaining))
		s.span.SetStatusOK()
	}
	s.span.End()
	s.cancel()
	return s.body.Close()
}

// doRequest posts one upstream call. It is the only place in the process
// that writes traceparent outbound: every chat, embed, stream and rerank
// call funnels through here, so propagation is one code path rather than
// one per adapter.
//
// The returned span is still open on success and must be ended by the
// caller -- doJSON on the way out, streamBody.Close when the stream ends.
// On failure the span is already ended.
func doRequest(ctx context.Context, cfg Config, op, url string, headers map[string]string, body any) (*http.Response, *tracing.Span, error) {
	ctx, span := cfg.Tracer.Start(ctx, "provider."+op, tracing.KindClient)
	if span.IsRecording() {
		span.SetAttributes(
			tracing.String("server.address", hostOf(url)),
			tracing.String("gen_ai.system", cfg.ProviderType),
		)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			spanFailed(span, "encode_failed")
			span.End()
			return nil, span, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		spanFailed(span, "bad_request")
		span.End()
		return nil, span, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "ragmux/1.0")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if sc, ok := tracing.SpanContextFrom(ctx); ok {
		req.Header.Set("traceparent", tracing.Traceparent(sc))
		if sc.State != "" {
			req.Header.Set("tracestate", sc.State)
		}
	}
	resp, err := cfg.client().Do(req)
	if err != nil {
		var e *Error
		if ctx.Err() != nil {
			e = &Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream request cancelled or timed out"}
		} else {
			e = transportError(cfg, url, err)
		}
		// transportError's messages come from a closed set (plus the host a
		// guard refused), so they are safe on a span.
		spanFailed(span, e.Message)
		span.End()
		return nil, span, e
	}
	// Guarded: Attr.Value is an any, so the status code is boxed onto the
	// heap before SetAttributes ever gets the chance to discard it. Unguarded
	// that is one allocation per upstream call in a deployment with tracing
	// switched off, which is the default. This is the hottest span site in
	// the gateway.
	if span.IsRecording() {
		span.SetAttributes(tracing.Int("http.response.status_code", resp.StatusCode))
	}
	return resp, span, nil
}

// hostOf is the host of a URL without its port, for server.address.
func hostOf(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// transportError maps a client-side failure to a short, fixed message so the
// raw error (which may carry addresses, proxy details or the credential)
// never reaches API clients. The cause is logged, redacted, with the host.
func transportError(cfg Config, rawURL string, err error) *Error {
	host := rawURL
	if u, perr := neturl.Parse(rawURL); perr == nil {
		host = u.Host
	}
	cfg.logger().Warn("upstream request failed", "host", host, "provider", cfg.ProviderType,
		"err", RedactWith(err.Error(), cfg.APIKey))

	e := &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "upstream request failed"}
	var (
		guard    *netguard.Error
		redirect *netguard.RedirectError
		netErr   net.Error
		dnsErr   *net.DNSError
		recHdr   tls.RecordHeaderError
		certErr  *tls.CertificateVerificationError
		alert    tls.AlertError
	)
	switch {
	case errors.As(err, &guard):
		e.Message = guard.Error()
	case errors.As(err, &redirect):
		e.Message = redirect.Error()
	case errors.As(err, &netErr) && netErr.Timeout():
		e.Status, e.Type, e.Message = http.StatusGatewayTimeout, "timeout", "upstream request timed out"
	case errors.As(err, &recHdr), errors.As(err, &certErr), errors.As(err, &alert),
		strings.Contains(err.Error(), "tls:"), strings.Contains(err.Error(), "x509:"):
		e.Message = "upstream TLS handshake failed"
	case errors.As(err, &dnsErr), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, io.ErrUnexpectedEOF):
		e.Message = "upstream unreachable"
	case strings.Contains(err.Error(), "malformed HTTP"):
		e.Message = "upstream returned a non-HTTP response"
	}
	return e
}

// maxUpstreamMessage caps relayed provider error messages; anything longer
// is more likely an HTML error page than a useful message.
const maxUpstreamMessage = 512

// upstreamError normalises any provider error body into *Error. secrets are
// literal values (the connection's API key) to mask on top of the pattern
// based redaction.
func upstreamError(status int, raw []byte, secrets ...string) *Error {
	e := &Error{Status: status, Type: "upstream_error", Message: strings.TrimSpace(string(raw))}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			e.Message = obj.Message
			if obj.Type != "" {
				e.Type = obj.Type
			}
			if obj.Code != nil {
				e.Code = fmt.Sprint(obj.Code)
			}
		} else {
			var s string
			if json.Unmarshal(env.Error, &s) == nil && s != "" {
				e.Message = s
			}
		}
	}
	e.Message = RedactWith(e.Message, secrets...)
	if len(e.Message) > maxUpstreamMessage {
		e.Message = e.Message[:maxUpstreamMessage]
	}
	// Do not relay 5xx codes verbatim as our own; mark them as bad gateway.
	if status >= 500 {
		e.Status = http.StatusBadGateway
	}
	return e
}

func chatID() string {
	return "chatcmpl-" + randomID(24)
}
