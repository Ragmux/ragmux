package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// noWait replaces the backoff sleep so retry tests do not spend wall time.
func (e *env) noWait() {
	e.gw.Sleep = func(context.Context, time.Duration) error { return nil }
}

// status makes an upstream answer chat calls with a fixed error status and
// count them. Extra headers are written before the status.
func status(up *upstream, hits *atomic.Int64, code int, headers ...string) {
	up.setChat(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"scripted failure"}}`))
	})
}

// counted makes an upstream answer normally and count the calls.
func counted(up *upstream, hits *atomic.Int64) { countingChat(up, hits, nil) }

// reset makes an upstream read the request and then abort the connection with
// a TCP reset: the request left the gateway, no response came back.
func reset(up *upstream, hits *atomic.Int64) {
	up.setChat(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	})
}

// orderedRoute builds a two-target route that tries first before second.
func (e *env) orderedRoute(name string, first, second *store.ModelConnection) {
	e.addRoute(name,
		store.RouteTarget{ConnectionID: first.ID, Weight: 1, Priority: 0},
		store.RouteTarget{ConnectionID: second.ID, Weight: 1, Priority: 1})
}

func TestRetryTurnsA503IntoA200(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	var hits atomic.Int64
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		okChat(w, r)
	})
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 || hits.Load() != 2 {
		t.Fatalf("status %d after %d upstream calls: %v", resp.StatusCode, hits.Load(), out)
	}
	row := e.lastLog()
	if row.Attempt != 2 || row.FallbackFrom != nil || row.StatusCode != 200 {
		t.Errorf("log attempt = %d fallback_from = %v status = %d", row.Attempt, row.FallbackFrom, row.StatusCode)
	}
}

func TestRetryStopsAtTheRetryLimit(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	var hits atomic.Int64
	status(e.up, &hits, http.StatusServiceUnavailable)
	resp, _ := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 3 {
		t.Fatalf("status %d after %d upstream calls, want 503 after 1 + 2 retries", resp.StatusCode, hits.Load())
	}
	if row := e.lastLog(); row.Attempt != 3 {
		t.Errorf("log attempt = %d", row.Attempt)
	}
}

func TestZeroRetriesOnARoutelessProjectIsTheOldBehaviour(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 0
	var hits atomic.Int64
	status(e.up, &hits, http.StatusServiceUnavailable)
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Fatalf("status %d after %d upstream calls: %v", resp.StatusCode, hits.Load(), out)
	}
	if errorField(t, out, "type") == nil {
		t.Errorf("not an error envelope: %v", out)
	}
	row := e.lastLog()
	if row.Attempt != 1 || row.FallbackFrom != nil {
		t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
	}
}

func TestA500FallsBackToTheSecondTarget(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusInternalServerError)
	var lastModel atomic.Value
	countingChat(up2, &hits2, &lastModel)
	e.orderedRoute("pair", e.conn, conn2)

	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	// A 500 is not retried: one call at the first target, then the next one.
	if hits1.Load() != 1 || hits2.Load() != 1 {
		t.Errorf("calls = %d, %d, want 1, 1", hits1.Load(), hits2.Load())
	}
	if lastModel.Load() != "second-model" {
		t.Errorf("second target saw model %v", lastModel.Load())
	}
	row := e.lastLog()
	if row.Attempt != 2 || row.FallbackFrom == nil || *row.FallbackFrom != e.conn.ID {
		t.Errorf("log attempt = %d fallback_from = %v, want 2 and %d", row.Attempt, row.FallbackFrom, e.conn.ID)
	}
	if row.ModelName != "second-model" {
		t.Errorf("log model = %q, want the model that answered", row.ModelName)
	}
	rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 10)
	if err != nil || len(rows) != 1 {
		t.Errorf("request_logs rows = %d, %v, want exactly one", len(rows), err)
	}
}

func TestFallbackReportsTheLastError(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 0
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusInternalServerError)
	status(up2, &hits2, http.StatusBadGateway)
	e.orderedRoute("pair", e.conn, conn2)
	resp, _ := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != http.StatusBadGateway || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Errorf("status %d calls %d, %d", resp.StatusCode, hits1.Load(), hits2.Load())
	}
	row := e.lastLog()
	if row.Attempt != 2 || row.FallbackFrom == nil || row.StatusCode != http.StatusBadGateway {
		t.Errorf("log attempt = %d fallback_from = %v status = %d", row.Attempt, row.FallbackFrom, row.StatusCode)
	}
}

func TestRetryAfterBeyondTwoSecondsFallsBackWithoutRetrying(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusTooManyRequests, "Retry-After", "30")
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)
	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("status %d calls %d, %d: %v", resp.StatusCode, hits1.Load(), hits2.Load(), out)
	}
}

func TestRetryAfterBeyondTwoSecondsReachesTheClientWithoutAFallback(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	var hits atomic.Int64
	status(e.up, &hits, http.StatusServiceUnavailable, "Retry-After", "30")
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Fatalf("status %d after %d upstream calls: %v", resp.StatusCode, hits.Load(), out)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
}

func TestShortRetryAfterIsWaitedAndRetried(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 1
	var waits []time.Duration
	e.gw.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	e.gw.Rand = func() float64 { return 0 }
	var hits atomic.Int64
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		okChat(w, r)
	})
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 || hits.Load() != 2 {
		t.Fatalf("status %d after %d calls: %v", resp.StatusCode, hits.Load(), out)
	}
	// Zero jitter makes the backoff zero, so the wait is the Retry-After.
	if len(waits) != 1 || waits[0] != time.Second {
		t.Errorf("waits = %v, want one second", waits)
	}
}

func TestConnectionResetAfterSendIsNotRetried(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	reset(e.up, &hits1)
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)
	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("status %d calls %d, %d: %v", resp.StatusCode, hits1.Load(), hits2.Load(), out)
	}
	if row := e.lastLog(); row.Attempt != 2 || row.FallbackFrom == nil {
		t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
	}
}

func TestConnectionResetWithoutAFallbackIsNotRetried(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	var hits atomic.Int64
	reset(e.up, &hits)
	resp, _ := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode < 500 || hits.Load() != 1 {
		t.Errorf("status %d after %d calls, want a 5xx after one", resp.StatusCode, hits.Load())
	}
}

func TestRefusedConnectionIsRetriedBeforeItFallsBack(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	// A closed listener refuses the connection: the request never left.
	dead := newUpstream(t)
	deadConn, err := e.st.CreateConnection(context.Background(), &store.ModelConnection{Name: "dead",
		ProviderType: "custom_openai", BaseURL: dead.srv.URL + "/v1", APIKey: "sk-secretsecretsecret123", ModelName: "dead-model"})
	if err != nil {
		t.Fatal(err)
	}
	dead.srv.Close()
	var hits atomic.Int64
	counted(e.up, &hits)
	e.orderedRoute("pair", deadConn, e.conn)
	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 || hits.Load() != 1 {
		t.Fatalf("status %d, live target calls %d: %v", resp.StatusCode, hits.Load(), out)
	}
	// 1 + 2 retries at the dead target, then the live one.
	row := e.lastLog()
	if row.Attempt != 4 || row.FallbackFrom == nil || *row.FallbackFrom != deadConn.ID {
		t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
	}
}

func TestTargetSpecificClientErrorsFallBack(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			e := newEnv(t, nil)
			e.gw.MaxRetries = 2
			e.noWait()
			conn2, up2 := e.secondConn("second", "second-model")
			var hits1, hits2 atomic.Int64
			status(e.up, &hits1, code)
			counted(up2, &hits2)
			e.orderedRoute("pair", e.conn, conn2)
			resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
			if resp.StatusCode != 200 || hits1.Load() != 1 || hits2.Load() != 1 {
				t.Errorf("status %d calls %d, %d: %v", resp.StatusCode, hits1.Load(), hits2.Load(), out)
			}
		})
	}
}

func TestRequestErrorsDoNotFallBack(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			e := newEnv(t, nil)
			e.gw.MaxRetries = 2
			e.noWait()
			conn2, up2 := e.secondConn("second", "second-model")
			var hits1, hits2 atomic.Int64
			status(e.up, &hits1, code)
			counted(up2, &hits2)
			e.orderedRoute("pair", e.conn, conn2)
			resp, _ := e.chat(map[string]any{"model": "pair", "messages": userMsg})
			if resp.StatusCode < 400 || resp.StatusCode >= 500 || hits1.Load() != 1 || hits2.Load() != 0 {
				t.Errorf("status %d calls %d, %d, want a 4xx and the second target untouched", resp.StatusCode, hits1.Load(), hits2.Load())
			}
			if row := e.lastLog(); row.Attempt != 1 || row.FallbackFrom != nil {
				t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
			}
		})
	}
}

func TestStreamFailureBeforeTheFirstChunkFallsBack(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 0
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusInternalServerError)
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)
	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"model": "pair", "messages": userMsg, "stream": true}, e.key)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("status %d calls %d, %d: %s", resp.StatusCode, hits1.Load(), hits2.Load(), body)
	}
	if got := string(body); !strings.Contains(got, "Hel") || !strings.Contains(got, "[DONE]") {
		t.Errorf("stream body = %q", got)
	}
	if row := e.lastLog(); row.Attempt != 2 || row.FallbackFrom == nil || *row.FallbackFrom != e.conn.ID {
		t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
	}
}

func TestStreamCutAfterHeadersDoesNotFallBack(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		hits1.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hel"}}}})
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	})
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)
	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"model": "pair", "messages": userMsg, "stream": true}, e.key)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if hits1.Load() != 1 || hits2.Load() != 0 {
		t.Errorf("calls = %d, %d, want 1, 0: a started stream is never restarted", hits1.Load(), hits2.Load())
	}
	if !strings.Contains(string(body), "Hel") {
		t.Errorf("the chunk already sent is missing: %q", body)
	}
	if row := e.lastLog(); row.Attempt != 1 || row.FallbackFrom != nil {
		t.Errorf("log attempt = %d fallback_from = %v", row.Attempt, row.FallbackFrom)
	}
}

func TestTotalTimeStaysWithinTheUpstreamBudget(t *testing.T) {
	const budget = 400 * time.Millisecond
	hang := func(up *upstream, hits *atomic.Int64) {
		up.setChat(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			// The server only notices a gone client once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		})
	}
	t.Run("a hanging target spends the budget and nothing is left for the next", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gw.MaxRetries = 2
		e.gw.UpstreamTimeout = budget
		conn2, up2 := e.secondConn("second", "second-model")
		var hits1, hits2 atomic.Int64
		hang(e.up, &hits1)
		counted(up2, &hits2)
		e.orderedRoute("pair", e.conn, conn2)
		start := time.Now()
		resp, _ := e.chat(map[string]any{"model": "pair", "messages": userMsg})
		elapsed := time.Since(start)
		if resp.StatusCode != http.StatusGatewayTimeout || hits1.Load() != 1 || hits2.Load() != 0 {
			t.Errorf("status %d calls %d, %d", resp.StatusCode, hits1.Load(), hits2.Load())
		}
		if elapsed > budget+250*time.Millisecond {
			t.Errorf("took %v with a %v budget", elapsed, budget)
		}
	})
	t.Run("retries with real waits stay within the budget", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gw.MaxRetries = 10
		e.gw.UpstreamTimeout = budget
		var hits atomic.Int64
		status(e.up, &hits, http.StatusServiceUnavailable)
		start := time.Now()
		resp, _ := e.chat(map[string]any{"messages": userMsg})
		elapsed := time.Since(start)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d", resp.StatusCode)
		}
		if elapsed > budget+250*time.Millisecond {
			t.Errorf("took %v with a %v budget and %d calls", elapsed, budget, hits.Load())
		}
	})
	t.Run("a stream that never starts is cut by the budget", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gw.MaxRetries = 2
		e.gw.UpstreamTimeout = budget
		var hits atomic.Int64
		hang(e.up, &hits)
		start := time.Now()
		resp := e.post(context.Background(), "/v1/chat/completions",
			map[string]any{"messages": userMsg, "stream": true}, e.key)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if elapsed := time.Since(start); elapsed > budget+250*time.Millisecond {
			t.Errorf("took %v with a %v budget", elapsed, budget)
		}
		if resp.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("status %d, want 504", resp.StatusCode)
		}
	})
}

func TestRetryAndFallbackCountersMove(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.gw.MaxRetries = 1
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	status(e.up, &hits1, http.StatusServiceUnavailable)
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)
	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	e.lastLog()
	mustContain(t, reg.Text(),
		`ragmux_upstream_retries_total{reason="unavailable"} 1`,
		`ragmux_upstream_fallbacks_total{reason="unavailable"} 1`)
}

// upstreamCountersStayZero fails the test when a retry or fallback counter moved.
func upstreamCountersStayZero(t *testing.T, metrics string) {
	t.Helper()
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, "ragmux_upstream_retries_total") &&
			!strings.HasPrefix(line, "ragmux_upstream_fallbacks_total") {
			continue
		}
		if f := strings.Fields(line); len(f) == 0 || f[len(f)-1] != "0" {
			t.Errorf("counter moved: %s", line)
		}
	}
}

// An image URL the client supplied that cannot be fetched is the request's
// fault, not the target's: no retry, no fallback, no upstream failure counted.
func TestUnfetchableImageURLIsNeitherRetriedNorFallenBackFrom(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits1, hits2 atomic.Int64
	counted(e.up, &hits1)
	counted(up2, &hits2)
	e.orderedRoute("pair", e.conn, conn2)

	var dials atomic.Int64
	fetcher := &provider.ImageFetcher{Client: &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, &net.DNSError{Err: "no such host", Name: "images.invalid", IsNotFound: true}
		}}}}
	e.gw.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
		return provider.New(provider.Config{ProviderType: "gemini", BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: 10 * time.Second, Images: fetcher})
	}

	resp, out := e.chat(map[string]any{"model": "pair", "messages": []map[string]any{{"role": "user",
		"content": []map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "http://images.invalid/a.png"}}}}}})
	if resp.StatusCode < 400 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if dials.Load() != 1 || hits1.Load() != 0 || hits2.Load() != 0 {
		t.Errorf("image dials = %d, upstream calls = %d, %d, want 1, 0, 0", dials.Load(), hits1.Load(), hits2.Load())
	}
	row := e.lastLog()
	if row.Attempt != 1 || row.FallbackFrom != nil {
		t.Errorf("log attempt = %d fallback_from = %v, want 1 and none", row.Attempt, row.FallbackFrom)
	}
	upstreamCountersStayZero(t, reg.Text())
}

// A first target whose adapter cannot be built is its own configuration
// fault: the route moves on to the next target instead of answering 500.
func TestFirstTargetWithoutAnAdapterFallsBack(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.gw.MaxRetries = 2
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var hits atomic.Int64
	counted(up2, &hits)
	e.orderedRoute("pair", e.conn, conn2)
	broken := e.conn.ID
	e.gw.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
		if c.ID == broken {
			return nil, errors.New("unsupported provider type")
		}
		return provider.New(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: 10 * time.Second})
	}

	resp, out := e.chat(map[string]any{"model": "pair", "messages": userMsg})
	if resp.StatusCode != 200 || hits.Load() != 1 {
		t.Fatalf("status %d after %d calls at the second target: %v", resp.StatusCode, hits.Load(), out)
	}
	// Only the call that reached an upstream counts as an attempt.
	row := e.lastLog()
	if row.Attempt != 1 || row.FallbackFrom == nil || *row.FallbackFrom != broken {
		t.Errorf("log attempt = %d fallback_from = %v, want 1 and %d", row.Attempt, row.FallbackFrom, broken)
	}
	mustContain(t, reg.Text(), `ragmux_upstream_fallbacks_total{reason="target_config"} 1`)
}

// With a single target there is nothing to fall back to: the 500 stays.
func TestSingleTargetWithoutAnAdapterAnswers500(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.Providers = func(*store.ModelConnection) (provider.Provider, error) {
		return nil, errors.New("unsupported provider type")
	}
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if got := errorField(t, out, "message"); got != "model connection unavailable" {
		t.Errorf("message = %v", got)
	}
}

// Retrieval does not depend on the first target's chat adapter: when that one
// cannot be built, the request is still answered from the next target with
// the retrieved context in it.
func TestRAGRetrievalRunsWhenTheFirstTargetHasNoAdapter(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	e.noWait()
	conn2, up2 := e.secondConn("second", "second-model")
	var sawContext atomic.Bool
	up2.setChat(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "apples are red") {
			sawContext.Store(true)
		}
		okChat(w, r)
	})
	e.orderedRoute("pair", e.conn, conn2)
	broken := e.conn.ID
	e.gw.Providers = func(c *store.ModelConnection) (provider.Provider, error) {
		if c.ID == broken {
			return nil, errors.New("unsupported provider type")
		}
		return provider.New(provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: 10 * time.Second})
	}

	resp, out := e.chat(map[string]any{"model": "pair", "messages": []map[string]any{{"role": "user", "content": "what colour are apples?"}}})
	if resp.StatusCode != 200 || resp.Header.Get("x-ragmux-rag-hits") != "1" || !sawContext.Load() {
		t.Fatalf("status %d hits %q context at the second target %v: %v", resp.StatusCode, resp.Header.Get("x-ragmux-rag-hits"), sawContext.Load(), out)
	}
	row := e.lastLog()
	if !row.RAGUsed || row.FallbackFrom == nil || *row.FallbackFrom != broken {
		t.Errorf("log rag_used = %v fallback_from = %v", row.RAGUsed, row.FallbackFrom)
	}
}
