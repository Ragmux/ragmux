// Package alerts sends the limit and budget warnings (ADR-036): a signed
// webhook when a project's, a key's or an end user's token budget, or a
// project's monthly money budget, reaches 80 % or 100 %.
//
// It is a background job and nothing more. The gateway only counts (tokens and
// cost go into the usage counters as before); once a minute one replica, the
// one that wins the advisory lock, reads those counters, records each crossed
// threshold once in alert_events and queues the signed delivery in the webhook
// outbox in the same transaction. Delivery is the webhook worker's business. A
// failing receiver therefore delays or loses a warning; it cannot slow, fail
// or refuse a request. The money budget in particular only ever warns: nothing
// here, or anywhere, turns a cost into a 429 (PRD rule 7).
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/bits"
	"strconv"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// DefaultInterval is the pause between evaluation rounds, and so the longest a
// crossed threshold waits before its alert is queued.
const DefaultInterval = 60 * time.Second

// Event types of the alert payloads.
const (
	EventLimitThreshold  = "limit.threshold"
	EventUnpricedTraffic = "budget.unpriced_traffic"
)

// LateWindow is how long after a day or a month ends the job still looks at it.
// A round that had not yet run when the counters crossed a limit in the last
// minute of a window would otherwise lose that alert for good (the next round
// reads the next window); this gives a restart, a deploy or a database outage
// of up to this long the same chance as an ordinary minute. Beyond it a
// breach of the closed window is not reported: a limit changed since would
// make an old window warn about something nobody can act on.
const LateWindow = 6 * time.Hour

// Thresholds are the fixed warning levels in percent of a limit. An end user's
// budget warns at the last one only: with thousands of end users the first
// would be a flood.
var Thresholds = []int{80, 100}

// Store is the part of *store.Store the job needs.
type Store interface {
	TryAdvisoryLock(ctx context.Context, key int64) (func(), bool, error)
	AlertTargets(ctx context.Context) ([]store.AlertTarget, error)
	BudgetUsages(ctx context.Context, w store.UsageWindows, projectIDs []int64) ([]store.BudgetUsage, error)
	EndUsersOverCap(ctx context.Context, w store.UsageWindows, projectIDs []int64) ([]store.EndUserOverCap, error)
	AlertEventKeys(ctx context.Context, projectIDs []int64, since time.Time) ([]store.AlertEvent, error)
	RecordAlertEvent(ctx context.Context, e store.AlertEvent, endpointID int64, build store.AlertPayload) (bool, error)
}

// Job evaluates the limits and queues the alerts. Start it with Run on every
// replica: the advisory lock lets one of them work at a time.
type Job struct {
	store    Store
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time
}

// Option customises a Job.
type Option func(*Job)

// WithLogger sets the job's logger.
func WithLogger(l *slog.Logger) Option { return func(j *Job) { j.log = l } }

// WithInterval sets the pause between rounds (default 60 s).
func WithInterval(d time.Duration) Option {
	return func(j *Job) {
		if d > 0 {
			j.interval = d
		}
	}
}

// WithClock replaces the clock the windows are read from (tests).
func WithClock(now func() time.Time) Option { return func(j *Job) { j.now = now } }

// NewJob builds the job. Start it with Run.
func NewJob(st Store, opts ...Option) *Job {
	j := &Job{store: st, log: slog.Default(), interval: DefaultInterval, now: time.Now}
	for _, o := range opts {
		o(j)
	}
	return j
}

// Run evaluates once at once and then every interval until ctx is cancelled.
func (j *Job) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
			j.log.Warn("limit alert round failed", "err", err)
		}
		t.Reset(j.interval)
	}
}

// RunOnce makes one round and reports whether this replica made it: false when
// another replica holds the leader lock. A round that fails on one event goes
// on with the rest and returns the errors joined; the events not recorded are
// found again by the next round.
func (j *Job) RunOnce(ctx context.Context) (bool, error) {
	release, ok, err := j.store.TryAdvisoryLock(ctx, store.LockAlerts)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	defer release()
	return true, j.evaluate(ctx, j.now().UTC())
}

