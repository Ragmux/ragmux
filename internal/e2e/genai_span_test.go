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
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/internal/tracing"
)

// otlpSpan is the part of an exported OTLP/JSON span these tests read.
type otlpSpan struct {
	Name       string `json:"name"`
	Attributes []struct {
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
		case "gateway.chat_completion":
			chats++
			for key, want := range map[string]string{
				"gen_ai.operation.name":          `{"stringValue":"chat"}`,
				"gen_ai.provider.name":           `{"stringValue":"custom_openai"}`,
				"gen_ai.request.model":           `{"stringValue":"mock-model"}`,
				"gen_ai.response.model":          `{"stringValue":"` + upstreamModel + `"}`,
				"gen_ai.response.finish_reasons": `{"arrayValue":{"values":[{"stringValue":"stop"}]}}`,
			} {
				if got := s.attr(key); got != want {
					t.Errorf("gateway.chat_completion %s = %s, want %s", key, got, want)
				}
			}
			if s.attr("gen_ai.usage.cache_read.input_tokens") == "" {
				t.Error("gateway.chat_completion lacks gen_ai.usage.cache_read.input_tokens")
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
		t.Errorf("exported %d gateway.chat_completion spans, want 2 (plain and streamed)", chats)
	}
	if providerChats == 0 || embeds == 0 {
		t.Errorf("provider spans missing: chat=%d embed=%d", providerChats, embeds)
	}
}
