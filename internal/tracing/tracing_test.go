package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// collector is a fake OTLP receiver.
type collector struct {
	mu       sync.Mutex
	payloads []map[string]any
	raw      []string
	status   int
	hits     int
	srv      *httptest.Server
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{status: http.StatusOK}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.hits++
		c.raw = append(c.raw, string(body))
		var p map[string]any
		if err := json.Unmarshal(body, &p); err == nil {
			c.payloads = append(c.payloads, p)
		}
		st := c.status
		c.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// payload is every OTLP body the exporter posted, concatenated, for tests
// that search the wire bytes rather than the decoded spans.
func (c *collector) payload(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.raw, "")
}

func (c *collector) spans(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, p := range c.payloads {
		rs, _ := p["resourceSpans"].([]any)
		for _, r := range rs {
			ss, _ := r.(map[string]any)["scopeSpans"].([]any)
			for _, s := range ss {
				sp, _ := s.(map[string]any)["spans"].([]any)
				for _, one := range sp {
					out = append(out, one.(map[string]any))
				}
			}
		}
	}
	return out
}

func newTracer(t *testing.T, c *collector, cfg Config) *Tracer {
	t.Helper()
	cfg.Endpoint = c.srv.URL
	if cfg.SampleRatio == 0 {
		cfg.SampleRatio = 1
	}
	tr := New(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tr.Shutdown(ctx)
	})
	return tr
}

// Package-level so the compiler cannot fold these into constants. With
// literals the attribute values box into an interface for free (a string
// constant's header is static, a small int comes from the runtime's cache)
// and the test would pass without proving anything about a real call site,
// where the status code and the provider name are variables.
var (
	sinkProvider = "openai"
	sinkStatus   = 503
)

func TestDisabledTracerAllocatesNothing(t *testing.T) {
	tr := New(Config{})
	if tr.Enabled() {
		t.Fatal("a tracer without an endpoint must be disabled")
	}
	ctx := context.Background()
	allocs := testing.AllocsPerRun(200, func() {
		_, span := tr.Start(ctx, "gateway.chat_completion", KindInternal)
		span.End()
	})
	if allocs != 0 {
		t.Fatalf("a disabled span site allocated %v times per run, want 0", allocs)
	}

	// The same for a site that sets attributes, written the way every site
	// in this repository is written: behind IsRecording.
	//
	// The guard is load-bearing, not decorative. Attr.Value is an any, so
	// each value is boxed before SetAttributes ever gets the chance to
	// decline it, and a variable int or string boxes onto the heap. Dropping
	// the guard costs one allocation per attribute on every request of a
	// deployment that has tracing switched off -- which is the default.
	// internal/provider's doRequest and internal/rag's embedQuery are the
	// hot sites this protects.
	withAttrs := testing.AllocsPerRun(200, func() {
		_, span := tr.Start(ctx, "provider.chat", KindClient)
		if span.IsRecording() {
			span.SetAttributes(
				String("gen_ai.system", sinkProvider),
				Int("http.response.status_code", sinkStatus),
			)
		}
		span.End()
	})
	if withAttrs != 0 {
		t.Fatalf("a disabled span site that sets attributes allocated %v times per run, want 0", withAttrs)
	}
}

func TestDisabledSpanMethodsAreSafe(t *testing.T) {
	tr := New(Config{})
	_, s := tr.Start(context.Background(), "x", KindClient)
	s.SetAttributes(String("a", "b"))
	s.RecordError(errors.New("boom"))
	s.SetStatusOK()
	s.End()
	s.End()
	if s.IsRecording() {
		t.Fatal("a no-op span must not report itself as recording")
	}
}

func TestShutdownFlushes(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{ServiceName: "ragmux", ServiceVersion: "0.4.0"})
	_, s := tr.Start(context.Background(), "http.server", KindServer)
	s.SetAttributes(String("http.route", "/v1/chat/completions"), Int("http.response.status_code", 200))
	s.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	spans := c.spans(t)
	if len(spans) != 1 {
		t.Fatalf("collector received %d spans, want 1", len(spans))
	}
	if spans[0]["name"] != "http.server" {
		t.Errorf("name = %v", spans[0]["name"])
	}
}

func TestOTLPEncodesIDsAsHexAndNanosAsStrings(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	_, s := tr.Start(context.Background(), "provider.chat", KindClient)
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tr.Shutdown(ctx)

	spans := c.spans(t)
	if len(spans) == 0 {
		t.Fatal("no spans exported")
	}
	sp := spans[0]

	// Lowercase hex, not base64: an explicit exception in the JSON mapping.
	tid, ok := sp["traceId"].(string)
	if !ok || len(tid) != 32 || strings.ToLower(tid) != tid {
		t.Errorf("traceId = %v, want 32 lowercase hex characters", sp["traceId"])
	}
	if _, err := strconv.ParseUint(tid[:16], 16, 64); err != nil {
		t.Errorf("traceId is not hex: %v", err)
	}
	sid, ok := sp["spanId"].(string)
	if !ok || len(sid) != 16 {
		t.Errorf("spanId = %v, want 16 hex characters", sp["spanId"])
	}

	// Nanosecond timestamps are JSON strings; as numbers they would lose
	// precision past 2^53 and some receivers reject them.
	if _, ok := sp["startTimeUnixNano"].(string); !ok {
		t.Errorf("startTimeUnixNano = %T, want a JSON string", sp["startTimeUnixNano"])
	}
	if _, ok := sp["endTimeUnixNano"].(string); !ok {
		t.Errorf("endTimeUnixNano = %T, want a JSON string", sp["endTimeUnixNano"])
	}
}

