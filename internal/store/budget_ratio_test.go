package store_test

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestBudgetUsedRatios(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, daily, monthly int64) *store.Project {
		t.Helper()
		p, _, err := s.CreateProject(ctx, &store.Project{Name: name, ModelConnectionID: conn.ID,
			BudgetDailyTokens: daily, BudgetMonthlyTokens: monthly})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	daily := mk("daily", 1000, 0)
	monthly := mk("monthly", 0, 10000)
	both := mk("both", 1000, 2000)
	idle := mk("idle", 500, 0)
	unlimited := mk("unlimited", 0, 0)

	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	w := store.UsageWindows{
		Minute: now.Truncate(time.Minute),
		Day:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Month:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, u := range []struct {
		p          *store.Project
		prompt, cp int
	}{
		{daily, 200, 50},      // day 250/1000
		{monthly, 3000, 1000}, // month 4000/10000
		{both, 300, 200},      // day 500/1000, month 500/2000 -> 0.5 wins
		{unlimited, 999, 999},
	} {
		if err := s.RecordUsage(ctx, u.p.ID, w, u.prompt, u.cp, 0, false, false); err != nil {
			t.Fatal(err)
		}
	}
	// Usage outside the current windows must not count.
	old := store.UsageWindows{Minute: w.Minute.AddDate(0, -1, 0), Day: w.Day.AddDate(0, -1, 0), Month: w.Month.AddDate(0, -1, 0)}
	if err := s.RecordUsage(ctx, idle.ID, old, 500, 500, 0, false, false); err != nil {
		t.Fatal(err)
	}

	got, err := s.BudgetUsedRatios(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	want := map[*store.Project]float64{daily: 0.25, monthly: 0.4, both: 0.5, idle: 0}
	if len(got) != len(want) {
		t.Fatalf("got %d projects (%v), want %d: a project without a budget must be absent", len(got), got, len(want))
	}
	for p, ratio := range want {
		v, ok := got[strconv.FormatInt(p.ID, 10)]
		if !ok {
			t.Errorf("project %q missing", p.Name)
			continue
		}
		if math.Abs(v-ratio) > 1e-9 {
			t.Errorf("project %q ratio = %v, want %v", p.Name, v, ratio)
		}
	}
	if _, ok := got[strconv.FormatInt(unlimited.ID, 10)]; ok {
		t.Error("a project with no budget has a ratio")
	}
}

// RecordUsage adds the request's cost to the day and month rows only, counts
// the unpriced requests beside it, and BudgetUsages reads both together with
// the token usage -- for projects and for the keys granted to them, whether or
// not the project sets a token budget itself.
func TestRecordUsageCostAndBudgetUsages(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, daily int64) *store.Project {
		t.Helper()
		p, _, err := s.CreateProject(ctx, &store.Project{Name: name, ModelConnectionID: conn.ID, BudgetDailyTokens: daily})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	budgeted, plain, other := mk("budgeted", 1000), mk("plain", 0), mk("other", 1000)
	u, err := s.CreateUser(ctx, "owner", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	mkKey := func(name string, daily int64, projects ...int64) *store.APIKey {
		t.Helper()
		k, _, err := s.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: name, UserID: u.ID,
			BudgetDailyTokens: daily, ProjectIDs: projects})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	keyed := mkKey("keyed", 400, budgeted.ID, plain.ID)
	mkKey("no-budget", 0, budgeted.ID)
	revoked := mkKey("revoked", 400, budgeted.ID)
	if _, err := s.RevokeAPIKey(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	mkKey("elsewhere", 400, other.ID)

	now := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	w := store.UsageWindows{Minute: now.Truncate(time.Minute),
		Day: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), Month: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := s.RecordUsage(ctx, budgeted.ID, w, 100, 50, 1200, false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, budgeted.ID, w, 10, 5, 0, true, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, budgeted.ID, w, 1, 1, -50, false, true); err != nil { // never subtracts
		t.Fatal(err)
	}
	if err := s.RecordKeyUsage(ctx, keyed.ID, w, 120, 30, true); err != nil {
		t.Fatal(err)
	}
	// Last month: the current month must not see it.
	old := store.UsageWindows{Minute: w.Minute.AddDate(0, -1, 0), Day: w.Day.AddDate(0, -1, 0), Month: w.Month.AddDate(0, -1, 0)}
	if err := s.RecordUsage(ctx, budgeted.ID, old, 1, 1, 999, true, true); err != nil {
		t.Fatal(err)
	}

	var minuteCost int64
	if err := s.DB().QueryRow(ctx, `SELECT cost_micros FROM project_usage
		WHERE project_id = $1 AND period = 'minute' AND period_start = $2`, budgeted.ID, w.Minute).Scan(&minuteCost); err != nil || minuteCost != 0 {
		t.Errorf("minute row cost = %d, %v; want 0", minuteCost, err)
	}

	got, err := s.BudgetUsages(ctx, w, []int64{budgeted.ID, plain.ID})
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ project, key int64 }
	byRow := map[row]store.BudgetUsage{}
	for _, g := range got {
		byRow[row{g.ProjectID, g.KeyID}] = g
	}
	if len(got) != 4 {
		t.Fatalf("got %d rows (%+v), want 2 projects + the keyed key under both projects", len(got), got)
	}
	want := store.BudgetUsage{ProjectID: budgeted.ID, DailyUsed: 167, DailyLimit: 1000, MonthlyUsed: 167,
		MonthlyCostMicros: 1200, MonthlyUnpriced: 1}
	g := byRow[row{budgeted.ID, 0}]
	if g.UpdatedAt.IsZero() {
		t.Error("the project row carries no UpdatedAt")
	}
	g.UpdatedAt = time.Time{}
	if g != want {
		t.Errorf("budgeted project = %+v, want %+v", g, want)
	}
	// A project without a token budget is still reported: its spend matters.
	if g := byRow[row{plain.ID, 0}]; g.ProjectID != plain.ID || g.DailyLimit != 0 || g.MonthlyCostMicros != 0 {
		t.Errorf("plain project = %+v", g)
	}
	for _, p := range []int64{budgeted.ID, plain.ID} {
		g, ok := byRow[row{p, keyed.ID}]
		if !ok || g.DailyUsed != 150 || g.DailyLimit != 400 || g.MonthlyCostMicros != 0 {
			t.Errorf("key under project %d = %+v, %v", p, g, ok)
		}
	}
	if len(byRow) != 4 {
		t.Errorf("a key without a budget, a revoked key or another project's key was reported: %+v", got)
	}
	if empty, err := s.BudgetUsages(ctx, w, nil); err != nil || len(empty) != 0 {
		t.Errorf("no projects asked for: %v, %v", empty, err)
	}
	// The gauge keeps its own view: projects with a token budget, no keys.
	ratios, err := s.BudgetUsedRatios(ctx, w)
	if err != nil || len(ratios) != 2 {
		t.Errorf("ratios = %v, %v; want the two budgeted projects", ratios, err)
	}
	if r := ratios[strconv.FormatInt(budgeted.ID, 10)]; math.Abs(r-0.167) > 1e-9 {
		t.Errorf("budgeted ratio = %v, want 0.167", r)
	}
}

func TestBudgetUsageRatio(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    store.BudgetUsage
		want float64
		ok   bool
	}{
		{"none", store.BudgetUsage{DailyUsed: 5, MonthlyUsed: 5}, 0, false},
		{"daily", store.BudgetUsage{DailyUsed: 50, DailyLimit: 100}, 0.5, true},
		{"monthly", store.BudgetUsage{MonthlyUsed: 30, MonthlyLimit: 100}, 0.3, true},
		{"larger wins", store.BudgetUsage{DailyUsed: 50, DailyLimit: 100, MonthlyUsed: 90, MonthlyLimit: 100}, 0.9, true},
		{"unused", store.BudgetUsage{DailyLimit: 100}, 0, true},
	} {
		got, ok := tc.u.Ratio()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: Ratio() = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// An end user is reported only once its tokens in the current window reach the
// project's ceiling for that window, and only for a project that sets one.
func TestEndUsersOverCap(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	capped, _, err := s.CreateProject(ctx, &store.Project{Name: "capped", ModelConnectionID: conn.ID,
		EndUserDailyTokens: 100, EndUserMonthlyTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	free, _, err := s.CreateProject(ctx, &store.Project{Name: "free", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	w := store.UsageWindows{Minute: now.Truncate(time.Minute),
		Day: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), Month: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	rec := func(p *store.Project, user string, w store.UsageWindows, prompt, completion int64) {
		t.Helper()
		if err := s.RecordEndUserUsage(ctx, p.ID, user, w, prompt, completion, true); err != nil {
			t.Fatal(err)
		}
	}
	rec(capped, "under", w, 60, 39)   // 99 of 100
	rec(capped, "exact", w, 60, 40)   // 100 of 100
	rec(capped, "heavy", w, 700, 400) // over both
	rec(free, "heavy", w, 5000, 5000) // no ceiling
	yesterday := store.UsageWindows{Minute: w.Minute.AddDate(0, 0, -1), Day: w.Day.AddDate(0, 0, -1), Month: w.Month}
	rec(capped, "yesterday", yesterday, 500, 500) // day row of another day; month 1000 of 1000

	got, err := s.EndUsersOverCap(ctx, w, []int64{capped.ID, free.ID})
	if err != nil {
		t.Fatal(err)
	}
	type hit struct{ user, period string }
	seen := map[hit]store.EndUserOverCap{}
	for _, g := range got {
		seen[hit{g.EndUser, g.Period}] = g
	}
	for _, h := range []hit{{"exact", "day"}, {"heavy", "day"}, {"heavy", "month"}, {"yesterday", "month"}} {
		if _, ok := seen[h]; !ok {
			t.Errorf("%v missing from %+v", h, got)
		}
	}
	if len(seen) != 4 || len(got) != 4 {
		t.Errorf("got %+v, want exactly the four hits above", got)
	}
	if g := seen[hit{"exact", "day"}]; g.Used != 100 || g.Cap != 100 || !g.PeriodStart.Equal(w.Day) {
		t.Errorf("exact = %+v", g)
	}
	if none, err := s.EndUsersOverCap(ctx, w, nil); err != nil || len(none) != 0 {
		t.Errorf("no projects asked for: %v, %v", none, err)
	}
}

// RecordAlertEvent is once-only per event key, and it queues the delivery in
// the same transaction: an event that cannot be queued is not remembered.
func TestRecordAlertEventOnceAndQueued(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts,
		URL: "https://example.com/hook", Secret: "whsec_x"})
	if err != nil {
		t.Fatal(err)
	}
	ev := store.AlertEvent{ProjectID: p.ID, SubjectKind: store.AlertSubjectProject, SubjectID: "1",
		LimitKind: store.AlertLimitDailyTokens, Threshold: 80, WindowStart: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), LimitValue: 1000}
	var seenID int64
	build := func(id int64, _ time.Time) (string, []byte, error) {
		seenID = id
		return "limit.threshold", []byte(`{"type":"limit.threshold"}`), nil
	}
	added, err := s.RecordAlertEvent(ctx, ev, ep.ID, build)
	if err != nil || !added {
		t.Fatalf("first: %v, %v", added, err)
	}
	if added, err = s.RecordAlertEvent(ctx, ev, ep.ID, build); err != nil || added {
		t.Fatalf("repeat: %v, %v", added, err)
	}
	ds, err := s.ListWebhookDeliveries(ctx, ep.ID, 10)
	if err != nil || len(ds) != 1 || ds[0].DedupeKey != strconv.FormatInt(seenID, 10) || ds[0].EventType != "limit.threshold" {
		t.Fatalf("deliveries = %+v, %v; want one keyed by the event id %d", ds, err, seenID)
	}
	// The limit moved: a new event.
	ev.LimitValue = 1200
	if added, err = s.RecordAlertEvent(ctx, ev, ep.ID, build); err != nil || !added {
		t.Fatalf("new limit value: %v, %v", added, err)
	}

	// A closed endpoint queues nothing and the event is not remembered, so it
	// is sent if the endpoint comes back while the limit is still crossed.
	if err := s.DisableWebhookEndpoint(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	ev.Threshold = 100
	if added, err = s.RecordAlertEvent(ctx, ev, ep.ID, build); err != nil || added {
		t.Fatalf("disabled endpoint: %v, %v", added, err)
	}
	var n int
	if err := s.DB().QueryRow(ctx, "SELECT count(*) FROM alert_events WHERE threshold = 100").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d events remembered for a closed endpoint, %v", n, err)
	}
	// A builder error rolls the event back too.
	if _, err := s.UpdateWebhookEndpointURL(ctx, ep.ID, "https://example.com/hook"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err = s.RecordAlertEvent(ctx, ev, ep.ID, func(int64, time.Time) (string, []byte, error) { return "", nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("builder error: %v", err)
	}
	if err := s.DB().QueryRow(ctx, "SELECT count(*) FROM alert_events WHERE threshold = 100").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d events remembered after a failed build, %v", n, err)
	}
	if added, err = s.RecordAlertEvent(ctx, ev, ep.ID, build); err != nil || !added {
		t.Errorf("after the endpoint came back: %v, %v", added, err)
	}
}
