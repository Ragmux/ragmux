package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
)

// Circuit breaker of a model connection (ADR-020).
//
// A connection that keeps failing is taken out of the candidate list for a
// while, so retry and fallback do not spend the request's time budget on a
// dead target. One breaker per connection, in memory, per replica: the state
// is never written to the database (ADR-011), so N replicas probe a bad
// upstream N times.
//
//	closed     calls pass; consecutive counted failures are tallied
//	open       the connection is skipped until the open duration has passed
//	half-open  one probe call goes through: success closes the breaker, a
//	           counted failure opens it again
//
// The breaker only decides whether a target is tried and learns how the try
// went; what to do about a failed attempt stays with retry.go.

// Breaker states as the ragmux_upstream_breaker_state gauge reports them.
const (
	BreakerClosed   = 0
	BreakerOpen     = 1
	BreakerHalfOpen = 2
)

// breakerIdleTTL is how long a connection that saw no traffic stays in the
// scrape snapshot. It keeps the gauge from reporting the state of a deleted
// connection for the life of the process.
const breakerIdleTTL = time.Hour

// probeToken names one half-open probe. Zero means the call is not a probe.
// Without it a probe that reports late (after its slot timed out and went to a
// newer probe) would free the newer probe's slot.
type probeToken uint64

// attemptOutcome is how one attempt reads to the breaker.
type attemptOutcome int

const (
	// outcomeNeutral says nothing about the target's health: the client left,
	// the request itself was refused, or the target's own configuration
	// (a 401, 403 or 404) is wrong. The failure counter stays where it is.
	outcomeNeutral attemptOutcome = iota
	// outcomeSuccess closes the breaker.
	outcomeSuccess
	// outcomeFailure is a failure ADR-020 counts: a dial or transport error,
	// 429, 503/529, another 5xx, a 504 or a timeout.
	outcomeFailure
)

// outcomeOf classifies the result of one attempt for the breaker. It reuses
// classifyAttempt, so the breaker counts exactly what retry and fallback
// treat as an upstream fault: everything with a reason except target_config.
//
// A stream reports nil even when it did not finish cleanly, and says how it
// ended through the request record: a client that left before or during the
// stream (status 499) and an upstream that failed after the first byte
// (status 502, written as an SSE error event) both come back neutral. Neither
// proves the target healthy, so neither may close a breaker or reset its
// failure count.
func outcomeOf(c *call, err error) attemptOutcome {
	if c.ctx().Err() != nil {
		// The client went away; whatever the attempt reported is not the
		// upstream's doing, success included.
		return outcomeNeutral
	}
	if err == nil {
		switch c.rec.StatusCode {
		case statusClientClosed, http.StatusBadGateway:
			return outcomeNeutral
		}
		return outcomeSuccess
	}
	switch cl := classifyAttempt(err); cl.reason {
	case "", obs.UpstreamTargetConfig:
		return outcomeNeutral
	}
	return outcomeFailure
}

// Breakers holds the circuit breaker of every connection this replica has
// called. The zero value and nil are both disabled: every method is safe on
// them and nothing is ever blocked.
type Breakers struct {
	// Threshold is the number of consecutive counted failures that opens a
	// breaker. Zero or less disables breaking.
	Threshold int
	// OpenFor is how long an open breaker stays open before it lets a probe
	// through.
	OpenFor time.Duration
	// Log receives state transitions; nil logs nothing.
	Log *slog.Logger
	// Now is the clock; nil uses time.Now. Tests inject a fake one.
	Now func() time.Time

	mu sync.Mutex
	m  map[int64]*breaker
}

// breaker is the state of one connection.
type breaker struct {
	name     string
	failures int
	open     bool
	openedAt time.Time
	// probing marks the half-open probe as in flight, probeAt is when it left
	// and probeID is its identity: only that probe's report frees the slot.
	probing  bool
	probeAt  time.Time
	probeID  probeToken
	nextID   probeToken
	lastSeen time.Time
}

