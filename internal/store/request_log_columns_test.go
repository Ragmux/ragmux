package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// TestRequestedModelIsBounded checks that the client's model string cannot
// make a request log row arbitrarily large, and that a NUL byte in it does
// not cost the row.
func TestRequestedModelIsBounded(t *testing.T) {
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

	for id, model := range map[string]string{
		"long":  strings.Repeat("m", 1<<20),
		"multi": strings.Repeat("é", 1000),
		"nul":   "gpt\x00-4o",
		"plain": "gpt-4o",
		"empty": "",
	} {
		if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200,
			RequestID: id, RequestedModel: model}); err != nil {
			t.Fatalf("%s: insert: %v", id, err)
		}
	}

	stored := func(id string) (string, bool) {
		var v *string
		if err := s.DB().QueryRow(ctx, "SELECT requested_model FROM request_logs WHERE request_id = $1", id).Scan(&v); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if v == nil {
			return "", false
		}
		return *v, true
	}
	if v, _ := stored("long"); len(v) != store.MaxRequestedModelBytes || strings.Trim(v, "m") != "" {
		t.Errorf("long model stored with %d bytes, want %d", len(v), store.MaxRequestedModelBytes)
	}
	if v, _ := stored("multi"); len(v) > store.MaxRequestedModelBytes || strings.Trim(v, "é") != "" {
		t.Errorf("multi-byte model stored with %d bytes or a split character", len(v))
	}
	if v, _ := stored("nul"); v != "gpt-4o" {
		t.Errorf("model with a NUL stored as %q, want %q", v, "gpt-4o")
	}
	if v, _ := stored("plain"); v != "gpt-4o" {
		t.Errorf("plain model stored as %q", v)
	}
	if _, present := stored("empty"); present {
		t.Error("an empty model should be stored as NULL")
	}
}

// TestGuardrailColumns checks migration 0032 end to end: a row that says
// nothing about guardrails reads as "none triggered", the counters and the
// rule ids round-trip, and the rule list is bounded and de-duplicated.
func TestGuardrailColumns(t *testing.T) {
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
	many := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, "rule-"+strings.Repeat("x", i%3)+string(rune('a'+i%26)))
	}
	many = append(many, "dup", "dup", "nul\x00byte")
	for _, l := range []*store.RequestLog{
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "plain"},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 400, RequestID: "blocked", GuardrailBlockedStage: "pre_request",
			GuardrailRules: []string{"tckn", "tckn"}},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "counted", GuardrailRedactions: 3, GuardrailFlags: 2,
			GuardrailRules: []string{"iban", "words", "webhook:pii"}, GuardrailWebhook: "ok"},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "many", GuardrailRules: many, GuardrailRedactions: -4},
	} {
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatalf("%s: %v", l.RequestID, err)
		}
	}
	type row struct {
		stage    *string
		redacted int
		flags    int
		rules    []string
		webhook  *string
	}
	read := func(id string) row {
		var r row
		if err := s.DB().QueryRow(ctx, `SELECT guardrail_blocked_stage, guardrail_redactions, guardrail_flags,
			guardrail_rules, guardrail_webhook FROM request_logs WHERE request_id = $1`, id).
			Scan(&r.stage, &r.redacted, &r.flags, &r.rules, &r.webhook); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return r
	}
	if r := read("plain"); r.stage != nil || r.redacted != 0 || r.flags != 0 || r.rules != nil || r.webhook != nil {
		t.Errorf("a row without guardrails: %+v", r)
	}
	if r := read("blocked"); r.stage == nil || *r.stage != "pre_request" || strings.Join(r.rules, ",") != "tckn" {
		t.Errorf("blocked row: %+v", r)
	}
	if r := read("counted"); r.redacted != 3 || r.flags != 2 || strings.Join(r.rules, ",") != "iban,words,webhook:pii" ||
		r.webhook == nil || *r.webhook != "ok" {
		t.Errorf("counted row: %+v", r)
	}
	r := read("many")
	if len(r.rules) != store.MaxGuardrailRules {
		t.Errorf("rule list has %d entries, want the cap %d", len(r.rules), store.MaxGuardrailRules)
	}
	if r.redacted != 0 {
		t.Errorf("a negative count must be stored as 0, got %d", r.redacted)
	}
	seen := map[string]bool{}
	for _, id := range r.rules {
		if seen[id] || strings.ContainsRune(id, 0) {
			t.Errorf("rule list has a duplicate or a NUL byte: %q", r.rules)
		}
		seen[id] = true
	}
}

