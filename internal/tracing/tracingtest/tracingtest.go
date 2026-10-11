// Package tracingtest is a test double for an OTLP/gRPC collector: an
// in-process HTTP/2 server on a loopback port speaking cleartext (h2c), and a
// minimal protobuf reader that decodes what the exporter sent. It needs no TLS
// material and no network beyond the loopback interface.
//
// The reader is written independently of the encoder under test and walks the
// wire format by field number, so an encoder bug cannot cancel out a decoder
// bug. It does not import package tracing, so tests inside that package can
// use it.
package tracingtest

import (
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// ExportPath is the full method name of TraceService.Export.
const ExportPath = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"

// StatusMode says where the collector puts its grpc-status.
type StatusMode int

const (
	// StatusInTrailer answers like a handler that ran: an empty response
	// message, then the status as a trailer. It is the default.
	StatusInTrailer StatusMode = iota
	// StatusInHeaders answers "trailers-only", the way a gRPC server rejects
	// a call before it reaches a handler (UNIMPLEMENTED, UNAUTHENTICATED):
	// the status rides in the response headers and there is no body.
	StatusInHeaders
	// StatusMissing answers 200 without any grpc-status at all.
	StatusMissing
)

// Request is one Export call as the collector received it.
type Request struct {
	Path, ContentType, TE, Authorization string
	Body                                 []byte // the whole gRPC frame
}

// Collector is the receiver. Create it with NewCollector.
type Collector struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []Request
	mode     StatusMode
	status   string
	message  string
}

// NewCollector starts a collector that answers every call with grpc-status 0
// until Reply says otherwise, and stops it when the test ends.
func NewCollector(t *testing.T) *Collector {
	t.Helper()
	c := &Collector{status: "0"}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(c.serve))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	c.srv.Config.Protocols = protos
	c.srv.Start()
	t.Cleanup(c.srv.Close)
	return c
}

// URL is the collector's endpoint, http://127.0.0.1:port.
func (c *Collector) URL() string { return c.srv.URL }

// Reply sets the grpc-status and grpc-message the next calls are answered
// with, and where the status is put. The message is sent as given, so a test
// can hand over a percent-encoded one.
func (c *Collector) Reply(mode StatusMode, status, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode, c.status, c.message = mode, status, message
}

// Requests returns every call received so far.
func (c *Collector) Requests() []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Request(nil), c.requests...)
}

func (c *Collector) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.requests = append(c.requests, Request{
		Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"),
		TE: r.Header.Get("Te"), Authorization: r.Header.Get("Authorization"), Body: body,
	})
	mode, status, message := c.mode, c.status, c.message
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/grpc")
	switch mode {
	case StatusMissing:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
	case StatusInHeaders:
		w.Header().Set("Grpc-Status", status)
		if message != "" {
			w.Header().Set("Grpc-Message", message)
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0}) // an empty ExportTraceServiceResponse
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", status)
		if message != "" {
			w.Header().Set(http.TrailerPrefix+"Grpc-Message", message)
		}
	}
}

// ---- a minimal protobuf reader, just enough to read the export back ----

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// varintInt64 reads a protobuf int64 varint: the value holds the two's
// complement bits, so one above MaxInt64 is a negative number.
func varintInt64(u uint64) int64 {
	if u <= math.MaxInt64 {
		return int64(u)
	}
	// Drop the sign bit, then subtract 2^63 from the remainder.
	return int64(u&math.MaxInt64) - math.MaxInt64 - 1
}

type field struct {
	num  int
	wire int
	v    uint64 // varint or fixed64
	data []byte // length-delimited
}

