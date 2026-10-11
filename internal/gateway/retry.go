package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
)

// Retry and fallback policy of invoke (ADR-020).
//
// The adapters only describe a failure (provider.Error: whether the request
// left the gateway, the upstream status, its Retry-After); this file decides
// what to do about it. A failed attempt is one of:
//
//	retry     the same target again, after a backoff
//	fallback  the route's next target
//	terminal  neither: the error belongs to the request, not to the target
//
// Nothing here runs once a byte has reached the client; the attempt contract
// (see upstreamCall) only lets an attempt fail while the client is untouched.

const (
	// retryBaseDelay is the ceiling of the first backoff; each further retry of
	// the same target doubles it.
	retryBaseDelay = 250 * time.Millisecond
	// retryMaxDelay caps a single wait, and is also the longest upstream
	// Retry-After the gateway sits out: anything longer is not waited for
	// (PRD rule 11), the next target takes over instead.
	retryMaxDelay = 2 * time.Second
)

// attemptClass is what invoke does about one failed attempt.
type attemptClass struct {
	// reason is the obs.Upstream* value the retry and fallback counters carry;
	// empty for a terminal failure.
	reason string
	// retry allows another attempt at the same target.
	retry bool
	// fallback allows an attempt at the next target.
	fallback bool
	// retryAfter is the delay the upstream asked for; it only matters when
	// retry is set, in which case it is at most retryMaxDelay.
	retryAfter time.Duration
}

// targetConfigError marks a failure that is the target's own configuration: a
// connection whose provider cannot even be built. It wraps the error the
// client sees.
type targetConfigError struct{ pe *provider.Error }

func (e *targetConfigError) Error() string { return e.pe.Error() }
func (e *targetConfigError) Unwrap() error { return e.pe }

// classifyAttempt maps a failed attempt onto the ADR-020 table.
func classifyAttempt(err error) attemptClass {
	var tc *targetConfigError
	if errors.As(err, &tc) {
		return attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		if errors.Is(err, context.DeadlineExceeded) {
			return attemptClass{reason: obs.UpstreamTimeout, fallback: true}
		}
		if errors.Is(err, context.Canceled) {
			return attemptClass{}
		}
		// Not an adapter error: a panic inside a stream, say. Nothing says
		// the request is at fault, so another target may do better.
		return attemptClass{reason: obs.UpstreamServerError, fallback: true}
	}

	// A failure in something the client supplied (an image URL it asked to
	// have fetched) is the request's, whatever its status says: another
	// target would fetch the same URL and fail the same way.
	if pe.ClientInput {
		return attemptClass{}
	}

	// The request never left: dial, DNS, refusal, TLS handshake. Whether it is
	// worth repeating is the adapter's word (a guard refusal is not).
	if pe.NotSent {
		return attemptClass{reason: obs.UpstreamDial, retry: pe.Retryable, fallback: true}
	}

	// A status the upstream answered with beats the normalised one, which
	// folds 500 into 502 and 529 into 503. Zero means no response arrived (or
	// the error is the gateway's own), and then only the server-side classes
	// below apply: an adapter's 4xx is a verdict on the request.
	status, answered := pe.UpstreamStatus, true
	if status == 0 {
		status, answered = pe.Status, false
	}
	switch {
	case answered && status == http.StatusTooManyRequests:
		return withRetryAfter(obs.UpstreamRateLimited, pe)
	case status == http.StatusServiceUnavailable || (answered && status == 529):
		return withRetryAfter(obs.UpstreamUnavailable, pe)
	case status == http.StatusGatewayTimeout || pe.Type == "timeout":
		return attemptClass{reason: obs.UpstreamTimeout, fallback: true}
	case status >= 500:
		return attemptClass{reason: obs.UpstreamServerError, fallback: true}
	case answered && (status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound):
		// The key, the permission or the model path is wrong for this target
		// only; another one may be configured correctly.
		return attemptClass{reason: obs.UpstreamTargetConfig, fallback: true}
	}
	return attemptClass{}
}

// withRetryAfter builds the class of an upstream 429 or 503. It is retried
// unless the upstream asked to be left alone for longer than a retry may wait.
func withRetryAfter(reason string, pe *provider.Error) attemptClass {
	cl := attemptClass{reason: reason, fallback: true}
	if pe.Retryable && pe.UpstreamRetryDelay <= retryMaxDelay {
		cl.retry, cl.retryAfter = true, pe.UpstreamRetryDelay
	}
	return cl
}

// backoffDelay is the full-jitter wait before the n-th retry of one target
// (n starts at 0): a uniform draw from [0, min(base*2^n, max)). rnd is in
// [0, 1).
func backoffDelay(n int, rnd float64) time.Duration {
	ceiling := retryMaxDelay
	if n < 3 { // base*2^3 already reaches the cap
		ceiling = min(retryBaseDelay<<n, retryMaxDelay)
	}
	return time.Duration(rnd * float64(ceiling))
}

// retryWait is how long to wait before the n-th retry: the backoff, or the
// upstream's Retry-After when that asks for longer.
func retryWait(n int, rnd float64, retryAfter time.Duration) time.Duration {
	return max(backoffDelay(n, rnd), retryAfter)
}

// attemptContext is the context of one attempt: the call's, bounded by what is
// left of the upstream budget. The cancel function must be called.
func (c *call) attemptContext() (context.Context, context.CancelFunc) {
	if c.deadline.IsZero() {
		return c.ctx(), func() {}
	}
	return context.WithDeadline(c.ctx(), c.deadline)
}

// budgetLeft is the time left of the upstream budget, or zero when the budget
// is unlimited. A budget that has run out reports one nanosecond so the caller
// still sees a limit.
func (c *call) budgetLeft() time.Duration {
	if c.deadline.IsZero() {
		return 0
	}
	return max(time.Until(c.deadline), time.Nanosecond)
}

// budgetSpent reports whether the upstream budget has run out.
func (c *call) budgetSpent() bool {
	return !c.deadline.IsZero() && !time.Now().Before(c.deadline)
}

// sleep waits d or until ctx ends, whichever comes first, and reports ctx's
// error in the second case. Tests replace it through Gateway.Sleep.
func (g *Gateway) sleep(ctx context.Context, d time.Duration) error {
	if g.Sleep != nil {
		return g.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
