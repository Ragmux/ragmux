package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

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

// TestRequestIDIgnoresTheClientHeader: the request id reaches the
// ragmux.request_id span attribute, the X-Request-Id response header and the
// request log, so it may never come from the client.
//
// A charset filter was tried first and does not work, which is what this
// table is really about: every value below is a credential or a token, every
// one of them is unreserved ASCII of a plausible length, and so is a
// generated id. Nothing about the shape separates them. The middleware
// therefore does not read the header at all.
func TestRequestIDIgnoresTheClientHeader(t *testing.T) {
	var got []string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, middleware.GetReqID(r.Context()))
	}))

	sent := []string{
		// Ragmux's own gateway key: "sk-user-" plus 43 keyAlphabet
		// characters. It passed the charset filter this replaced.
		"sk-user-" + strings.Repeat("a", 43),
		"sk-mgmt-" + strings.Repeat("b", 43),
		"AKIAIOSFODNN7EXAMPLE",
		"sk-" + strings.Repeat("c", 48),
		"ghp_" + strings.Repeat("d", 36),
		strings.Repeat("0123456789abcdef", 2), // a plain hex token
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0",
		"",                            // no header at all
		strings.Repeat("x", 1<<16),    // a megabyte-ish header
		"has spaces and \n newlines ", // the shapes a filter does catch
	}
	for _, v := range sent {
		req := httptest.NewRequest("GET", "/healthz", nil)
		if v != "" {
			req.Header.Set("X-Request-Id", v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(got) != len(sent) {
		t.Fatalf("handler ran %d times, want %d", len(got), len(sent))
	}
	seen := map[string]bool{}
	for i, id := range got {
		if id == "" {
			t.Errorf("request %d got no id; one must always be generated", i)
		}
		// The point of the test: nothing the client sent survives.
		if sent[i] != "" && strings.Contains(id, sent[i]) {
			t.Errorf("request %d: the client's X-Request-Id reached the id as %q", i, id)
		}
		if !strings.HasPrefix(id, requestIDPrefix+"-") {
			t.Errorf("request %d: id %q is not one this process generated", i, id)
		}
		if seen[id] {
			t.Errorf("request %d: id %q was handed out twice", i, id)
		}
		seen[id] = true
	}
}