func parse(t *testing.T, b []byte) []field {
	t.Helper()
	var out []field
	for len(b) > 0 {
		tag, n := varint(t, b)
		b = b[n:]
		f := field{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case wireVarint:
			f.v, n = varint(t, b)
			b = b[n:]
		case wireFixed64:
			if len(b) < 8 {
				t.Fatal("truncated fixed64")
			}
			f.v = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case wireBytes:
			l, n := varint(t, b)
			b = b[n:]
			if uint64(len(b)) < l {
				t.Fatal("truncated length-delimited field")
			}
			f.data, b = b[:l], b[l:]
		default:
			t.Fatalf("unexpected wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out
}

func varint(t *testing.T, b []byte) (uint64, int) {
	t.Helper()
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	t.Fatal("bad varint")
	return 0, 0
}

func all(fs []field, num int) []field {
	var out []field
	for _, f := range fs {
		if f.num == num {
			out = append(out, f)
		}
	}
	return out
}

func one(t *testing.T, fs []field, num int) field {
	t.Helper()
	got := all(fs, num)
	if len(got) != 1 {
		t.Fatalf("field %d occurs %d times, want 1", num, len(got))
	}
	return got[0]
}

// attrs decodes a repeated KeyValue field into a Go map: string, bool, int64,
// float64 or []string (an array of strings).
func attrs(t *testing.T, fs []field, num int) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, kv := range all(fs, num) {
		parts := parse(t, kv.data)
		key := string(one(t, parts, 1).data)
		val := parse(t, one(t, parts, 2).data)
		if len(val) != 1 {
			t.Fatalf("AnyValue for %q has %d members, want 1", key, len(val))
		}
		switch v := val[0]; v.num {
		case 1:
			out[key] = string(v.data)
		case 2:
			out[key] = v.v == 1
		case 3:
			out[key] = varintInt64(v.v)
		case 4:
			out[key] = math.Float64frombits(v.v)
		case 5:
			var list []string
			for _, e := range all(parse(t, v.data), 1) {
				list = append(list, string(one(t, parse(t, e.data), 1).data))
			}
			out[key] = list
		default:
			t.Fatalf("unexpected AnyValue member %d", v.num)
		}
	}
	return out
}

// Span is one Span read back from the wire.
type Span struct {
	TraceID, SpanID, ParentID []byte // ParentID is nil on a root span
	Name                      string
	Kind                      uint64
	Start, End                uint64
	Attrs                     map[string]any
	StatusCode                uint64
	StatusMessage             string
}

// Unframe strips the 5-byte gRPC message header and checks it.
func Unframe(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 5 || body[0] != 0 {
		t.Fatalf("bad gRPC frame header: % x", body[:min(5, len(body))])
	}
	if n := binary.BigEndian.Uint32(body[1:5]); int(n) != len(body)-5 {
		t.Fatalf("frame says %d bytes, body has %d", n, len(body)-5)
	}
	return body[5:]
}

// DecodeExport reads an ExportTraceServiceRequest into its resource
// attributes, scope name and spans.
func DecodeExport(t *testing.T, msg []byte) (res map[string]any, scope string, spans []Span) {
	t.Helper()
	rs := parse(t, one(t, parse(t, msg), 1).data)
	res = attrs(t, parse(t, one(t, rs, 1).data), 1)
	ss := parse(t, one(t, rs, 2).data)
	scope = string(one(t, parse(t, one(t, ss, 1).data), 1).data)
	for _, sp := range all(ss, 2) {
		f := parse(t, sp.data)
		d := Span{
			TraceID: one(t, f, 1).data, SpanID: one(t, f, 2).data,
			Name: string(one(t, f, 5).data), Kind: one(t, f, 6).v,
			Start: one(t, f, 7).v, End: one(t, f, 8).v,
			Attrs: attrs(t, f, 9),
		}
		if p := all(f, 4); len(p) == 1 {
			d.ParentID = p[0].data
		}
		if st := all(f, 15); len(st) == 1 {
			sf := parse(t, st[0].data)
			d.StatusCode = one(t, sf, 3).v
			if m := all(sf, 2); len(m) == 1 {
				d.StatusMessage = string(m[0].data)
			}
		}
		spans = append(spans, d)
	}
	return res, scope, spans
}

// Spans decodes every export the collector has received, in arrival order.
func (c *Collector) Spans(t *testing.T) []Span {
	t.Helper()
	var out []Span
	for _, r := range c.Requests() {
		_, _, spans := DecodeExport(t, Unframe(t, r.Body))
		out = append(out, spans...)
	}
	return out
}
