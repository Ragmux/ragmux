package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

// seededRand is a concurrency-safe, deterministic randFloat.
func seededRand(seed uint64) func() float64 {
	var mu sync.Mutex
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return r.Float64()
	}
}

func strp(s string) *string { return &s }

// secondConn adds a connection to a second scripted upstream and returns both.
func (e *env) secondConn(name, model string) (*store.ModelConnection, *upstream) {
	e.t.Helper()
	up := newUpstream(e.t)
	conn, err := e.st.CreateConnection(context.Background(), &store.ModelConnection{Name: name,
		ProviderType: "custom_openai", BaseURL: up.srv.URL + "/v1", APIKey: "sk-secretsecretsecret123", ModelName: model})
	if err != nil {
		e.t.Fatal(err)
	}
	return conn, up
}

func (e *env) addRoute(name string, targets ...store.RouteTarget) *store.Route {
	e.t.Helper()
	rt, err := e.st.CreateRoute(context.Background(), &store.Route{ProjectID: e.proj.ID, Name: name,
		Kind: store.RouteKindChat, Strategy: store.RouteStrategyWeighted, Targets: targets})
	if err != nil {
		e.t.Fatal(err)
	}
	return rt
}

// countingChat makes an upstream answer chat calls and count them, keeping
// the model string of the last request it saw.
func countingChat(up *upstream, hits *atomic.Int64, lastModel *atomic.Value) {
	up.setChat(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if lastModel != nil {
			lastModel.Store(req.Model)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		okChat(w, r)
	})
}

func TestOrderTargetsKeepsPriorityGroupsInOrder(t *testing.T) {
	mk := func(id int64, w, p int) store.RouteTarget { return store.RouteTarget{ID: id, Weight: w, Priority: p} }
	targets := []store.RouteTarget{mk(1, 1, 0), mk(2, 5, 0), mk(3, 1, 1), mk(4, 1, 1), mk(5, 9, 2)}
	rnd := seededRand(7)
	for i := 0; i < 200; i++ {
		got := orderTargets(targets, rnd)
		if len(got) != len(targets) {
			t.Fatalf("lost targets: %+v", got)
		}
		for j, want := range []int{0, 0, 1, 1, 2} {
			if got[j].Priority != want {
				t.Fatalf("priority order broken at %d: %+v", j, got)
			}
		}
		seen := map[int64]bool{}
		for _, g := range got {
			if seen[g.ID] {
				t.Fatalf("target repeated: %+v", got)
			}
			seen[g.ID] = true
		}
	}
	if targets[0].ID != 1 || targets[4].ID != 5 {
		t.Error("orderTargets modified its input")
	}
}

func TestOrderTargetsWeightedFirstPick(t *testing.T) {
	targets := []store.RouteTarget{{ID: 1, Weight: 3}, {ID: 2, Weight: 1}}
	rnd := seededRand(42)
	const n = 8000
	first := 0
	for i := 0; i < n; i++ {
		if orderTargets(targets, rnd)[0].ID == 1 {
			first++
		}
	}
	if share := float64(first) / n; share < 0.72 || share > 0.78 {
		t.Errorf("heavier target led %.3f of the time, want about 0.75", share)
	}
}

func TestRouteWeightedDistributionThroughTheGateway(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.Rand = seededRand(2024)
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	countingChat(e.up, &hits1, nil)
	countingChat(up2, &hits2, nil)
	e.addRoute("balanced",
		store.RouteTarget{ConnectionID: e.conn.ID, Weight: 3},
		store.RouteTarget{ConnectionID: conn2.ID, Weight: 1})

	const n = 200
	for i := 0; i < n; i++ {
		resp, out := e.chat(map[string]any{"model": "balanced", "messages": userMsg})
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %v", i, resp.StatusCode, out)
		}
	}
	a, b := hits1.Load(), hits2.Load()
	if a+b != n {
		t.Fatalf("calls = %d + %d, want %d", a, b, n)
	}
	if share := float64(a) / n; share < 0.65 || share > 0.85 {
		t.Errorf("weight 3 target served %.2f of %d calls (%d vs %d), want about 0.75", share, n, a, b)
	}
}

