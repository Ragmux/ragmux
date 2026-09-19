package metrics

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// contentType is the text exposition format this registry writes.
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves the registry.
//
// When token is non-empty every request must present it as a bearer token.
// The metric set names every project and model and reports per-project token
// counts and spend — the same data the authenticated admin metrics endpoints
// serve — so an unauthenticated /metrics would be the one place the product
// gives that away for free.
func (r *Registry) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if token != "" && !bearerEquals(req.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		if req.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = r.WriteTo(w)
	})
}

// bearerEquals compares an Authorization header against the expected token in
// constant time, so a wrong token cannot be recovered a byte at a time.
func bearerEquals(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
