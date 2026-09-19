// Package limits enforces requests-per-minute and tokens-per-minute rate
// limits and daily / monthly token budgets on two tiers: the project a
// request is routed to, and the user-owned api key that made it. Counters
// live in the project_usage and key_usage tables so every replica shares
// them; all windows are aligned to UTC minute, day and month boundaries.
//
// The two tiers cannot be collapsed into a single, tighter limit: they count
// against different rows, so each one is evaluated against its own counters
// and the first violation decides. Decision.Scope names the tier that denied.
package limits

import (
	"context"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// Deny reasons reported in Decision.Reason and the 429 body's "code".
const (
	ReasonRPM           = "rate_limit_rpm"
	ReasonTPM           = "rate_limit_tpm"
	ReasonBudgetDaily   = "budget_daily"
	ReasonBudgetMonth   = "budget_monthly"
	ReasonBudgetMonthly = ReasonBudgetMonth
)

// Limit tiers reported in Decision.Scope.
const (
	ScopeProject = "project"
	ScopeKey     = "key"
)

// Retention for project_usage and key_usage rows, applied by PurgeUsage.
const (
	KeepMinuteRows = 2 * time.Hour
	KeepDayRows    = 400 * 24 * time.Hour
	KeepMonthRows  = 3 * 366 * 24 * time.Hour
)

// Limiter checks and records project usage. A nil Now uses time.Now.
type Limiter struct {
	Store *store.Store
	Now   func() time.Time
}

// Decision is the outcome of Check plus the figures the gateway exposes as
// x-ratelimit-* headers.
type Decision struct {
	Allowed bool
	// Reason is one of the Reason* constants when Allowed is false.
	Reason string
	// Scope is the tier that denied (ScopeProject or ScopeKey), empty while
	// Allowed. The figures below always describe the tighter of the two, so
	// a client sees the bound it will actually hit first.
	Scope string
	// RetryAfter is the time until the violated window resets.
	RetryAfter time.Duration
	// Request figures for the current minute; LimitRequests is 0 when no
	// RPM limit is configured.
	LimitRequests     int
	RemainingRequests int
	ResetRequests     time.Duration
	// Token budgets; limits are 0 when unset.
	DailyUsed, DailyLimit     int64
	MonthlyUsed, MonthlyLimit int64
	// Reserved reports whether the project's minute request slot was taken
	// by Check, in which case Record must not count the request again. The
	// key tier is reserved independently, under its own RPM limit.
	Reserved bool
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

// Windows returns the UTC minute, day and month starts containing t.
func Windows(t time.Time) store.UsageWindows {
	t = t.UTC()
	return store.UsageWindows{
		Minute: t.Truncate(time.Minute),
		Day:    time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC),
		Month:  time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC),
	}
}

func nextDay(day time.Time) time.Time     { return day.AddDate(0, 0, 1) }
func nextMonth(month time.Time) time.Time { return month.AddDate(0, 1, 0) }

// Caps is one tier's four limits; zero means unlimited.
type Caps struct {
	RPM, TPM                   int
	DailyTokens, MonthlyTokens int64
}

// caps reads a project's limits as a Caps.
func projectCaps(p *store.Project) Caps {
	return Caps{RPM: p.RateLimitRPM, TPM: p.RateLimitTPM,
		DailyTokens: p.BudgetDailyTokens, MonthlyTokens: p.BudgetMonthlyTokens}
}

// Subject is what a request is charged to: always a project, plus the api key
// that made the call when it was a user-owned one. A nil KeyID means only the
// project tier applies, which is exactly the pre-0.4 behaviour.
type Subject struct {
	Project *store.Project
	KeyID   *int64
	Key     Caps
}

// Check decides whether a request may proceed against the project alone.
func (l *Limiter) Check(ctx context.Context, p *store.Project, estimatedPromptTokens int) (Decision, error) {
	return l.CheckSubject(ctx, Subject{Project: p}, estimatedPromptTokens)
}

