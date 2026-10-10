package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// fakeClock is a hand-wound clock for breaker tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func testBreakers(threshold int, openFor time.Duration) (*Breakers, *fakeClock) {
	clk := newFakeClock()
	b := NewBreakers(threshold, openFor, nil)
	b.Now = clk.now
	return b, clk
}

var breakerConn = &store.ModelConnection{ID: 7, Name: "primary"}

// stateOf reads the connection's state the way the metric does.
func stateOf(t *testing.T, b *Breakers, name string) float64 {
	t.Helper()
	snap, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	v, ok := snap[name]
	if !ok {
		t.Fatalf("no state for %q in %v", name, snap)
	}
	return v
}

func TestBreakerOpensAfterTheThreshold(t *testing.T) {
	b, _ := testBreakers(3, 30*time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow(breakerConn); !ok {
			t.Fatalf("blocked after %d failures, threshold is 3", i)
		}
		b.report(breakerConn, 0, outcomeFailure)
	}
	if got := stateOf(t, b, "primary"); got != BreakerClosed {
		t.Fatalf("state after 2 failures = %v, want closed", got)
	}
	b.report(breakerConn, 0, outcomeFailure)
	if got := stateOf(t, b, "primary"); got != BreakerOpen {
		t.Fatalf("state after 3 failures = %v, want open", got)
	}
	if ok, _ := b.allow(breakerConn); ok {
		t.Error("an open breaker let a call through")
	}
	if !b.isOpen(breakerConn.ID) {
		t.Error("isOpen = false for an open breaker")
	}
}

