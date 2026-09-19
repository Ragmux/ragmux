package obs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
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

// Request id generation. The prefix identifies this process, the counter
// the request within it, which is chi's scheme and keeps ids sortable and
// cheap.
var (
	requestIDPrefix  string
	requestIDCounter atomic.Uint64
)

func init() {
	host, err := os.Hostname()
	if host == "" || err != nil {
		host = "localhost"
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A process that cannot read randomness still has to serve. The
		// counter alone keeps ids unique within this process, which is what
		// correlating one process's logs needs.
		requestIDPrefix = host + "/0000000000"
		return
	}
	requestIDPrefix = host + "/" + base64.RawURLEncoding.EncodeToString(buf[:])
}

// RequestID injects a request id that is always generated here, replacing
// chi's middleware.RequestID.
//
// chi's version starts from the client's X-Request-Id header and only
// generates when it is absent, so middleware.GetReqID returns
// attacker-chosen bytes: that id then reaches the ragmux.request_id span
// attribute exported to a third-party collector, the X-Request-Id response
// header and the request log.
//
// Validating the header instead does not work, and the shape of the failure
// is worth recording. A charset-and-length filter cannot tell a generated id
// from a credential, because they are the same shape: Ragmux's own gateway
// key is "sk-user-" plus 43 characters of keyAlphabet, 51 bytes that are all
// unreserved -- and so are an AWS AKIA... key, an OpenAI sk- key, and every
// hex or base64url token short enough to pass a length cap. A filter that
// admits those admits exactly what it was written to exclude.
//
// So the header is not read at all. PRD rule 10's guarantee is then
// structural rather than a matter of how good the filter is: there is no
// path from a header to a label, a span or a log line.
//
// The id is stored under chi's middleware.RequestIDKey, so
// middleware.GetReqID keeps working everywhere it is already called.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDPrefix + "-" + strconv.FormatUint(requestIDCounter.Add(1), 10)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), middleware.RequestIDKey, id)))
	})
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
					// The method goes through the same mapping the metric
					// label uses: a span leaves this process for a
					// third-party collector, so PRD rule 10 binds it too.
					// The request id needs no guard because RequestID above
					// generates it and never reads a header.
					span.SetAttributes(
						tracing.String("http.request.method", Method(r.Method)),
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
