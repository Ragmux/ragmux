package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every header the gateway sets for clients must be readable from browser
// JS, which sees only what Access-Control-Expose-Headers lists.
func TestCORSExposesGatewayHeaders(t *testing.T) {
	h := cors([]string{"https://app.example"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	exposed := map[string]bool{}
	for _, name := range strings.Split(w.Header().Get("Access-Control-Expose-Headers"), ",") {
		exposed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range []string{"Retry-After", "Warning", "X-Request-Id", "x-ragmux-projects"} {
		if !exposed[strings.ToLower(name)] {
			t.Errorf("%s is not exposed: %q", name, w.Header().Get("Access-Control-Expose-Headers"))
		}
	}
}

// The MCP request headers are not CORS-safelisted, so a browser client's
// preflight has to be told it may send them (ADR-030).
func TestCORSAllowsMCPHeaders(t *testing.T) {
	h := cors([]string{"https://app.example"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	allowed := map[string]bool{}
	for _, name := range strings.Split(w.Header().Get("Access-Control-Allow-Headers"), ",") {
		allowed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range []string{"Authorization", "MCP-Protocol-Version", "Mcp-Method", "Mcp-Name"} {
		if !allowed[strings.ToLower(name)] {
			t.Errorf("%s is not allowed: %q", name, w.Header().Get("Access-Control-Allow-Headers"))
		}
	}
}

// The Anthropic SDKs send x-api-key and the anthropic-* headers, and the
// gateway reads X-Ragmux-Session on every endpoint, so a browser client's
// preflight must be told it may send them (ADR-028, ADR-032). request-id is
// the response header those SDKs read an error's id from.
func TestCORSAllowsAnthropicHeaders(t *testing.T) {
	h := cors([]string{"https://app.example"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "x-api-key, anthropic-version, anthropic-beta, X-Ragmux-Session")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	allowed := map[string]bool{}
	for _, name := range strings.Split(w.Header().Get("Access-Control-Allow-Headers"), ",") {
		allowed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range []string{"x-api-key", "anthropic-version", "anthropic-beta",
		"anthropic-dangerous-direct-browser-access", "X-Ragmux-Session"} {
		if !allowed[strings.ToLower(name)] {
			t.Errorf("%s is not allowed: %q", name, w.Header().Get("Access-Control-Allow-Headers"))
		}
	}
	exposed := map[string]bool{}
	for _, name := range strings.Split(w.Header().Get("Access-Control-Expose-Headers"), ",") {
		exposed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if !exposed["request-id"] {
		t.Errorf("request-id is not exposed: %q", w.Header().Get("Access-Control-Expose-Headers"))
	}
}
