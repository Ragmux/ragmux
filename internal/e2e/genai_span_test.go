package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/internal/tracing"
	"github.com/ragmux/ragmux/internal/tracing/tracingtest"
)

// otlpSpan is the part of an exported OTLP/JSON span these tests read.
type otlpSpan struct {
	Name         string `json:"name"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Attributes   []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"attributes"`
}

// attr returns the raw OTLP value of key, or "" when the span lacks it.
func (s otlpSpan) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			return string(a.Value)
		}
	}
	return ""
}

// spans decodes every payload the collector received. The exporter posts
// one JSON document per batch and the collector concatenates them, so the
// body is read as a stream of documents.
func (c *collector) spans(t *testing.T) []otlpSpan {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(c.payload()))
	var out []otlpSpan
	for {
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []otlpSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decode OTLP payload: %v", err)
		}
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
}

// datedModelUpstream forwards to the shared mock upstream and rewrites the
// "model" field of every chat completion response, plain or streamed, to
// model. The shared mock echoes the request model, which is the connection's
// own name and so cannot tell the upstream value from the fallback.
func datedModelUpstream(t *testing.T, target, model string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, err := http.Post(target+r.URL.Path, r.Header.Get("Content-Type"), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if r.URL.Path != "/v1/chat/completions" {
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			w.Write(raw)
			return
		}
		setModel := func(doc []byte) []byte {
			var m map[string]any
			if json.Unmarshal(doc, &m) != nil {
				return doc
			}
			m["model"] = model
			return mustJSON(m)
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			w.Write(setModel(raw))
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
				line = "data: " + string(setModel([]byte(data)))
			}
			fmt.Fprint(w, line+"\n")
		}
	}))
}

// TestChatSpanCarriesGenAIAttributes drives a plain and a streamed chat
// completion through the real stack and reads the exported spans. The
// chat span must carry the GenAI semantic-convention attributes this
// release adds, every one of them metadata: the client's model string, the
// message and the reply text stay out.
func TestChatSpanCarriesGenAIAttributes(t *testing.T) {
	const (
		clientModel   = "CLIENTMODEL-made-up-by-the-caller"
		secretMessage = "SECRETMESSAGE-genai-attrs-7731"
		// The upstream reports a dated snapshot of the configured model, as
		// real providers do, so the span shows the upstream's value rather
		// than the connection's model name it would fall back to.
		upstreamModel = "mock-model-2026-01-01"
	)
	inner := mockUpstream(t)
	defer inner.Close()
	up := datedModelUpstream(t, inner.URL, upstreamModel)
	defer up.Close()
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})

	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, tracer: tracer})
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Banana\n\nBananas are yellow and soft.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID,
		"rag_store_id": storeID, "budget_daily_tokens": 100000}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	projID := int64(proj["project"].(map[string]any)["id"].(float64))
	apiKey := proj["api_key"].(string)

	messages := []map[string]string{{"role": "user", "content": "banana " + secretMessage}}
	chat := e.call("POST", "/v1/chat/completions", map[string]any{"model": clientModel, "messages": messages}, apiKey)
	if chat["_status"] != float64(200) {
		t.Fatalf("chat: %v", chat)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions",
		bytes.NewReader(mustJSON(map[string]any{"model": clientModel, "stream": true, "messages": messages})))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasSuffix(strings.TrimSpace(string(raw)), "data: [DONE]") {
		t.Fatalf("stream body: %s", raw)
	}

	// Metrics first: the new families must be on /metrics with their type.
	text := e.registry.Text()
	for _, want := range []string{
		"# TYPE ragmux_budget_used_ratio gauge",
		fmt.Sprintf(`ragmux_budget_used_ratio{project="%d"}`, projID),
		"# TYPE ragmux_requests_unpriced_total counter",
		// The mock connection has no price row, so both completions count.
		`ragmux_requests_unpriced_total{provider="custom_openai"} 2`,
		"# TYPE ragmux_retention_last_success_timestamp_seconds gauge",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}

	payload := col.payload()
	for _, secret := range []string{secretMessage, clientModel, "Bananas are yellow", "SYS:"} {
		if strings.Contains(payload, secret) {
			t.Errorf("a span carried %q; GenAI attributes are metadata, never content or client input", secret)
		}
	}

	var chats, providerChats, embeds int
	for _, s := range col.spans(t) {
		switch s.Name {
		case "chat mock-model":
			chats++
			for key, want := range map[string]string{
				"gen_ai.operation.name":          `{"stringValue":"chat"}`,
				"gen_ai.provider.name":           `{"stringValue":"custom_openai"}`,
				"gen_ai.request.model":           `{"stringValue":"mock-model"}`,
				"gen_ai.response.model":          `{"stringValue":"` + upstreamModel + `"}`,
				"gen_ai.response.finish_reasons": `{"arrayValue":{"values":[{"stringValue":"stop"}]}}`,
			} {
				if got := s.attr(key); got != want {
					t.Errorf("chat span %s = %s, want %s", key, got, want)
				}
			}
			if s.attr("gen_ai.usage.cache_read.input_tokens") == "" {
				t.Error("chat span lacks gen_ai.usage.cache_read.input_tokens")
			}
		case "provider.chat":
			providerChats++
			if got := s.attr("gen_ai.provider.name"); got != `{"stringValue":"custom_openai"}` {
				t.Errorf("provider.chat gen_ai.provider.name = %s", got)
			}
			if got := s.attr("gen_ai.system"); got != `{"stringValue":"custom_openai"}` {
				t.Errorf("provider.chat must keep the deprecated gen_ai.system until it is removed; got %s", got)
			}
			if got := s.attr("gen_ai.operation.name"); got != `{"stringValue":"chat"}` {
				t.Errorf("provider.chat gen_ai.operation.name = %s", got)
			}
		case "provider.embed":
			embeds++
			if got := s.attr("gen_ai.operation.name"); got != `{"stringValue":"embeddings"}` {
				t.Errorf("provider.embed gen_ai.operation.name = %s", got)
			}
		case "rag.embed_query":
			if got := s.attr("gen_ai.provider.name"); got != `{"stringValue":"custom_openai"}` {
				t.Errorf("rag.embed_query gen_ai.provider.name = %s", got)
			}
		}
	}
	if chats != 2 {
		t.Errorf("exported %d chat spans, want 2 (plain and streamed)", chats)
	}
	if providerChats == 0 || embeds == 0 {
		t.Errorf("provider spans missing: chat=%d embed=%d", providerChats, embeds)
	}
}

