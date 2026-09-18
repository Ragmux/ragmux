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

// Record stores the outcome of an attempt.
func (l *LoginLimiter) Record(ctx context.Context, username, ip string, success bool) error {
	return l.Store.RecordLoginAttempt(ctx, username, ip, success)
}
