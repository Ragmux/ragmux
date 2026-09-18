package provider

import (
	"net/http"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"key sk-abcdefghijklmnop rejected":               "key [redacted] rejected",
		"anthropic sk-ant-api03-abcdefgh_ijk bad":        "anthropic [redacted] bad",
		"google AIzaSyA1234567890abcdefghijklmn invalid": "google [redacted] invalid",
		"header Bearer sk-proj-abcdefghijklmnop":         "header [redacted]",
		"header Bearer tok.en_value-123":                 "header [redacted]",
		"short sk-abc stays":                             "short sk-abc stays",
		"plain message":                                  "plain message",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpstreamErrorRedactsMessages(t *testing.T) {
	e := upstreamError(http.StatusUnauthorized, []byte(`{"error":{"message":"Incorrect API key provided: sk-abcdefghijklmnop1234","type":"invalid_request_error","code":"invalid_api_key"}}`))
	if strings.Contains(e.Message, "abcdefghijklmnop") || !strings.Contains(e.Message, "[redacted]") || e.Code != "invalid_api_key" {
		t.Errorf("error = %+v", e)
	}
	plain := upstreamError(http.StatusBadGateway, []byte("proxy refused Bearer AIzaSyA1234567890abcdefghijklmn"))
	if strings.Contains(plain.Message, "AIza") {
		t.Errorf("plain text not redacted: %q", plain.Message)
	}
}
