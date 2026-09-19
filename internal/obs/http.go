package obs

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ragmux/ragmux/internal/tracing"
)

// unmatchedRoute labels a request no route matched. Without it a 404 sweep
// would mint one series per probed URL, which is the classic way a metrics
// endpoint becomes the memory leak.
const unmatchedRoute = "unmatched"

// dashboardRoute collapses every embedded dashboard asset into one series.
const dashboardRoute = "/admin/*"

// RoutePattern names the route of a finished request the only way that is
// safe to use as a label: the chi pattern, which chi fills in during
// routing and which is therefore only readable after next.ServeHTTP has
// returned. r.URL.Path would be one series per document id, per project
// name and per 404 probe; the pattern is one series per handler.
func RoutePattern(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return unmatchedRoute
	}
	p := rc.RoutePattern()
	if p == "" {
		return unmatchedRoute
	}
	// The dashboard is a file server behind /admin/*; its assets are not
	// worth one series each.
	if strings.HasPrefix(p, "/admin/") && !strings.HasPrefix(p, "/admin/api") {
		return dashboardRoute
	}
	return p
}

// HTTPMetrics counts, times and sizes every request.
//
// It belongs directly after middleware.RequestID and before the request
// logger, so it measures everything below it including the recoverer: a
// request that panics is still a request that happened.
func HTTPMetrics(m *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		inFlight := m.httpInFlight.With()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			inFlight.Inc()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			defer func() {
				inFlight.Dec()
				route := RoutePattern(r)
				status := ww.Status()
				if status == 0 {
					// Nothing was written: the handler returned without
					// touching the writer, which net/http answers as 200.
					status = http.StatusOK
				}
				m.httpRequests.With(route, r.Method, strconv.Itoa(status)).Inc()
				m.httpDuration.With(route, r.Method).Observe(time.Since(start).Seconds())
				if n := ww.BytesWritten(); n > 0 {
					m.httpBytes.With(route).Add(uint64(n))
				}
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

// HTTPTracing opens the http.server span every other span hangs beneath.
//
// The inbound traceparent is only honoured when the tracer is configured to
// trust it; otherwise this is a trace root and the client's header is
// ignored, which is what stops an outsider pinning every request into one
// trace with the sampled flag forced on.
func HTTPTracing(tr *tracing.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !tr.Enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tr.Extract(r.Context(), r.Header.Get("traceparent"), r.Header.Get("tracestate"))
			ctx, span := tr.Start(ctx, "http.server", tracing.KindServer)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			r = r.WithContext(ctx)
			defer func() {
				if span.IsRecording() {
					status := ww.Status()
					if status == 0 {
						status = http.StatusOK
					}
					span.SetAttributes(
						tracing.String("http.request.method", r.Method),
						tracing.String("http.route", RoutePattern(r)),
						tracing.Int("http.response.status_code", status),
						tracing.String("ragmux.request_id", middleware.GetReqID(ctx)),
					)
					if status < 500 {
						span.SetStatusOK()
					}
				}
				span.End()
			}()
			next.ServeHTTP(ww, r)
		})
	}
}
