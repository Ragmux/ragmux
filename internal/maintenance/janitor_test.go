package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// fixture seeds one old and one fresh row in every table the janitor touches
// and returns the clock the janitor should use.
type fixture struct {
	t     *testing.T
	st    *store.Store
	now   time.Time
	proj  int64
	ctx   context.Context
	count func(table string) int
}

func seed(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "k", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := st.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, "u", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, now: now, proj: p.ID, ctx: ctx}
	f.count = func(table string) int {
		var n int
		if err := st.DB().QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// request_logs: 100 days old and 10 days old.
	for _, age := range []int{100, 10} {
		if err := st.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
		f.backdate("request_logs", now.AddDate(0, 0, -age))
	}
	// audit_logs: 400 days old and 30 days old.
	for _, age := range []int{400, 30} {
		if err := st.InsertAuditLog(ctx, &store.AuditLog{Action: "x", TargetType: "y", ActorUsername: "u"}); err != nil {
			t.Fatal(err)
		}
		f.backdate("audit_logs", now.AddDate(0, 0, -age))
	}
	// login_attempts: 2 days old and 1 hour old.
	for _, age := range []time.Duration{48 * time.Hour, time.Hour} {
		if err := st.RecordLoginAttempt(ctx, "u", "1.2.3.4", false); err != nil {
			t.Fatal(err)
		}
		f.backdate("login_attempts", now.Add(-age))
	}
	// sessions: one expired an hour before now, one valid for another day.
	for i, exp := range []time.Time{now.Add(-time.Hour), now.Add(24 * time.Hour)} {
		if err := st.CreateSession(ctx, u.ID, "tok"+string(rune('a'+i)), time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(ctx, "UPDATE sessions SET expires_at = $1 WHERE id = (SELECT MAX(id) FROM sessions)", exp); err != nil {
			t.Fatal(err)
		}
	}
	// usage: a minute row from three hours ago and the current minute.
	lim := &limits.Limiter{Store: st, Now: func() time.Time { return now.Add(-3 * time.Hour) }}
	if err := lim.Record(ctx, p.ID, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	lim.Now = func() time.Time { return now }
	if err := lim.Record(ctx, p.ID, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// backdate sets created_at of the newest row in table.
func (f *fixture) backdate(table string, at time.Time) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(f.ctx, "UPDATE "+table+" SET created_at = $1 WHERE id = (SELECT MAX(id) FROM "+table+")", at); err != nil {
		f.t.Fatal(err)
	}
}

func TestRunOnceDeletesOnlyExpiredRows(t *testing.T) {
	f := seed(t)
	j := &Janitor{Store: f.st, Limiter: &limits.Limiter{Store: f.st, Now: func() time.Time { return f.now }},
		Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365}
	rep, err := j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Report{RequestLogs: 1, AuditLogs: 1, LoginAttempts: 1, Sessions: 1, UsagePurged: true}
	if rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	for _, tb := range []string{"request_logs", "audit_logs", "login_attempts", "sessions"} {
		if n := f.count(tb); n != 1 {
			t.Errorf("%s: %d rows left, want 1", tb, n)
		}
	}
	if n := f.count("project_usage WHERE period = 'minute'"); n != 1 {
		t.Errorf("minute usage rows left = %d, want 1", n)
	}
	// A second pass finds nothing.
	rep, err = j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestLogs+rep.AuditLogs+rep.LoginAttempts+rep.Sessions != 0 {
		t.Errorf("second pass removed rows: %+v", rep)
	}
}

func TestZeroDaysKeepsForever(t *testing.T) {
	f := seed(t)
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 0, AuditDays: 0}
	rep, err := j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestLogs != 0 || rep.AuditLogs != 0 || rep.UsagePurged {
		t.Errorf("report = %+v", rep)
	}
	if n := f.count("request_logs"); n != 2 {
		t.Errorf("request_logs = %d, want 2", n)
	}
	if n := f.count("audit_logs"); n != 2 {
		t.Errorf("audit_logs = %d, want 2", n)
	}
	// Login attempts and sessions are not configurable and still go.
	if n := f.count("login_attempts"); n != 1 {
		t.Errorf("login_attempts = %d, want 1", n)
	}
	if n := f.count("sessions"); n != 1 {
		t.Errorf("sessions = %d, want 1", n)
	}
}

func TestCutoffIsRelativeToClock(t *testing.T) {
	f := seed(t)
	// Move the clock a year ahead: everything is older than the windows.
	later := f.now.AddDate(1, 0, 0)
	j := &Janitor{Store: f.st, Now: func() time.Time { return later }, RequestLogDays: 90, AuditDays: 365}
	rep, err := j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestLogs != 2 || rep.AuditLogs != 2 || rep.LoginAttempts != 2 || rep.Sessions != 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	f := seed(t)
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { j.Run(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if n := f.count("request_logs"); n != 2 {
		t.Errorf("Run deleted rows before StartDelay: %d left", n)
	}
}