func TestRoutelessProjectKeepsEchoAndModelList(t *testing.T) {
	e := newEnv(t, nil)
	resp, out := e.chat(map[string]any{"model": "whatever-the-client-says", "messages": userMsg})
	if resp.StatusCode != 200 || out["model"] != "whatever-the-client-says" {
		t.Fatalf("echo = %d %v", resp.StatusCode, out)
	}
	rec := e.lastLog()
	if rec.ModelName != "mock-model" || rec.RequestedModel != "whatever-the-client-says" || rec.Route != "" {
		t.Errorf("log = model %q requested %q route %q", rec.ModelName, rec.RequestedModel, rec.Route)
	}

	list := e.post(context.Background(), "/v1/models", nil, e.key)
	defer list.Body.Close()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(list.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0]["id"] != "mock-model" || body.Data[0]["owned_by"] != "custom_openai" {
		t.Errorf("model list changed: %v", body.Data)
	}
}

func TestRoutedProjectRejectsUnknownModel(t *testing.T) {
	e := newEnv(t, nil)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})
	var hits atomic.Int64
	countingChat(e.up, &hits, nil)

	resp, out := e.chat(map[string]any{"model": "nope", "messages": userMsg})
	if resp.StatusCode != 404 || errorField(t, out, "code") != "model_not_found" ||
		errorField(t, out, "type") != "invalid_request_error" {
		t.Fatalf("unknown model = %d %v", resp.StatusCode, out)
	}
	msg, _ := errorField(t, out, "message").(string)
	if !strings.Contains(msg, "nope") || strings.Contains(msg, "smart") || strings.Contains(msg, "mock-model") {
		t.Errorf("message = %q: must name the request and not list valid names", msg)
	}
	if hits.Load() != 0 {
		t.Error("an unmatched model reached the upstream")
	}

	long := strings.Repeat("x", 500)
	_, out = e.chat(map[string]any{"model": long, "messages": userMsg})
	if msg, _ := errorField(t, out, "message").(string); len(msg) > 300 {
		t.Errorf("message echoes an unbounded model: %d bytes", len(msg))
	}
}

// A NUL byte cannot be bound to a PostgreSQL text parameter. A model that
// carries one is just another model that matches nothing: 404, not a 500.
func TestRoutedProjectModelWithNULByteIsNotFound(t *testing.T) {
	e := newEnv(t, nil)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})
	var hits atomic.Int64
	countingChat(e.up, &hits, nil)

	for _, model := range []string{"x\x00", "\x00", "smart\x00", "mock-model\x00"} {
		resp, out := e.chat(map[string]any{"model": model, "messages": userMsg})
		if resp.StatusCode != 404 || errorField(t, out, "code") != "model_not_found" {
			t.Errorf("model %q = %d %v, want 404 model_not_found", model, resp.StatusCode, out)
		}
	}
	if hits.Load() != 0 {
		t.Error("a NUL-byte model reached the upstream")
	}
}

func TestRoutedProjectImplicitRoute(t *testing.T) {
	e := newEnv(t, nil)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})

	for _, model := range []string{"mock-model", ""} {
		body := map[string]any{"messages": userMsg}
		if model != "" {
			body["model"] = model
		}
		resp, out := e.chat(body)
		if resp.StatusCode != 200 || out["model"] != "mock-model" {
			t.Fatalf("model %q = %d %v", model, resp.StatusCode, out)
		}
		rec := e.lastLog()
		if rec.Route != "" || rec.ModelName != "mock-model" || rec.RequestedModel != model {
			t.Errorf("model %q log = route %q model %q requested %q", model, rec.Route, rec.ModelName, rec.RequestedModel)
		}
	}
}