// eventKey is an alert event's identity as the round compares it.
type eventKey struct {
	project         int64
	kind, id, limit string
	threshold       int
	window          int64
	value           int64
}

func keyOf(e store.AlertEvent) eventKey {
	return eventKey{e.ProjectID, e.SubjectKind, e.SubjectID, e.LimitKind, e.Threshold, e.WindowStart.UnixNano(), e.LimitValue}
}

// round is the state of one pass over one pair of windows: which windows, where
// they end, whether the day and the month are looked at, the projects being
// examined, the events already recorded and the errors met on the way.
type round struct {
	j        *Job
	ctx      context.Context
	w        store.UsageWindows
	dayEnd   time.Time
	monEnd   time.Time
	day, mon bool
	// late marks the pass over the windows that just closed.
	late    bool
	targets map[int64]store.AlertTarget
	known   map[eventKey]struct{}
	errs    []error
}

func (j *Job) evaluate(ctx context.Context, now time.Time) error {
	targets, err := j.store.AlertTargets(ctx)
	if err != nil || len(targets) == 0 {
		return err
	}
	cur := limits.Windows(now)
	// The current windows, and the ones just closed while they are still within
	// LateWindow of their end.
	prev := store.UsageWindows{Minute: cur.Minute, Day: cur.Day.AddDate(0, 0, -1), Month: cur.Month.AddDate(0, -1, 0)}
	type pass struct {
		w        store.UsageWindows
		day, mon bool
		late     bool
	}
	passes := []pass{{w: cur, day: true, mon: true}}
	if lateDay, lateMon := now.Sub(cur.Day) < LateWindow, now.Sub(cur.Month) < LateWindow; lateDay || lateMon {
		passes = append(passes, pass{w: prev, day: lateDay, mon: lateMon, late: true})
	}

	tmap := make(map[int64]store.AlertTarget, len(targets))
	ids := make([]int64, 0, len(targets))
	var withEndUserCap []int64
	for _, t := range targets {
		tmap[t.ProjectID] = t
		ids = append(ids, t.ProjectID)
		if t.EndUserDailyTokens > 0 || t.EndUserMonthlyTokens > 0 {
			withEndUserCap = append(withEndUserCap, t.ProjectID)
		}
	}
	// One read of what was already recorded, so a limit that stays crossed for
	// weeks costs this round a lookup in a map and not a transaction per event.
	since := cur.Month // the earliest window of the current pair
	if len(passes) > 1 {
		if passes[1].day && prev.Day.Before(since) {
			since = prev.Day
		}
		if passes[1].mon && prev.Month.Before(since) {
			since = prev.Month
		}
	}
	recorded, err := j.store.AlertEventKeys(ctx, ids, since)
	if err != nil {
		return err
	}
	known := make(map[eventKey]struct{}, len(recorded))
	for _, e := range recorded {
		known[keyOf(e)] = struct{}{}
	}

	var errs []error
	for _, p := range passes {
		r := &round{j: j, ctx: ctx, w: p.w, dayEnd: p.w.Day.AddDate(0, 0, 1), monEnd: p.w.Month.AddDate(0, 1, 0),
			day: p.day, mon: p.mon, late: p.late, targets: tmap, known: known}
		usages, err := j.store.BudgetUsages(ctx, p.w, ids)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, u := range usages {
			if t, ok := tmap[u.ProjectID]; ok {
				r.budget(t, u)
			}
		}
		if len(withEndUserCap) > 0 {
			over, err := j.store.EndUsersOverCap(ctx, p.w, withEndUserCap)
			if err != nil {
				r.errs = append(r.errs, err)
			}
			for _, o := range over {
				r.endUser(tmap[o.ProjectID], o)
			}
		}
		errs = append(errs, r.errs...)
	}
	return errors.Join(errs...)
}