// CheckSubject decides whether a request may proceed. Budgets and the token
// limit are evaluated against the usage recorded so far (plus the estimated
// prompt size for TPM); the RPM slot is reserved atomically so concurrent
// requests cannot exceed the limit. A rejected reservation is released again.
//
// Each tier is checked against its own counters in the order monthly →
// daily → TPM, project before key, and the first violation wins.
func (l *Limiter) CheckSubject(ctx context.Context, s Subject, estimatedPromptTokens int) (Decision, error) {
	now := l.now()
	w := Windows(now)
	pc := projectCaps(s.Project)
	var d Decision
	d.Allowed = true
	d.ResetRequests = w.Minute.Add(time.Minute).Sub(now)

	pMinute, pDay, pMonth, err := l.Store.GetUsage(ctx, s.Project.ID, w)
	if err != nil {
		return d, err
	}
	kMinute, kDay, kMonth := zeroRows(w)
	if s.KeyID != nil {
		if kMinute, kDay, kMonth, err = l.Store.GetKeyUsage(ctx, *s.KeyID, w); err != nil {
			return d, err
		}
	}
	kc := s.Key
	if s.KeyID == nil {
		kc = Caps{}
	}
	d.LimitRequests, d.RemainingRequests = tighterRequests(pc.RPM, pMinute.Requests, kc.RPM, kMinute.Requests)
	d.DailyLimit, d.DailyUsed = tighter(pc.DailyTokens, pDay.Tokens(), kc.DailyTokens, kDay.Tokens())
	d.MonthlyLimit, d.MonthlyUsed = tighter(pc.MonthlyTokens, pMonth.Tokens(), kc.MonthlyTokens, kMonth.Tokens())

	est := int64(estimatedPromptTokens)
	for _, c := range []struct {
		over    bool
		reason  string
		scope   string
		retryAt time.Duration
	}{
		{pc.MonthlyTokens > 0 && pMonth.Tokens() >= pc.MonthlyTokens, ReasonBudgetMonthly, ScopeProject, nextMonth(w.Month).Sub(now)},
		{kc.MonthlyTokens > 0 && kMonth.Tokens() >= kc.MonthlyTokens, ReasonBudgetMonthly, ScopeKey, nextMonth(w.Month).Sub(now)},
		{pc.DailyTokens > 0 && pDay.Tokens() >= pc.DailyTokens, ReasonBudgetDaily, ScopeProject, nextDay(w.Day).Sub(now)},
		{kc.DailyTokens > 0 && kDay.Tokens() >= kc.DailyTokens, ReasonBudgetDaily, ScopeKey, nextDay(w.Day).Sub(now)},
		{pc.TPM > 0 && pMinute.Tokens()+est > int64(pc.TPM), ReasonTPM, ScopeProject, d.ResetRequests},
		{kc.TPM > 0 && kMinute.Tokens()+est > int64(kc.TPM), ReasonTPM, ScopeKey, d.ResetRequests},
	} {
		if c.over {
			return l.deny(d, c.reason, c.scope, c.retryAt), nil
		}
	}

	// The two minute slots are taken in order; if the second one overflows
	// the first must be given back, or the project keeps a phantom request
	// for the rest of the minute.
	pReq, kReq := pMinute.Requests, kMinute.Requests
	if pc.RPM > 0 {
		n, err := l.Store.ReserveMinuteRequest(ctx, s.Project.ID, w.Minute)
		if err != nil {
			return d, err
		}
		if n > pc.RPM {
			if err := l.rollback(ctx, s, w, true, false); err != nil {
				return d, err
			}
			d.LimitRequests, d.RemainingRequests = pc.RPM, 0
			return l.deny(d, ReasonRPM, ScopeProject, d.ResetRequests), nil
		}
		d.Reserved, pReq = true, n
	}
	if kc.RPM > 0 {
		n, err := l.Store.ReserveKeyMinuteRequest(ctx, *s.KeyID, w.Minute)
		if err != nil {
			// The project slot is already taken and the caller aborts on the
			// error without ever reaching Record, so give it back here too.
			// Otherwise a failing key counter leaves the project a phantom
			// request for the rest of the minute, and a few of those close a
			// low-RPM project entirely.
			if d.Reserved {
				// A failed release is ignored: the reservation error below is
				// what the caller acts on, and the minute row it leaves
				// behind expires with the window anyway.
				_ = l.rollback(ctx, s, w, true, false)
				d.Reserved = false
			}
			return d, err
		}
		if n > kc.RPM {
			if err := l.rollback(ctx, s, w, d.Reserved, true); err != nil {
				return d, err
			}
			d.Reserved = false
			d.LimitRequests, d.RemainingRequests = kc.RPM, 0
			return l.deny(d, ReasonRPM, ScopeKey, d.ResetRequests), nil
		}
		kReq = n
	}
	d.LimitRequests, d.RemainingRequests = tighterRequests(pc.RPM, pReq, kc.RPM, kReq)
	return d, nil
}

// rollback releases the minute slots a denied request had already taken.
func (l *Limiter) rollback(ctx context.Context, s Subject, w store.UsageWindows, project, key bool) error {
	if project {
		if err := l.Store.ReleaseMinuteRequest(ctx, s.Project.ID, w.Minute); err != nil {
			return err
		}
	}
	if key && s.KeyID != nil {
		if err := l.Store.ReleaseKeyMinuteRequest(ctx, *s.KeyID, w.Minute); err != nil {
			return err
		}
	}
	return nil
}

