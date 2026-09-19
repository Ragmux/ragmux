package tracing

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// TraceID identifies a trace.
type TraceID [16]byte

// SpanID identifies one span within a trace.
type SpanID [8]byte

// String renders the id as lowercase hex, which is how OTLP/JSON and the
// traceparent header both spell it.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// String renders the id as lowercase hex.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// IsValid reports whether the id is non-zero. The all-zero id is reserved and
// a traceparent carrying one must be rejected.
func (t TraceID) IsValid() bool { return t != TraceID{} }

// IsValid reports whether the id is non-zero.
func (s SpanID) IsValid() bool { return s != SpanID{} }

func newTraceID() TraceID {
	var t TraceID
	_, _ = rand.Read(t[:])
	// crypto/rand cannot fail on any supported platform, but an all-zero id
	// is invalid by specification, so make one impossible rather than rely on
	// that.
	if !t.IsValid() {
		t[0] = 1
	}
	return t
}

func newSpanID() SpanID {
	var s SpanID
	_, _ = rand.Read(s[:])
	if !s.IsValid() {
		s[0] = 1
	}
	return s
}

// sampled applies the trace-ID ratio rule: the decision is a function of the
// id alone, so every service that sees this trace samples it identically
// without having to agree on anything else.
func sampled(t TraceID, ratio float64) bool {
	if ratio <= 0 {
		return false
	}
	if ratio >= 1 {
		return true
	}
	return binary.BigEndian.Uint64(t[8:]) < uint64(ratio*math.MaxUint64)
}
