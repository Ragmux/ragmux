package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// costEnv is a gateway wired to a priced upstream: one project whose
// connection is of providerType/model and answers with handler.
type costEnv struct {
	*env
	prices *pricing.Cache
}

func newCostEnv(t *testing.T, providerType, model string, handler http.HandlerFunc) *costEnv {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	if _, err := pricing.Seed(ctx, st.DB(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "priced", ProviderType: providerType,
		BaseURL: up.URL, APIKey: "sk-ant-secretsecret1234", ModelName: model})
	if err != nil {
		t.Fatal(err)
	}
	proj, key, err := st.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prices := pricing.NewCache(st.DB(), log)
	gw := &Gateway{Store: st, Log: log, Limiter: &limits.Limiter{Store: st}, Prices: prices,
		Providers: func(c *store.ModelConnection) (provider.Provider, error) {
			return provider.New(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL,
				APIKey: c.APIKey, Model: c.ModelName, Timeout: 10 * time.Second})
		}}
	r := chi.NewRouter()
	r.Route("/v1", gw.Routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &costEnv{env: &env{t: t, st: st, srv: srv, conn: conn, proj: proj, key: key, gw: gw}, prices: prices}
}

// anthropicCacheReply answers with a cached completion: 10 fresh input
// tokens, 40 written to the cache, 200 read from it, 500 output.
func anthropicCacheReply(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":200,"output_tokens":500}}`))
}

func TestRequestCostFromCachedAnthropicUsage(t *testing.T) {
	e := newCostEnv(t, "anthropic", "claude-sonnet-4-5-20250929", anthropicCacheReply)
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Usage *provider.Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// prompt_tokens folds both cache counters in, so it stays comparable
	// with OpenAI's and with prompt + completion == total.
	if body.Usage.PromptTokens != 250 || body.Usage.CachedTokens() != 200 || body.Usage.CacheWriteTokens() != 40 {
		t.Errorf("usage = %+v", body.Usage)
	}

	rec := e.lastLog()
	if rec.CachedPromptTokens != 200 || rec.CacheWriteTokens != 40 || rec.PromptTokens != 250 {
		t.Errorf("log tokens = %+v", rec)
	}
	// claude-sonnet-4-5*: 3 / 15 with cache write 3.75 and read 0.30.
	// 10 uncached x 3 + 40 x 3.75 + 200 x 0.30 + 500 x 15 = 30+150+60+7500.
	const want = 7740
	if rec.CostMicros != want {
		t.Errorf("cost = %d micros, want %d", rec.CostMicros, want)
	}
	if rec.CostSource != store.PriceSourceBuiltin {
		t.Errorf("cost source = %q", rec.CostSource)
	}
	if rec.CostUSD != float64(want)/1e6 {
		t.Errorf("cost usd = %v", rec.CostUSD)
	}
}

func TestUnpricedModelLogsCostSourceNone(t *testing.T) {
	e := newCostEnv(t, "anthropic", "claude-experimental-x", anthropicCacheReply)
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
	resp.Body.Close()
	rec := e.lastLog()
	if rec.CostMicros != 0 || rec.CostSource != store.CostSourceNone {
		t.Errorf("unpriced model: cost %d source %q", rec.CostMicros, rec.CostSource)
	}
	// The cache tokens are still recorded; only the price is missing.
	if rec.CachedPromptTokens != 200 {
		t.Errorf("cached prompt tokens = %d", rec.CachedPromptTokens)
	}
}