func TestOTLPIntAttributeIsAString(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	_, s := tr.Start(context.Background(), "gateway.chat_completion", KindInternal)
	s.SetAttributes(Int64("gen_ai.usage.input_tokens", 1234), Bool("ragmux.stream", true),
		Float("ragmux.ratio", 0.5))
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tr.Shutdown(ctx)

	raw := strings.Join(c.raw, "")
	if !strings.Contains(raw, `"intValue":"1234"`) {
		t.Errorf("int attribute not encoded as a string: %s", raw)
	}
	if !strings.Contains(raw, `"boolValue":true`) {
		t.Errorf("bool attribute missing: %s", raw)
	}
}

func TestParentChildShareATrace(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	ctx, parent := tr.Start(context.Background(), "http.server", KindServer)
	_, child := tr.Start(ctx, "rag.retrieve", KindInternal)
	child.End()
	parent.End()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tr.Shutdown(sctx)

	spans := c.spans(t)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if spans[0]["traceId"] != spans[1]["traceId"] {
		t.Error("parent and child are in different traces")
	}
	var childSpan map[string]any
	for _, s := range spans {
		if s["name"] == "rag.retrieve" {
			childSpan = s
		}
	}
	if childSpan["parentSpanId"] != parent.SpanContext().SpanID.String() {
		t.Errorf("parentSpanId = %v, want %s", childSpan["parentSpanId"], parent.SpanContext().SpanID)
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	sc := SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true}
	got, ok := ParseTraceparent(Traceparent(sc))
	if !ok {
		t.Fatal("a value we rendered must parse")
	}
	if got.TraceID != sc.TraceID || got.SpanID != sc.SpanID || !got.Sampled {
		t.Errorf("round trip lost data: %+v", got)
	}
	if !got.Remote {
		t.Error("a parsed context must be marked remote")
	}
}

func TestParseTraceparentRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"too few fields":   "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
		"short trace id":   "00-4bf92f35-00f067aa0ba902b7-01",
		"zero trace id":    "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero span id":     "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"invalid version":  "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"not hex":          "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01",
		"trailing on v 00": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
	}
	for name, v := range cases {
		if _, ok := ParseTraceparent(v); ok {
			t.Errorf("%s: %q was accepted", name, v)
		}
	}
}

func TestParseTraceparentAcceptsFutureVersion(t *testing.T) {
	// The specification asks that a higher version be parsed for its first
	// four fields rather than rejected outright.
	v := "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-somethingnew"
	sc, ok := ParseTraceparent(v)
	if !ok {
		t.Fatal("a future version must still parse")
	}
	if !sc.Sampled {
		t.Error("sampled flag lost")
	}
}

func TestExtractRequiresTrust(t *testing.T) {
	c := newCollector(t)
	parent := SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true}
	header := Traceparent(parent)

	untrusting := newTracer(t, c, Config{})
	ctx := untrusting.Extract(context.Background(), header, "")
	if _, ok := SpanContextFrom(ctx); ok {
		t.Error("an inbound traceparent was honoured without TRACING_TRUST_INCOMING")
	}

	trusting := newTracer(t, c, Config{TrustIncoming: true})
	ctx = trusting.Extract(context.Background(), header, "vendor=x")
	got, ok := SpanContextFrom(ctx)
	if !ok {
		t.Fatal("a trusted traceparent was ignored")
	}
	if got.TraceID != parent.TraceID {
		t.Error("trace id not adopted")
	}
	if got.State != "vendor=x" {
		t.Errorf("tracestate = %q", got.State)
	}
}

func TestExtractDropsOversizedTracestate(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{TrustIncoming: true})
	parent := SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true}
	ctx := tr.Extract(context.Background(), Traceparent(parent), strings.Repeat("a", maxTraceStateLen+1))
	got, _ := SpanContextFrom(ctx)
	if got.State != "" {
		t.Error("an oversized tracestate was carried into every upstream request")
	}
}

func TestSamplerIsDeterministicForATraceID(t *testing.T) {
	id := newTraceID()
	first := sampled(id, 0.5)
	for i := 0; i < 100; i++ {
		if sampled(id, 0.5) != first {
			t.Fatal("the sampler is not a pure function of the trace id")
		}
	}
	if sampled(id, 0) {
		t.Error("ratio 0 must sample nothing")
	}
	if !sampled(id, 1) {
		t.Error("ratio 1 must sample everything")
	}
}

