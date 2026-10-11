package tracing

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OTLP/gRPC, hand-written.
//
// Collectors accept traces on a gRPC port (4317 by convention) as one unary
// call, opentelemetry.proto.collector.trace.v1.TraceService/Export. A unary
// gRPC call is an HTTP/2 POST whose body is one length-prefixed protobuf
// message and whose outcome is reported in the grpc-status trailer, all of
// which net/http can do. Writing those few messages by hand keeps the module
// free of google.golang.org/grpc and the protobuf runtime, which together are
// larger than the rest of this package and its dependencies combined.
//
// What this deliberately is not: a general gRPC client. There is no
// compression, no per-RPC deadline header, no service config and no
// load balancing. The server's partial_success field in the response is not
// read: a collector that drops some spans is the collector's to report.

// exportPath is the full method name of TraceService.Export.
const exportPath = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"

// maxResponseBytes bounds how much of a collector's reply is read. A healthy
// ExportTraceServiceResponse is a few bytes.
const maxResponseBytes = 1 << 20

// ValidateEndpoint reports whether endpoint is usable for the protocol. The
// HTTP transport accepts anything the caller hands it (a bad URL fails at the
// first export); the gRPC one has a stricter syntax worth rejecting at start.
func ValidateEndpoint(p Protocol, endpoint string) error {
	if p != ProtocolGRPC {
		return nil
	}
	_, _, err := grpcTarget(endpoint, false)
	return err
}

// grpcTarget splits an OTLP/gRPC endpoint into the HTTP/2 URL of the Export
// method and whether the connection is plaintext. An http:// scheme is
// plaintext, an https:// scheme is TLS, and a bare host:port is TLS unless
// insecure is set: the safe default is the one that needs no opt-in to be
// correct. A scheme always wins over insecure. Any path in the
// endpoint is ignored; a gRPC target names a server, not a route.
func grpcTarget(endpoint string, insecure bool) (target string, plaintext bool, err error) {
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		scheme := "https://"
		if insecure {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("invalid OTLP/gRPC endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, fmt.Errorf("invalid OTLP/gRPC endpoint %q (scheme must be http or https, or omitted)", endpoint)
	}
	if u.Hostname() == "" {
		return "", false, fmt.Errorf("invalid OTLP/gRPC endpoint %q (no host)", endpoint)
	}
	return u.Scheme + "://" + u.Host + exportPath, u.Scheme == "http", nil
}

// grpcTransport exports spans over OTLP/gRPC.
type grpcTransport struct {
	target  string
	headers map[string]string
	client  *http.Client
	// initErr is an endpoint that failed to parse. It is reported by every
	// post so the failure is counted and logged like any other export error;
	// config validation normally catches it before this point.
	initErr error
}

func newGRPCTransport(endpoint string, insecure bool, headers map[string]string, timeout time.Duration) *grpcTransport {
	target, plaintext, err := grpcTarget(endpoint, insecure)
	protos := new(http.Protocols)
	if plaintext {
		protos.SetUnencryptedHTTP2(true)
	} else {
		protos.SetHTTP2(true)
	}
	return &grpcTransport{
		target:  target,
		headers: headers,
		initErr: err,
		// Like the HTTP transport, a plain client: the collector address is
		// operator-configured and usually private, so the outbound SSRF guard
		// does not apply. Proxy is left nil, so no environment proxy is used.
		client: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{Protocols: protos, IdleConnTimeout: 90 * time.Second},
		},
	}
}

// encode builds the framed ExportTraceServiceRequest: one compressed-flag byte
// (always 0) and a big-endian message length, then the message.
func (g *grpcTransport) encode(res []Attr, batch []*Span) ([]byte, error) {
	msg := encodeExportRequest(res, batch)
	size := uint64(len(msg))
	if size > math.MaxUint32 {
		return nil, fmt.Errorf("export request of %d bytes exceeds the gRPC frame limit", len(msg))
	}
	out := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:], uint32(size))
	return append(out, msg...), nil
}

func (g *grpcTransport) post(body []byte) error {
	if g.initErr != nil {
		return g.initErr
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	req.Header.Set("User-Agent", "ragmux-tracing")
	for k, v := range g.headers {
		req.Header.Set(k, v)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("collector returned HTTP %d", resp.StatusCode)
	}
	// Trailers are only complete once the body has been read to its end.
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
		return err
	}
	// A call that fails before any message is sent back ("trailers-only")
	// carries grpc-status in the headers instead.
	status := resp.Trailer.Get("Grpc-Status")
	message := resp.Trailer.Get("Grpc-Message")
	if status == "" {
		status = resp.Header.Get("Grpc-Status")
		message = resp.Header.Get("Grpc-Message")
	}
	switch status {
	case "0":
		return nil
	case "":
		return fmt.Errorf("collector reply carried no grpc-status")
	}
	return fmt.Errorf("collector returned grpc-status %s: %s", status, statusMessage(message))
}

