package tracing

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/tracing/tracingtest"
)

func TestGRPCExporterDeliversSpans(t *testing.T) {
	c := tracingtest.NewCollector(t)
	tr := newTracerWith(t, Config{
		Endpoint: c.URL(), Protocol: ProtocolGRPC, ServiceName: "ragmux-test",
		Headers: map[string]string{"Authorization": "Bearer t0ken"},
	})

	ctx, parent := tr.Start(context.Background(), "chat gpt-4o", KindInternal)
	_, child := tr.Start(ctx, "attempt 1", KindClient)
	child.SetAttributes(
		String("gen_ai.operation.name", "chat"),
		Int("attempt", 1),
		Int64("negative", -7),
		Bool("ragmux.stream", false),
		Float("ratio", 0.5),
		StringSlice("gen_ai.response.finish_reasons", []string{"stop", "length"}),
	)
	child.RecordError(io.ErrUnexpectedEOF)
	child.End()
	parent.End()
	flush(t, tr)

	reqs := c.Requests()
	if len(reqs) != 1 {
		t.Fatalf("collector saw %d export calls, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != tracingtest.ExportPath {
		t.Errorf("path = %q", r.Path)
	}
	if r.ContentType != "application/grpc" || r.TE != "trailers" {
		t.Errorf("content-type %q, te %q; want application/grpc and trailers", r.ContentType, r.TE)
	}
	if r.Authorization != "Bearer t0ken" {
		t.Errorf("a configured header must be sent on every export, got authorization %q", r.Authorization)
	}

	res, scope, spans := tracingtest.DecodeExport(t, tracingtest.Unframe(t, r.Body))
	if res["service.name"] != "ragmux-test" || scope != "ragmux" {
		t.Errorf("resource %v scope %q", res, scope)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	// The child ends first, so it leaves first.
	ch, pa := spans[0], spans[1]
	if ch.Name != "attempt 1" || pa.Name != "chat gpt-4o" {
		t.Errorf("names = %q, %q", ch.Name, pa.Name)
	}
	if string(ch.TraceID) != string(pa.TraceID) || string(ch.ParentID) != string(pa.SpanID) {
		t.Errorf("the child must share the trace and name the parent: %x/%x parent %x vs %x", ch.TraceID, pa.TraceID, ch.ParentID, pa.SpanID)
	}
	if pa.ParentID != nil {
		t.Errorf("a root span must carry no parent id, got %x", pa.ParentID)
	}
	if ch.Kind != uint64(KindClient) || pa.Kind != uint64(KindInternal) {
		t.Errorf("kinds = %d, %d", ch.Kind, pa.Kind)
	}
	if ch.Start == 0 || ch.End < ch.Start {
		t.Errorf("timestamps start=%d end=%d", ch.Start, ch.End)
	}
	if ch.StatusCode != uint64(codeError) || ch.StatusMessage != io.ErrUnexpectedEOF.Error() {
		t.Errorf("status = %d %q", ch.StatusCode, ch.StatusMessage)
	}
	if pa.StatusCode != 0 {
		t.Errorf("an unset status must stay unset, got %d", pa.StatusCode)
	}
	want := map[string]any{
		"gen_ai.operation.name": "chat", "attempt": int64(1), "negative": int64(-7),
		"ragmux.stream": false, "ratio": 0.5,
		"exception.message": io.ErrUnexpectedEOF.Error(),
	}
	for k, v := range want {
		if ch.Attrs[k] != v {
			t.Errorf("attr %q = %#v, want %#v", k, ch.Attrs[k], v)
		}
	}
	if fr, _ := ch.Attrs["gen_ai.response.finish_reasons"].([]string); strings.Join(fr, ",") != "stop,length" {
		t.Errorf("finish reasons = %v", ch.Attrs["gen_ai.response.finish_reasons"])
	}
}

func TestGRPCExporterCountsARejectedExport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   tracingtest.StatusMode
		status string
	}{
		{"status in the trailer", tracingtest.StatusInTrailer, "14"},
		// A server that rejects the call before a handler runs answers
		// "trailers-only": the status is in the headers and there is no body.
		{"status in the headers (trailers-only)", tracingtest.StatusInHeaders, "16"},
		{"no status at all", tracingtest.StatusMissing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tracingtest.NewCollector(t)
			c.Reply(tc.mode, tc.status, "collector says no")
			tr := newTracerWith(t, Config{Endpoint: c.URL(), Protocol: ProtocolGRPC})
			_, s := tr.Start(context.Background(), "chat m", KindInternal)
			s.End()
			flush(t, tr)

			if tr.ExportFailures() == 0 {
				t.Fatal("a call without grpc-status 0 must be counted as an export failure")
			}
			if got := len(c.Requests()); got != 2 {
				t.Errorf("collector saw %d calls, want 2 (one try plus one retry)", got)
			}
		})
	}
}

