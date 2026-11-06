// Package limits enforces requests-per-minute and tokens-per-minute rate
// limits and daily / monthly token budgets on three tiers: the project a
// request is routed to, the user-owned api key that made it, and the end user
// it was made for (ADR-035). Counters live in the project_usage, key_usage and
// end_user_usage tables so every replica shares them; all windows are aligned
// to UTC minute, day and month boundaries.
//
// The tiers cannot be collapsed into a single, tighter limit: they count
// against different rows, so each one is evaluated against its own counters
// and the first violation decides, project before key before end user.
// Decision.Scope names the tier that denied.
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
	// ReasonSessionTokenCap is the project's per-session token cap (ADR-032).
	ReasonSessionTokenCap = "session_token_cap"
)

// Limit tiers reported in Decision.Scope.
const (
	ScopeProject = "project"
	ScopeKey     = "key"
	// ScopeSession is the per-session cap; it applies only to a request that
	// carried a session id.
	ScopeSession = "session"
	// ScopeEndUser is the project's per-end-user ceiling (ADR-035); it applies
	// only to a request that named an end user, and only while the project
	// sets one. A request without an end user is not held to it: it is a
	// guardrail, not a security check.
	ScopeEndUser = "end_user"
)

// Retention for project_usage and key_usage rows, applied by PurgeUsage.
const (
	KeepMinuteRows = 2 * time.Hour
	KeepDayRows    = 400 * 24 * time.Hour
	KeepMonthRows  = 3 * 366 * 24 * time.Hour
)

// Retention for end_user_usage rows (ADR-035). The limiter reads only the
// current window, and a row holds the end user's id as the client sent it, so
// the rows are kept no longer than the budgets need: minute rows the two hours
// the other tiers keep them, day rows two days, and month rows for the current
// and the previous month (EndUserKeepMonths). The project and key counters keep
// their longer retention above.
const (
	EndUserKeepMinuteRows = KeepMinuteRows
	EndUserKeepDayRows    = 2 * 24 * time.Hour
	EndUserKeepMonths     = 2
)

// endUserMonthCutoff is the instant before which an end-user month row is
// expired: the start of the month EndUserKeepMonths-1 months before the current
// one. It is computed from the first of the current month, so a day that the
// earlier month does not have (31 January, 30 April) cannot push the cutoff
// into a later month, and it never reaches the current month's row, which is
// the one the limiter reads.
func endUserMonthCutoff(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -(EndUserKeepMonths - 1), 0)
}

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
	// Scope is the tier that denied (ScopeProject, ScopeKey, ScopeSession or
	// ScopeEndUser), empty while Allowed. The figures below always describe the
	// tightest tier, so a client sees the bound it will actually hit first.
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
	// Session cap (ADR-032); the limit is 0 when the request has no session or
	// the project sets no cap.
	SessionUsed, SessionLimit int64
	// Reserved reports whether the project's minute request slot was taken
	// by Check, in which case Record must not count the request again. The
	// key and end-user tiers are reserved independently, under their own RPM
	// limits.
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

// endUserCaps reads a project's per-end-user ceilings as a Caps.
func endUserCaps(p *store.Project) Caps {
	return Caps{RPM: p.EndUserRPM, TPM: p.EndUserTPM,
		DailyTokens: p.EndUserDailyTokens, MonthlyTokens: p.EndUserMonthlyTokens}
}

// any reports whether at least one of the limits is set.
func (c Caps) any() bool {
	return c.RPM > 0 || c.TPM > 0 || c.DailyTokens > 0 || c.MonthlyTokens > 0
}

// Subject is what a request is charged to: always a project, plus the api key
// that made the call when it was a user-owned one. A nil KeyID means only the
// project tier applies, which is exactly the pre-0.4 behaviour.
//
// SessionID, when not empty, is the validated X-Ragmux-Session value. The
// session tier is the project's SessionTokenCap compared with that session's
// own running total; a request without a session is not subject to it.
//
// EndUser, when not empty, is the validated end user (ADR-035). The end-user
// tier is the project's per-end-user ceilings compared with that end user's
// own counters; a request without an end user, and a project without a
// ceiling, never read or write them.
type Subject struct {
	Project   *store.Project
	KeyID     *int64
	Key       Caps
	SessionID string
	EndUser   string
}

// endUserTier returns the caps the end-user tier holds the subject to: the
// project's ceilings when the request names an end user, no limit at all
// otherwise. A zero Caps means the tier does not apply.
func endUserTier(s Subject) Caps {
	if s.EndUser == "" {
		return Caps{}
	}
	return endUserCaps(s.Project)
}

