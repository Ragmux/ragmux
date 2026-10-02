package store_test

import (
	"context"
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
		if err := s.RecordUsage(ctx, u.p.ID, w, u.prompt, u.cp, false); err != nil {
			t.Fatal(err)
		}
	}
	// Usage outside the current windows must not count.
	old := store.UsageWindows{Minute: w.Minute.AddDate(0, -1, 0), Day: w.Day.AddDate(0, -1, 0), Month: w.Month.AddDate(0, -1, 0)}
	if err := s.RecordUsage(ctx, idle.ID, old, 500, 500, false); err != nil {
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
