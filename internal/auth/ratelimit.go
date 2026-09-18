package auth

import (
	"context"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// LoginLimiter throttles login attempts using the login_attempts table so
// every replica sees the same counters. Only failed attempts count towards
// the limits; a success does not reset them, the windows simply expire.
type LoginLimiter struct {
	Store *store.Store
	// PerIP is the maximum number of failures per minute from one address.
	PerIP int
	// PerUser is the maximum number of failures per minute for one username.
	PerUser int
	// LockoutFailures failures within LockoutWindow from one address lock
	// that username/address pair out. Keying the lockout on the pair means a
	// stranger cannot lock a user out by hammering their username.
	LockoutFailures int
	LockoutWindow   time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// DefaultLoginLimiter returns a limiter with the documented defaults.
func DefaultLoginLimiter(st *store.Store) *LoginLimiter {
	return &LoginLimiter{Store: st, PerIP: 10, PerUser: 5, LockoutFailures: 20, LockoutWindow: 15 * time.Minute}
}

func (l *LoginLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Check reports whether the username/IP pair may attempt a login. When it
// may not, retryAfter is how long the caller should wait and locked tells
// whether the lockout (as opposed to a per-minute burst limit) triggered.
func (l *LoginLimiter) Check(ctx context.Context, username, ip string) (allowed bool, retryAfter time.Duration, locked bool, err error) {
	now := l.now()
	if l.LockoutFailures > 0 && l.LockoutWindow > 0 {
		byPair, err := l.Store.CountFailedLoginAttemptsForPair(ctx, username, ip, now.Add(-l.LockoutWindow))
		if err != nil {
			return false, 0, false, err
		}
		if byPair >= l.LockoutFailures {
			return false, l.lockoutRemaining(ctx, username, ip, now), true, nil
		}
	}
	byUser, byIP, err := l.Store.CountFailedLoginAttempts(ctx, username, ip, now.Add(-time.Minute))
	if err != nil {
		return false, 0, false, err
	}
	if (l.PerUser > 0 && byUser >= l.PerUser) || (l.PerIP > 0 && byIP >= l.PerIP) {
		return false, time.Minute, false, nil
	}
	return true, 0, false, nil
}

// CheckAddress applies only the per-address budget, for endpoints that have
// no username to key on (the first-run setup). Failures are still recorded
// with Record under a synthetic username.
func (l *LoginLimiter) CheckAddress(ctx context.Context, ip string) (allowed bool, retryAfter time.Duration, err error) {
	if l.PerIP <= 0 {
		return true, 0, nil
	}
	_, byIP, err := l.Store.CountFailedLoginAttempts(ctx, "", ip, l.now().Add(-time.Minute))
	if err != nil {
		return false, 0, err
	}
	if byIP >= l.PerIP {
		return false, time.Minute, nil
	}
	return true, 0, nil
}

// lockoutRemaining approximates the time until the oldest counted failure
// leaves the lockout window. Without a per-row timestamp lookup the whole
// window is reported, which is the safe upper bound.
func (l *LoginLimiter) lockoutRemaining(ctx context.Context, username, ip string, now time.Time) time.Duration {
	oldest, err := l.Store.OldestFailedLoginAttempt(ctx, username, ip, now.Add(-l.LockoutWindow))
	if err != nil || oldest.IsZero() {
		return l.LockoutWindow
	}
	rem := oldest.Add(l.LockoutWindow).Sub(now)
	if rem < time.Second {
		rem = time.Second
	}
	return rem
}

// Remaining reports how many more failures the username may record this
// minute before the per-user limit triggers (never below zero). It counts
// recorded failures only, so the answer is the same for an unknown username
// and a wrong password. ok is false when there is no per-user limit.
func (l *LoginLimiter) Remaining(ctx context.Context, username string) (remaining int, ok bool, err error) {
	if l.PerUser <= 0 {
		return 0, false, nil
	}
	byUser, _, err := l.Store.CountFailedLoginAttempts(ctx, username, "", l.now().Add(-time.Minute))
	if err != nil {
		return 0, false, err
	}
	return max(l.PerUser-byUser, 0), true, nil
}

// Lockout is a username/address pair Check currently refuses.
type Lockout struct {
	Username string    `json:"username"`
	IP       string    `json:"ip"`
	Until    time.Time `json:"until"`
}

// ActiveLockouts lists the pairs whose failures inside LockoutWindow reach
// LockoutFailures, with the time the lockout ends: the same rule Check
// applies and the same estimate lockoutRemaining reports.
func (l *LoginLimiter) ActiveLockouts(ctx context.Context) ([]Lockout, error) {
	out := []Lockout{}
	if l.LockoutFailures <= 0 || l.LockoutWindow <= 0 {
		return out, nil
	}
	now := l.now()
	pairs, err := l.Store.FailedLoginPairsSince(ctx, now.Add(-l.LockoutWindow), l.LockoutFailures)
	if err != nil {
		return nil, err
	}
	for _, p := range pairs {
		out = append(out, Lockout{Username: p.Username, IP: p.IP, Until: p.Oldest.Add(l.LockoutWindow).UTC()})
	}
	return out, nil
}

// Record stores the outcome of an attempt.
func (l *LoginLimiter) Record(ctx context.Context, username, ip string, success bool) error {
	return l.Store.RecordLoginAttempt(ctx, username, ip, success)
}
