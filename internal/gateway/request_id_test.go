package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestValidRequestID(t *testing.T) {
	for _, ok := range []string{"a", "abc-123", "req_1.2:3", strings.Repeat("x", maxRequestIDLen)} {
		if !validRequestID(ok) {
			t.Errorf("validRequestID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " ", "a b", "a\nb", "a\x00b", `a"b`, "a/b", "a=b", "a,b", "é", strings.Repeat("x", maxRequestIDLen+1)} {
		if validRequestID(bad) {
			t.Errorf("validRequestID(%q) = true, want false", bad)
		}
	}
}

func TestCallLoggerRequestIDFields(t *testing.T) {
	logged := func(generated, requestID string) map[string]any {
		var buf bytes.Buffer
		callLogger(slog.New(slog.NewJSONHandler(&buf, nil)), 7, generated, requestID).Info("x")
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", buf.String(), err)
		}
		return line
	}

	// Generated id in use: one field, the one the access log carries.
	line := logged("host/abc-1", "host/abc-1")
	if line["req_id"] != "host/abc-1" {
		t.Errorf("req_id = %v", line["req_id"])
	}
	if _, has := line["client_req_id"]; has {
		t.Errorf("client_req_id present without a client id: %v", line)
	}

	// Client id in use: req_id stays the generated one, the client's is beside it.
	line = logged("host/abc-2", "trace-42")
	if line["req_id"] != "host/abc-2" || line["client_req_id"] != "trace-42" {
		t.Errorf("fields = req_id %v, client_req_id %v", line["req_id"], line["client_req_id"])
	}

	// No obs.RequestID in front of the router: the only id is req_id.
	line = logged("", "fallback-1")
	if line["req_id"] != "fallback-1" {
		t.Errorf("req_id = %v", line["req_id"])
	}
	if _, has := line["client_req_id"]; has {
		t.Errorf("client_req_id present: %v", line)
	}
}