// shutdownTracer flushes the tracer so every finished span has reached the
// collector.
func shutdownTracer(t *testing.T, tracer *tracing.Tracer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
}

// TestEmbeddingsSpanIsNamedAfterTheModel pins the request span of
// POST /v1/embeddings to the GenAI convention: "embeddings {model}", where
// the model is the one the request was served by.
func TestEmbeddingsSpanIsNamedAfterTheModel(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, tracer: tracer})

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "embed", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "embed-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	res := e.call("POST", "/v1/embeddings", map[string]any{"model": "embed-model", "input": "cherry"},
		proj["api_key"].(string))
	if res["_status"] != float64(200) {
		t.Fatalf("embeddings: %v", res)
	}
	shutdownTracer(t, tracer)

	var found int
	for _, s := range col.spans(t) {
		if s.Name != "embeddings embed-model" {
			continue
		}
		found++
		if got := s.attr("gen_ai.operation.name"); got != `{"stringValue":"embeddings"}` {
			t.Errorf("embeddings span gen_ai.operation.name = %s", got)
		}
		if got := s.attr("gen_ai.request.model"); got != `{"stringValue":"embed-model"}` {
			t.Errorf("embeddings span gen_ai.request.model = %s", got)
		}
	}
	if found != 1 {
		t.Errorf("exported %d spans named %q, want 1; names: %v", found, "embeddings embed-model", col.spanNames())
	}
}

// fallbackStack builds a route "smart" whose first target always answers 500
// and whose second one works, and sends one chat completion through it. The
// tracer is the one the caller wants to observe.
func fallbackStack(t *testing.T, tracer *tracing.Tracer) {
	t.Helper()
	fallbackStackWith(t, tracer, false)
}