// budget checks one project or key row: its daily and monthly token budgets,
// and for a project row the money budget and the unpriced-traffic notice.
func (r *round) budget(t store.AlertTarget, u store.BudgetUsage) {
	kind, id := store.AlertSubjectProject, strconv.FormatInt(u.ProjectID, 10)
	if u.KeyID != 0 {
		kind, id = store.AlertSubjectKey, strconv.FormatInt(u.KeyID, 10)
	}
	subj := subject{Kind: kind, ID: id}
	day := r.day && !r.limitChangedAfter(r.dayEnd, t.SettingsUpdatedAt, u.UpdatedAt)
	mon := r.mon && !r.limitChangedAfter(r.monEnd, t.SettingsUpdatedAt, u.UpdatedAt)
	for _, th := range Thresholds {
		if day {
			r.threshold(t, subj, store.AlertLimitDailyTokens, th, u.DailyUsed, u.DailyLimit, r.w.Day, r.dayEnd, nil)
		}
		if mon {
			r.threshold(t, subj, store.AlertLimitMonthlyTokens, th, u.MonthlyUsed, u.MonthlyLimit, r.w.Month, r.monEnd, nil)
		}
	}
	// The money budget lives in the alert settings alone.
	if !r.mon || u.KeyID != 0 || t.BudgetMonthlyUSDMicros <= 0 || r.limitChangedAfter(r.monEnd, t.SettingsUpdatedAt) {
		return
	}
	mn := &money{Unpriced: u.MonthlyUnpriced}
	for _, th := range Thresholds {
		r.threshold(t, subj, store.AlertLimitMonthlyUSD, th, u.MonthlyCostMicros, t.BudgetMonthlyUSDMicros, r.w.Month, r.monEnd, mn)
	}
	if u.MonthlyUnpriced > 0 {
		r.unpriced(t, u.MonthlyUnpriced)
	}
}

// limitChangedAfter reports, for a pass over closed windows, that the limit or
// the settings were edited after the window ended. The counters of a closed
// window are measured against what is stored now, so a limit set, raised or
// switched on after the window ended would warn about a limit that was not in
// force while it was open. The check errs toward saying nothing: any later edit
// of the project or key row counts, a rare late warning dropped and never a
// false one sent. The current windows are never in question.
func (r *round) limitChangedAfter(end time.Time, edited ...time.Time) bool {
	if !r.late {
		return false
	}
	for _, e := range edited {
		if e.After(end) {
			return true
		}
	}
	return false
}

// endUser checks one end user against its ceiling: the 100 % threshold only.
func (r *round) endUser(t store.AlertTarget, o store.EndUserOverCap) {
	kind, start, end := store.AlertLimitDailyTokens, r.w.Day, r.dayEnd
	if o.Period == store.PeriodMonth {
		kind, start, end = store.AlertLimitMonthlyTokens, r.w.Month, r.monEnd
	}
	if (o.Period == store.PeriodMonth && !r.mon) || (o.Period != store.PeriodMonth && !r.day) ||
		r.limitChangedAfter(end, t.SettingsUpdatedAt, t.ProjectUpdatedAt) {
		return
	}
	r.threshold(t, subject{Kind: store.AlertSubjectEndUser, ID: o.EndUser}, kind, Thresholds[len(Thresholds)-1],
		o.Used, o.Cap, start, end, nil)
}

// threshold queues the alert when used has reached th percent of limit and the
// event has not been recorded before. A limit of 0 is no limit.
func (r *round) threshold(t store.AlertTarget, subj subject, limitKind string, th int, used, limit int64,
	start, end time.Time, m *money) {
	if limit <= 0 || !reached(used, limit, int64(th)) {
		return
	}
	ev := store.AlertEvent{ProjectID: t.ProjectID, SubjectKind: subj.Kind, SubjectID: subj.ID, LimitKind: limitKind,
		Threshold: th, WindowStart: start, LimitValue: limit}
	r.record(t, ev, func(id int64, created time.Time) (string, []byte, error) {
		p := thresholdPayload{
			Type: EventLimitThreshold, ID: webhook.EventID(t.EndpointID, strconv.FormatInt(id, 10)), CreatedAt: created.UTC(),
			Project: project{ID: t.ProjectID, Name: t.ProjectName}, Subject: subj, Limit: limitKind,
			Threshold: th, Used: used, LimitValue: limit, Ratio: float64(used) / float64(limit),
			WindowStart: start.UTC(), WindowEnd: end.UTC(),
		}
		if m != nil {
			// A money figure is an estimate and a lower bound: a request
			// whose model has no price adds 0 and is counted here instead.
			yes, n := true, m.Unpriced
			p.Estimate, p.UnpricedRequests = &yes, &n
		}
		body, err := json.Marshal(p)
		return EventLimitThreshold, body, err
	})
}