// statusMessage readies a grpc-message for a log line. The header is
// percent-encoded (gRPC over HTTP/2: bytes outside printable ASCII and "%"
// travel as %XX); an undecodable value is kept as received. It is text the
// collector chose, so it is capped, on a rune boundary, like any error text
// that reaches a span.
func statusMessage(raw string) string {
	if dec, err := url.PathUnescape(raw); err == nil {
		raw = dec
	}
	return capMessage(raw)
}

// ---- protobuf encoding ----
//
// The field numbers below are those of opentelemetry-proto v1 (trace.proto,
// common.proto, resource.proto, trace_service.proto). Only the fields the JSON
// payload also carries are written.

const (
	wireVarint  uint64 = 0
	wireFixed64 uint64 = 1
	wireBytes   uint64 = 2
)

// nonNegative converts an enum-like int to its unsigned wire value. Negative
// input cannot occur (the span kinds and status codes are 0..3); it maps to 0
// rather than wrapping.
func nonNegative(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// int64Varint is the protobuf int64 wire value: the two's complement bits.
func int64Varint(v int64) uint64 {
	if v >= 0 {
		return uint64(v)
	}
	// The low 63 bits are the same in both representations; the sign bit is set.
	return uint64(v&math.MaxInt64) | 1<<63
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wire uint64) []byte {
	return appendVarint(b, field<<3|wire)
}

func appendBytesField(b []byte, field uint64, v []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendStringField(b []byte, field uint64, v string) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendVarintField(b []byte, field uint64, v uint64) []byte {
	return appendVarint(appendTag(b, field, wireVarint), v)
}

func appendFixed64Field(b []byte, field uint64, v uint64) []byte {
	b = appendTag(b, field, wireFixed64)
	return binary.LittleEndian.AppendUint64(b, v)
}

// encodeExportRequest is ExportTraceServiceRequest: one ResourceSpans holding
// one ScopeSpans, which is the same shape the JSON payload has.
func encodeExportRequest(res []Attr, spans []*Span) []byte {
	var scope []byte
	scope = appendBytesField(scope, 1, appendStringField(nil, 1, "ragmux")) // InstrumentationScope{name}
	for _, s := range spans {
		scope = appendBytesField(scope, 2, encodeSpan(s))
	}

	var resource []byte
	for _, a := range res {
		if kv, ok := encodeKeyValue(a); ok {
			resource = appendBytesField(resource, 1, kv)
		}
	}

	var rs []byte
	rs = appendBytesField(rs, 1, resource)
	rs = appendBytesField(rs, 2, scope)
	return appendBytesField(nil, 1, rs)
}

func encodeSpan(s *Span) []byte {
	s.mu.Lock()
	attrs := append([]Attr(nil), s.attrs...)
	status, msg, name := s.status, s.msg, s.name
	s.mu.Unlock()

	var b []byte
	b = appendBytesField(b, 1, s.sc.TraceID[:])
	b = appendBytesField(b, 2, s.sc.SpanID[:])
	if s.parent.IsValid() {
		b = appendBytesField(b, 4, s.parent[:])
	}
	b = appendStringField(b, 5, name)
	b = appendVarintField(b, 6, nonNegative(int(s.kind)))
	b = appendFixed64Field(b, 7, uint64(s.start.UnixNano()))
	b = appendFixed64Field(b, 8, uint64(s.end.UnixNano()))
	for _, a := range attrs {
		if kv, ok := encodeKeyValue(a); ok {
			b = appendBytesField(b, 9, kv)
		}
	}
	if status != codeUnset || msg != "" {
		var st []byte
		if msg != "" {
			st = appendStringField(st, 2, msg)
		}
		st = appendVarintField(st, 3, nonNegative(int(status)))
		b = appendBytesField(b, 15, st)
	}
	return b
}

// encodeKeyValue is KeyValue{key, AnyValue}. An unsupported value type is
// dropped, as in the JSON encoding.
func encodeKeyValue(a Attr) ([]byte, bool) {
	v, ok := encodeAnyValue(a.Value)
	if !ok {
		return nil, false
	}
	b := appendStringField(nil, 1, a.Key)
	return appendBytesField(b, 2, v), true
}

// encodeAnyValue is AnyValue, whose oneof member is always written, even when
// it holds the zero value.
func encodeAnyValue(v any) ([]byte, bool) {
	switch v := v.(type) {
	case string:
		return appendStringField(nil, 1, v), true
	case bool:
		var n uint64
		if v {
			n = 1
		}
		return appendVarintField(nil, 2, n), true
	case int64:
		return appendVarintField(nil, 3, int64Varint(v)), true
	case float64:
		return appendFixed64Field(nil, 4, math.Float64bits(v)), true
	case []string:
		var arr []byte
		for _, e := range v {
			arr = appendBytesField(arr, 1, appendStringField(nil, 1, e))
		}
		return appendBytesField(nil, 5, arr), true
	}
	return nil, false
}
