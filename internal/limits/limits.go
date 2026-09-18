// Package limits enforces per-project requests-per-minute and
// tokens-per-minute rate limits and daily / monthly token budgets. Counters
// live in the project_usage table so every replica shares them; all windows
// are aligned to UTC minute, day and month boundaries.
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

// Retention for project_usage rows, applied by PurgeUsage.
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
	// Reserved reports whether a minute request slot was taken by Check,
	// in which case Record must not count the request again.
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

// Check decides whether a request may proceed. Budgets and the token limit
// are evaluated against the usage recorded so far (plus the estimated prompt
// size for TPM); the RPM slot is reserved atomically so concurrent requests
// cannot exceed the limit. A rejected reservation is released again.
func (l *Limiter) Check(ctx context.Context, p *store.Project, estimatedPromptTokens int) (Decision, error) {
	now := l.now()
	w := Windows(now)
	d := Decision{Allowed: true, LimitRequests: p.RateLimitRPM, DailyLimit: p.BudgetDailyTokens, MonthlyLimit: p.BudgetMonthlyTokens}
	d.ResetRequests = w.Minute.Add(time.Minute).Sub(now)

	minute, day, month, err := l.Store.GetUsage(ctx, p.ID, w)
	if err != nil {
		return d, err
	}
	d.DailyUsed, d.MonthlyUsed = day.Tokens(), month.Tokens()
	if p.RateLimitRPM > 0 {
		d.RemainingRequests = max(p.RateLimitRPM-minute.Requests, 0)
	}

	switch {
	case p.BudgetMonthlyTokens > 0 && month.Tokens() >= p.BudgetMonthlyTokens:
		return l.deny(d, ReasonBudgetMonthly, nextMonth(w.Month).Sub(now)), nil
	case p.BudgetDailyTokens > 0 && day.Tokens() >= p.BudgetDailyTokens:
		return l.deny(d, ReasonBudgetDaily, nextDay(w.Day).Sub(now)), nil
	case p.RateLimitTPM > 0 && minute.Tokens()+int64(estimatedPromptTokens) > int64(p.RateLimitTPM):
		return l.deny(d, ReasonTPM, d.ResetRequests), nil
	}

	if p.RateLimitRPM > 0 {
		n, err := l.Store.ReserveMinuteRequest(ctx, p.ID, w.Minute)
		if err != nil {
			return d, err
		}
		if n > p.RateLimitRPM {
			if err := l.Store.ReleaseMinuteRequest(ctx, p.ID, w.Minute); err != nil {
				return d, err
			}
			d.RemainingRequests = 0
			return l.deny(d, ReasonRPM, d.ResetRequests), nil
		}
		d.Reserved = true
		d.RemainingRequests = p.RateLimitRPM - n
	}
	return d, nil
}

func (l *Limiter) deny(d Decision, reason string, retryAfter time.Duration) Decision {
	d.Allowed = false
	d.Reason = reason
	d.RetryAfter = retryAfter
	if d.RetryAfter < time.Second {
		d.RetryAfter = time.Second
	}
	return d
}

// Record adds a finished request's tokens to the current windows. Pass
// countRequest=false when Check already reserved the minute slot.
func (l *Limiter) Record(ctx context.Context, projectID int64, promptTokens, completionTokens int, countRequest bool) error {
	return l.Store.RecordUsage(ctx, projectID, Windows(l.now()), promptTokens, completionTokens, countRequest)
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

// UsageReport is the admin API view of a project's current counters.
type UsageReport struct {
	ProjectID   int64       `json:"project_id"`
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

// PurgeUsage deletes counter rows that no window can reference any more.
func (l *Limiter) PurgeUsage(ctx context.Context) error {
	now := l.now()
	for _, c := range []struct {
		period string
		keep   time.Duration
	}{{store.PeriodMinute, KeepMinuteRows}, {store.PeriodDay, KeepDayRows}, {store.PeriodMonth, KeepMonthRows}} {
		if _, err := l.Store.DeleteUsageBefore(ctx, c.period, now.Add(-c.keep)); err != nil {
			return err
		}
	}
	return nil
}