func TestBreakerSuccessResetsTheConsecutiveCount(t *testing.T) {
	b, _ := testBreakers(3, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	b.report(breakerConn, 0, outcomeFailure)
	b.report(breakerConn, 0, outcomeSuccess)
	b.report(breakerConn, 0, outcomeFailure)
	b.report(breakerConn, 0, outcomeFailure)
	if got := stateOf(t, b, "primary"); got != BreakerClosed {
		t.Errorf("state = %v: failures separated by a success must not add up", got)
	}
}

func TestBreakerNeutralOutcomesDoNotCount(t *testing.T) {
	b, _ := testBreakers(2, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	for i := 0; i < 5; i++ {
		b.report(breakerConn, 0, outcomeNeutral)
	}
	if got := stateOf(t, b, "primary"); got != BreakerClosed {
		t.Fatalf("state = %v after neutral outcomes, want closed", got)
	}
	// Nor do they reset the count: one more real failure opens it.
	b.report(breakerConn, 0, outcomeFailure)
	if got := stateOf(t, b, "primary"); got != BreakerOpen {
		t.Errorf("state = %v, want open", got)
	}
}

func TestBreakerGoesHalfOpenAfterTheOpenDuration(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(29 * time.Second)
	if got := stateOf(t, b, "primary"); got != BreakerOpen {
		t.Fatalf("state at 29s = %v, want open", got)
	}
	clk.advance(time.Second)
	if got := stateOf(t, b, "primary"); got != BreakerHalfOpen {
		t.Fatalf("state at 30s = %v, want half-open", got)
	}
}

func TestBreakerHalfOpenAllowsOneProbeAndClosesOnSuccess(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(30 * time.Second)

	ok, probe := b.allow(breakerConn)
	if !ok || probe == 0 {
		t.Fatalf("first call in half-open: ok=%v probe=%v, want both", ok, probe != 0)
	}
	if ok, _ := b.allow(breakerConn); ok {
		t.Error("a second call went through while the probe was in flight")
	}
	b.report(breakerConn, probe, outcomeSuccess)
	if got := stateOf(t, b, "primary"); got != BreakerClosed {
		t.Fatalf("state after a successful probe = %v, want closed", got)
	}
	if ok, probe := b.allow(breakerConn); !ok || probe != 0 {
		t.Errorf("closed breaker: ok=%v probe=%v, want a plain pass", ok, probe != 0)
	}
}

func TestBreakerFailedProbeOpensAgainForAFullDuration(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(30 * time.Second)

	_, probe := b.allow(breakerConn)
	b.report(breakerConn, probe, outcomeFailure)
	if got := stateOf(t, b, "primary"); got != BreakerOpen {
		t.Fatalf("state after a failed probe = %v, want open", got)
	}
	clk.advance(29 * time.Second)
	if ok, _ := b.allow(breakerConn); ok {
		t.Error("let through before the new open duration was over")
	}
	clk.advance(time.Second)
	if ok, probe := b.allow(breakerConn); !ok || probe == 0 {
		t.Errorf("after the second open duration: ok=%v probe=%v, want a probe", ok, probe != 0)
	}
}

func TestBreakerNeutralProbeFreesTheSlot(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(30 * time.Second)

	_, probe := b.allow(breakerConn)
	// The client hung up mid-probe: nothing was learned about the target.
	b.report(breakerConn, probe, outcomeNeutral)
	if got := stateOf(t, b, "primary"); got != BreakerHalfOpen {
		t.Fatalf("state = %v, want still half-open", got)
	}
	if ok, probe := b.allow(breakerConn); !ok || probe == 0 {
		t.Errorf("next call: ok=%v probe=%v, want the probe slot back", ok, probe != 0)
	}
}

func TestBreakerStuckProbeStopsHoldingTheSlot(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(30 * time.Second)
	if ok, _ := b.allow(breakerConn); !ok {
		t.Fatal("no probe")
	}
	// The probe never reports back.
	clk.advance(29 * time.Second)
	if ok, _ := b.allow(breakerConn); ok {
		t.Fatal("second probe let through while the first is recent")
	}
	clk.advance(time.Second)
	if ok, _ := b.allow(breakerConn); !ok {
		t.Error("a probe that never answered blocks the breaker for good")
	}
}

func TestBreakerStaleProbeReportDoesNotFreeANewerProbe(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	clk.advance(30 * time.Second)
	ok1, p1 := b.allow(breakerConn)
	if !ok1 || p1 == 0 {
		t.Fatal("no first probe")
	}
	// The first probe goes quiet; the stuck-probe rule hands out a second one.
	clk.advance(30 * time.Second)
	ok2, p2 := b.allow(breakerConn)
	if !ok2 || p2 == 0 || p2 == p1 {
		t.Fatalf("second probe: ok=%v, same identity as the first=%v", ok2, p2 == p1)
	}
	// The first probe finally reports: it no longer owns the slot.
	b.report(breakerConn, p1, outcomeNeutral)
	if ok, _ := b.allow(breakerConn); ok {
		t.Fatal("a stale probe's report freed the slot of the current probe")
	}
	b.report(breakerConn, p2, outcomeNeutral)
	if ok, _ := b.allow(breakerConn); !ok {
		t.Error("the current probe's report did not free the slot")
	}
}

func TestOutcomeOfReadsHowAStreamEnded(t *testing.T) {
	newCall := func(ctx context.Context, status int) *call {
		return &call{
			r:   httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx),
			rec: &store.RequestLog{StatusCode: status},
		}
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		c    *call
		err  error
		want attemptOutcome
	}{
		{"completed", newCall(context.Background(), http.StatusOK), nil, outcomeSuccess},
		{"stream: client left", newCall(context.Background(), statusClientClosed), nil, outcomeNeutral},
		{"stream: upstream failed after the first byte", newCall(context.Background(), http.StatusBadGateway), nil, outcomeNeutral},
		{"client context ended", newCall(gone, http.StatusOK), nil, outcomeNeutral},
		{"client context ended on an error", newCall(gone, 0), errors.New("boom"), outcomeNeutral},
		{"upstream error", newCall(context.Background(), 0), errors.New("boom"), outcomeFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeOf(tc.c, tc.err); got != tc.want {
				t.Errorf("outcome = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBreakerDisabledNeverBlocks(t *testing.T) {
	for name, b := range map[string]*Breakers{
		"nil":       nil,
		"zero":      {},
		"threshold": NewBreakers(0, time.Minute, nil),
	} {
		for i := 0; i < 10; i++ {
			b.report(breakerConn, 0, outcomeFailure)
		}
		if ok, probe := b.allow(breakerConn); !ok || probe != 0 {
			t.Errorf("%s: ok=%v probe=%v, want a plain pass", name, ok, probe)
		}
		if b.isOpen(breakerConn.ID) {
			t.Errorf("%s: isOpen", name)
		}
		if snap, err := b.Snapshot(); err != nil || len(snap) != 0 {
			t.Errorf("%s: snapshot = %v, %v, want empty", name, snap, err)
		}
	}
}

func TestBreakerSnapshotFollowsRenamesAndDropsIdleConnections(t *testing.T) {
	b, clk := testBreakers(1, 30*time.Second)
	b.report(breakerConn, 0, outcomeFailure)
	renamed := &store.ModelConnection{ID: breakerConn.ID, Name: "renamed"}
	b.report(renamed, 0, outcomeNeutral)
	snap, _ := b.Snapshot()
	if len(snap) != 1 || snap["renamed"] != BreakerOpen {
		t.Fatalf("snapshot = %v, want only renamed=open", snap)
	}
	clk.advance(breakerIdleTTL + time.Second)
	if snap, _ := b.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %v, want the idle connection gone", snap)
	}
}

// ---- inside the request pipeline ----

// breakerEnv is a two-target route "pair" over e.conn ("mock") and "second",
// with a breaker set on a fake clock and a registry exporting its state.
type breakerEnv struct {
	*env
	up2          *upstream
	conn2        *store.ModelConnection
	hits1, hits2 atomic.Int64
	clock        *fakeClock
	scrape       func() string
}

func newBreakerEnv(t *testing.T, threshold, retries int) *breakerEnv {
	t.Helper()
	e := newEnv(t, nil)
	b := &breakerEnv{env: e, clock: newFakeClock()}
	e.gw.MaxRetries = retries
	e.noWait()
	e.gw.Breakers = NewBreakers(threshold, 30*time.Second, nil)
	e.gw.Breakers.Now = b.clock.now
	reg := e.withMetrics()
	e.gw.Metrics.RegisterBreakers(e.gw.Breakers.Snapshot)
	b.scrape = reg.Text
	conn2, up2 := e.secondConn("second", "second-model")
	b.up2, b.conn2 = up2, conn2
	counted(e.up, &b.hits1)
	counted(up2, &b.hits2)
	e.orderedRoute("pair", e.conn, conn2)
	return b
}

// pair sends one chat request through the route and returns the status.
func (b *breakerEnv) pair() int {
	b.t.Helper()
	resp, _ := b.chat(map[string]any{"model": "pair", "messages": userMsg})
	return resp.StatusCode
}

func (b *breakerEnv) wantCalls(first, second int64) {
	b.t.Helper()
	if b.hits1.Load() != first || b.hits2.Load() != second {
		b.t.Errorf("upstream calls = %d, %d, want %d, %d", b.hits1.Load(), b.hits2.Load(), first, second)
	}
}

func TestBreakerSkipsAnOpenTargetOnFallback(t *testing.T) {
	b := newBreakerEnv(t, 2, 0)
	status(b.up, &b.hits1, http.StatusInternalServerError)

	for i := 0; i < 2; i++ {
		if got := b.pair(); got != 200 {
			t.Fatalf("request %d: status %d, want the fallback to answer", i+1, got)
		}
	}
	b.wantCalls(2, 2)
	mustContain(t, b.scrape(),
		`ragmux_upstream_breaker_state{connection="mock"} 1`,
		`ragmux_upstream_breaker_state{connection="second"} 0`)

	// The breaker is open now: the primary is not called at all.
	if got := b.pair(); got != 200 {
		t.Fatalf("status %d with the primary open", got)
	}
	b.wantCalls(2, 3)
	row := b.lastLog()
	if row.Attempt != 1 || row.FallbackFrom == nil || *row.FallbackFrom != b.conn.ID || row.ModelName != "second-model" {
		t.Errorf("log attempt = %d fallback_from = %v model = %q, want 1, the primary, second-model",
			row.Attempt, row.FallbackFrom, row.ModelName)
	}
}

func TestBreakerStopsRetryingATargetOnceItOpens(t *testing.T) {
	b := newBreakerEnv(t, 2, 5)
	status(b.up, &b.hits1, http.StatusServiceUnavailable)
	if got := b.pair(); got != 200 {
		t.Fatalf("status %d", got)
	}
	// Five retries were allowed; the second failure opened the breaker.
	b.wantCalls(2, 1)
}

func TestBreakerProbesAfterTheOpenDurationAndClosesOnSuccess(t *testing.T) {
	b := newBreakerEnv(t, 1, 0)
	status(b.up, &b.hits1, http.StatusInternalServerError)
	b.pair()
	b.wantCalls(1, 1)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 1`)

	// The upstream recovers, but the breaker keeps skipping it until the
	// open duration is over.
	counted(b.up, &b.hits1)
	b.pair()
	b.wantCalls(1, 2)

	b.clock.advance(30 * time.Second)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 2`)
	if got := b.pair(); got != 200 {
		t.Fatalf("status %d", got)
	}
	b.wantCalls(2, 2) // the probe went to the primary and was answered by it
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`)
}

func TestBreakerFailedProbeOpensTheTargetAgain(t *testing.T) {
	b := newBreakerEnv(t, 1, 0)
	status(b.up, &b.hits1, http.StatusInternalServerError)
	b.pair()
	b.clock.advance(30 * time.Second)
	b.pair() // the probe fails, the second target answers
	b.wantCalls(2, 2)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 1`)
	b.pair()
	b.wantCalls(2, 3)
}

func TestBreakerTriesThePrimaryOnceWhenEveryTargetIsOpen(t *testing.T) {
	// Retries are on: a forced call must still not repeat.
	b := newBreakerEnv(t, 1, 2)
	status(b.up, &b.hits1, http.StatusServiceUnavailable)
	status(b.up2, &b.hits2, http.StatusServiceUnavailable)
	if got := b.pair(); got != http.StatusServiceUnavailable {
		t.Fatalf("status %d", got)
	}
	b.wantCalls(1, 1)
	mustContain(t, b.scrape(),
		`ragmux_upstream_breaker_state{connection="mock"} 1`,
		`ragmux_upstream_breaker_state{connection="second"} 1`)

	if got := b.pair(); got != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want the primary's own answer, not a fail-fast one", got)
	}
	b.wantCalls(2, 1) // the primary exactly once; the second target is not touched
	row := b.lastLog()
	if row.Attempt != 1 || row.FallbackFrom != nil {
		t.Errorf("log attempt = %d fallback_from = %v, want 1 and none", row.Attempt, row.FallbackFrom)
	}

	// An all-open route recovers through the forced call too.
	counted(b.up, &b.hits1)
	if got := b.pair(); got != 200 {
		t.Fatalf("status %d after the primary recovered", got)
	}
	b.wantCalls(3, 1)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`)
}

func TestBreakerOnARoutelessProjectCallsTheOnlyConnectionOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.MaxRetries = 2
	e.noWait()
	e.gw.Breakers = NewBreakers(1, 30*time.Second, nil)
	var hits atomic.Int64
	status(e.up, &hits, http.StatusServiceUnavailable)
	for i := 0; i < 3; i++ {
		resp, _ := e.chat(map[string]any{"messages": userMsg})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("request %d: status %d, want the upstream's 503", i+1, resp.StatusCode)
		}
	}
	// Request 1: one call, the breaker opens so the retries are dropped.
	// Requests 2 and 3: the only candidate is open, so it is called once each.
	if hits.Load() != 3 {
		t.Errorf("upstream calls = %d, want 3", hits.Load())
	}
}

func TestBreakerThresholdZeroSkipsNothing(t *testing.T) {
	b := newBreakerEnv(t, 0, 0)
	status(b.up, &b.hits1, http.StatusInternalServerError)
	for i := 0; i < 4; i++ {
		if got := b.pair(); got != 200 {
			t.Fatalf("request %d: status %d", i+1, got)
		}
	}
	b.wantCalls(4, 4)
	if text := b.scrape(); strings.Contains(text, "ragmux_upstream_breaker_state{") {
		t.Errorf("a disabled breaker exported a state:\n%s", text)
	}
}

func TestBreakerDoesNotCountTargetConfigurationErrors(t *testing.T) {
	b := newBreakerEnv(t, 2, 0)
	// A 404 sends the request to the next target, but it is the connection's
	// configuration that is wrong, not its health.
	status(b.up, &b.hits1, http.StatusNotFound)
	for i := 0; i < 4; i++ {
		if got := b.pair(); got != 200 {
			t.Fatalf("request %d: status %d", i+1, got)
		}
	}
	b.wantCalls(4, 4)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`)
}

