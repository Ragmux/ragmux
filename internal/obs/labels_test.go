package obs

import "testing"

// TestMethodIsBounded: r.Method is whatever the client wrote on the request
// line. Only the seven verbs this repository serves may become a label
// value; everything else has to collapse into one bucket, or a client can
// mint a series per request by inventing a verb per request.
func TestMethodIsBounded(t *testing.T) {
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if got := Method(m); got != m {
			t.Errorf("Method(%q) = %q, want it unchanged", m, got)
		}
	}
	for _, m := range []string{
		"PROPFIND",                   // a real verb this gateway does not serve
		"get",                        // the wrong case is a different label value
		"TRACE",                      // known to HTTP, not to this closed set
		"9f2c1b7e-4a3d-11ef-9c1d-02", // a verb invented per request
		"",                           // net/http's zero value
	} {
		if got := Method(m); got != otherLabel {
			t.Errorf("Method(%q) = %q, want %q", m, got, otherLabel)
		}
	}
}

// TestErrorTypeIsBounded: provider.Error.Type is copied out of the upstream
// response body by upstreamError, so the upstream picks the value. A known
// type survives; anything else is counted under "other" rather than minting
// a series.
func TestErrorTypeIsBounded(t *testing.T) {
	for _, ty := range []string{"upstream_error", "rate_limit_error", "invalid_request_error", "timeout"} {
		if got := ErrorType(ty); got != ty {
			t.Errorf("ErrorType(%q) = %q, want it unchanged", ty, got)
		}
	}
	// What a hostile or merely chatty upstream can put in the field.
	for _, ty := range []string{
		"error_0f3a_b21c",             // an id per response
		"RATE_LIMIT_ERROR",            // the wrong case is a different series
		"you are over your quota, id", // a sentence
	} {
		if got := ErrorType(ty); got != otherLabel {
			t.Errorf("ErrorType(%q) = %q, want %q", ty, got, otherLabel)
		}
	}
}

// TestSafeRequestIDRejectsClientInput: chi's middleware.RequestID echoes the
// client's X-Request-Id header verbatim, so GetReqID returns bytes an
// outsider chose. PRD rule 10 keeps a header off a span, so anything not
// shaped like a generated id is dropped whole -- a trimmed secret is still a
// secret.
func TestSafeRequestIDRejectsClientInput(t *testing.T) {
	// chi's own shape, and the plain ids a well-behaved proxy sends.
	for _, id := range []string{
		"ragmux/server-01/000001",
		"c7f1a2b3d4e5",
		"01JD8Q4X9K2M7P.reqid",
		"YWJjZGVm+/=",
	} {
		if got := safeRequestID(id); got != id {
			t.Errorf("safeRequestID(%q) = %q, want it kept", id, got)
		}
	}

	long := make([]byte, maxRequestIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	for name, id := range map[string]string{
		"empty":            "",
		"too long":         string(long),
		"newline":          "abc\ndef",
		"quote":            `abc"def`,
		"space":            "abc def",
		"a parked secret":  "sk-live-51H8xYzAbCdEf GhIj",
		"non-ascii":        "abcçdef",
		"control byte":     "abc\x00def",
		"json fragment":    `{"a":1}`,
		"an entire header": "Bearer sk-user-0123456789",
	} {
		if got := safeRequestID(id); got != "" {
			t.Errorf("safeRequestID(%s) = %q, want it dropped", name, got)
		}
	}
}
