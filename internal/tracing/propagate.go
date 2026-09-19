package tracing

import (
	"context"
	"encoding/hex"
	"strings"
)

// maxTraceStateLen bounds the tracestate we are willing to carry. The
// specification allows more, but a header forwarded to every upstream is a
// place an unbounded client-supplied value does not belong.
const maxTraceStateLen = 512

// SpanContext is the part of a span that crosses a process boundary.
type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Sampled bool
	// Remote reports that this context was parsed from an inbound header.
	Remote bool
	// State is the tracestate header, forwarded verbatim.
	State string
}

// IsValid reports whether both ids are usable.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

type scKey struct{}

// ContextWithSpanContext stores sc for propagation.
func ContextWithSpanContext(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, scKey{}, sc)
}

// SpanContextFrom reports the span context carried by ctx.
func SpanContextFrom(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(scKey{}).(SpanContext)
	if !ok || !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

// Traceparent renders sc as a W3C traceparent header value.
func Traceparent(sc SpanContext) string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID.String() + "-" + sc.SpanID.String() + "-" + flags
}

// ParseTraceparent parses a W3C traceparent header.
//
// A version above 00 is parsed for its first four fields and its remainder is
// ignored, which is the forward-compatibility rule the specification asks
// for: a future version must not make this a hard failure.
func ParseTraceparent(v string) (SpanContext, bool) {
	parts := strings.Split(v, "-")
	if len(parts) < 4 {
		return SpanContext{}, false
	}
	if len(parts[0]) != 2 {
		return SpanContext{}, false
	}
	if parts[0] == "ff" {
		// The specification reserves ff as invalid.
		return SpanContext{}, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return SpanContext{}, false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return SpanContext{}, false
	}
	var sc SpanContext
	tid, err := hex.DecodeString(parts[1])
	if err != nil {
		return SpanContext{}, false
	}
	sid, err := hex.DecodeString(parts[2])
	if err != nil {
		return SpanContext{}, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil {
		return SpanContext{}, false
	}
	copy(sc.TraceID[:], tid)
	copy(sc.SpanID[:], sid)
	if !sc.IsValid() {
		return SpanContext{}, false
	}
	sc.Sampled = flags[0]&0x01 == 1
	sc.Remote = true
	return sc, true
}

// Extract reads a traceparent and tracestate pair into a context, so a span
// started next continues the caller's trace.
//
// The tracer ignores the inbound header unless it is configured to trust it:
// without that gate a client can pin every request into one trace id and
// force the sampled flag on all of it.
func (t *Tracer) Extract(ctx context.Context, traceparent, tracestate string) context.Context {
	if !t.Enabled() || !t.trustIncoming || traceparent == "" {
		return ctx
	}
	sc, ok := ParseTraceparent(traceparent)
	if !ok {
		return ctx
	}
	if len(tracestate) <= maxTraceStateLen {
		sc.State = tracestate
	}
	return ContextWithSpanContext(ctx, sc)
}
