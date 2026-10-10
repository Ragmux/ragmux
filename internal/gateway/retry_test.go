package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
)

func TestClassifyAttempt(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want attemptClass
	}{
		{"a failed image fetch is the client's, even when it looks like a dial error",
			&provider.Error{Status: 502, NotSent: true, Retryable: true, ClientInput: true},
			attemptClass{}},
		{"a timed out image fetch is the client's",
			&provider.Error{Status: 504, Type: "timeout", ClientInput: true},
			attemptClass{}},
		{"dial error is retried and falls back",
			&provider.Error{Status: 502, NotSent: true, Retryable: true},
			attemptClass{reason: obs.UpstreamDial, retry: true, fallback: true}},
		{"guard refusal is not retried",
			&provider.Error{Status: 502, NotSent: true},
			attemptClass{reason: obs.UpstreamDial, fallback: true}},
		{"connection reset after send falls back only",
			&provider.Error{Status: 502, Type: "server_error"},
			attemptClass{reason: obs.UpstreamServerError, fallback: true}},
		{"429 without Retry-After is retried",
			&provider.Error{Status: 429, UpstreamStatus: 429, Retryable: true},
			attemptClass{reason: obs.UpstreamRateLimited, retry: true, fallback: true}},
		{"429 with a short Retry-After is retried",
			&provider.Error{Status: 429, UpstreamStatus: 429, Retryable: true, UpstreamRetryDelay: 2 * time.Second},
			attemptClass{reason: obs.UpstreamRateLimited, retry: true, fallback: true, retryAfter: 2 * time.Second}},
		{"429 with a long Retry-After is not retried",
			&provider.Error{Status: 429, UpstreamStatus: 429, Retryable: true, UpstreamRetryDelay: 30 * time.Second},
			attemptClass{reason: obs.UpstreamRateLimited, fallback: true}},
		{"503 is retried",
			&provider.Error{Status: 503, UpstreamStatus: 503, Retryable: true},
			attemptClass{reason: obs.UpstreamUnavailable, retry: true, fallback: true}},
		{"529 is unavailable",
			&provider.Error{Status: 503, UpstreamStatus: 529},
			attemptClass{reason: obs.UpstreamUnavailable, fallback: true}},
		{"500 falls back without a retry",
			&provider.Error{Status: 502, UpstreamStatus: 500},
			attemptClass{reason: obs.UpstreamServerError, fallback: true}},
		{"502 falls back without a retry",
			&provider.Error{Status: 502, UpstreamStatus: 502},
			attemptClass{reason: obs.UpstreamServerError, fallback: true}},
		{"504 falls back",
			&provider.Error{Status: 504, UpstreamStatus: 504},
			attemptClass{reason: obs.UpstreamTimeout, fallback: true}},
		{"timeout type falls back only with budget",
			&provider.Error{Status: 504, Type: "timeout"},
			attemptClass{reason: obs.UpstreamTimeout, fallback: true}},
		{"401 is target config",
			&provider.Error{Status: 502, UpstreamStatus: 401},
			attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}},
		{"403 is target config",
			&provider.Error{Status: 502, UpstreamStatus: 403},
			attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}},
		{"404 is target config",
			&provider.Error{Status: 404, UpstreamStatus: 404},
			attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}},
		{"400 is terminal", &provider.Error{Status: 400, UpstreamStatus: 400}, attemptClass{}},
		{"413 is terminal", &provider.Error{Status: 413, UpstreamStatus: 413}, attemptClass{}},
		{"422 is terminal", &provider.Error{Status: 422, UpstreamStatus: 422}, attemptClass{}},
		{"a gateway-made 401 is terminal", &provider.Error{Status: 401}, attemptClass{}},
		{"deadline exceeded falls back only with budget", context.DeadlineExceeded,
			attemptClass{reason: obs.UpstreamTimeout, fallback: true}},
		{"client cancel is terminal", context.Canceled, attemptClass{}},
		{"unknown error falls back", errors.New("boom"),
			attemptClass{reason: obs.UpstreamServerError, fallback: true}},
		{"target config error falls back",
			&targetConfigError{pe: &provider.Error{Status: 500}},
			attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}},
		{"wrapped errors are unwrapped",
			fmt.Errorf("wrap: %w", &provider.Error{Status: 503, UpstreamStatus: 503, Retryable: true}),
			attemptClass{reason: obs.UpstreamUnavailable, retry: true, fallback: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyAttempt(c.err); got != c.want {
				t.Errorf("classifyAttempt = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		n    int
		rnd  float64
		want time.Duration
	}{
		{0, 0, 0},
		{0, 0.5, 125 * time.Millisecond},
		{1, 0.5, 250 * time.Millisecond},
		{2, 0.5, 500 * time.Millisecond},
		{3, 0.5, time.Second},
		{4, 0.5, time.Second},
		{10, 0.999, 1998 * time.Millisecond},
	}
	for _, c := range cases {
		if got := backoffDelay(c.n, c.rnd); got != c.want {
			t.Errorf("backoffDelay(%d, %v) = %v, want %v", c.n, c.rnd, got, c.want)
		}
	}
	for n := 0; n < 40; n++ {
		if got := backoffDelay(n, 0.9999999); got >= retryMaxDelay {
			t.Errorf("backoffDelay(%d) = %v reaches the cap", n, got)
		}
	}
}

func TestRetryWaitTakesTheLongerOfBackoffAndRetryAfter(t *testing.T) {
	if got := retryWait(0, 0.5, time.Second); got != time.Second {
		t.Errorf("Retry-After longer than backoff: %v", got)
	}
	if got := retryWait(2, 0.5, 100*time.Millisecond); got != 500*time.Millisecond {
		t.Errorf("backoff longer than Retry-After: %v", got)
	}
	if got := retryWait(1, 0.5, 0); got != 250*time.Millisecond {
		t.Errorf("no Retry-After: %v", got)
	}
}

func TestCallBudget(t *testing.T) {
	c := &call{}
	if c.budgetSpent() || c.budgetLeft() != 0 {
		t.Errorf("an unlimited budget reports spent=%v left=%v", c.budgetSpent(), c.budgetLeft())
	}
	c.deadline = time.Now().Add(time.Hour)
	if c.budgetSpent() || c.budgetLeft() <= 0 {
		t.Errorf("open budget reports spent=%v left=%v", c.budgetSpent(), c.budgetLeft())
	}
	c.deadline = time.Now().Add(-time.Second)
	if !c.budgetSpent() || c.budgetLeft() != time.Nanosecond {
		t.Errorf("spent budget reports spent=%v left=%v", c.budgetSpent(), c.budgetLeft())
	}
}

func TestGatewaySleepStopsWithTheContext(t *testing.T) {
	g := &Gateway{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := g.sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep on a cancelled context = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("sleep ignored the cancelled context")
	}
	if err := g.sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("short sleep = %v", err)
	}
}
