package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/internal/tracing"
)

// stack builds a connection, a RAG store with one document and a project,
// and returns the project id, its api key and the store id.
func (e *env) stack(t *testing.T, upstreamURL, filename, content string) (projID int64, apiKey string, storeID int64) {
	t.Helper()
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": upstreamURL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID = int64(rs["id"].(float64))

	doc := e.upload(storeID, filename, content)
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app",
		"model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	return int64(proj["project"].(map[string]any)["id"].(float64)), proj["api_key"].(string), storeID
}

// TestMetricsAfterRealTraffic drives the real stack and then reads the
// registry the way a scrape would. The point is not that the counters exist
// but that their labels are the bounded ones: a chi route pattern rather
// than the concrete path, and the connection's model rather than the model
// string the client sent.
func TestMetricsAfterRealTraffic(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	projID, apiKey, storeID := e.stack(t, up.URL, "fruits.md",
		"# Apple\n\nApples are red and crunchy and grow on trees.\n\n# Banana\n\nBananas are yellow and soft.")

	chat := e.call("POST", "/v1/chat/completions", map[string]any{
		"model":    "a-model-name-the-client-made-up",
		"messages": []map[string]string{{"role": "user", "content": "What color is a banana?"}}}, apiKey)
	if chat["_status"] != float64(200) {
		t.Fatalf("chat: %v", chat)
	}

	text := e.registry.Text()
	project := strconv.FormatInt(projID, 10)

	want := fmt.Sprintf(`ragmux_gateway_requests_total{project="%s",model="mock-model",status="200",streamed="false"} 1`, project)
	if !strings.Contains(text, want) {
		t.Errorf("missing %s in:\n%s", want, gatewayLines(text))
	}
	// The client asked for a model that does not exist as a connection. If
	// that string ever reaches a label, the metric is unbounded.
	if strings.Contains(text, "a-model-name-the-client-made-up") {
		t.Error("the client-sent model string reached a label; only conn.ModelName may")
	}
	for _, name := range []string{
		fmt.Sprintf(`ragmux_gateway_tokens_total{project="%s",model="mock-model",kind="prompt"}`, project),
		fmt.Sprintf(`ragmux_gateway_tokens_total{project="%s",model="mock-model",kind="completion"}`, project),
		fmt.Sprintf(`ragmux_rag_searches_total{store="%d",used="pgvector"}`, storeID),
		fmt.Sprintf(`ragmux_rag_hits_total{store="%d"}`, storeID),
		`ragmux_ingest_jobs_total{outcome="ready"}`,
		`ragmux_search_backend_available{backend="pgvector"} 1`,
		`ragmux_db_pool_max_conns`,
		`ragmux_documents{status="ready"}`,
	} {
		if !strings.Contains(text, name) {
			t.Errorf("missing %s", name)
		}
	}

	// The route label is the chi pattern. The document upload is the case
	// that matters: its path carries a store id.
	if !strings.Contains(text, `ragmux_http_requests_total{route="/v1/chat/completions",method="POST",status="200"}`) {
		t.Errorf("chat route label missing:\n%s", httpLines(text))
	}
	if !strings.Contains(text, `route="/admin/api/rag-stores/{id}/documents"`) {
		t.Errorf("the upload route should be the pattern, not the path:\n%s", httpLines(text))
	}
	concrete := fmt.Sprintf("/admin/api/rag-stores/%d/documents", storeID)
	if strings.Contains(text, concrete) {
		t.Errorf("a concrete path reached a route label: %s", concrete)
	}
}

// TestUnmatchedRouteIsOneSeries: a 404 sweep must not mint one series per
// probed URL.
func TestUnmatchedRouteIsOneSeries(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	for _, p := range []string{"/nope", "/also-nope", "/wp-admin/setup-config.php"} {
		req, _ := http.NewRequest("GET", e.srv.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	text := e.registry.Text()
	if !strings.Contains(text, `ragmux_http_requests_total{route="unmatched",method="GET",status="404"} 3`) {
		t.Errorf("three probes should collapse into one series:\n%s", httpLines(text))
	}
	if strings.Contains(text, "wp-admin") {
		t.Error("a probed path reached a label")
	}
}

// ---- tracing ----

// collector is a stand-in for an OpenTelemetry Collector: it keeps every
// OTLP/JSON payload the exporter posts so a test can search it.
type collector struct {
	*httptest.Server
	mu   sync.Mutex
	body []byte
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.body = append(c.body, raw...)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *collector) payload() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.body)
}

var spanNameRe = regexp.MustCompile(`"name":"([a-z_.]+)"`)

func (c *collector) spanNames() map[string]int {
	out := map[string]int{}
	for _, m := range spanNameRe.FindAllStringSubmatch(c.payload(), -1) {
		out[m[1]]++
	}
	return out
}

// TestSpansCarryNoSecrets is the enforcement half of the one tracing rule:
// no attribute value ever carries message content, the retrieval query,
// passage text, a filename, the system prompt or a credential.
//
// It pushes a distinct marker through every one of those slots and then
// greps the captured OTLP payload for each of them. A span is exported to a
// third-party collector; it is metadata, not payload.
func TestSpansCarryNoSecrets(t *testing.T) {
	const (
		secretMessage  = "SECRETMESSAGE-hunter2-do-not-export"
		secretPassage  = "SECRETPASSAGE-banana-marzipan-9471"
		secretFilename = "SECRETFILENAME-quarterly-payroll.md"
		secretPrompt   = "SECRETPROMPT-you-are-a-helpful-assistant"
		secretKey      = "SECRETAPIKEY-sk-abcdefghijklmnop"
	)
	up := mockUpstream(t)
	defer up.Close()
	col := newCollector(t)
	tracer := tracing.New(tracing.Config{Endpoint: col.URL, ServiceName: "ragmux-test",
		SampleRatio: 1, Timeout: 5 * time.Second})

	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, tracer: tracer})
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": secretKey, "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_candidates": 4}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))

	doc := e.upload(storeID, secretFilename,
		"# Banana\n\n"+secretPassage+" Bananas are yellow and soft.\n\n# Apple\n\nApples are red and crunchy.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app",
		"model_connection_id": connID, "rag_store_id": storeID, "system_prompt": secretPrompt}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	apiKey := proj["api_key"].(string)

	chat := e.call("POST", "/v1/chat/completions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "banana " + secretMessage}}}, apiKey)
	if chat["_status"] != float64(200) {
		t.Fatalf("chat: %v", chat)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}

	payload := col.payload()
	if payload == "" {
		t.Fatal("the collector received no spans at all; the test proves nothing")
	}
	for _, secret := range []string{secretMessage, secretPassage, secretFilename, secretPrompt, secretKey, apiKey} {
		if strings.Contains(payload, secret) {
			t.Errorf("a span carried %q; no attribute may carry content, a query, passage text, "+
				"a filename, the system prompt or a credential", secret)
		}
	}

	// And the spans that should exist, do. Without this the test above
	// would pass on an exporter that sent nothing useful.
	names := col.spanNames()
	for _, want := range []string{"http.server", "gateway.chat_completion", "rag.retrieve",
		"rag.embed_query", "rag.rerank", "provider.chat", "provider.embed", "ingest.document"} {
		if names[want] == 0 {
			t.Errorf("no %s span was exported; got %v", want, names)
		}
	}
}

// gatewayLines and httpLines trim a failed assertion's output to the family
// it is about.
func gatewayLines(text string) string { return grepLines(text, "ragmux_gateway_") }
func httpLines(text string) string    { return grepLines(text, "ragmux_http_requests_total") }

func grepLines(text, prefix string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