func TestBreakerDoesNotCountClientErrors(t *testing.T) {
	b := newBreakerEnv(t, 2, 0)
	status(b.up, &b.hits1, http.StatusBadRequest)
	for i := 0; i < 4; i++ {
		if got := b.pair(); got != http.StatusBadRequest {
			t.Fatalf("request %d: status %d, want the upstream's 400", i+1, got)
		}
	}
	b.wantCalls(4, 0)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`)
}

func TestBreakerCountsTransportErrors(t *testing.T) {
	b := newBreakerEnv(t, 2, 0)
	reset(b.up, &b.hits1)
	b.pair()
	b.pair()
	b.pair()
	b.wantCalls(2, 3)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 1`)
}

// waitForLog waits until the newest request log row carries the status.
func (b *breakerEnv) waitForLog(statusCode int) *store.RequestLog {
	b.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := b.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &b.proj.ID}, 1)
		if err != nil {
			b.t.Fatal(err)
		}
		if len(rows) > 0 && rows[0].StatusCode == statusCode {
			return rows[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.t.Fatalf("no request log with status %d", statusCode)
	return nil
}

// cutStream makes an upstream send one chunk and then drop the connection.
func cutStream(up *upstream, hits *atomic.Int64) {
	up.setChat(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "c", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hel"}}}})
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
}

func (b *breakerEnv) stream() {
	b.t.Helper()
	resp := b.post(context.Background(), "/v1/chat/completions",
		map[string]any{"model": "pair", "messages": userMsg, "stream": true}, b.key)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		b.t.Fatalf("stream status %d", resp.StatusCode)
	}
	b.waitForLog(http.StatusBadGateway)
}

func TestBreakerClientLeavingBeforeTheFirstChunkIsNotASuccess(t *testing.T) {
	b := newBreakerEnv(t, 1, 0)
	status(b.up, &b.hits1, http.StatusInternalServerError)
	b.pair()
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 1`)
	b.clock.advance(30 * time.Second)
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 2`)

	// The probe is a stream the upstream never answers; the client gives up.
	b.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		b.hits1.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		b.up.seenCancel <- struct{}{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw := `{"model":"pair","messages":[{"role":"user","content":"hi"}],"stream":true}`
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.srv.URL+"/v1/chat/completions", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+b.key)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for b.hits1.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.hits1.Load() < 2 {
		t.Fatal("the probe never reached the upstream")
	}
	cancel()
	select {
	case <-b.up.seenCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request context was not cancelled after the client left")
	}
	<-done
	b.waitForLog(statusClientClosed)

	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 2`)
	if strings.Contains(b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`) {
		t.Error("a client that left closed the breaker")
	}
	// The slot is free again: the next probe goes to the primary.
	counted(b.up, &b.hits1)
	before := b.hits1.Load()
	if got := b.pair(); got != 200 || b.hits1.Load() != before+1 {
		t.Errorf("status %d, primary calls %d -> %d, want the next probe to reach it", got, before, b.hits1.Load())
	}
	mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 0`)
}

func TestBreakerMidStreamFailureNeitherResetsNorClosesIt(t *testing.T) {
	t.Run("does not reset the failure count", func(t *testing.T) {
		b := newBreakerEnv(t, 2, 0)
		status(b.up, &b.hits1, http.StatusInternalServerError)
		b.pair() // one failure, the breaker stays closed

		cutStream(b.up, &b.hits1)
		b.stream() // a started stream that dies says nothing good about the target

		status(b.up, &b.hits1, http.StatusInternalServerError)
		b.pair() // the second failure in a row opens it
		mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 1`)
	})
	t.Run("does not close a half-open breaker", func(t *testing.T) {
		b := newBreakerEnv(t, 1, 0)
		status(b.up, &b.hits1, http.StatusInternalServerError)
		b.pair()
		b.clock.advance(30 * time.Second)

		cutStream(b.up, &b.hits1)
		b.stream()
		mustContain(t, b.scrape(), `ragmux_upstream_breaker_state{connection="mock"} 2`)
	})
}

func TestBreakerFallbackIsCountedOnlyWhenTheNextTargetIsTried(t *testing.T) {
	b := newBreakerEnv(t, 1, 0)
	b.gw.Breakers.report(b.conn2, 0, outcomeFailure) // the second target is open
	status(b.up, &b.hits1, http.StatusInternalServerError)

	if got := b.pair(); got != http.StatusBadGateway {
		t.Fatalf("status %d, want the primary's own error (a 500 reaches the client as 502)", got)
	}
	b.wantCalls(1, 0)
	if strings.Contains(b.scrape(), `ragmux_upstream_fallbacks_total{`) {
		t.Errorf("a fallback to an open target was counted:\n%s", b.scrape())
	}

	// Once the open duration is over the second target is tried, and the
	// fallback is real.
	b.clock.advance(30 * time.Second)
	if got := b.pair(); got != 200 {
		t.Fatalf("status %d, want the second target to answer", got)
	}
	b.wantCalls(2, 1)
	mustContain(t, b.scrape(), `ragmux_upstream_fallbacks_total{reason="server_error"} 1`)
}