func TestUnsampledTraceStillPropagates(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{SampleRatio: -1})
	ctx, s := tr.Start(context.Background(), "http.server", KindServer)
	if s.IsRecording() {
		t.Fatal("ratio 0 must not record")
	}
	// The downstream service must still be able to sample its own side into
	// the same trace id.
	if _, ok := SpanContextFrom(ctx); !ok {
		t.Fatal("an unsampled trace lost its context and cannot be propagated")
	}
}

// TestSpansAfterShutdownAreCounted: once the exporter has stopped, nothing
// reads its queue, so a span that ends afterwards can never reach the
// collector. It has to be counted as dropped rather than vanish -- the spans
// that outlive Shutdown belong to the requests that were still draining,
// which is exactly what an operator goes looking for after a restart.
func TestSpansAfterShutdownAreCounted(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})

	_, s := tr.Start(context.Background(), "http.server", KindServer)
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The span that ended before shutdown was flushed, not dropped.
	if got := tr.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d before any late span, want 0", got)
	}
	if n := len(c.spans(t)); n != 1 {
		t.Fatalf("collector received %d spans, want the one flushed at shutdown", n)
	}

	// A late span: a handler that was still draining when the server stopped.
	_, late := tr.Start(context.Background(), "provider.chat", KindClient)
	late.End()
	if got := tr.Dropped(); got != 1 {
		t.Errorf("Dropped = %d after a span ended past shutdown, want 1; "+
			"a span that cannot be exported must move the counter, not disappear", got)
	}
	if n := len(c.spans(t)); n != 1 {
		t.Errorf("collector received %d spans; the late one must not have been exported", n)
	}
}

// TestRecordErrorIsCapped: the cap is the backstop behind the rule that
// callers pass errors from a closed set. A caller that forgets must cost a
// truncated attribute, not a document, a stack trace or a base64 payload
// shipped to a third-party collector.
func TestRecordErrorIsCapped(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	_, s := tr.Start(context.Background(), "ingest.document", KindInternal)

	const secret = "SECRETPAYROLL-do-not-export"
	s.RecordError(errors.New(secret + strings.Repeat("x", 4096)))
	s.End()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	payload := c.payload(t)
	if payload == "" {
		t.Fatal("the collector received nothing; the test proves nothing")
	}
	if len(payload) > 4096 {
		t.Errorf("a %d-byte payload carried a 4 KiB error message; RecordError must cap it", len(payload))
	}
	if !strings.Contains(payload, truncated) {
		t.Errorf("a capped message must say it was truncated:\n%s", payload)
	}

	// A message that fits is recorded whole: the cap must not quietly
	// mangle the ordinary case.
	short := errors.New("embed_failed")
	if got := capMessage(short.Error()); got != short.Error() {
		t.Errorf("capMessage(%q) = %q, want it unchanged", short, got)
	}
}

// TestCapMessageKeepsValidUTF8: the capped message is exported as JSON text,
// so the cut may not land in the middle of a rune.
func TestCapMessageKeepsValidUTF8(t *testing.T) {
	// Every rune is 3 bytes, so some cap offsets necessarily fall inside one.
	for n := 1; n <= 200; n++ {
		got := capMessage(strings.Repeat("ç", n) + strings.Repeat("三", n))
		if !utf8.ValidString(got) {
			t.Fatalf("capMessage cut a rune in half at n=%d: %q", n, got)
		}
	}
}

func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	// Fill the queue past its capacity without letting the exporter drain it.
	for i := 0; i < queueSize*2; i++ {
		tr.exp.enqueue(&Span{tr: tr})
	}
	if tr.Dropped() == 0 {
		t.Fatal("a full queue must drop and count rather than block")
	}
}

func TestExportFailureIsCountedNotFatal(t *testing.T) {
	c := newCollector(t)
	c.mu.Lock()
	c.status = http.StatusInternalServerError
	c.mu.Unlock()
	tr := newTracer(t, c, Config{})
	_, s := tr.Start(context.Background(), "provider.chat", KindClient)
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = tr.Shutdown(ctx)
	if tr.ExportFailures() == 0 {
		t.Fatal("a rejected export must be counted")
	}
	c.mu.Lock()
	hits := c.hits
	c.mu.Unlock()
	if hits != 2 {
		t.Errorf("collector saw %d attempts, want 2 (one try plus one retry)", hits)
	}
}

func TestTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://c:4318":            "http://c:4318/v1/traces",
		"http://c:4318/":           "http://c:4318/v1/traces",
		"http://c:4318/v1/traces":  "http://c:4318/v1/traces",
		"http://c:4318/v1/traces/": "http://c:4318/v1/traces",
	} {
		if got := tracesURL(in); got != want {
			t.Errorf("tracesURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordErrorSetsStatus(t *testing.T) {
	c := newCollector(t)
	tr := newTracer(t, c, Config{})
	_, s := tr.Start(context.Background(), "provider.chat", KindClient)
	s.RecordError(errors.New("upstream refused the connection"))
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tr.Shutdown(ctx)

	spans := c.spans(t)
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	st, _ := spans[0]["status"].(map[string]any)
	if st["code"] != float64(codeError) {
		t.Errorf("status code = %v, want %d", st["code"], codeError)
	}
	if st["message"] != "upstream refused the connection" {
		t.Errorf("status message = %v", st["message"])
	}
}
