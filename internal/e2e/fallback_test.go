package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/testdb"
)

// failingUpstream answers every call with the current status and counts the
// calls.
func failingUpstream(hits, status *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"error":{"message":"upstream says no"}}`))
	}))
}

// loggedAttempts reads attempt and fallback_from of the request_logs row
// stamped with id; the row is written after the response, so it polls.
func (e *env) loggedAttempts(id string) (attempt int, fallbackFrom *int64) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var at *int
		err := e.store.DB().QueryRow(context.Background(),
			`SELECT attempt, fallback_from FROM request_logs WHERE request_id = $1`, id).Scan(&at, &fallbackFrom)
		if err == nil {
			if at != nil {
				attempt = *at
			}
			return attempt, fallbackFrom
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no request_logs row for %s: %v", id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRouteFallsBackToTheSecondTarget(t *testing.T) {
	var hitsA, hitsB, statusA atomic.Int64
	statusA.Store(http.StatusInternalServerError)
	upA, upB := failingUpstream(&hitsA, &statusA), countingUpstream(&hitsB)
	defer upA.Close()
	defer upB.Close()
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, gwTune: func(g *gateway.Gateway) {
		g.MaxRetries, g.UpstreamTimeout = 2, 10*time.Second
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
	apiKey := proj["api_key"].(string)
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	route := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/routes", pid), map[string]any{"name": "smart", "targets": []any{
		map[string]any{"connection_id": connA, "priority": 0},
		map[string]any{"connection_id": connB, "priority": 1}}}, "")
	if route["_status"] != float64(201) {
		t.Fatalf("create route: %v", route)
	}

	body := map[string]any{"model": "smart", "messages": []map[string]string{{"role": "user", "content": "ping"}}}
	r, hdr := e.callRaw("POST", "/v1/chat/completions", body, apiKey)
	if r["_status"] != float64(200) {
		t.Fatalf("fallback call: %v", r)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Errorf("upstream calls a=%d b=%d, want 1 and 1 (a 500 is not retried)", hitsA.Load(), hitsB.Load())
	}
	attempt, from := e.loggedAttempts(hdr.Get("X-Request-Id"))
	if attempt != 2 || from == nil || *from != connA {
		t.Errorf("request log attempt=%d fallback_from=%v, want 2 and %d", attempt, from, connA)
	}

	// A 400 from the first target is the request's fault: nothing is tried
	// on the second one.
	hitsA.Store(0)
	hitsB.Store(0)
	statusA.Store(http.StatusBadRequest)
	r, _ = e.callRaw("POST", "/v1/chat/completions", body, apiKey)
	if r["_status"] != float64(400) || hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Errorf("400 must not fall back: %v a=%d b=%d", r, hitsA.Load(), hitsB.Load())
	}
}