// fallbackStackWith is fallbackStack; with breakFirst the first target's
// adapter cannot be built (a target_config fault), so no call ever reaches it
// and the route moves on to the second target.
func fallbackStackWith(t *testing.T, tracer *tracing.Tracer, breakFirst bool) {
	t.Helper()
	var hitsA, hitsB, statusA atomic.Int64
	statusA.Store(http.StatusInternalServerError)
	upA, upB := failingUpstream(&hitsA, &statusA), countingUpstream(&hitsB)
	t.Cleanup(upA.Close)
	t.Cleanup(upB.Close)
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, tracer: tracer, gwTune: func(g *gateway.Gateway) {
		g.MaxRetries, g.UpstreamTimeout = 2, 10*time.Second
		if breakFirst {
			build := g.Providers
			g.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
				if c.ModelName == "model-a" {
					return nil, errors.New("unsupported provider type")
				}
				return build(c)
			}
		}
	}})
	mkConn := func(name, url string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": url + "/v1", "api_key": "k", "model_name": "model-" + name}, "")
		if c["_status"] != float64(201) {
			t.Fatalf("create connection %s: %v", name, c)
		}
		return int64(c["id"].(float64))
	}
	connA, connB := mkConn("a", upA.URL), mkConn("b", upB.URL)
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "fb", "model_connection_id": connA}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	route := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/routes", pid), map[string]any{"name": "smart", "targets": []any{
		map[string]any{"connection_id": connA, "priority": 0},
		map[string]any{"connection_id": connB, "priority": 1}}}, "")
	if route["_status"] != float64(201) {
		t.Fatalf("create route: %v", route)
	}
	r := e.call("POST", "/v1/chat/completions", map[string]any{"model": "smart",
		"messages": []map[string]string{{"role": "user", "content": "ping"}}}, proj["api_key"].(string))
	if r["_status"] != float64(200) {
		t.Fatalf("fallback call: %v", r)
	}
	wantA := int64(1)
	if breakFirst {
		wantA = 0
	}
	if hitsA.Load() != wantA || hitsB.Load() != 1 {
		t.Fatalf("upstream calls a=%d b=%d, want %d and 1", hitsA.Load(), hitsB.Load(), wantA)
	}
}

// A target whose adapter cannot be built makes no upstream call, so it is not
// an attempt: its span carries no ragmux.attempt, and the fallback that
// answers is attempt 1 (the number the request log and ragmux.attempts give
// it too), not a second span claiming the same number.
func TestConfigFaultAttemptSpanHasNoAttemptNumber(t *testing.T) {
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})
	fallbackStackWith(t, tracer, true)
	shutdownTracer(t, tracer)

	var request *otlpSpan
	var attempts []otlpSpan
	spans := col.spans(t)
	for i, s := range spans {
		switch s.Name {
		case "chat model-b":
			request = &spans[i]
		case "gateway.attempt":
			attempts = append(attempts, s)
		}
	}
	if request == nil || len(attempts) != 2 {
		t.Fatalf("request span %v, %d attempt spans, want a request and 2; names: %v", request != nil, len(attempts), col.spanNames())
	}
	if got := request.attr("ragmux.attempts"); got != `{"intValue":"1"}` {
		t.Errorf("request span ragmux.attempts = %s, want 1", got)
	}
	var numbered, unnumbered int
	for _, a := range attempts {
		switch n := a.attr("ragmux.attempt"); n {
		case "":
			unnumbered++
			if got := a.attr("error.type"); got != `{"stringValue":"target_config"}` {
				t.Errorf("the unnumbered attempt span error.type = %s, want target_config", got)
			}
		case `{"intValue":"1"}`:
			numbered++
			if a.attr("error.type") != "" {
				t.Error("the attempt that answered carries error.type")
			}
		default:
			t.Errorf("unexpected ragmux.attempt %s", n)
		}
	}
	if numbered != 1 || unnumbered != 1 {
		t.Errorf("%d numbered and %d unnumbered attempt spans, want 1 and 1", numbered, unnumbered)
	}
}

