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