// TestNegativeUpstreamUsageIsClamped: a broken or hostile upstream reporting
// negative token counts must not write a credit into the request log. The row
// is the input to the cost, the budget counters and the metrics, and a
// negative one subtracts from spend that really happened. custom_openai is
// the connection an operator points wherever they like, so it is the one
// whose numbers are least worth trusting.
func TestNegativeUpstreamUsageIsClamped(t *testing.T) {
	e := newCostEnv(t, "custom_openai", "gpt-4o", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":-78,"total_tokens":22}}`))
	})
	if _, err := e.st.CreateModelPrice(context.Background(), &store.ModelPrice{ProviderType: "custom_openai",
		ModelPattern: "gpt-4o*", InputPerMTok: 2.5, OutputPerMTok: 10, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	e.prices.Invalidate()

	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
	resp.Body.Close()

	rec := e.lastLog()
	if rec.PromptTokens < 0 || rec.CompletionTokens < 0 || rec.CachedPromptTokens < 0 || rec.CacheWriteTokens < 0 {
		t.Errorf("negative tokens reached the log: %+v", rec)
	}
	if rec.CostMicros < 0 || rec.CostUSD < 0 {
		t.Errorf("negative cost: %d micros, %v usd", rec.CostMicros, rec.CostUSD)
	}
	// The usable half survives; only the negative one is floored. 100 prompt
	// tokens at 2.5 per Mtok is 250 micros, and the output adds nothing.
	if rec.PromptTokens != 100 || rec.CompletionTokens != 0 || rec.CostMicros != 250 {
		t.Errorf("clamped row = %+v, want 100/0 tokens at 250 micros", rec)
	}
	// And the summary the dashboard reads stays non-negative.
	m, err := e.st.Summarize(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if m.PromptTokens < 0 || m.CompletionTokens < 0 || m.CostMicros < 0 || m.CostUSD < 0 {
		t.Errorf("summary went negative: %+v", m)
	}
}

// TestFullyNegativeUpstreamUsageFallsBackToTheEstimate: a usage block with
// nothing usable in it is no usage at all, so the row is marked estimated
// rather than recorded as a request that spent nothing.
func TestFullyNegativeUpstreamUsageFallsBackToTheEstimate(t *testing.T) {
	e := newCostEnv(t, "custom_openai", "gpt-4o", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":-100,"completion_tokens":-78,"total_tokens":-178}}`))
	})
	resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
	resp.Body.Close()

	rec := e.lastLog()
	if !rec.Estimated {
		t.Errorf("row = %+v, want the character estimate", rec)
	}
	if rec.PromptTokens <= 0 || rec.CompletionTokens <= 0 || rec.CostMicros < 0 {
		t.Errorf("estimated row = %+v", rec)
	}
}

// TestCostRecordedOnClientDisconnect: the deferred block runs on the 499
// path too, with whatever tokens the stream reported before the hang-up.
func TestCostRecordedOnClientDisconnect(t *testing.T) {
	released := make(chan struct{})
	e := newCostEnv(t, "anthropic", "claude-sonnet-4-5-20250929", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range []string{
			`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"cache_read_input_tokens":200}}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}`,
			`data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(ev + "\n\n"))
			f.Flush()
		}
		// Hold the response open until the client has gone away, so the
		// gateway takes the disconnect path rather than finishing cleanly.
		select {
		case <-r.Context().Done():
		case <-released:
		case <-time.After(5 * time.Second):
		}
	})
	defer close(released)

	ctx, cancel := context.WithCancel(context.Background())
	resp := e.post(ctx, "/v1/chat/completions", map[string]any{"messages": userMsg, "stream": true}, e.key)
	// Read enough for the usage-carrying chunk to have been written, then
	// hang up mid-stream.
	buf := make([]byte, 4096)
	_, _ = resp.Body.Read(buf)
	time.Sleep(100 * time.Millisecond)
	cancel()
	resp.Body.Close()

	rec := e.lastLog()
	if rec.StatusCode != statusClientClosed {
		t.Fatalf("status = %d, want 499", rec.StatusCode)
	}
	if rec.CostSource != store.PriceSourceBuiltin || rec.CostMicros == 0 {
		t.Errorf("499 path lost the cost: %d micros, source %q", rec.CostMicros, rec.CostSource)
	}
}

// TestRateLimitedRequestCostsNothing: nothing was sent upstream, so the row
// must not carry a cost or claim a price source.
func TestRateLimitedRequestCostsNothing(t *testing.T) {
	e := newCostEnv(t, "anthropic", "claude-sonnet-4-5-20250929", anthropicCacheReply)
	e.proj.RateLimitRPM = 1
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		resp := e.post(context.Background(), "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
		resp.Body.Close()
	}
	rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	rec := rows[0]
	if rec.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the throttled row, got %+v", rec)
	}
	if rec.CostMicros != 0 || rec.CostSource != store.CostSourceNone {
		t.Errorf("throttled row cost = %d source %q", rec.CostMicros, rec.CostSource)
	}
}

// TestCostSummariesAggregate checks the cost columns on the metric queries.
func TestCostSummariesAggregate(t *testing.T) {
	ctx := context.Background()
	e := newCostEnv(t, "anthropic", "claude-sonnet-4-5-20250929", anthropicCacheReply)
	for i := 0; i < 3; i++ {
		resp := e.post(ctx, "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key)
		resp.Body.Close()
	}
	e.lastLog()
	f := store.MetricsFilter{ProjectID: &e.proj.ID}
	sum, err := e.st.Summarize(ctx, f, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const perRequest = 7740
	if sum.CostMicros != 3*perRequest || sum.CostUSD != float64(3*perRequest)/1e6 {
		t.Errorf("summary cost = %d / %v", sum.CostMicros, sum.CostUSD)
	}
	byProject, err := e.st.SummarizeByProject(ctx, f, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(byProject) != 1 || byProject[0].CostMicros != 3*perRequest {
		t.Errorf("by-project cost = %+v", byProject)
	}
	daily, err := e.st.DailySeries(ctx, f, 2)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, d := range daily {
		total += d.CostMicros
	}
	if total != 3*perRequest {
		t.Errorf("daily cost total = %d", total)
	}
}
