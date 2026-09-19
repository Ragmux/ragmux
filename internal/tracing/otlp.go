package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// queueSize bounds the spans waiting to be exported. Tracing must never
	// block a request, so a full queue drops rather than waits.
	queueSize = 2048
	// batchSize and batchInterval decide when a batch leaves.
	batchSize     = 512
	batchInterval = 5 * time.Second
)

type exporter struct {
	url     string
	headers map[string]string
	client  *http.Client
	res     []Attr
	log     *slog.Logger

	in   chan *Span
	done chan struct{}
	// stopped is set before done is closed, so enqueue can tell a span that
	// arrived after the exporter stopped from one that is merely late.
	stopped  atomic.Bool
	stopOnce sync.Once
	wg       sync.WaitGroup

	dropped  atomic.Uint64
	failures atomic.Uint64
}

func newExporter(cfg Config) *exporter {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	res := []Attr{
		String("service.name", cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		res = append(res, String("service.version", cfg.ServiceVersion))
	}
	for k, v := range cfg.ResourceAttrs {
		res = append(res, String(k, v))
	}
	e := &exporter{
		url:     tracesURL(cfg.Endpoint),
		headers: cfg.Headers,
		// A plain client on purpose. The collector normally sits at an
		// address like http://otel-collector:4318, which is exactly what the
		// outbound SSRF guard exists to block. That guard governs provider
		// calls a user can influence; this endpoint is operator-configured,
		// like DATABASE_URL.
		client: &http.Client{Timeout: timeout},
		res:    res,
		log:    cfg.Logger,
		in:     make(chan *Span, queueSize),
		done:   make(chan struct{}),
	}
	e.wg.Add(1)
	go e.run()
	return e
}

// tracesURL appends the signal path unless the endpoint already names it, so
// both OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// work as their users expect.
func tracesURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1/traces") {
		return endpoint
	}
	return endpoint + "/v1/traces"
}

func (e *exporter) enqueue(s *Span) {
	// After shutdown nothing reads e.in any more, so a span accepted here
	// would sit in the buffer until the process exited and never reach the
	// collector. It is still a dropped span and is counted as one: the
	// requests that outlive Shutdown — a long stream still draining, a
	// handler the graceful-shutdown deadline cut short — are exactly the
	// ones an operator goes looking for in a trace, and their absence has to
	// show up in ragmux_tracing_spans_dropped_total rather than in nothing
	// at all.
	if e.stopped.Load() {
		e.dropped.Add(1)
		return
	}
	select {
	case e.in <- s:
	default:
		e.dropped.Add(1)
	}
}

func (e *exporter) run() {
	defer e.wg.Done()
	batch := make([]*Span, 0, batchSize)
	t := time.NewTicker(batchInterval)
	defer t.Stop()
	for {
		select {
		case s := <-e.in:
			batch = append(batch, s)
			if len(batch) >= batchSize {
				e.send(batch)
				batch = batch[:0]
			}
		case <-t.C:
			if len(batch) > 0 {
				e.send(batch)
				batch = batch[:0]
			}
		case <-e.done:
			// Drain whatever is queued before the last flush, so the spans of
			// requests that were still draining at shutdown make it out.
			for {
				select {
				case s := <-e.in:
					batch = append(batch, s)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				e.send(batch)
			}
			// Close the queue only now, after the last batch has gone: up to
			// this point a span that ended during the drain still had a
			// chance to be exported. From here enqueue counts instead of
			// accepting, and the second drain below counts the spans that
			// raced in between the first drain and this store. Between them
			// every span is either exported or counted, which is what makes
			// ragmux_tracing_spans_dropped_total trustworthy at shutdown.
			e.stopped.Store(true)
			for {
				select {
				case <-e.in:
					e.dropped.Add(1)
					continue
				default:
				}
				break
			}
			return
		}
	}
}

func (e *exporter) shutdown(ctx context.Context) error {
	e.stopOnce.Do(func() { close(e.done) })
	finished := make(chan struct{})
	go func() { e.wg.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *exporter) send(batch []*Span) {
	body, err := json.Marshal(buildPayload(e.res, batch))
	if err != nil {
		e.failures.Add(1)
		e.log.Warn("tracing: encode spans", "err", err)
		return
	}
	// One retry, then give up. A backlog of retries would compete with the
	// requests that produced it, and a dropped trace is not worth that.
	if err := e.post(body); err != nil {
		time.Sleep(time.Second)
		if err = e.post(body); err != nil {
			e.failures.Add(1)
			e.log.Warn("tracing: export spans", "spans", len(batch), "err", err)
		}
	}
}

func (e *exporter) post(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("collector returned %d", resp.StatusCode)
}

// ---- OTLP/JSON encoding ----
//
// Two details of the JSON mapping are easy to get wrong and silently produce
// a payload some collectors reject:
//
//   - trace and span ids are lowercase hex strings, not base64. That is an
//     explicit exception in the JSON mapping; the protobuf encoding uses
//     bytes, and encoding/json would have rendered those as base64.
//   - 64-bit integers, including the nanosecond timestamps, are JSON strings.
//     Emitting them as numbers loses precision past 2^53 and some receivers
//     refuse them outright.

type otlpPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpan struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

func buildPayload(res []Attr, spans []*Span) otlpPayload {
	out := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		s.mu.Lock()
		attrs := attrsToOTLP(s.attrs)
		status := otlpStatus{Code: int(s.status), Message: s.msg}
		s.mu.Unlock()

		os := otlpSpan{
			TraceID:           s.sc.TraceID.String(),
			SpanID:            s.sc.SpanID.String(),
			Name:              s.name,
			Kind:              int(s.kind),
			StartTimeUnixNano: strconv.FormatInt(s.start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(s.end.UnixNano(), 10),
			Attributes:        attrs,
			Status:            status,
		}
		if s.parent.IsValid() {
			os.ParentSpanID = s.parent.String()
		}
		out = append(out, os)
	}
	return otlpPayload{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: attrsToOTLP(res)},
		ScopeSpans: []otlpScopeSpan{{Scope: otlpScope{Name: "ragmux"}, Spans: out}},
	}}}
}

func attrsToOTLP(attrs []Attr) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(attrs))
	for _, a := range attrs {
		kv := otlpKeyValue{Key: a.Key}
		switch v := a.Value.(type) {
		case string:
			kv.Value.StringValue = &v
		case int64:
			s := strconv.FormatInt(v, 10)
			kv.Value.IntValue = &s
		case float64:
			kv.Value.DoubleValue = &v
		case bool:
			kv.Value.BoolValue = &v
		default:
			// An unsupported type is dropped rather than guessed at.
			continue
		}
		out = append(out, kv)
	}
	return out
}
