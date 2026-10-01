package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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

// Retired keys go once they are past APIKeyRetention, but only when no
// request log still attributes spend to them: that NOT EXISTS is what makes
// request_logs.api_key_id's ON DELETE SET NULL safe.
func TestAPIKeyPurgeKeepsAttributedKeys(t *testing.T) {
	f := seed(t)
	u, err := f.st.CreateUser(f.ctx, "owner", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, revokedAgo time.Duration, withLog bool) int64 {
		t.Helper()
		k, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindGateway, Name: name,
			UserID: u.ID, ProjectIDs: []int64{f.proj}})
		if err != nil {
			t.Fatal(err)
		}
		if revokedAgo > 0 {
			if _, err := f.st.DB().Exec(f.ctx, "UPDATE api_keys SET revoked_at = $1 WHERE id = $2",
				f.now.Add(-revokedAgo), k.ID); err != nil {
				t.Fatal(err)
			}
		}
		if withLog {
			if err := f.st.InsertRequestLog(f.ctx, &store.RequestLog{ProjectID: f.proj, ModelName: "m",
				StatusCode: 200, APIKeyID: &k.ID, UserID: &u.ID}); err != nil {
				t.Fatal(err)
			}
		}
		return k.ID
	}
	live := mk("live", 0, false)
	recent := mk("recent", time.Hour, false)
	old := mk("old", 60*24*time.Hour, false)
	billed := mk("billed", 60*24*time.Hour, true)

	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365}
	rep, err := j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.APIKeys != 1 {
		t.Errorf("purged %d key(s), want 1", rep.APIKeys)
	}
	for _, c := range []struct {
		name string
		id   int64
		want bool
	}{{"live", live, true}, {"recently revoked", recent, true}, {"long revoked", old, false},
		{"revoked but billed", billed, true}} {
		_, err := f.st.GetAPIKey(f.ctx, c.id)
		if (err == nil) != c.want {
			t.Errorf("%s key present = %v, want %v (%v)", c.name, err == nil, c.want, err)
		}
	}
	// The billed key's request log keeps its attribution.
	rows, err := f.st.RecentRequests(f.ctx, store.MetricsFilter{ProjectID: &f.proj}, 10)
	if err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for _, r := range rows {
		if r.APIKeyID != nil {
			attributed++
		}
	}
	if attributed != 1 {
		t.Errorf("%d attributed request log(s), want 1", attributed)
	}
}

// TestRetentionPassRunsOnLeaderOnly: every replica schedules the retention
// job, and the advisory lock is what keeps them from all deleting the same
// rows at the same time.
func TestRetentionPassRunsOnLeaderOnly(t *testing.T) {
	f := seed(t)
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365}

	// Another replica is mid-pass: this one skips its turn entirely.
	release, ok, err := f.st.TryAdvisoryLock(f.ctx, store.LockJanitor)
	if err != nil || !ok {
		t.Fatalf("take the leader lock: ok=%v err=%v", ok, err)
	}
	if j.runLeader(f.ctx) {
		t.Error("a replica without the leader lock must not run the pass")
	}
	if n := f.count("request_logs"); n != 2 {
		t.Errorf("a follower deleted rows: %d left, want 2", n)
	}
	release()

	// The leader is gone; the next pass runs here.
	if !j.runLeader(f.ctx) {
		t.Fatal("the pass should run once the lock is free")
	}
	if n := f.count("request_logs"); n != 1 {
		t.Errorf("request_logs after the leader pass = %d, want 1", n)
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

// backlog adds n request logs created at, on top of what seed made.
func (f *fixture) backlog(n int, at time.Time) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(f.ctx, `INSERT INTO request_logs (project_id, model_name, status_code, created_at)
		SELECT $1, 'm', 200, $2 FROM generate_series(1, $3)`, f.proj, at, n); err != nil {
		f.t.Fatal(err)
	}
}