// TestFallbackOpensOneAttemptSpanPerTarget checks the routing view of a
// trace: the request span is named after the model that finally answered,
// and every attempt (the failed first target, then the fallback) is its own
// child span carrying the attempt number, with the upstream call below it.
func TestFallbackOpensOneAttemptSpanPerTarget(t *testing.T) {
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})
	fallbackStack(t, tracer)
	shutdownTracer(t, tracer)

	spans := col.spans(t)
	var request *otlpSpan
	var attempts []otlpSpan
	byID := map[string]otlpSpan{}
	for i, s := range spans {
		byID[s.SpanID] = s
		switch s.Name {
		case "chat model-b":
			request = &spans[i]
		case "gateway.attempt":
			attempts = append(attempts, s)
		}
	}
	if request == nil {
		t.Fatalf("no request span named %q; names: %v", "chat model-b", col.spanNames())
	}
	if got := request.attr("ragmux.route"); got != `{"stringValue":"smart"}` {
		t.Errorf("request span ragmux.route = %s", got)
	}
	if got := request.attr("ragmux.attempts"); got != `{"intValue":"2"}` {
		t.Errorf("request span ragmux.attempts = %s, want 2", got)
	}
	if request.attr("ragmux.fallback.from_connection") == "" {
		t.Error("request span lacks ragmux.fallback.from_connection")
	}
	if len(attempts) != 2 {
		t.Fatalf("exported %d gateway.attempt spans, want 2", len(attempts))
	}
	seen := map[string]bool{}
	for _, a := range attempts {
		n := a.attr("ragmux.attempt")
		if n == "" {
			t.Errorf("attempt span lacks ragmux.attempt: %+v", a)
		}
		seen[n] = true
		if a.ParentSpanID != request.SpanID {
			t.Errorf("attempt span %s is not a child of the request span", n)
		}
		if got := a.attr("ragmux.route"); got != `{"stringValue":"smart"}` {
			t.Errorf("attempt span ragmux.route = %s", got)
		}
	}
	if !seen[`{"intValue":"1"}`] || !seen[`{"intValue":"2"}`] {
		t.Errorf("attempt numbers = %v, want 1 and 2", seen)
	}
	for _, a := range attempts {
		switch a.attr("ragmux.attempt") {
		case `{"intValue":"1"}`:
			if a.attr("error.type") == "" {
				t.Error("the failed first attempt span lacks error.type")
			}
			if got := a.attr("gen_ai.request.model"); got != `{"stringValue":"model-a"}` {
				t.Errorf("first attempt gen_ai.request.model = %s", got)
			}
		case `{"intValue":"2"}`:
			if a.attr("error.type") != "" {
				t.Error("the successful second attempt span carries error.type")
			}
			if got := a.attr("gen_ai.request.model"); got != `{"stringValue":"model-b"}` {
				t.Errorf("second attempt gen_ai.request.model = %s", got)
			}
		}
	}
	var providerUnderAttempt int
	for _, s := range spans {
		if s.Name != "provider.chat" {
			continue
		}
		if p, ok := byID[s.ParentSpanID]; ok && p.Name == "gateway.attempt" {
			providerUnderAttempt++
		}
	}
	if providerUnderAttempt != 2 {
		t.Errorf("%d provider.chat spans sit under an attempt span, want 2", providerUnderAttempt)
	}
}

// TestGRPCExporterDeliversGatewaySpans selects the OTLP/gRPC transport and
// sends a fallback request through the real stack; the spans must reach an
// in-process collector, with the same names and attempt attributes the HTTP
// transport carries.
func TestGRPCExporterDeliversGatewaySpans(t *testing.T) {
	col := tracingtest.NewCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL(), Protocol: tracing.ProtocolGRPC,
		ServiceName: "ragmux-test", SampleRatio: 1, Timeout: 5 * time.Second})
	fallbackStack(t, tracer)
	shutdownTracer(t, tracer)

	reqs := col.Requests()
	if len(reqs) == 0 {
		t.Fatal("the gRPC collector received no export")
	}
	for _, r := range reqs {
		if r.Path != tracingtest.ExportPath {
			t.Errorf("collector got a call to %q", r.Path)
		}
	}
	var request, attempts int
	for _, s := range col.Spans(t) {
		switch s.Name {
		case "chat model-b":
			request++
		case "gateway.attempt":
			attempts++
			if _, ok := s.Attrs["ragmux.attempt"]; !ok {
				t.Errorf("gRPC attempt span lacks ragmux.attempt: %v", s.Attrs)
			}
		}
	}
	if request != 1 || attempts != 2 {
		t.Errorf("gRPC export carried %d request spans and %d attempt spans, want 1 and 2", request, attempts)
	}
}