// TestProjectGuardrails: the configuration is stored as given, survives a
// project update that does not mention it, and can be cleared.
func TestProjectGuardrails(t *testing.T) {
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
	if p.Guardrails != nil {
		t.Fatalf("a new project has guardrails: %s", p.Guardrails)
	}
	cfg := json.RawMessage(`{"rules":[{"id":"t","type":"tckn","action":"redact","stages":["pre_request"]}],"stream_window_chars":64}`)
	if err := s.SetProjectGuardrails(ctx, p.ID, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	if json.Unmarshal(got.Guardrails, &a) != nil || json.Unmarshal(cfg, &b) != nil || len(a) != 2 || a["stream_window_chars"] != b["stream_window_chars"] {
		t.Errorf("stored %s, want %s", got.Guardrails, cfg)
	}
	// An ordinary project update leaves the column alone.
	got.Name = "renamed"
	got.Guardrails = nil
	if _, err := s.UpdateProject(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetProject(ctx, p.ID); again.Name != "renamed" || again.Guardrails == nil {
		t.Errorf("project update lost the guardrails: %+v", again)
	}
	if err := s.SetProjectGuardrails(ctx, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := s.GetProject(ctx, p.ID); cleared.Guardrails != nil {
		t.Errorf("cleared guardrails read back as %s", cleared.Guardrails)
	}
	if err := s.SetProjectGuardrails(ctx, p.ID+999, cfg); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
}

// TestEndUserAndTagsColumns checks migration 0033 end to end: a row that names
// no end user and no tags reads as such (NULL), the values round-trip through
// the admin reads and the CSV export, and the tag list is bounded, sorted and
// de-duplicated.
func TestEndUserAndTagsColumns(t *testing.T) {
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
	many := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, "tag-"+string(rune('z'-i)))
	}
	for _, l := range []*store.RequestLog{
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "plain"},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "named", EndUser: "ali@firma.com", Tags: []string{"b", "a", "b"}},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "empty", EndUser: "", Tags: []string{"", "\x00"}},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "long", EndUser: strings.Repeat("u", 500) + "\x00",
			Tags: append([]string{strings.Repeat("t", 200)}, many...)},
	} {
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatalf("%s: %v", l.RequestID, err)
		}
	}
	read := func(id string) (user *string, tags []string) {
		if err := s.DB().QueryRow(ctx, `SELECT end_user, tags FROM request_logs WHERE request_id = $1`, id).Scan(&user, &tags); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return user, tags
	}
	if u, tg := read("plain"); u != nil || tg != nil {
		t.Errorf("a row without an end user: %v %v, want NULLs", u, tg)
	}
	if u, tg := read("named"); u == nil || *u != "ali@firma.com" || strings.Join(tg, ",") != "a,b" {
		t.Errorf("named row: %v %v", u, tg)
	}
	if u, tg := read("empty"); u != nil || tg != nil {
		t.Errorf("empty values must be stored as NULL: %v %v", u, tg)
	}
	u, tg := read("long")
	if u == nil || len(*u) != store.MaxEndUserBytes || strings.ContainsRune(*u, 0) {
		t.Errorf("long end user: %v", u)
	}
	if len(tg) != store.MaxTags {
		t.Errorf("long tag list has %d entries, want the cap %d", len(tg), store.MaxTags)
	}
	for i, tag := range tg {
		if len(tag) > store.MaxTagBytes || (i > 0 && tg[i-1] >= tag) {
			t.Errorf("tags not bounded, sorted and distinct: %q", tg)
			break
		}
	}

	// The admin reads carry both.
	rows, err := s.RecentRequests(ctx, store.MetricsFilter{ProjectID: &p.ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.RequestID == "named" {
			found = true
			if r.EndUser != "ali@firma.com" || strings.Join(r.Tags, ",") != "a,b" {
				t.Errorf("RecentRequests: %q %v", r.EndUser, r.Tags)
			}
		}
		if r.RequestID == "plain" && (r.EndUser != "" || len(r.Tags) != 0) {
			t.Errorf("RecentRequests plain: %q %v", r.EndUser, r.Tags)
		}
	}
	if !found {
		t.Error("RecentRequests did not return the named row")
	}
	// ... and so does the export the CSV is written from.
	var exported []*store.RequestExportRow
	if err := s.ExportRequests(ctx, store.MetricsFilter{ProjectID: &p.ID}, time.Now().Add(-time.Hour), func(r *store.RequestExportRow) error {
		exported = append(exported, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var seenNamed bool
	for _, r := range exported {
		if r.EndUser == "ali@firma.com" {
			seenNamed = strings.Join(r.Tags, ",") == "a,b"
		}
	}
	if len(exported) != 4 || !seenNamed {
		t.Errorf("export returned %d rows, named row with end user and tags: %v", len(exported), seenNamed)
	}
}

// TestProjectEndUserCeilingsRoundTrip: the four ceilings are stored as given
// and are not lost by a read-modify-write of the project.
func TestProjectEndUserCeilingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID,
		EndUserRPM: 5, EndUserTPM: 6, EndUserDailyTokens: 7, EndUserMonthlyTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if p.EndUserRPM != 5 || p.EndUserTPM != 6 || p.EndUserDailyTokens != 7 || p.EndUserMonthlyTokens != 8 {
		t.Fatalf("created = %+v", p)
	}
	p.Name, p.EndUserDailyTokens = "renamed", 70
	got, err := s.UpdateProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.EndUserRPM != 5 || got.EndUserTPM != 6 || got.EndUserDailyTokens != 70 || got.EndUserMonthlyTokens != 8 {
		t.Errorf("updated = %+v", got)
	}
}

// TestEndUserUsageCounters: the counters accumulate per (project, end user,
// window), reserve and release a minute slot, clamp negative figures, are
// purged by window and go with their project.
func TestEndUserUsageCounters(t *testing.T) {
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
	now := time.Date(2026, 11, 4, 10, 30, 20, 0, time.UTC)
	w := store.UsageWindows{Minute: now.Truncate(time.Minute),
		Day: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), Month: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

	minute, day, month, err := s.GetEndUserUsage(ctx, p.ID, "alice", w)
	if err != nil || minute.Requests != 0 || day.Tokens() != 0 || month.Tokens() != 0 || !day.PeriodStart.Equal(w.Day) {
		t.Fatalf("empty read = %+v %+v %+v (%v)", minute, day, month, err)
	}
	if n, err := s.ReserveEndUserMinuteRequest(ctx, p.ID, "alice", w.Minute); err != nil || n != 1 {
		t.Fatalf("reserve = %d (%v)", n, err)
	}
	if n, err := s.ReserveEndUserMinuteRequest(ctx, p.ID, "alice", w.Minute); err != nil || n != 2 {
		t.Fatalf("reserve = %d (%v)", n, err)
	}
	if err := s.ReleaseEndUserMinuteRequest(ctx, p.ID, "alice", w.Minute); err != nil {
		t.Fatal(err)
	}
	// One reserved slot is not counted again; the day and month rows count it.
	if err := s.RecordEndUserUsage(ctx, p.ID, "alice", w, 10, 5, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEndUserUsage(ctx, p.ID, "alice", w, -3, -4, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEndUserUsage(ctx, p.ID, "bob", w, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	minute, day, month, err = s.GetEndUserUsage(ctx, p.ID, "alice", w)
	if err != nil {
		t.Fatal(err)
	}
	if minute.Requests != 2 || minute.Tokens() != 15 || day.Requests != 2 || day.PromptTokens != 10 || day.CompletionTokens != 5 || month.Requests != 2 {
		t.Errorf("alice = %+v %+v %+v", minute, day, month)
	}
	if _, bobDay, _, err := s.GetEndUserUsage(ctx, p.ID, "bob", w); err != nil || bobDay.Tokens() != 2 {
		t.Errorf("bob = %+v (%v)", bobDay, err)
	}

	// Retention deletes by window.
	n, err := s.DeleteEndUserUsageBefore(ctx, store.PeriodMinute, w.Minute.Add(time.Hour))
	if err != nil || n != 2 {
		t.Errorf("purged %d minute rows (%v), want alice's and bob's", n, err)
	}
	if n, err := s.DeleteEndUserUsageBefore(ctx, store.PeriodDay, w.Day); err != nil || n != 0 {
		t.Errorf("purged %d day rows (%v) that no cutoff reaches", n, err)
	}
	// A project takes its counters with it.
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM end_user_usage`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d end_user_usage rows outlived the project (%v)", left, err)
	}
}