func TestExplicitRouteWinsOverImplicitName(t *testing.T) {
	e := newEnv(t, nil)
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	countingChat(e.up, &hits1, nil)
	countingChat(up2, &hits2, nil)
	e.addRoute("mock-model", store.RouteTarget{ConnectionID: conn2.ID, Weight: 1})

	resp, _ := e.chat(map[string]any{"model": "mock-model", "messages": userMsg})
	if resp.StatusCode != 200 || hits2.Load() != 1 || hits1.Load() != 0 {
		t.Fatalf("explicit route did not win: status %d, hits %d/%d", resp.StatusCode, hits1.Load(), hits2.Load())
	}
	if rec := e.lastLog(); rec.Route != "mock-model" {
		t.Errorf("route = %q", rec.Route)
	}
}

func TestRouteTargetModelOverrideReachesUpstream(t *testing.T) {
	e := newEnv(t, nil)
	var hits atomic.Int64
	var seen atomic.Value
	countingChat(e.up, &hits, &seen)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Model: strp("override-model"), Weight: 1})

	resp, out := e.chat(map[string]any{"model": "smart", "messages": userMsg})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	if got, _ := seen.Load().(string); got != "override-model" {
		t.Errorf("upstream saw model %q, want override-model", got)
	}
	if out["model"] != "smart" {
		t.Errorf("response model = %v, want the client's value", out["model"])
	}
	rec := e.lastLog()
	if rec.ModelName != "override-model" || rec.RequestedModel != "smart" || rec.Route != "smart" || rec.Attempt != 1 {
		t.Errorf("log = model %q requested %q route %q attempt %d", rec.ModelName, rec.RequestedModel, rec.Route, rec.Attempt)
	}
	// The stored connection is untouched by the override.
	got, err := e.st.GetConnection(context.Background(), e.conn.ID)
	if err != nil || got.ModelName != "mock-model" {
		t.Errorf("stored connection = %+v, %v", got, err)
	}
}

func TestRouteStreamEchoesRouteName(t *testing.T) {
	e := newEnv(t, nil)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Model: strp("override-model"), Weight: 1})
	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"model": "smart", "stream": true, "messages": userMsg}, e.key)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"model":"smart"`) || strings.Contains(string(raw), "override-model") {
		t.Fatalf("stream = %d %s", resp.StatusCode, raw)
	}
	if rec := e.lastLog(); rec.Route != "smart" || rec.ModelName != "override-model" {
		t.Errorf("log = route %q model %q", rec.Route, rec.ModelName)
	}
}

func TestRoutedProjectModelList(t *testing.T) {
	e := newEnv(t, nil)
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})
	e.addRoute("cheap", store.RouteTarget{ConnectionID: e.conn.ID, Model: strp("small"), Weight: 1})

	resp := e.post(context.Background(), "/v1/models", nil, e.key)
	defer resp.Body.Close()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m["id"].(string)] = true
	}
	if len(body.Data) != 3 || !ids["smart"] || !ids["cheap"] || !ids["mock-model"] {
		t.Errorf("model list = %v", body.Data)
	}
}

func TestEmbeddingsOnlyRoutesDoNotActivateChatRouting(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.st.CreateRoute(context.Background(), &store.Route{ProjectID: e.proj.ID, Name: "emb",
		Kind: store.RouteKindEmbeddings, Strategy: store.RouteStrategyWeighted,
		Targets: []store.RouteTarget{{ConnectionID: e.conn.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	resp, out := e.chat(map[string]any{"model": "anything", "messages": userMsg})
	if resp.StatusCode != 200 || out["model"] != "anything" {
		t.Fatalf("chat = %d %v", resp.StatusCode, out)
	}
}

func TestThrottledRoutedRequestLogsItsRoute(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1 })
	e.addRoute("smart", store.RouteTarget{ConnectionID: e.conn.ID, Weight: 1})
	e.chat(map[string]any{"model": "smart", "messages": userMsg})
	resp, _ := e.chat(map[string]any{"model": "smart", "messages": userMsg})
	if resp.StatusCode != 429 {
		t.Fatalf("second request was not throttled: status %d", resp.StatusCode)
	}
	if rec := e.lastLog(); rec.Route != "smart" || rec.RequestedModel != "smart" {
		t.Errorf("429 row = route %q requested %q", rec.Route, rec.RequestedModel)
	}
}
