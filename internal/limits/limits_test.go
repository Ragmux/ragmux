package limits

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, p store.Project) (*Limiter, *store.Project, *clock) {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p.Name, p.ModelConnectionID = "p", conn.ID
	proj, _, err := st.CreateProject(ctx, &p)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 9, 18, 10, 30, 20, 0, time.UTC)}
	return &Limiter{Store: st, Now: c.now}, proj, c
}

func mustCheck(t *testing.T, l *Limiter, p *store.Project, est int) Decision {
	t.Helper()
	d, err := l.Check(context.Background(), p, est)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRequestsPerMinute(t *testing.T) {
	l, p, c := setup(t, store.Project{RateLimitRPM: 2})
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		d := mustCheck(t, l, p, 5)
		if !d.Allowed || !d.Reserved || d.LimitRequests != 2 || d.RemainingRequests != 2-i {
			t.Fatalf("check %d: %+v", i, d)
		}
		if err := l.Record(ctx, p.ID, 10, 2, !d.Reserved); err != nil {
			t.Fatal(err)
		}
	}
	d := mustCheck(t, l, p, 5)
	if d.Allowed || d.Reason != ReasonRPM || d.RemainingRequests != 0 || d.RetryAfter != 40*time.Second || d.Reserved {
		t.Fatalf("third request: %+v", d)
	}
	// The rejected reservation was released: the minute row still says 2.
	u, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Minute.Requests != 2 || u.Minute.RequestLimit != 2 || u.Minute.RequestPercent != 100 || u.Minute.Tokens != 24 {
		t.Fatalf("usage: %+v", u.Minute)
	}
	if !u.Minute.ResetsAt.Equal(time.Date(2026, 9, 18, 10, 31, 0, 0, time.UTC)) {
		t.Fatalf("resets_at: %v", u.Minute.ResetsAt)
	}
	// Next minute is a fresh window.
	c.t = c.t.Add(time.Minute)
	if d := mustCheck(t, l, p, 5); !d.Allowed || d.RemainingRequests != 1 {
		t.Fatalf("next minute: %+v", d)
	}
}

func TestTokensPerMinute(t *testing.T) {
	l, p, c := setup(t, store.Project{RateLimitTPM: 100})
	ctx := context.Background()
	d := mustCheck(t, l, p, 30)
	if !d.Allowed || d.Reserved || d.LimitRequests != 0 {
		t.Fatalf("first: %+v", d)
	}
	if err := l.Record(ctx, p.ID, 80, 15, true); err != nil {
		t.Fatal(err)
	}
	// 95 used + 10 estimated > 100.
	d = mustCheck(t, l, p, 10)
	if d.Allowed || d.Reason != ReasonTPM || d.RetryAfter != 40*time.Second {
		t.Fatalf("over tpm: %+v", d)
	}
	// 95 + 5 fits exactly.
	if d := mustCheck(t, l, p, 5); !d.Allowed {
		t.Fatalf("exact fit: %+v", d)
	}
	c.t = c.t.Add(time.Minute)
	if d := mustCheck(t, l, p, 100); !d.Allowed {
		t.Fatalf("next minute: %+v", d)
	}
}

// CheckTokens re-checks TPM for a request already admitted: it takes no
// request slot, keeps the slot the first check took, and denies once the
// larger estimate no longer fits.
func TestCheckTokensGrowsTheEstimateWithoutReserving(t *testing.T) {
	l, p, c := setup(t, store.Project{RateLimitTPM: 100, RateLimitRPM: 5})
	ctx := context.Background()
	subject := Subject{Project: p}
	first, err := l.CheckSubject(ctx, subject, 10)
	if err != nil || !first.Allowed || !first.Reserved {
		t.Fatalf("first: %+v %v", first, err)
	}
	if d, err := l.CheckTokens(ctx, subject, first, 100); err != nil || !d.Allowed || !d.Reserved || d.RemainingRequests != first.RemainingRequests {
		t.Fatalf("exact fit: %+v %v", d, err)
	}
	d, err := l.CheckTokens(ctx, subject, first, 101)
	if err != nil || d.Allowed || d.Reason != ReasonTPM || d.Scope != ScopeProject || d.RetryAfter != 40*time.Second || !d.Reserved {
		t.Fatalf("over tpm: %+v %v", d, err)
	}
	// Neither call took a request slot of its own.
	if u, err := l.Usage(ctx, p); err != nil || u.Minute.Requests != 1 {
		t.Fatalf("usage after rechecks: %+v %v", u.Minute, err)
	}
	// Usage already recorded in the minute counts against the new estimate.
	if err := l.Record(ctx, p.ID, 60, 0, false); err != nil {
		t.Fatal(err)
	}
	if d, _ := l.CheckTokens(ctx, subject, first, 41); d.Allowed {
		t.Fatalf("60 used + 41 should not fit: %+v", d)
	}
	if d, _ := l.CheckTokens(ctx, subject, first, 40); !d.Allowed {
		t.Fatalf("60 used + 40 should fit: %+v", d)
	}
	c.t = c.t.Add(time.Minute)
	if d, _ := l.CheckTokens(ctx, subject, first, 100); !d.Allowed {
		t.Fatalf("next minute: %+v", d)
	}
}