// olderThan counts request logs created before the cutoff.
func (f *fixture) olderThan(cutoff time.Time) int {
	f.t.Helper()
	var n int
	if err := f.st.DB().QueryRow(f.ctx, "SELECT COUNT(*) FROM request_logs WHERE created_at < $1", cutoff).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// A backlog larger than one batch goes in full-size batches until one comes
// back short: 1050 expired rows at 100 per batch is ten full batches and a
// last one of 50.
func TestRetentionDeletesInBatches(t *testing.T) {
	f := seed(t)
	cutoff := f.now.AddDate(0, 0, -90)
	f.backlog(1049, f.now.AddDate(0, 0, -200)) // + seed's 100-day-old row = 1050
	f.backlog(20, f.now.AddDate(0, 0, -89))    // inside the window, with seed's 10-day-old row
	if n := f.olderThan(cutoff); n != 1050 {
		t.Fatalf("expired rows before the pass = %d, want 1050", n)
	}
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90,
		batchSize: 100, batchPause: time.Millisecond}

	n, batches, err := j.deleteBatched(f.ctx, store.RetainRequestLogs, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1050 || batches != 11 {
		t.Errorf("deleted %d rows in %d batches, want 1050 in 11", n, batches)
	}
	if left := f.olderThan(cutoff); left != 0 {
		t.Errorf("%d expired rows left", left)
	}
	if n := f.count("request_logs"); n != 21 {
		t.Errorf("request_logs left = %d, want the 21 inside the window", n)
	}

	// Through RunOnce the same backlog lands in the report.
	f.backlog(250, f.now.AddDate(0, 0, -120))
	rep, err := j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestLogs != 250 {
		t.Errorf("RunOnce removed %d request logs, want 250", rep.RequestLogs)
	}
	if n := f.count("request_logs"); n != 21 {
		t.Errorf("request_logs left = %d, want 21", n)
	}
}

// Shutting down mid-backlog stops the loop between batches without an
// error; the next pass deletes what is left.
func TestRetentionCancelStopsCleanlyAndResumes(t *testing.T) {
	f := seed(t)
	cutoff := f.now.AddDate(0, 0, -90)
	f.backlog(1049, f.now.AddDate(0, 0, -200))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365,
		batchSize: 100, batchPause: time.Millisecond,
		wait: func(context.Context, time.Duration) bool { cancel(); return false }}

	rep, err := j.RunOnce(ctx)
	if err != nil {
		t.Fatalf("a cancelled pass must not fail: %v", err)
	}
	if rep.RequestLogs != 100 {
		t.Errorf("cancelled pass removed %d request logs, want the first batch of 100", rep.RequestLogs)
	}
	if left := f.olderThan(cutoff); left != 950 {
		t.Errorf("%d expired rows left after the cancel, want 950", left)
	}
	// The pass stopped where it was: later tables were not touched.
	if n := f.count("audit_logs"); n != 2 {
		t.Errorf("audit_logs = %d after a cancelled pass, want 2", n)
	}

	// A loop entered with an already cancelled context returns at once.
	n, batches, err := j.deleteBatched(ctx, store.RetainRequestLogs, cutoff)
	if err != nil || n != 0 || batches != 0 {
		t.Errorf("deleteBatched on a cancelled ctx = (%d, %d, %v), want (0, 0, nil)", n, batches, err)
	}

	j.wait = nil
	rep, err = j.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestLogs != 950 || rep.AuditLogs != 1 {
		t.Errorf("next pass = %+v, want 950 request logs and 1 audit log", rep)
	}
	if left := f.olderThan(cutoff); left != 0 {
		t.Errorf("%d expired rows left after the next pass", left)
	}
	if n := f.count("request_logs"); n != 1 {
		t.Errorf("request_logs left = %d, want the fresh one", n)
	}
}

func TestReportDeleted(t *testing.T) {
	r := Report{RequestLogs: 1, AuditLogs: 2, LoginAttempts: 3, Sessions: 4, APIKeys: 5, DocumentsFailed: 100}
	if got := r.Deleted(); got != 15 {
		t.Errorf("Deleted() = %d, want 15", got)
	}
}

// logRecords decodes the JSON lines a slog.JSONHandler wrote.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// passRecord returns the "retention pass" summary and fails on a
// "retention pass failed" warning.
func passRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var pass map[string]any
	for _, rec := range logRecords(t, buf) {
		switch rec["msg"] {
		case "retention pass failed":
			t.Errorf("unexpected warning: %v", rec)
		case "retention pass":
			pass = rec
		}
	}
	if pass == nil {
		t.Fatalf("no retention pass record in %q", buf.String())
	}
	return pass
}

