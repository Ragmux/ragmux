// Package httpx holds small net/http helpers shared by the admin API and the
// gateway: per-handler read deadlines and response header middleware.
package httpx

import (
	"net/http"
	"time"
)

// Deadline bounds how long the handler may spend reading the request body:
// the connection's read deadline is set to now+d. d <= 0 clears it, which
// callers do once the body is consumed so a long streaming response is not
// cut short. Writers that do not expose the connection (test recorders) are
// ignored.
func Deadline(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	if d <= 0 {
		_ = rc.SetReadDeadline(time.Time{})
		return
	}
	_ = rc.SetReadDeadline(time.Now().Add(d))
}

// ReadDeadline is Deadline as middleware: every request through it gets d to
// deliver its body. Handlers that need a different budget call Deadline
// themselves, which overrides this one.
func ReadDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Deadline(w, d)
			next.ServeHTTP(w, r)
		})
	}
}

// NoStore marks responses as not cacheable, for API responses that carry
// session-scoped data.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