// Without a TPM limit on either tier CheckTokens has nothing to enforce.
func TestCheckTokensWithoutTPMLimit(t *testing.T) {
	l, p, _ := setup(t, store.Project{RateLimitRPM: 5})
	subject := Subject{Project: p}
	first, err := l.CheckSubject(context.Background(), subject, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := l.CheckTokens(context.Background(), subject, first, 1<<30); err != nil || !d.Allowed {
		t.Fatalf("no limit: %+v %v", d, err)
	}
}

func TestDailyAndMonthlyBudget(t *testing.T) {
	l, p, c := setup(t, store.Project{BudgetDailyTokens: 50, BudgetMonthlyTokens: 120})
	ctx := context.Background()
	if d := mustCheck(t, l, p, 1000); !d.Allowed || d.DailyLimit != 50 || d.DailyUsed != 0 {
		t.Fatalf("empty day: %+v", d)
	}
	// A single request may overshoot; the next one is blocked.
	if err := l.Record(ctx, p.ID, 40, 20, true); err != nil {
		t.Fatal(err)
	}
	d := mustCheck(t, l, p, 1)
	if d.Allowed || d.Reason != ReasonBudgetDaily || d.DailyUsed != 60 {
		t.Fatalf("daily exhausted: %+v", d)
	}
	if want := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC).Sub(c.t); d.RetryAfter != want {
		t.Fatalf("retry after %v, want %v", d.RetryAfter, want)
	}
	u, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Day.Tokens != 60 || u.Day.TokenPercent != 100 || u.Month.Tokens != 60 || u.Month.TokenPercent != 50 {
		t.Fatalf("usage: day %+v month %+v", u.Day, u.Month)
	}
	// Next UTC day: daily budget is fresh, monthly keeps accumulating.
	c.t = time.Date(2026, 9, 19, 0, 0, 5, 0, time.UTC)
	if d := mustCheck(t, l, p, 1); !d.Allowed || d.DailyUsed != 0 || d.MonthlyUsed != 60 {
		t.Fatalf("next day: %+v", d)
	}
	if err := l.Record(ctx, p.ID, 40, 20, true); err != nil {
		t.Fatal(err)
	}
	d = mustCheck(t, l, p, 1)
	if d.Allowed || d.Reason != ReasonBudgetMonthly {
		t.Fatalf("monthly exhausted: %+v", d)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Sub(c.t); d.RetryAfter != want {
		t.Fatalf("monthly retry after %v, want %v", d.RetryAfter, want)
	}
	c.t = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if d := mustCheck(t, l, p, 1); !d.Allowed || d.MonthlyUsed != 0 {
		t.Fatalf("next month: %+v", d)
	}
}

func TestUnlimitedStillCounts(t *testing.T) {
	l, p, c := setup(t, store.Project{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		d := mustCheck(t, l, p, 1000000)
		if !d.Allowed || d.Reserved || d.LimitRequests != 0 || d.DailyLimit != 0 {
			t.Fatalf("unlimited check %d: %+v", i, d)
		}
		if err := l.Record(ctx, p.ID, 7, 3, !d.Reserved); err != nil {
			t.Fatal(err)
		}
	}
	u, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Minute.Requests != 5 || u.Day.Requests != 5 || u.Month.Tokens != 50 || u.Day.TokenPercent != 0 {
		t.Fatalf("usage: %+v", u)
	}

	// Purge: minute rows older than two hours go, day/month rows stay.
	c.t = c.t.Add(3 * time.Hour)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(-3 * time.Hour)
	u, _ = l.Usage(ctx, p)
	if u.Minute.Requests != 0 || u.Day.Requests != 5 || u.Month.Requests != 5 {
		t.Fatalf("after purge: %+v", u)
	}
}

func TestBudgetForecast(t *testing.T) {
	l, p, c := setup(t, store.Project{BudgetDailyTokens: 1000, BudgetMonthlyTokens: 5000})
	ctx := context.Background()
	u, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Forecast.DailyExhaustedAt != nil || u.Forecast.MonthlyExhaustedAt != nil {
		t.Fatalf("forecast without usage: %+v", u.Forecast)
	}
	// 500 tokens 70 minutes ago fall outside the lookback; 100 + 200 inside.
	base := c.t
	c.t = base.Add(-70 * time.Minute)
	if err := l.Record(ctx, p.ID, 400, 100, true); err != nil {
		t.Fatal(err)
	}
	c.t = base.Add(-30 * time.Minute)
	if err := l.Record(ctx, p.ID, 80, 20, true); err != nil {
		t.Fatal(err)
	}
	c.t = base.Add(-time.Minute)
	if err := l.Record(ctx, p.ID, 150, 50, true); err != nil {
		t.Fatal(err)
	}
	c.t = base
	u, err = l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// Day used 800 of 1000 at 300 tokens/hour: 200 left is 40 minutes.
	if got := u.Forecast.DailyExhaustedAt; got == nil || !got.Equal(base.Add(40*time.Minute)) {
		t.Errorf("daily forecast: %v", got)
	}
	// Month used 800 of 5000: 4200 left is 14 hours, still inside September.
	if got := u.Forecast.MonthlyExhaustedAt; got == nil || !got.Equal(base.Add(14*time.Hour)) {
		t.Errorf("monthly forecast: %v", got)
	}

	// A budget that outlasts its window has no projection.
	p.BudgetDailyTokens = 1_000_000
	u, _ = l.Usage(ctx, p)
	if u.Forecast.DailyExhaustedAt != nil || u.Forecast.MonthlyExhaustedAt == nil {
		t.Errorf("large daily budget: %+v", u.Forecast)
	}
	// No budget at all: nothing is projected.
	p.BudgetDailyTokens, p.BudgetMonthlyTokens = 0, 0
	u, _ = l.Usage(ctx, p)
	if u.Forecast.DailyExhaustedAt != nil || u.Forecast.MonthlyExhaustedAt != nil {
		t.Errorf("no budget: %+v", u.Forecast)
	}
	// An exhausted budget projects to now.
	p.BudgetDailyTokens = 700
	u, _ = l.Usage(ctx, p)
	if got := u.Forecast.DailyExhaustedAt; got == nil || !got.Equal(base) {
		t.Errorf("exhausted: %v", got)
	}
}

// ---- key sub-limits ----

// keySubject creates a gateway key owned by a fresh user and returns the
// Subject the gateway would build for it.
func keySubject(t *testing.T, l *Limiter, p *store.Project, caps Caps) Subject {
	t.Helper()
	ctx := context.Background()
	u, err := l.Store.CreateUser(ctx, "owner", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := l.Store.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "k", UserID: u.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{p.ID},
		RateLimitRPM: caps.RPM, RateLimitTPM: caps.TPM,
		BudgetDailyTokens: caps.DailyTokens, BudgetMonthlyTokens: caps.MonthlyTokens})
	if err != nil {
		t.Fatal(err)
	}
	return Subject{Project: p, KeyID: &k.ID, Key: caps}
}

func mustCheckSubject(t *testing.T, l *Limiter, s Subject, est int) Decision {
	t.Helper()
	d, err := l.CheckSubject(context.Background(), s, est)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustKey(t *testing.T, l *Limiter, s Subject) *store.APIKey {
	t.Helper()
	k, err := l.Store.GetAPIKey(context.Background(), *s.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyOnlyLimits(t *testing.T) {
	l, p, _ := setup(t, store.Project{})
	ctx := context.Background()
	s := keySubject(t, l, p, Caps{RPM: 2, DailyTokens: 50})

	for i := 1; i <= 2; i++ {
		d := mustCheckSubject(t, l, s, 1)
		if !d.Allowed || d.Reserved || d.LimitRequests != 2 || d.RemainingRequests != 2-i {
			t.Fatalf("check %d: %+v", i, d)
		}
		// The project has no RPM limit, so Reserved stays false even though
		// the key slot was taken; RecordSubject derives the key tier itself.
		if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 10, CompletionTokens: 2, CountRequest: !d.Reserved}); err != nil {
			t.Fatal(err)
		}
	}
	d := mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Reason != ReasonRPM || d.Scope != ScopeKey || d.RemainingRequests != 0 {
		t.Fatalf("third request: %+v", d)
	}
	// The rejected key reservation was released: two requests, not three.
	u, err := l.KeyUsage(ctx, mustKey(t, l, s))
	if err != nil {
		t.Fatal(err)
	}
	if u.APIKeyID != *s.KeyID || u.ProjectID != 0 || u.Minute.Requests != 2 || u.Minute.Tokens != 24 || u.Day.TokenPercent != 48 {
		t.Fatalf("key usage: %+v %+v", u.Minute, u.Day)
	}
	// The project counters ran alongside without a project limit.
	pu, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Minute.Requests != 2 || pu.Minute.RequestLimit != 0 {
		t.Fatalf("project usage: %+v", pu.Minute)
	}
}

func TestKeyAndProjectTiersPickTheTighterBound(t *testing.T) {
	ctx := context.Background()

	// The key budget is the smaller one: it denies, and the headers describe it.
	l, p, _ := setup(t, store.Project{BudgetDailyTokens: 1000})
	s := keySubject(t, l, p, Caps{DailyTokens: 20})
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 15, CompletionTokens: 10, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	d := mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Scope != ScopeKey || d.Reason != ReasonBudgetDaily || d.DailyLimit != 20 || d.DailyUsed != 25 {
		t.Fatalf("key budget denies: %+v", d)
	}

	// The project budget is the smaller one: same request, other scope.
	l, p, _ = setup(t, store.Project{BudgetDailyTokens: 20})
	s = keySubject(t, l, p, Caps{DailyTokens: 1000})
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 15, CompletionTokens: 10, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Scope != ScopeProject || d.DailyLimit != 20 || d.DailyUsed != 25 {
		t.Fatalf("project budget denies: %+v", d)
	}

	// Both set and neither exhausted: the headers report the tighter tier.
	l, p, _ = setup(t, store.Project{BudgetMonthlyTokens: 1000, RateLimitRPM: 10})
	s = keySubject(t, l, p, Caps{MonthlyTokens: 100, RPM: 3})
	d = mustCheckSubject(t, l, s, 1)
	if !d.Allowed || d.MonthlyLimit != 100 || d.LimitRequests != 3 || d.RemainingRequests != 2 || !d.Reserved {
		t.Fatalf("tighter tier in headers: %+v", d)
	}
}

// The project slot is taken before the key slot; when the key slot overflows
// both have to be released or the project leaks a reservation for the minute.
func TestRPMReservationRollsBackBothTiers(t *testing.T) {
	l, p, _ := setup(t, store.Project{RateLimitRPM: 10})
	ctx := context.Background()
	s := keySubject(t, l, p, Caps{RPM: 1})

	d := mustCheckSubject(t, l, s, 1)
	if !d.Allowed || !d.Reserved {
		t.Fatalf("first: %+v", d)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 1, CompletionTokens: 1, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Scope != ScopeKey || d.Reason != ReasonRPM || d.Reserved {
		t.Fatalf("second: %+v", d)
	}
	// One request got through, so exactly one slot is held on each tier.
	pu, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	ku, err := l.KeyUsage(ctx, mustKey(t, l, s))
	if err != nil {
		t.Fatal(err)
	}
	if pu.Minute.Requests != 1 || pu.Minute.RequestLimit != 10 || pu.Minute.RequestPercent != 10 {
		t.Errorf("project leaked a reservation: %+v", pu.Minute)
	}
	if ku.Minute.Requests != 1 {
		t.Errorf("key leaked a reservation: %+v", ku.Minute)
	}
}

func TestPurgeUsageCoversBothTiers(t *testing.T) {
	l, p, c := setup(t, store.Project{})
	ctx := context.Background()
	s := keySubject(t, l, p, Caps{})
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 5, CompletionTokens: 5, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(3 * time.Hour)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(-3 * time.Hour)
	ku, err := l.KeyUsage(ctx, mustKey(t, l, s))
	if err != nil {
		t.Fatal(err)
	}
	if ku.Minute.Requests != 0 || ku.Day.Requests != 1 || ku.Month.Tokens != 10 {
		t.Fatalf("key usage after purge: %+v", ku)
	}
}

// The session cap (ADR-032) reads the session's own running total, applies only
// to a request that carries a session, and answers without a retry hint.
func TestSessionTokenCap(t *testing.T) {
	l, p, _ := setup(t, store.Project{SessionTokenCap: 100})
	ctx := context.Background()
	s := Subject{Project: p, SessionID: "run-1"}

	d, err := l.CheckSubject(ctx, s, 5)
	if err != nil || !d.Allowed || d.SessionLimit != 100 || d.SessionUsed != 0 {
		t.Fatalf("first check = %+v (%v)", d, err)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 60, CompletionTokens: 30, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	d, err = l.CheckSubject(ctx, s, 5)
	if err != nil || !d.Allowed || d.SessionUsed != 90 {
		t.Fatalf("under the cap = %+v (%v)", d, err)
	}
	// The request that crosses the cap is served: the cap compares what was
	// spent, with no estimate, so it can be overshot by one request.
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 40, CompletionTokens: 10, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	d, err = l.CheckSubject(ctx, s, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.Reason != ReasonSessionTokenCap || d.Scope != ScopeSession || d.SessionUsed != 140 || d.SessionLimit != 100 {
		t.Fatalf("over the cap = %+v", d)
	}
	if d.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0: a session does not reset", d.RetryAfter)
	}

	// Another session of the project, and a request without a session, are
	// not affected.
	if d, _ := l.CheckSubject(ctx, Subject{Project: p, SessionID: "run-2"}, 1); !d.Allowed {
		t.Errorf("another session denied: %+v", d)
	}
	if d, _ := l.CheckSubject(ctx, Subject{Project: p}, 1); !d.Allowed || d.SessionLimit != 0 {
		t.Errorf("a request without a session was held to the cap: %+v", d)
	}
}

// Without a cap the session is still counted, so a cap set later sees it.
func TestSessionCountedWithoutCap(t *testing.T) {
	l, p, _ := setup(t, store.Project{})
	ctx := context.Background()
	s := Subject{Project: p, SessionID: "run-1"}
	d, err := l.CheckSubject(ctx, s, 5)
	if err != nil || !d.Allowed || d.SessionLimit != 0 {
		t.Fatalf("check = %+v (%v)", d, err)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 7, CompletionTokens: 3, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	u, err := l.Store.GetSessionUsage(ctx, p.ID, "run-1")
	if err != nil || u.Tokens != 10 || u.Requests != 1 {
		t.Errorf("session usage = %+v (%v)", u, err)
	}
	// A request without a session writes no session row.
	if err := l.RecordSubject(ctx, Subject{Project: p}, Spend{PromptTokens: 1, CompletionTokens: 1, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	list, err := l.Store.ListSessions(ctx, p.ID, 0, 0)
	if err != nil || len(list) != 1 {
		t.Errorf("sessions = %d (%v), want 1", len(list), err)
	}
}

// A per-minute denial that clears by itself is reported after the cap that
// never clears, and a session denial leaves no phantom request slot.
func TestSessionCapBeforeRPMReservation(t *testing.T) {
	l, p, _ := setup(t, store.Project{SessionTokenCap: 10, RateLimitRPM: 5})
	ctx := context.Background()
	s := Subject{Project: p, SessionID: "run-1"}
	if err := l.Store.RecordSessionUsage(ctx, p.ID, "run-1", 10, 0); err != nil {
		t.Fatal(err)
	}
	d, err := l.CheckSubject(ctx, s, 1)
	if err != nil || d.Allowed || d.Reason != ReasonSessionTokenCap || d.Reserved {
		t.Fatalf("check = %+v (%v)", d, err)
	}
	u, err := l.Usage(ctx, p)
	if err != nil || u.Minute.Requests != 0 {
		t.Errorf("a denied session request took a minute slot: %+v (%v)", u.Minute, err)
	}
}

// ---- the end-user tier (ADR-035) ----

func endUserRows(t *testing.T, l *Limiter, projectID int64) int {
	t.Helper()
	var n int
	if err := l.Store.DB().QueryRow(context.Background(), `SELECT count(*) FROM end_user_usage WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEndUserCeilingDeniesOnlyThatEndUser(t *testing.T) {
	l, p, _ := setup(t, store.Project{EndUserDailyTokens: 20})
	ctx := context.Background()
	s := Subject{Project: p, EndUser: "alice"}

	d := mustCheckSubject(t, l, s, 1)
	if !d.Allowed || d.DailyLimit != 20 || d.DailyUsed != 0 {
		t.Fatalf("first check = %+v", d)
	}
	// The request that crosses the ceiling is served: it compares what was
	// spent, with no estimate.
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 15, CompletionTokens: 10, CostMicros: 99, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Reason != ReasonBudgetDaily || d.Scope != ScopeEndUser || d.DailyLimit != 20 || d.DailyUsed != 25 {
		t.Fatalf("over the ceiling = %+v", d)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v: a daily ceiling resets", d.RetryAfter)
	}

	// Another end user is not affected, and neither is a request that names no
	// end user at all: it is a guardrail, not a security check.
	if d := mustCheckSubject(t, l, Subject{Project: p, EndUser: "bob"}, 1); !d.Allowed {
		t.Errorf("another end user denied: %+v", d)
	}
	if d := mustCheckSubject(t, l, Subject{Project: p}, 1); !d.Allowed || d.DailyLimit != 0 {
		t.Errorf("a request without an end user was held to the ceiling: %+v", d)
	}
}

func TestEndUserMonthlyAndTokensPerMinute(t *testing.T) {
	l, p, _ := setup(t, store.Project{EndUserMonthlyTokens: 30, EndUserTPM: 50})
	ctx := context.Background()
	s := Subject{Project: p, EndUser: "alice"}

	// The per-minute figure counts the estimate; the monthly one does not.
	if d := mustCheckSubject(t, l, s, 60); d.Allowed || d.Reason != ReasonTPM || d.Scope != ScopeEndUser {
		t.Fatalf("tpm = %+v", d)
	}
	d := mustCheckSubject(t, l, s, 10)
	if !d.Allowed {
		t.Fatalf("check = %+v", d)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 20, CompletionTokens: 10, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Reason != ReasonBudgetMonthly || d.Scope != ScopeEndUser || d.MonthlyUsed != 30 {
		t.Fatalf("monthly = %+v", d)
	}

	// CheckTokens re-checks the minute tokens against a larger estimate.
	l, p, _ = setup(t, store.Project{EndUserTPM: 50})
	s = Subject{Project: p, EndUser: "alice"}
	d = mustCheckSubject(t, l, s, 10)
	if !d.Allowed {
		t.Fatalf("check = %+v", d)
	}
	grown, err := l.CheckTokens(ctx, s, d, 80)
	if err != nil || grown.Allowed || grown.Reason != ReasonTPM || grown.Scope != ScopeEndUser {
		t.Fatalf("CheckTokens = %+v (%v)", grown, err)
	}
	if ok, err := l.CheckTokens(ctx, s, d, 40); err != nil || !ok.Allowed {
		t.Fatalf("CheckTokens within the limit = %+v (%v)", ok, err)
	}
	if ok, err := l.CheckTokens(ctx, Subject{Project: p}, d, 4000); err != nil || !ok.Allowed {
		t.Fatalf("CheckTokens without an end user = %+v (%v)", ok, err)
	}
}

func TestEndUserTierComesAfterProjectAndKey(t *testing.T) {
	ctx := context.Background()
	// Everything over at once: the project is reported.
	l, p, _ := setup(t, store.Project{BudgetDailyTokens: 5, EndUserDailyTokens: 5})
	s := keySubject(t, l, p, Caps{DailyTokens: 5})
	s.EndUser = "alice"
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 6, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	if d := mustCheckSubject(t, l, s, 1); d.Allowed || d.Scope != ScopeProject {
		t.Fatalf("all three over: %+v, want the project first", d)
	}
	// Key and end user over, project not: the key.
	l, p, _ = setup(t, store.Project{BudgetDailyTokens: 500, EndUserDailyTokens: 5})
	s = keySubject(t, l, p, Caps{DailyTokens: 5})
	s.EndUser = "alice"
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 6, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	if d := mustCheckSubject(t, l, s, 1); d.Allowed || d.Scope != ScopeKey {
		t.Fatalf("key and end user over: %+v, want the key before the end user", d)
	}
	// Only the end user over.
	l, p, _ = setup(t, store.Project{BudgetDailyTokens: 500, EndUserDailyTokens: 5})
	s = keySubject(t, l, p, Caps{DailyTokens: 500})
	s.EndUser = "alice"
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 6, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	if d := mustCheckSubject(t, l, s, 1); d.Allowed || d.Scope != ScopeEndUser {
		t.Fatalf("only the end user over: %+v", d)
	}
}

func TestEndUserTierPicksTheTighterBoundForTheHeaders(t *testing.T) {
	// The end user's RPM is the smallest of the three: the figures describe it.
	l, p, _ := setup(t, store.Project{RateLimitRPM: 10, EndUserRPM: 2, EndUserMonthlyTokens: 100, BudgetMonthlyTokens: 1000})
	s := keySubject(t, l, p, Caps{RPM: 5, MonthlyTokens: 500})
	s.EndUser = "alice"
	d := mustCheckSubject(t, l, s, 1)
	if !d.Allowed || d.LimitRequests != 2 || d.RemainingRequests != 1 || d.MonthlyLimit != 100 || !d.Reserved {
		t.Fatalf("headers = %+v", d)
	}
	// The three-tier figure when the end user has no ceiling of its own.
	l, p, _ = setup(t, store.Project{RateLimitRPM: 10})
	s = keySubject(t, l, p, Caps{RPM: 5})
	s.EndUser = "alice"
	if d := mustCheckSubject(t, l, s, 1); d.LimitRequests != 5 || d.RemainingRequests != 4 {
		t.Fatalf("headers = %+v", d)
	}
}

// The minute slots are taken project, key, end user; a denial at any of them
// gives back the ones already taken.
func TestRPMReservationRollsBackAllThreeTiers(t *testing.T) {
	l, p, _ := setup(t, store.Project{RateLimitRPM: 10, EndUserRPM: 1})
	ctx := context.Background()
	s := keySubject(t, l, p, Caps{RPM: 10})
	s.EndUser = "alice"

	d := mustCheckSubject(t, l, s, 1)
	if !d.Allowed || !d.Reserved {
		t.Fatalf("first: %+v", d)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 1, CompletionTokens: 1, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if d.Allowed || d.Scope != ScopeEndUser || d.Reason != ReasonRPM || d.Reserved || d.RemainingRequests != 0 {
		t.Fatalf("second: %+v", d)
	}
	pu, err := l.Usage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	ku, err := l.KeyUsage(ctx, mustKey(t, l, s))
	if err != nil {
		t.Fatal(err)
	}
	if pu.Minute.Requests != 1 || ku.Minute.Requests != 1 {
		t.Errorf("a reservation leaked: project %d key %d, want 1 and 1", pu.Minute.Requests, ku.Minute.Requests)
	}
	minute, _, _, err := l.Store.GetEndUserUsage(ctx, p.ID, "alice", Windows(l.now()))
	if err != nil || minute.Requests != 1 {
		t.Errorf("end user minute requests = %d (%v), want 1", minute.Requests, err)
	}
	// A different end user has its own slot.
	s2 := s
	s2.EndUser = "bob"
	if d := mustCheckSubject(t, l, s2, 1); !d.Allowed {
		t.Errorf("another end user: %+v", d)
	}
}

// The counters are written only for an end user under a project that sets a
// ceiling: nothing reads them otherwise.
func TestEndUserCountersAreWrittenOnlyWhenTheyCanBeRead(t *testing.T) {
	ctx := context.Background()
	l, p, _ := setup(t, store.Project{})
	s := Subject{Project: p, EndUser: "alice"}
	d := mustCheckSubject(t, l, s, 1)
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 5, CompletionTokens: 5, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	if n := endUserRows(t, l, p.ID); n != 0 {
		t.Errorf("a project without a ceiling wrote %d end_user_usage rows", n)
	}

	l, p, _ = setup(t, store.Project{EndUserRPM: 100})
	if err := l.RecordSubject(ctx, Subject{Project: p}, Spend{PromptTokens: 5, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	if n := endUserRows(t, l, p.ID); n != 0 {
		t.Errorf("a request without an end user wrote %d end_user_usage rows", n)
	}
	s = Subject{Project: p, EndUser: "alice"}
	d = mustCheckSubject(t, l, s, 1)
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 5, CompletionTokens: 2, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	minute, day, month, err := l.Store.GetEndUserUsage(ctx, p.ID, "alice", Windows(l.now()))
	if err != nil {
		t.Fatal(err)
	}
	// The slot Check reserved is not counted a second time.
	if minute.Requests != 1 || day.Requests != 1 || month.Requests != 1 || day.Tokens() != 7 {
		t.Errorf("counters = minute %+v day %+v month %+v", minute, day, month)
	}
}

func TestPurgeUsageCoversTheEndUserTier(t *testing.T) {
	l, p, c := setup(t, store.Project{EndUserDailyTokens: 100})
	ctx := context.Background()
	start := c.t
	s := Subject{Project: p, EndUser: "alice"}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 5, CompletionTokens: 5, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	// The project's own counters are written alongside and keep their retention.
	projectRows := func() int {
		var n int
		if err := l.Store.DB().QueryRow(ctx, `SELECT count(*) FROM project_usage WHERE project_id = $1`, p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := endUserRows(t, l, p.ID); n != 3 {
		t.Fatalf("rows before = %d, want minute, day and month", n)
	}
	purgeAt := func(d time.Duration) {
		t.Helper()
		c.t = start.Add(d)
		if err := l.PurgeUsage(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// 2026-09-18 10:30 recorded. Three hours on only the minute row is gone.
	purgeAt(3 * time.Hour)
	if n := endUserRows(t, l, p.ID); n != 2 {
		t.Errorf("rows after the minute window expired = %d, want day and month", n)
	}
	// A day and a half later the day row is still inside its two days.
	purgeAt(36 * time.Hour)
	if n := endUserRows(t, l, p.ID); n != 2 {
		t.Errorf("rows after 36 hours = %d, want day and month", n)
	}
	// Past two days the day row goes, the month row stays.
	purgeAt(3 * 24 * time.Hour)
	if n := endUserRows(t, l, p.ID); n != 1 {
		t.Errorf("rows after 3 days = %d, want only the month row", n)
	}
	// The September month row starts 2026-09-01. Month rows are kept for the
	// current and the previous month: still there in October, gone with the
	// first instant of November.
	c.t = time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if n := endUserRows(t, l, p.ID); n != 1 {
		t.Errorf("month row in the month after = %d rows, want 1 (the previous month is kept)", n)
	}
	c.t = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if n := endUserRows(t, l, p.ID); n != 0 {
		t.Errorf("rows after the month row expired = %d, want 0", n)
	}
	// The project's counters were untouched by all of it: their day and month
	// rows outlive the end-user ones by design.
	if n := projectRows(); n != 2 {
		t.Errorf("project_usage rows = %d, want the day and month rows (minute expired after 2 hours)", n)
	}
}

// A purge at the first instant of a day or a month must not take the window the
// limiter is about to read, whatever the clock says to the second.
func TestPurgeUsageKeepsTheCurrentEndUserWindowAtBoundaries(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),    // first instant of a month
		time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC),    // one second in
		time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC), // last second of a month
		time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),    // AddDate month overflow: 31 March minus two months
		time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC),  // year end
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),     // year start
		time.Date(2024, 2, 29, 23, 0, 0, 0, time.UTC),   // leap day
	} {
		l, p, c := setup(t, store.Project{EndUserDailyTokens: 100, EndUserMonthlyTokens: 1000})
		ctx := context.Background()
		c.t = at
		s := Subject{Project: p, EndUser: "alice"}
		if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 30, CompletionTokens: 10, CountRequest: true}); err != nil {
			t.Fatal(err)
		}
		if err := l.PurgeUsage(ctx); err != nil {
			t.Fatal(err)
		}
		_, day, month, err := l.Store.GetEndUserUsage(ctx, p.ID, "alice", Windows(at))
		if err != nil || day.Tokens() != 40 || month.Tokens() != 40 {
			t.Errorf("%s: after a purge day %d month %d tokens (%v), want 40 and 40", at.Format(time.RFC3339), day.Tokens(), month.Tokens(), err)
		}
		// The ceiling still sees the spend: 100 daily, 40 used, then 61 more.
		if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 61, CountRequest: true}); err != nil {
			t.Fatal(err)
		}
		if d := mustCheckSubject(t, l, s, 1); d.Allowed || d.Scope != ScopeEndUser || d.Reason != ReasonBudgetDaily {
			t.Errorf("%s: the daily ceiling forgot the spend after a purge: %+v", at.Format(time.RFC3339), d)
		}
	}
}

// A date that the earlier month does not have must not move the month cutoff
// into a later month: on 31 January, 30 April or 29 April the previous month's
// row is still kept, whatever the hour.
func TestEndUserMonthCutoffIsTheStartOfThePreviousMonth(t *testing.T) {
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2024, 4, 30, 23, 59, 59, 0, time.UTC), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2025, 4, 29, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if got := endUserMonthCutoff(tc.now); !got.Equal(tc.want) {
			t.Errorf("cutoff(%s) = %s, want %s", tc.now.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
		}
	}
}

func TestPurgeKeepsThePreviousMonthRowOnOverflowDates(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC),
		time.Date(2024, 4, 30, 23, 0, 0, 0, time.UTC),
		time.Date(2025, 4, 29, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC),
	} {
		l, p, c := setup(t, store.Project{EndUserMonthlyTokens: 1000})
		ctx := context.Background()
		s := Subject{Project: p, EndUser: "alice"}
		prev := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		c.t = prev.Add(5 * time.Hour)
		if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 7, CountRequest: true}); err != nil {
			t.Fatal(err)
		}
		c.t = at
		if err := l.PurgeUsage(ctx); err != nil {
			t.Fatal(err)
		}
		_, _, month, err := l.Store.GetEndUserUsage(ctx, p.ID, "alice", Windows(prev))
		if err != nil || month.Tokens() != 7 {
			t.Errorf("%s: previous month row = %d tokens (%v), want it kept", at.Format(time.RFC3339), month.Tokens(), err)
		}
	}
}

// Day and month rows of the previous window are gone at the right time while
// the new window starts from zero, at a day boundary and at a month boundary.
func TestEndUserWindowsRollOverAtBoundaries(t *testing.T) {
	l, p, c := setup(t, store.Project{EndUserDailyTokens: 50, EndUserMonthlyTokens: 120})
	ctx := context.Background()
	s := Subject{Project: p, EndUser: "alice"}
	c.t = time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 60, CountRequest: true}); err != nil {
		t.Fatal(err)
	}
	if d := mustCheckSubject(t, l, s, 1); d.Allowed || d.Reason != ReasonBudgetDaily {
		t.Fatalf("same day: %+v", d)
	}
	// A minute later it is October 1st: a new day and a new month, both empty.
	c.t = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	d := mustCheckSubject(t, l, s, 1)
	if !d.Allowed || d.DailyUsed != 0 || d.MonthlyUsed != 0 {
		t.Fatalf("new day and month: %+v, want a fresh allowance", d)
	}
	if err := l.RecordSubject(ctx, s, Spend{PromptTokens: 40, CountRequest: !d.Reserved}); err != nil {
		t.Fatal(err)
	}
	// Later the same month, a new day: the day is empty, the month remembers.
	c.t = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if err := l.PurgeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	d = mustCheckSubject(t, l, s, 1)
	if !d.Allowed || d.DailyUsed != 0 || d.MonthlyUsed != 40 {
		t.Fatalf("next day: %+v, want an empty day and 40 for the month", d)
	}
}

func TestTighterOverThreeTiers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tiers        []tier
		limit, used  int64
		requestsRem  int
		requestLimit int
	}{
		{"none set", []tier{{0, 3}, {0, 0}, {0, 0}}, 0, 3, 0, 0},
		{"only the last", []tier{{0, 0}, {0, 0}, {10, 4}}, 10, 4, 6, 10},
		{"least headroom wins", []tier{{100, 10}, {50, 10}, {20, 5}}, 20, 5, 15, 20},
		{"a tie goes to the earlier tier", []tier{{10, 5}, {20, 15}, {30, 25}}, 10, 5, 5, 10},
		{"unlimited never wins", []tier{{10, 9}, {0, 0}, {0, 0}}, 10, 9, 1, 10},
		{"over its limit has no headroom", []tier{{10, 3}, {5, 9}, {8, 1}}, 5, 9, 0, 5},
	} {
		limit, used := tighter(tc.tiers...)
		if limit != tc.limit || used != tc.used {
			t.Errorf("%s: tighter = (%d, %d), want (%d, %d)", tc.name, limit, used, tc.limit, tc.used)
		}
		lim, rem := tighterRequests(tc.tiers...)
		if rem != tc.requestsRem || lim != tc.requestLimit {
			t.Errorf("%s: tighterRequests = (%d, %d), want (%d, %d)", tc.name, lim, rem, tc.requestLimit, tc.requestsRem)
		}
	}
}

// The minute slot is taken with one atomic upsert per tier. Sixty requests
// that arrive together must let exactly RPM through on the end-user tier and on
// the project tier, and the slots the denied ones took must be given back.
func TestEndUserRPMHoldsUnderConcurrentRequests(t *testing.T) {
	const rpm, requests = 5, 60
	l, p, _ := setup(t, store.Project{RateLimitRPM: 1000, EndUserRPM: rpm})
	ctx := context.Background()
	s := Subject{Project: p, EndUser: "alice"}

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, denied := 0, 0
	errs := make(chan error, requests)
	start := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := l.CheckSubject(ctx, s, 1)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if d.Allowed {
				allowed++
			} else if d.Scope == ScopeEndUser && d.Reason == ReasonRPM {
				denied++
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if allowed != rpm || denied != requests-rpm {
		t.Fatalf("allowed %d denied %d, want %d and %d", allowed, denied, rpm, requests-rpm)
	}
	minute, _, _, err := l.Store.GetEndUserUsage(ctx, p.ID, "alice", Windows(l.now()))
	if err != nil || minute.Requests != rpm {
		t.Errorf("end user minute counter = %d (%v), want %d", minute.Requests, err, rpm)
	}
	pu, err := l.Usage(ctx, p)
	if err != nil || pu.Minute.Requests != rpm {
		t.Errorf("project minute counter = %d (%v), want %d: a denied request kept its project slot", pu.Minute.Requests, err, rpm)
	}
}

// RecordSubject takes the request's cost along: the day and month counters of
// the project add it up and count the requests that had no price. The price
// is only an estimate and never a limit -- nothing here refuses a request for
// what it cost (ADR-036), which the last check pins.
func TestRecordSubjectCountsCostAndUnpriced(t *testing.T) {
	l, p, c := setup(t, store.Project{})
	ctx := context.Background()
	s := Subject{Project: p}
	for _, sp := range []Spend{
		{PromptTokens: 100, CompletionTokens: 50, CostMicros: 1500, CountRequest: true},
		{PromptTokens: 10, CompletionTokens: 5, CostMicros: 250, CountRequest: true},
		// A model without a price: it costs 0 and is counted apart.
		{PromptTokens: 20, CompletionTokens: 10, CostMicros: 0, Unpriced: true, CountRequest: true},
		{PromptTokens: 1, CompletionTokens: 1, CostMicros: -7, CountRequest: true}, // never subtracts
	} {
		if err := l.RecordSubject(ctx, s, sp); err != nil {
			t.Fatal(err)
		}
	}
	w := Windows(c.t)
	for _, row := range []struct {
		period string
		start  time.Time
		cost   int64
		unpr   int
	}{
		{"day", w.Day, 1750, 1},
		{"month", w.Month, 1750, 1},
		{"minute", w.Minute, 0, 0}, // the rate limiter's rows carry no money
	} {
		var cost int64
		var unpriced int
		if err := l.Store.DB().QueryRow(ctx, `SELECT cost_micros, unpriced_requests FROM project_usage
			WHERE project_id = $1 AND period = $2 AND period_start = $3`, p.ID, row.period, row.start).Scan(&cost, &unpriced); err != nil {
			t.Fatalf("%s row: %v", row.period, err)
		}
		if cost != row.cost || unpriced != row.unpr {
			t.Errorf("%s row: cost_micros=%d unpriced_requests=%d, want %d and %d", row.period, cost, unpriced, row.cost, row.unpr)
		}
	}
	d := mustCheck(t, l, p, 10)
	if !d.Allowed {
		t.Errorf("spend must not deny a request: %+v", d)
	}
}
