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

// otherLabel is the bucket every unrecognised label value falls into. It is
// the shape of the guard used throughout this package: a closed set plus one
// catch-all, so an attacker choosing the input chooses between existing
// series and never mints a new one.
const otherLabel = "other"

// knownMethods is the closed set the method label is drawn from.
//
// r.Method is whatever the client put on the request line: net/http accepts
// any token there, so PROPFIND, a random UUID or a 200-byte string all
// arrive as a method. Used raw it is one series per invented verb, per
// route, per status — the same unbounded-memory bug as labelling with the
// path, reached from a slightly different direction.
var knownMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// Method maps a request method onto the closed set, so the label is bounded
// by the seven verbs this repository serves rather than by traffic.
func Method(m string) string {
	if knownMethods[m] {
		return m
	}
	return otherLabel
}

// maxRequestIDLen bounds the request id recorded on a span.
const maxRequestIDLen = 64

// safeRequestID decides whether a request id may go on a span.
//
// chi's middleware.RequestID echoes the client's X-Request-Id header
// verbatim when one is present, so middleware.GetReqID returns
// attacker-chosen bytes: a megabyte of text, a credential someone parked in
// the header, or newlines and quotes aimed at whatever reads the collector's
// output. PRD rule 10 forbids taking a span attribute from a header, so
// anything that is not shaped like a generated id is dropped rather than
// trimmed — a truncated secret is still a secret.
//
// chi generates <base64 prefix>/<hostname>-<counter>, which this charset
// admits; the empty string means no id and is dropped too.
func safeRequestID(id string) string {
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '/', c == '+', c == '=':
		default:
			return ""
		}
	}
	return id
}

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
				method := Method(r.Method)
				m.httpRequests.With(route, method, strconv.Itoa(status)).Inc()
				m.httpDuration.With(route, method).Observe(time.Since(start).Seconds())
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
					// Both values are put through the same guards the metric
					// labels use: a span leaves this process for a
					// third-party collector, so PRD rule 10 binds it too.
					attrs := []tracing.Attr{
						tracing.String("http.request.method", Method(r.Method)),
						tracing.String("http.route", RoutePattern(r)),
						tracing.Int("http.response.status_code", status),
					}
					if id := safeRequestID(middleware.GetReqID(ctx)); id != "" {
						attrs = append(attrs, tracing.String("ragmux.request_id", id))
					}
					span.SetAttributes(attrs...)
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