func zeroRows(w store.UsageWindows) (minute, day, month store.UsageRow) {
	return store.UsageRow{Period: store.PeriodMinute, PeriodStart: w.Minute},
		store.UsageRow{Period: store.PeriodDay, PeriodStart: w.Day},
		store.UsageRow{Period: store.PeriodMonth, PeriodStart: w.Month}
}

// tighter picks the tier with less headroom, so the headers report the bound
// the caller hits first. A zero limit is unlimited and never wins.
func tighter(limit, used, keyLimit, keyUsed int64) (int64, int64) {
	switch {
	case keyLimit == 0:
		return limit, used
	case limit == 0 || keyLimit-keyUsed < limit-used:
		return keyLimit, keyUsed
	}
	return limit, used
}

func tighterRequests(limit, used, keyLimit, keyUsed int) (int, int) {
	l, u := tighter(int64(limit), int64(used), int64(keyLimit), int64(keyUsed))
	if l == 0 {
		return 0, 0
	}
	return int(l), int(max(l-u, 0))
}

func (l *Limiter) deny(d Decision, reason, scope string, retryAfter time.Duration) Decision {
	d.Allowed = false
	d.Reason = reason
	d.Scope = scope
	d.RetryAfter = retryAfter
	if d.RetryAfter < time.Second {
		d.RetryAfter = time.Second
	}
	return d
}

// Record adds a finished request's tokens to the project's current windows.
// Pass countRequest=false when Check already reserved the minute slot.
func (l *Limiter) Record(ctx context.Context, projectID int64, promptTokens, completionTokens int, countRequest bool) error {
	return l.Store.RecordUsage(ctx, projectID, Windows(l.now()), promptTokens, completionTokens, countRequest)
}

// RecordSubject adds a finished request's tokens to both tiers. countRequest
// says whether the project's minute request still has to be counted (Check
// reserves it when an RPM limit is set); the key tier is derived the same way
// from its own cap, because the two slots are reserved independently.
func (l *Limiter) RecordSubject(ctx context.Context, s Subject, promptTokens, completionTokens int, countRequest bool) error {
	w := Windows(l.now())
	if err := l.Store.RecordUsage(ctx, s.Project.ID, w, promptTokens, completionTokens, countRequest); err != nil {
		return err
	}
	if s.KeyID == nil {
		return nil
	}
	return l.Store.RecordKeyUsage(ctx, *s.KeyID, w, promptTokens, completionTokens, s.Key.RPM == 0)
}