// A shutdown that lands while a DELETE is running makes the statement fail
// with the context error; that is a clean stop, not a failed pass, and the
// summary still reports what the pass removed.
func TestRetentionCancelDuringStatementIsClean(t *testing.T) {
	f := seed(t)
	cutoff := f.now.AddDate(0, 0, -90)
	f.backlog(1049, f.now.AddDate(0, 0, -200))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	var buf bytes.Buffer
	calls := 0
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365,
		Log:       slog.New(slog.NewJSONHandler(&buf, nil)),
		batchSize: 100, batchPause: time.Millisecond,
		deleteBatch: func(ctx context.Context, table store.RetentionTable, cutoff time.Time, limit int) (int64, error) {
			calls++
			if calls == 2 {
				cancel() // the statement now runs on a cancelled context
			}
			return f.st.DeleteExpiredBatch(ctx, table, cutoff, limit)
		}}

	j.runLogged(ctx)

	pass := passRecord(t, &buf)
	if pass["interrupted"] != true {
		t.Errorf("interrupted = %v, want true", pass["interrupted"])
	}
	if pass["deleted"] != float64(100) || pass["request_logs"] != float64(100) {
		t.Errorf("deleted = %v, request_logs = %v, want 100 each", pass["deleted"], pass["request_logs"])
	}
	if _, ok := pass["duration"]; !ok {
		t.Error("summary has no duration")
	}
	if left := f.olderThan(cutoff); left != 950 {
		t.Errorf("%d expired rows left, want 950", left)
	}

	// The same path straight through deleteBatched: no error, the rows of
	// the batches that completed.
	ctx2, cancel2 := context.WithCancel(f.ctx)
	defer cancel2()
	calls = 0
	j.deleteBatch = func(ctx context.Context, table store.RetentionTable, cutoff time.Time, limit int) (int64, error) {
		calls++
		if calls == 2 {
			cancel2()
		}
		return f.st.DeleteExpiredBatch(ctx, table, cutoff, limit)
	}
	n, batches, err := j.deleteBatched(ctx2, store.RetainRequestLogs, cutoff)
	if err != nil || n != 100 || batches != 1 {
		t.Errorf("deleteBatched = (%d, %d, %v), want (100, 1, nil)", n, batches, err)
	}
}

// The real pause between batches gives way to a cancelled context instead
// of sitting out the timer.
func TestRetentionPauseYieldsToCancel(t *testing.T) {
	f := seed(t)
	cutoff := f.now.AddDate(0, 0, -90)
	f.backlog(1049, f.now.AddDate(0, 0, -200))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90,
		batchSize: 100, batchPause: time.Hour,
		deleteBatch: func(ctx context.Context, table store.RetentionTable, cutoff time.Time, limit int) (int64, error) {
			n, err := f.st.DeleteExpiredBatch(ctx, table, cutoff, limit)
			time.AfterFunc(20*time.Millisecond, cancel) // fires during the hour-long pause
			return n, err
		}}

	start := time.Now()
	n, batches, err := j.deleteBatched(ctx, store.RetainRequestLogs, cutoff)
	if err != nil || n != 100 || batches != 1 {
		t.Errorf("deleteBatched = (%d, %d, %v), want (100, 1, nil)", n, batches, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("deleteBatched took %v; the pause did not observe the cancel", d)
	}
}

// A completed pass logs its total, its duration and that it ran to the end.
func TestRunLoggedReportsTotals(t *testing.T) {
	f := seed(t)
	var buf bytes.Buffer
	j := &Janitor{Store: f.st, Now: func() time.Time { return f.now }, RequestLogDays: 90, AuditDays: 365,
		Log: slog.New(slog.NewJSONHandler(&buf, nil))}

	j.runLogged(f.ctx)

	pass := passRecord(t, &buf)
	if pass["interrupted"] != false {
		t.Errorf("interrupted = %v, want false", pass["interrupted"])
	}
	want := pass["request_logs"].(float64) + pass["audit_logs"].(float64) + pass["login_attempts"].(float64) +
		pass["sessions"].(float64) + pass["api_keys"].(float64)
	if pass["deleted"] != want || want == 0 {
		t.Errorf("deleted = %v, want the per-table sum %v (non-zero)", pass["deleted"], want)
	}
	if d, ok := pass["duration"].(float64); !ok || d <= 0 {
		t.Errorf("duration = %v, want a positive value", pass["duration"])
	}
}
