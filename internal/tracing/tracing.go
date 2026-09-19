// Package tracing emits OpenTelemetry spans over OTLP/HTTP with the JSON
// encoding, using nothing outside the standard library.
//
// It is a deliberately small subset. There is no auto-instrumentation: only
// the span sites this repository creates by hand exist, so a slow database
// query shows up as unexplained time inside its parent rather than as a span
// of its own. There are no events, no links, no baggage, no metrics or logs
// signal, no gRPC transport and no resource auto-detection.
//
// Tracing is off unless an endpoint is configured. While it is off a span
// site costs one atomic load and one branch, and allocates nothing.
package tracing

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Kind is the span kind. The values are OTLP's own numbering.
type Kind int

const (
	// KindInternal is work inside this process.
	KindInternal Kind = 1
	// KindServer is a request this process handled.
	KindServer Kind = 2
	// KindClient is a call this process made to something else.
	KindClient Kind = 3
)

// Code is the span status.
type Code int

const (
	codeUnset Code = 0
	codeOK    Code = 1
	codeError Code = 2
)

// Attr is one span attribute. Value must be a string, int64, float64 or bool;
// anything else is dropped at export time. The mapping for richer types is
// not worth hand-writing for the handful of attributes this repository sets.
type Attr struct {
	Key   string
	Value any
}

// String builds a string attribute.
func String(k, v string) Attr { return Attr{Key: k, Value: v} }

// Int builds an integer attribute.
func Int(k string, v int) Attr { return Attr{Key: k, Value: int64(v)} }

// Int64 builds an integer attribute.
func Int64(k string, v int64) Attr { return Attr{Key: k, Value: v} }

// Float builds a float attribute.
func Float(k string, v float64) Attr { return Attr{Key: k, Value: v} }

// Bool builds a boolean attribute.
func Bool(k string, v bool) Attr { return Attr{Key: k, Value: v} }

// Span is one unit of work.
//
// No attribute this repository sets may carry message content, a retrieval
// query, passage text, a filename, a system prompt or a credential. A span is
// exported to a third-party collector; it is metadata, not payload.
type Span struct {
	tr     *Tracer
	sc     SpanContext
	parent SpanID
	name   string
	kind   Kind
	start  time.Time
	end    time.Time

	mu     sync.Mutex
	attrs  []Attr
	status Code
	msg    string
	ended  bool
}

// noSpan is handed out whenever tracing is off or a trace is not sampled.
// Its nil tracer makes every method a no-op, so callers never nil-check.
var noSpan = &Span{}

// Tracer creates spans and hands finished ones to its exporter.
type Tracer struct {
	exp         *exporter
	sampleRatio float64
	// trustIncoming decides whether a client's traceparent is honoured.
	// Left off, a client could pin every request into one trace and force
	// the sampled flag on all of it, which is a cheap way to flood the
	// collector from outside.
	trustIncoming bool
	log           *slog.Logger
}

// Config configures a tracer.
type Config struct {
	// Endpoint is the OTLP/HTTP base or full traces URL. Empty disables
	// tracing entirely.
	Endpoint string
	// Headers are sent with every export request.
	Headers map[string]string
	// ServiceName defaults to "ragmux".
	ServiceName string
	// ServiceVersion is the build version.
	ServiceVersion string
	// ResourceAttrs are extra resource attributes.
	ResourceAttrs map[string]string
	// SampleRatio is the head-based sampling probability for new traces.
	SampleRatio float64
	// TrustIncoming honours a client-supplied traceparent.
	TrustIncoming bool
	// Timeout bounds one export request.
	Timeout time.Duration
	Logger  *slog.Logger
}

// New starts a tracer and its export goroutine. An empty Endpoint returns a
// disabled tracer whose spans cost nothing.
func New(cfg Config) *Tracer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Endpoint == "" {
		return &Tracer{log: cfg.Logger}
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "ragmux"
	}
	if cfg.SampleRatio <= 0 {
		cfg.SampleRatio = 0
	}
	if cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}
	t := &Tracer{
		exp:           newExporter(cfg),
		sampleRatio:   cfg.SampleRatio,
		trustIncoming: cfg.TrustIncoming,
		log:           cfg.Logger,
	}
	return t
}