// Spend is what a finished request spent, as RecordSubject takes it. CostMicros
// and Unpriced are the request's cost in USD millionths and whether its model
// had no price; the limiter does not store them yet. CountRequest says whether
// the project's minute request still has to be counted (Check reserves it when
// an RPM limit is set).
type Spend struct {
	PromptTokens, CompletionTokens, CostMicros int64
	Unpriced, CountRequest                     bool
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
// daily → TPM, project before key before end user, and the first violation
// wins.
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
	// The end-user counters are read only for a request that names an end user
	// under a project that sets a ceiling.
	eMinute, eDay, eMonth := zeroRows(w)
	ec := endUserTier(s)
	if ec.any() {
		if eMinute, eDay, eMonth, err = l.Store.GetEndUserUsage(ctx, s.Project.ID, s.EndUser, w); err != nil {
			return d, err
		}
	}
	d.LimitRequests, d.RemainingRequests = tighterRequests(
		tier{int64(pc.RPM), int64(pMinute.Requests)}, tier{int64(kc.RPM), int64(kMinute.Requests)}, tier{int64(ec.RPM), int64(eMinute.Requests)})
	d.DailyLimit, d.DailyUsed = tighter(
		tier{pc.DailyTokens, pDay.Tokens()}, tier{kc.DailyTokens, kDay.Tokens()}, tier{ec.DailyTokens, eDay.Tokens()})
	d.MonthlyLimit, d.MonthlyUsed = tighter(
		tier{pc.MonthlyTokens, pMonth.Tokens()}, tier{kc.MonthlyTokens, kMonth.Tokens()}, tier{ec.MonthlyTokens, eMonth.Tokens()})

	// The session cap reads one row, and only for a request that has a session
	// under a project that set a cap. Like the budgets it compares usage so far
	// with the cap, no estimate: a request may run a session past its cap by
	// one request.
	var sUsed int64
	if sessionCapped(s) {
		su, err := l.Store.GetSessionUsage(ctx, s.Project.ID, s.SessionID)
		if err != nil {
			return d, err
		}
		sUsed = su.Tokens
		d.SessionLimit, d.SessionUsed = s.Project.SessionTokenCap, sUsed
	}

	est := int64(estimatedPromptTokens)
	for _, c := range []struct {
		over    bool
		reason  string
		scope   string
		retryAt time.Duration
	}{
		{pc.MonthlyTokens > 0 && pMonth.Tokens() >= pc.MonthlyTokens, ReasonBudgetMonthly, ScopeProject, nextMonth(w.Month).Sub(now)},
		{kc.MonthlyTokens > 0 && kMonth.Tokens() >= kc.MonthlyTokens, ReasonBudgetMonthly, ScopeKey, nextMonth(w.Month).Sub(now)},
		{ec.MonthlyTokens > 0 && eMonth.Tokens() >= ec.MonthlyTokens, ReasonBudgetMonthly, ScopeEndUser, nextMonth(w.Month).Sub(now)},
		{pc.DailyTokens > 0 && pDay.Tokens() >= pc.DailyTokens, ReasonBudgetDaily, ScopeProject, nextDay(w.Day).Sub(now)},
		{kc.DailyTokens > 0 && kDay.Tokens() >= kc.DailyTokens, ReasonBudgetDaily, ScopeKey, nextDay(w.Day).Sub(now)},
		{ec.DailyTokens > 0 && eDay.Tokens() >= ec.DailyTokens, ReasonBudgetDaily, ScopeEndUser, nextDay(w.Day).Sub(now)},
		// Permanent for the session, so it is reported ahead of the per-minute
		// limit that would clear by itself.
		{d.SessionLimit > 0 && sUsed >= d.SessionLimit, ReasonSessionTokenCap, ScopeSession, 0},
		{pc.TPM > 0 && pMinute.Tokens()+est > int64(pc.TPM), ReasonTPM, ScopeProject, d.ResetRequests},
		{kc.TPM > 0 && kMinute.Tokens()+est > int64(kc.TPM), ReasonTPM, ScopeKey, d.ResetRequests},
		{ec.TPM > 0 && eMinute.Tokens()+est > int64(ec.TPM), ReasonTPM, ScopeEndUser, d.ResetRequests},
	} {
		if c.over {
			return l.deny(d, c.reason, c.scope, c.retryAt), nil
		}
	}

	// The minute slots are taken in order -- project, key, end user; if one
	// overflows, the ones already taken must be given back, or the tier keeps a
	// phantom request for the rest of the minute.
	var taken slots
	pReq, kReq, eReq := pMinute.Requests, kMinute.Requests, eMinute.Requests
	if pc.RPM > 0 {
		n, err := l.Store.ReserveMinuteRequest(ctx, s.Project.ID, w.Minute)
		if err != nil {
			return d, err
		}
		taken.project = true
		if n > pc.RPM {
			if err := l.rollback(ctx, s, w, taken); err != nil {
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
			l.giveBack(ctx, s, w, taken, &d)
			return d, err
		}
		taken.key = true
		if n > kc.RPM {
			if err := l.rollback(ctx, s, w, taken); err != nil {
				return d, err
			}
			d.Reserved = false
			d.LimitRequests, d.RemainingRequests = kc.RPM, 0
			return l.deny(d, ReasonRPM, ScopeKey, d.ResetRequests), nil
		}
		kReq = n
	}
	if ec.RPM > 0 {
		n, err := l.Store.ReserveEndUserMinuteRequest(ctx, s.Project.ID, s.EndUser, w.Minute)
		if err != nil {
			l.giveBack(ctx, s, w, taken, &d)
			return d, err
		}
		taken.endUser = true
		if n > ec.RPM {
			if err := l.rollback(ctx, s, w, taken); err != nil {
				return d, err
			}
			d.Reserved = false
			d.LimitRequests, d.RemainingRequests = ec.RPM, 0
			return l.deny(d, ReasonRPM, ScopeEndUser, d.ResetRequests), nil
		}
		eReq = n
	}
	d.LimitRequests, d.RemainingRequests = tighterRequests(
		tier{int64(pc.RPM), int64(pReq)}, tier{int64(kc.RPM), int64(kReq)}, tier{int64(ec.RPM), int64(eReq)})
	return d, nil
}

// slots says which minute request slots a check has taken so far.
type slots struct{ project, key, endUser bool }

// giveBack releases the slots already taken when the check aborts on a store
// error. A failed release is ignored: the error the caller acts on is the
// reservation's, and the minute row it leaves behind expires with the window
// anyway.
func (l *Limiter) giveBack(ctx context.Context, s Subject, w store.UsageWindows, taken slots, d *Decision) {
	if taken.project || taken.key || taken.endUser {
		_ = l.rollback(ctx, s, w, taken)
	}
	d.Reserved = false
}

// CheckTokens re-checks the per-minute token limit of every tier against a
// larger prompt estimate, for a request CheckSubject already admitted. It is
// for the part of a prompt that is only known after that check -- retrieved
// context -- so a request cannot slip under TPM by being small until
// retrieval makes it big.
//
// It reserves nothing and releases nothing: prior is the decision
// CheckSubject returned and keeps the request slot it took, and the figures in
// prior are carried over. Budgets are not looked at again, because they are
// compared with usage so far, not with the estimate. When no tier has a
// TPM limit it answers without reading usage.
func (l *Limiter) CheckTokens(ctx context.Context, s Subject, prior Decision, estimatedPromptTokens int) (Decision, error) {
	pc := projectCaps(s.Project)
	kc := s.Key
	if s.KeyID == nil {
		kc = Caps{}
	}
	ec := endUserTier(s)
	if pc.TPM == 0 && kc.TPM == 0 && ec.TPM == 0 {
		return prior, nil
	}
	now := l.now()
	w := Windows(now)
	pMinute, _, _, err := l.Store.GetUsage(ctx, s.Project.ID, w)
	if err != nil {
		return prior, err
	}
	est := int64(estimatedPromptTokens)
	retryAt := w.Minute.Add(time.Minute).Sub(now)
	if pc.TPM > 0 && pMinute.Tokens()+est > int64(pc.TPM) {
		return l.deny(prior, ReasonTPM, ScopeProject, retryAt), nil
	}
	if kc.TPM > 0 {
		kMinute, _, _, err := l.Store.GetKeyUsage(ctx, *s.KeyID, w)
		if err != nil {
			return prior, err
		}
		if kMinute.Tokens()+est > int64(kc.TPM) {
			return l.deny(prior, ReasonTPM, ScopeKey, retryAt), nil
		}
	}
	if ec.TPM > 0 {
		eMinute, _, _, err := l.Store.GetEndUserUsage(ctx, s.Project.ID, s.EndUser, w)
		if err != nil {
			return prior, err
		}
		if eMinute.Tokens()+est > int64(ec.TPM) {
			return l.deny(prior, ReasonTPM, ScopeEndUser, retryAt), nil
		}
	}
	return prior, nil
}

// rollback releases the minute slots a denied request had already taken.
func (l *Limiter) rollback(ctx context.Context, s Subject, w store.UsageWindows, taken slots) error {
	if taken.project {
		if err := l.Store.ReleaseMinuteRequest(ctx, s.Project.ID, w.Minute); err != nil {
			return err
		}
	}
	if taken.key && s.KeyID != nil {
		if err := l.Store.ReleaseKeyMinuteRequest(ctx, *s.KeyID, w.Minute); err != nil {
			return err
		}
	}
	if taken.endUser {
		if err := l.Store.ReleaseEndUserMinuteRequest(ctx, s.Project.ID, s.EndUser, w.Minute); err != nil {
			return err
		}
	}
	return nil
}

// sessionCapped reports whether the session cap applies to the subject.
func sessionCapped(s Subject) bool {
	return s.SessionID != "" && s.Project.SessionTokenCap > 0
}

func zeroRows(w store.UsageWindows) (minute, day, month store.UsageRow) {
	return store.UsageRow{Period: store.PeriodMinute, PeriodStart: w.Minute},
		store.UsageRow{Period: store.PeriodDay, PeriodStart: w.Day},
		store.UsageRow{Period: store.PeriodMonth, PeriodStart: w.Month}
}

// tier is one tier's limit and what it has used so far.
type tier struct{ limit, used int64 }

// tighter picks the tier with the least headroom, so the headers report the
// bound the caller hits first. A zero limit is unlimited and never wins over a
// set one; on a tie the earlier tier (project, then key, then end user) wins.
// With no limit set anywhere it reports the first tier as it is.
func tighter(tiers ...tier) (limit, used int64) {
	limit, used = tiers[0].limit, tiers[0].used
	for _, t := range tiers[1:] {
		if t.limit == 0 {
			continue
		}
		if limit == 0 || t.limit-t.used < limit-used {
			limit, used = t.limit, t.used
		}
	}
	return limit, used
}

func tighterRequests(tiers ...tier) (int, int) {
	l, u := tighter(tiers...)
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
	// A session never resets, so its denial carries no retry hint at all.
	if d.RetryAfter < time.Second && reason != ReasonSessionTokenCap {
		d.RetryAfter = time.Second
	}
	return d
}

// Record adds a finished request's tokens to the project's current windows.
// Pass countRequest=false when Check already reserved the minute slot.
func (l *Limiter) Record(ctx context.Context, projectID int64, promptTokens, completionTokens int, countRequest bool) error {
	return l.Store.RecordUsage(ctx, projectID, Windows(l.now()), promptTokens, completionTokens, countRequest)
}

// RecordSubject adds a finished request's tokens to every tier that applies.
// sp.CountRequest says whether the project's minute request still has to be
// counted (Check reserves it when an RPM limit is set); the key and end-user
// tiers are derived the same way from their own caps, because the slots are
// reserved independently.
func (l *Limiter) RecordSubject(ctx context.Context, s Subject, sp Spend) error {
	w := Windows(l.now())
	if err := l.Store.RecordUsage(ctx, s.Project.ID, w, int(sp.PromptTokens), int(sp.CompletionTokens), sp.CountRequest); err != nil {
		return err
	}
	if s.KeyID != nil {
		if err := l.Store.RecordKeyUsage(ctx, *s.KeyID, w, int(sp.PromptTokens), int(sp.CompletionTokens), s.Key.RPM == 0); err != nil {
			return err
		}
	}
	// An end user is counted only under a project that sets a ceiling: nothing
	// reads the counters otherwise, and the table would grow with every
	// distinct end user for no reason.
	if ec := endUserTier(s); ec.any() {
		if err := l.Store.RecordEndUserUsage(ctx, s.Project.ID, s.EndUser, w, sp.PromptTokens, sp.CompletionTokens, ec.RPM == 0); err != nil {
			return err
		}
	}
	if s.SessionID != "" {
		// Counted whether or not the project sets a cap: the admin session
		// list reads these totals, and a cap set later should see the tokens
		// the session already spent.
		return l.Store.RecordSessionUsage(ctx, s.Project.ID, s.SessionID, int(sp.PromptTokens), int(sp.CompletionTokens))
	}
	return nil
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

// PurgeUsage deletes counter rows that no window can reference any more: the
// project and key tiers under the long retention, the end-user tier under its
// own, shorter one.
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
	for _, c := range []struct {
		period string
		cutoff time.Time
	}{
		{store.PeriodMinute, now.Add(-EndUserKeepMinuteRows)},
		{store.PeriodDay, now.Add(-EndUserKeepDayRows)},
		{store.PeriodMonth, endUserMonthCutoff(now)},
	} {
		if _, err := l.Store.DeleteEndUserUsageBefore(ctx, c.period, c.cutoff); err != nil {
			return err
		}
	}
	return nil
}
