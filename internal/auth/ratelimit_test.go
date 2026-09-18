package auth

import (
	"context"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
)

func TestLoginLimiter(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	clock := time.Now()
	l := &LoginLimiter{Store: s, PerIP: 10, PerUser: 5, LockoutFailures: 20, LockoutWindow: 15 * time.Minute,
		Now: func() time.Time { return clock }}
	mustAllow := func(user, ip string, want bool) {
		t.Helper()
		allowed, retry, locked, err := l.Check(ctx, user, ip)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != want {
			t.Fatalf("Check(%s, %s) allowed=%v retry=%s locked=%v, want allowed=%v", user, ip, allowed, retry, locked, want)
		}
		if !allowed && !locked && retry != time.Minute {
			t.Errorf("per-minute limit should report a 60s Retry-After, got %s", retry)
		}
	}
	fail := func(user, ip string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := l.Record(ctx, user, ip, false); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Five failures for alice block alice even with the right password (the
	// handler checks before verifying), but bob from the same address may
	// still try: the per-IP budget is larger.
	fail("alice", "10.0.0.1", 5)
	mustAllow("alice", "10.0.0.1", false)
	mustAllow("alice", "10.0.0.2", false) // limit is per username, not per pair
	mustAllow("bob", "10.0.0.1", true)
	fail("bob", "10.0.0.1", 5)
	mustAllow("carol", "10.0.0.1", false) // 10 failures from the address now
	mustAllow("carol", "10.0.0.9", true)

	// Successes never count.
	if err := l.Record(ctx, "carol", "10.0.0.9", true); err != nil {
		t.Fatal(err)
	}
	mustAllow("carol", "10.0.0.9", true)

	// The minute window expires.
	clock = clock.Add(61 * time.Second)
	mustAllow("alice", "10.0.0.1", true)
	mustAllow("carol", "10.0.0.1", true)

	// Lockout: 20 failures inside 15 minutes from one address lock that
	// username/address pair out until the oldest failure leaves the window,
	// regardless of the minute budget. The same user from another address is
	// not locked, so an attacker cannot deny a user their own login.
	fail("dave", "10.0.0.3", 20)
	clock = clock.Add(2 * time.Minute)
	allowed, retry, locked, err := l.Check(ctx, "dave", "10.0.0.3")
	if err != nil || allowed || !locked {
		t.Fatalf("dave should be locked out: allowed=%v locked=%v err=%v", allowed, locked, err)
	}
	if retry <= 0 || retry > 15*time.Minute {
		t.Errorf("lockout Retry-After = %s, want within (0, 15m]", retry)
	}
	if allowed, _, locked, err := l.Check(ctx, "dave", "10.0.0.4"); err != nil || !allowed || locked {
		t.Errorf("dave from another address: allowed=%v locked=%v err=%v, want allowed", allowed, locked, err)
	}
	clock = clock.Add(14 * time.Minute)
	mustAllow("dave", "10.0.0.3", true)

	// Purging old rows does not touch recent ones.
	if _, err := s.DeleteLoginAttemptsBefore(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, byIP, _ := s.CountFailedLoginAttempts(ctx, "x", "10.0.0.3", time.Now().Add(-time.Hour)); byIP != 20 {
		t.Errorf("recent attempts purged: %d", byIP)
	}

	// Zero limits disable the corresponding check.
	l.PerUser, l.PerIP, l.LockoutFailures = 0, 0, 0
	clock = clock.Add(-16 * time.Minute)
	mustAllow("dave", "10.0.0.3", true)
}

func TestLoginLimiterRemainingAndLockouts(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	clock := time.Now()
	l := &LoginLimiter{Store: s, PerIP: 10, PerUser: 5, LockoutFailures: 3, LockoutWindow: 15 * time.Minute,
		Now: func() time.Time { return clock }}
	remaining := func(user string, want int) {
		t.Helper()
		got, ok, err := l.Remaining(ctx, user)
		if err != nil || !ok || got != want {
			t.Errorf("Remaining(%s) = %d ok=%v err=%v, want %d", user, got, ok, err, want)
		}
	}
	remaining("alice", 5)
	remaining("nobody", 5) // unknown names get the same budget as real ones
	for i := 0; i < 2; i++ {
		if err := l.Record(ctx, "alice", "10.0.0.1", false); err != nil {
			t.Fatal(err)
		}
	}
	remaining("alice", 3)
	if err := l.Record(ctx, "alice", "10.0.0.2", false); err != nil {
		t.Fatal(err)
	}
	remaining("alice", 2) // per username, whatever the address
	if err := l.Record(ctx, "alice", "10.0.0.1", true); err != nil {
		t.Fatal(err)
	}
	remaining("alice", 2) // successes do not count
	locks, err := l.ActiveLockouts(ctx)
	if err != nil || len(locks) != 0 {
		t.Fatalf("no lockout yet: %v %+v", err, locks)
	}
	if err := l.Record(ctx, "alice", "10.0.0.1", false); err != nil {
		t.Fatal(err)
	}
	remaining("alice", 1)
	locks, err = l.ActiveLockouts(ctx)
	if err != nil || len(locks) != 1 || locks[0].Username != "alice" || locks[0].IP != "10.0.0.1" {
		t.Fatalf("alice@10.0.0.1 should be locked: %v %+v", err, locks)
	}
	if allowed, retry, locked, _ := l.Check(ctx, "alice", "10.0.0.1"); allowed || !locked ||
		locks[0].Until.Sub(clock).Round(time.Second) != retry.Round(time.Second) {
		t.Errorf("lockout list and Check disagree: allowed=%v locked=%v retry=%s until=%s", allowed, locked, retry, locks[0].Until)
	}
	for i := 0; i < 10; i++ {
		if err := l.Record(ctx, "alice", "10.0.0.1", false); err != nil {
			t.Fatal(err)
		}
	}
	remaining("alice", 0) // never negative
	clock = clock.Add(16 * time.Minute)
	if locks, _ := l.ActiveLockouts(ctx); len(locks) != 0 {
		t.Errorf("expired lockout still listed: %+v", locks)
	}
	l.PerUser, l.LockoutFailures = 0, 0
	if _, ok, _ := l.Remaining(ctx, "alice"); ok {
		t.Error("no per-user limit should report ok=false")
	}
	if locks, _ := l.ActiveLockouts(ctx); locks == nil || len(locks) != 0 {
		t.Errorf("disabled lockout should list nothing: %+v", locks)
	}
}