// unpriced queues the once-a-month notice that requests without a price were
// made, so the money figures are lower bounds. The event key has no threshold
// and no limit (both 0): changing the budget does not make a second one.
func (r *round) unpriced(t store.AlertTarget, n int64) {
	ev := store.AlertEvent{ProjectID: t.ProjectID, SubjectKind: store.AlertSubjectProject,
		SubjectID: strconv.FormatInt(t.ProjectID, 10), LimitKind: store.AlertLimitUnpriced, Threshold: 0,
		WindowStart: r.w.Month, LimitValue: 0}
	r.record(t, ev, func(id int64, created time.Time) (string, []byte, error) {
		yes := true
		body, err := json.Marshal(unpricedPayload{
			Type: EventUnpricedTraffic, ID: webhook.EventID(t.EndpointID, strconv.FormatInt(id, 10)), CreatedAt: created.UTC(),
			Project: project{ID: t.ProjectID, Name: t.ProjectName},
			Subject: subject{Kind: store.AlertSubjectProject, ID: strconv.FormatInt(t.ProjectID, 10)},
			Limit:   store.AlertLimitMonthlyUSD, WindowStart: r.w.Month.UTC(), WindowEnd: r.monEnd.UTC(),
			Estimate: yes, UnpricedRequests: n,
		})
		return EventUnpricedTraffic, body, err
	})
}

func (r *round) record(t store.AlertTarget, ev store.AlertEvent, build store.AlertPayload) {
	if _, done := r.known[keyOf(ev)]; done {
		return
	}
	added, err := r.j.store.RecordAlertEvent(r.ctx, ev, t.EndpointID, build)
	if err != nil {
		r.errs = append(r.errs, err)
		return
	}
	if added {
		r.known[keyOf(ev)] = struct{}{}
		// Ids and counts only: no URL, no subject (an end user is the
		// operator's identifier for a person).
		r.j.log.Info("limit alert queued", "project", t.ProjectID, "subject_kind", ev.SubjectKind,
			"limit", ev.LimitKind, "threshold", ev.Threshold)
	}
}

// reached reports whether used is at least pct percent of limit, exactly: the
// product is taken in 128 bits so a huge counter cannot wrap.
func reached(used, limit, pct int64) bool {
	if used < 0 || limit <= 0 || pct < 0 || pct > 100 {
		return false
	}
	uh, ul := bits.Mul64(uint64(used), 100)
	lh, ll := bits.Mul64(uint64(limit), uint64(pct))
	return uh > lh || (uh == lh && ul >= ll)
}

// ---- payloads (tasarim L7) ----

type project struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// money carries what only a money event reports.
type money struct{ Unpriced int64 }

// thresholdPayload is the body of limit.threshold. Estimate and
// UnpricedRequests are set on money events only.
type thresholdPayload struct {
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	Project          project   `json:"project"`
	Subject          subject   `json:"subject"`
	Limit            string    `json:"limit"`
	Threshold        int       `json:"threshold"`
	Used             int64     `json:"used"`
	LimitValue       int64     `json:"limit_value"`
	Ratio            float64   `json:"ratio"`
	WindowStart      time.Time `json:"window_start"`
	WindowEnd        time.Time `json:"window_end"`
	Estimate         *bool     `json:"estimate,omitempty"`
	UnpricedRequests *int64    `json:"unpriced_requests,omitempty"`
}

// unpricedPayload is the body of budget.unpriced_traffic: no threshold, used,
// limit_value or ratio, because the event is about the figures' quality, not a
// limit.
type unpricedPayload struct {
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	Project          project   `json:"project"`
	Subject          subject   `json:"subject"`
	Limit            string    `json:"limit"`
	WindowStart      time.Time `json:"window_start"`
	WindowEnd        time.Time `json:"window_end"`
	Estimate         bool      `json:"estimate"`
	UnpricedRequests int64     `json:"unpriced_requests"`
}