// NewBreakers builds a breaker set. A threshold of zero or less returns a set
// that never blocks.
func NewBreakers(threshold int, openFor time.Duration, log *slog.Logger) *Breakers {
	return &Breakers{Threshold: threshold, OpenFor: openFor, Log: log}
}

func (b *Breakers) enabled() bool { return b != nil && b.Threshold > 0 }

func (b *Breakers) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// get returns the breaker of conn, creating it on first use. b.mu is held.
func (b *Breakers) get(conn *store.ModelConnection, now time.Time) *breaker {
	if b.m == nil {
		b.m = map[int64]*breaker{}
	}
	br := b.m[conn.ID]
	if br == nil {
		br = &breaker{}
		b.m[conn.ID] = br
	}
	br.name = conn.Name // a renamed connection reports under its new name
	br.lastSeen = now
	return br
}

// state is the breaker's state at now. b.mu is held.
func (b *Breakers) state(br *breaker, now time.Time) int {
	switch {
	case !br.open:
		return BreakerClosed
	case now.Sub(br.openedAt) < b.OpenFor:
		return BreakerOpen
	}
	return BreakerHalfOpen
}

// allow reports whether conn may be called now. probe is non-zero when the
// call is the half-open probe: its outcome must be reported with that token so
// a neutral result can release the probe slot.
func (b *Breakers) allow(conn *store.ModelConnection) (ok bool, probe probeToken) {
	if !b.enabled() {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	br := b.get(conn, now)
	switch b.state(br, now) {
	case BreakerClosed:
		return true, 0
	case BreakerHalfOpen:
		// One probe at a time. A probe that never reported back (its request
		// is stuck) stops holding the slot after another open duration.
		if !br.probing || now.Sub(br.probeAt) >= b.OpenFor {
			br.nextID++
			br.probing, br.probeAt, br.probeID = true, now, br.nextID
			return true, br.probeID
		}
	}
	return false, 0
}

// isOpen reports whether conn's breaker is open right now (not half-open),
// without taking a probe slot.
func (b *Breakers) isOpen(id int64) bool {
	if !b.enabled() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.m[id]
	return br != nil && b.state(br, b.now()) == BreakerOpen
}

// report records the outcome of an attempt allow let through.
func (b *Breakers) report(conn *store.ModelConnection, probe probeToken, o attemptOutcome) {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	br := b.get(conn, now)
	if probe != 0 && probe == br.probeID {
		br.probing = false
	}
	switch o {
	case outcomeSuccess:
		wasOpen := br.open
		br.failures, br.open = 0, false
		if wasOpen {
			b.logf(slog.LevelInfo, "circuit breaker closed", conn, br)
		}
	case outcomeFailure:
		br.failures++
		switch b.state(br, now) {
		case BreakerClosed:
			if br.failures >= b.Threshold {
				br.open, br.openedAt = true, now
				b.logf(slog.LevelWarn, "circuit breaker opened", conn, br)
			}
		case BreakerHalfOpen:
			br.openedAt = now
			b.logf(slog.LevelWarn, "circuit breaker reopened", conn, br)
		}
		// Open: a forced call (every candidate was open) failed. The open
		// window keeps running from the failure that started it.
	}
}

func (b *Breakers) logf(level slog.Level, msg string, conn *store.ModelConnection, br *breaker) {
	if b.Log == nil {
		return
	}
	b.Log.Log(context.Background(), level, msg, "connection", conn.Name, "connection_id", conn.ID,
		"consecutive_failures", br.failures, "open_for", b.OpenFor.String())
}

// Snapshot is the state of every breaker this replica has seen traffic for,
// by connection name, as the BreakerClosed/Open/HalfOpen values. It is cheap
// and in memory only: the metrics registry calls it while a scrape is built.
// The names come from the control plane (ADR-002), never from a request.
func (b *Breakers) Snapshot() (map[string]float64, error) {
	out := map[string]float64{}
	if !b.enabled() {
		return out, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for id, br := range b.m {
		if now.Sub(br.lastSeen) > breakerIdleTTL {
			delete(b.m, id)
			continue
		}
		out[br.name] = float64(b.state(br, now))
	}
	return out, nil
}