// postErr sends one frame to a collector that answers as configured and
// returns what the transport made of the reply.
func postErr(t *testing.T, mode tracingtest.StatusMode, status, message string) error {
	t.Helper()
	c := tracingtest.NewCollector(t)
	c.Reply(mode, status, message)
	tp := newGRPCTransport(c.URL(), false, nil, 5*time.Second)
	body, err := tp.encode(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tp.post(body)
}

func TestGRPCStatusIsReadFromTheRightPlace(t *testing.T) {
	if err := postErr(t, tracingtest.StatusInTrailer, "0", ""); err != nil {
		t.Errorf("status 0 in the trailer: %v", err)
	}
	if err := postErr(t, tracingtest.StatusInHeaders, "0", ""); err != nil {
		t.Errorf("status 0 in the headers: %v", err)
	}
	err := postErr(t, tracingtest.StatusInHeaders, "16", "no token")
	if err == nil || !strings.Contains(err.Error(), "grpc-status 16") || !strings.Contains(err.Error(), "no token") {
		t.Errorf("a trailers-only rejection must surface its real code and message, got %v", err)
	}
	err = postErr(t, tracingtest.StatusMissing, "", "")
	if err == nil || !strings.Contains(err.Error(), "no grpc-status") {
		t.Errorf("a reply without a status must say so, got %v", err)
	}
}

func TestGRPCMessageIsDecodedAndCapped(t *testing.T) {
	err := postErr(t, tracingtest.StatusInTrailer, "13", "caf%C3%A9%20%25")
	if err == nil || !strings.Contains(err.Error(), "café %") {
		t.Errorf("a percent-encoded grpc-message must be decoded, got %v", err)
	}
	// A value that is not valid percent-encoding is kept as received.
	if got := statusMessage("100% broken"); got != "100% broken" {
		t.Errorf("undecodable message = %q", got)
	}
	// The cap must not cut a multi-byte rune in half.
	for n := maxErrorMessage - 3; n <= maxErrorMessage+3; n++ {
		got := statusMessage(strings.Repeat("三", n))
		if !utf8.ValidString(got) {
			t.Fatalf("statusMessage cut a rune in half at n=%d", n)
		}
	}
}

// A bare host:port is TLS unless Insecure is set; the h2c collector can only
// be reached once it is.
func TestGRPCInsecureOptsABareEndpointOutOfTLS(t *testing.T) {
	c := tracingtest.NewCollector(t)
	bare := strings.TrimPrefix(c.URL(), "http://")

	tr := newTracerWith(t, Config{Endpoint: bare, Protocol: ProtocolGRPC, Insecure: true})
	_, s := tr.Start(context.Background(), "chat m", KindInternal)
	s.End()
	flush(t, tr)
	if tr.ExportFailures() != 0 || len(c.Requests()) != 1 {
		t.Errorf("insecure bare endpoint: %d failures, %d calls; want 0 and 1", tr.ExportFailures(), len(c.Requests()))
	}

	tr = newTracerWith(t, Config{Endpoint: bare, Protocol: ProtocolGRPC})
	_, s = tr.Start(context.Background(), "chat m", KindInternal)
	s.End()
	flush(t, tr)
	if tr.ExportFailures() == 0 {
		t.Error("without Insecure a bare endpoint is TLS, which a cleartext collector must refuse")
	}
}

func TestGRPCTarget(t *testing.T) {
	const path = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
	for _, tc := range []struct {
		in        string
		target    string
		plaintext bool
		insecure  bool
		bad       bool
	}{
		{in: "http://otel:4317", target: "http://otel:4317" + path, plaintext: true},
		{in: "https://otel:4317", target: "https://otel:4317" + path},
		{in: "otel:4317", target: "https://otel:4317" + path},
		{in: " https://otel:4317/ignored/path ", target: "https://otel:4317" + path},
		{in: "[::1]:4317", target: "https://[::1]:4317" + path},
		// insecure only matters when no scheme says otherwise.
		{in: "otel:4317", insecure: true, target: "http://otel:4317" + path, plaintext: true},
		{in: "https://otel:4317", insecure: true, target: "https://otel:4317" + path},
		{in: "http://otel:4317", insecure: true, target: "http://otel:4317" + path, plaintext: true},
		{in: "grpc://otel:4317", bad: true},
		{in: "http://", bad: true},
		{in: "", bad: true},
	} {
		target, plaintext, err := grpcTarget(tc.in, tc.insecure)
		if tc.bad {
			if err == nil {
				t.Errorf("grpcTarget(%q) = %q, want an error", tc.in, target)
			}
			if ValidateEndpoint(ProtocolGRPC, tc.in) == nil {
				t.Errorf("ValidateEndpoint(grpc, %q) = nil, want an error", tc.in)
			}
			continue
		}
		if err != nil || target != tc.target || plaintext != tc.plaintext {
			t.Errorf("grpcTarget(%q, %v) = %q, %v, %v; want %q, %v", tc.in, tc.insecure, target, plaintext, err, tc.target, tc.plaintext)
		}
	}
	if err := ValidateEndpoint(ProtocolHTTP, "anything goes"); err != nil {
		t.Errorf("the HTTP transport is not validated here: %v", err)
	}
}

func TestHTTPStaysTheDefaultTransport(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	if _, ok := tr.exp.tp.(*httpTransport); !ok {
		t.Fatalf("default transport is %T, want *httpTransport", tr.exp.tp)
	}
}

// newTracerWith starts a tracer from a full Config (newTracer fills in an
// HTTP collector's endpoint) and shuts it down on cleanup.
func newTracerWith(t *testing.T, cfg Config) *Tracer {
	t.Helper()
	if cfg.SampleRatio == 0 {
		cfg.SampleRatio = 1
	}
	tr := New(cfg)
	t.Cleanup(func() { flush(t, tr) })
	return tr
}
