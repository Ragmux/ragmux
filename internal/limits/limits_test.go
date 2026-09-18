package limits

import (
	"context"
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