// Enabled reports whether spans are recorded.
func (t *Tracer) Enabled() bool { return t != nil && t.exp != nil }

// Start begins a span. The returned context carries it as the parent of any
// span started beneath it.
func (t *Tracer) Start(ctx context.Context, name string, kind Kind) (context.Context, *Span) {
	if !t.Enabled() {
		return ctx, noSpan
	}
	parent, hasParent := SpanContextFrom(ctx)
	sc := SpanContext{SpanID: newSpanID()}
	var parentID SpanID
	if hasParent {
		sc.TraceID = parent.TraceID
		sc.Sampled = parent.Sampled
		sc.State = parent.State
		parentID = parent.SpanID
	} else {
		sc.TraceID = newTraceID()
		sc.Sampled = sampled(sc.TraceID, t.sampleRatio)
	}
	// An unsampled trace still needs its context to propagate, so the
	// downstream service can sample its own side into the same trace id.
	ctx = ContextWithSpanContext(ctx, sc)
	if !sc.Sampled {
		return ctx, noSpan
	}
	s := &Span{tr: t, sc: sc, parent: parentID, name: name, kind: kind, start: time.Now()}
	return context.WithValue(ctx, spanKey{}, s), s
}

type spanKey struct{}

// SpanFromContext returns the span started in ctx, or a no-op span.
func SpanFromContext(ctx context.Context) *Span {
	if s, ok := ctx.Value(spanKey{}).(*Span); ok {
		return s
	}
	return noSpan
}

// SetAttributes adds attributes to the span.
func (s *Span) SetAttributes(attrs ...Attr) {
	if s.tr == nil {
		return
	}
	s.mu.Lock()
	s.attrs = append(s.attrs, attrs...)
	s.mu.Unlock()
}

// RecordError marks the span failed. Only the error's message is recorded,
// and callers must pass errors that have already been redacted: provider
// errors travel through transportError, which strips credentials.
func (s *Span) RecordError(err error) {
	if s.tr == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.status = codeError
	s.msg = err.Error()
	s.attrs = append(s.attrs, String("exception.message", err.Error()))
	s.mu.Unlock()
}

// SetStatusOK marks the span explicitly successful.
func (s *Span) SetStatusOK() {
	if s.tr == nil {
		return
	}
	s.mu.Lock()
	if s.status != codeError {
		s.status = codeOK
	}
	s.mu.Unlock()
}

// IsRecording reports whether this span will be exported.
//
// Start and End cost nothing on a disabled tracer, but building the
// attributes to pass to SetAttributes does: the variadic slice escapes into
// append, so it is heap-allocated whether or not anything reads it. A hot
// call site that assembles several attributes should guard them with this.
func (s *Span) IsRecording() bool { return s.tr != nil }

// SpanContext reports the span's identity, for propagation.
func (s *Span) SpanContext() SpanContext {
	if s.tr == nil {
		return SpanContext{}
	}
	return s.sc
}

// End finishes the span and queues it for export. Calling it twice is
// harmless: the second call is ignored rather than exporting a duplicate.
func (s *Span) End() {
	if s.tr == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = time.Now()
	s.mu.Unlock()
	s.tr.exp.enqueue(s)
}

// Shutdown flushes queued spans and stops the exporter.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	return t.exp.shutdown(ctx)
}

// Dropped reports spans discarded because the export queue was full.
func (t *Tracer) Dropped() uint64 {
	if !t.Enabled() {
		return 0
	}
	return t.exp.dropped.Load()
}

// ExportFailures reports export attempts that were abandoned.
func (t *Tracer) ExportFailures() uint64 {
	if !t.Enabled() {
		return 0
	}
	return t.exp.failures.Load()
}