// PeriodUsage is one window of a UsageReport.
type PeriodUsage struct {
	PeriodStart      time.Time `json:"period_start"`
	ResetsAt         time.Time `json:"resets_at"`
	Requests         int       `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	Tokens           int64     `json:"tokens"`
	// RequestLimit only applies to the minute window (RPM).
	RequestLimit   int     `json:"request_limit"`
	RequestPercent float64 `json:"request_percent"`
	// TokenLimit is TPM for the minute and the budget for day / month.
	TokenLimit   int64   `json:"token_limit"`
	TokenPercent float64 `json:"token_percent"`
}

// UsageReport is the admin API view of one subject's current counters:
// exactly one of ProjectID and APIKeyID is set.
type UsageReport struct {
	ProjectID   int64       `json:"project_id,omitempty"`
	APIKeyID    int64       `json:"api_key_id,omitempty"`
	GeneratedAt time.Time   `json:"generated_at"`
	Minute      PeriodUsage `json:"minute"`
	Day         PeriodUsage `json:"day"`
	Month       PeriodUsage `json:"month"`
	Forecast    Forecast    `json:"forecast"`
}

// Forecast projects when the token budgets run out if the last hour's pace
// continues. A projection is nil when there is no budget, no recent usage,
// or the budget outlasts its window.
type Forecast struct {
	DailyExhaustedAt   *time.Time `json:"daily_exhausted_at"`
	MonthlyExhaustedAt *time.Time `json:"monthly_exhausted_at"`
}

// forecastLookback is the span the forecast rate is measured over. It must
// stay within KeepMinuteRows or purged rows would flatten the rate.
const forecastLookback = time.Hour

// Usage reports the current minute / day / month counters against the
// project's limits.
func (l *Limiter) Usage(ctx context.Context, p *store.Project) (*UsageReport, error) {
	now := l.now()
	w := Windows(now)
	minute, day, month, err := l.Store.GetUsage(ctx, p.ID, w)
	if err != nil {
		return nil, err
	}
	u := &UsageReport{
		ProjectID:   p.ID,
		GeneratedAt: now,
		Minute:      period(minute, w.Minute.Add(time.Minute), p.RateLimitRPM, int64(p.RateLimitTPM)),
		Day:         period(day, nextDay(w.Day), 0, p.BudgetDailyTokens),
		Month:       period(month, nextMonth(w.Month), 0, p.BudgetMonthlyTokens),
	}
	if p.BudgetDailyTokens > 0 || p.BudgetMonthlyTokens > 0 {
		recent, err := l.Store.MinuteTokensSince(ctx, p.ID, now.Add(-forecastLookback))
		if err != nil {
			return nil, err
		}
		u.Forecast.DailyExhaustedAt = exhaustedAt(now, recent, day.Tokens(), p.BudgetDailyTokens, u.Day.ResetsAt)
		u.Forecast.MonthlyExhaustedAt = exhaustedAt(now, recent, month.Tokens(), p.BudgetMonthlyTokens, u.Month.ResetsAt)
	}
	return u, nil
}

// KeyUsage reports an api key's own counters against its own limits. A key
// without limits still has counters, so the dashboard can show what it spent.
func (l *Limiter) KeyUsage(ctx context.Context, k *store.APIKey) (*UsageReport, error) {
	now := l.now()
	w := Windows(now)
	minute, day, month, err := l.Store.GetKeyUsage(ctx, k.ID, w)
	if err != nil {
		return nil, err
	}
	u := &UsageReport{
		APIKeyID:    k.ID,
		GeneratedAt: now,
		Minute:      period(minute, w.Minute.Add(time.Minute), k.RateLimitRPM, int64(k.RateLimitTPM)),
		Day:         period(day, nextDay(w.Day), 0, k.BudgetDailyTokens),
		Month:       period(month, nextMonth(w.Month), 0, k.BudgetMonthlyTokens),
	}
	if k.BudgetDailyTokens > 0 || k.BudgetMonthlyTokens > 0 {
		recent, err := l.Store.MinuteKeyTokensSince(ctx, k.ID, now.Add(-forecastLookback))
		if err != nil {
			return nil, err
		}
		u.Forecast.DailyExhaustedAt = exhaustedAt(now, recent, day.Tokens(), k.BudgetDailyTokens, u.Day.ResetsAt)
		u.Forecast.MonthlyExhaustedAt = exhaustedAt(now, recent, month.Tokens(), k.BudgetMonthlyTokens, u.Month.ResetsAt)
	}
	return u, nil
}

// exhaustedAt extends the lookback rate linearly until used reaches limit.
// An already exhausted budget projects to now.
func exhaustedAt(now time.Time, recent, used, limit int64, resetsAt time.Time) *time.Time {
	if limit <= 0 || recent <= 0 {
		return nil
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	at := now.Add(time.Duration(float64(forecastLookback) * float64(remaining) / float64(recent))).Truncate(time.Second)
	if !at.Before(resetsAt) {
		return nil
	}
	return &at
}

func period(r store.UsageRow, resetsAt time.Time, reqLimit int, tokLimit int64) PeriodUsage {
	u := PeriodUsage{PeriodStart: r.PeriodStart, ResetsAt: resetsAt, Requests: r.Requests,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens, Tokens: r.Tokens(),
		RequestLimit: reqLimit, TokenLimit: tokLimit}
	if reqLimit > 0 {
		u.RequestPercent = pct(float64(r.Requests), float64(reqLimit))
	}
	if tokLimit > 0 {
		u.TokenPercent = pct(float64(r.Tokens()), float64(tokLimit))
	}
	return u
}

func pct(used, limit float64) float64 {
	v := 100 * used / limit
	if v > 100 {
		v = 100
	}
	return float64(int(v*10+0.5)) / 10
}

// PurgeUsage deletes counter rows that no window can reference any more,
// from both tiers under the same retention.
func (l *Limiter) PurgeUsage(ctx context.Context) error {
	now := l.now()
	for _, c := range []struct {
		period string
		keep   time.Duration
	}{{store.PeriodMinute, KeepMinuteRows}, {store.PeriodDay, KeepDayRows}, {store.PeriodMonth, KeepMonthRows}} {
		cutoff := now.Add(-c.keep)
		if _, err := l.Store.DeleteUsageBefore(ctx, c.period, cutoff); err != nil {
			return err
		}
		if _, err := l.Store.DeleteKeyUsageBefore(ctx, c.period, cutoff); err != nil {
			return err
		}
	}
	return nil
}
