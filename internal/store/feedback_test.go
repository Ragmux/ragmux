package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

func logRequest(t *testing.T, s *store.Store, projectID int64, requestID string) {
	t.Helper()
	if err := s.InsertRequestLog(context.Background(), &store.RequestLog{ProjectID: projectID, ModelName: "m",
		StatusCode: 200, RequestID: requestID}); err != nil {
		t.Fatal(err)
	}
}

func str(s string) *string { return &s }

func feedbackCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(context.Background(), "SELECT count(*) FROM request_feedback").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Migration 0040 is recorded and the table has the shape ADR-042 gives it: the
// pair as the key, a score that can only be 1 or -1, and the index the summary
// reads. There is no migration 0041.
func TestRequestFeedbackMigration(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	if !migrationRecorded(t, s, 40) {
		t.Error("migration 40 is not recorded")
	}
	if migrationRecorded(t, s, 41) {
		t.Error("migration 41 is recorded; the feedback table needs none")
	}
	var pk string
	if err := s.DB().QueryRow(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM pg_index i JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = to_regclass('request_feedback') AND i.indisprimary`).Scan(&pk); err != nil || pk != "project_id,request_id" {
		t.Errorf("primary key = %q (%v), want project_id,request_id", pk, err)
	}
	var idx int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM (SELECT i.indexrelid FROM pg_index i
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = to_regclass('request_feedback') AND NOT i.indisprimary
		GROUP BY i.indexrelid HAVING string_agg(a.attname, ',' ORDER BY k.ord) = 'project_id,created_at') x`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("(project_id, created_at) index count = %d (%v), want 1", idx, err)
	}
	// Retention walks the table by created_at, across projects.
	var createdIdx int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		WHERE i.indrelid = to_regclass('request_feedback') AND i.indnatts = 1 AND a.attname = 'created_at'`).Scan(&createdIdx); err != nil || createdIdx != 1 {
		t.Errorf("created_at index count = %d (%v), want 1", createdIdx, err)
	}
	// The api_key_id foreign key is indexed (partial), so deleting a key does
	// not scan the table.
	var keyIdx int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		WHERE i.indrelid = to_regclass('request_feedback') AND a.attname = 'api_key_id' AND i.indpred IS NOT NULL`).Scan(&keyIdx); err != nil || keyIdx != 1 {
		t.Errorf("partial api_key_id index count = %d (%v), want 1", keyIdx, err)
	}
	logRequest(t, s, p.ID, "r")
	for _, bad := range []int{0, 2, -2} {
		if _, err := s.DB().Exec(ctx, "INSERT INTO request_feedback (project_id, request_id, score) VALUES ($1, 'r', $2)", p.ID, bad); err == nil {
			t.Errorf("score %d was accepted by the table", bad)
		}
	}
}

func TestUpsertFeedback(t *testing.T) {
	ctx := context.Background()
	s, p, conn := newRAGSettingsProject(t)
	other, _, err := s.CreateProject(ctx, &store.Project{Name: "other", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	logRequest(t, s, p.ID, "r1")
	logRequest(t, s, other.ID, "theirs")

	// A request the project has no row for, and another project's, are not found.
	for _, id := range []string{"unknown", "theirs"} {
		if err := s.UpsertFeedback(ctx, p.ID, id, 1, nil, nil); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", id, err)
		}
	}
	if n := feedbackCount(t, s); n != 0 {
		t.Fatalf("%d rows after refused writes", n)
	}

	if err := s.UpsertFeedback(ctx, p.ID, "r1", 0, nil, nil); !errors.Is(err, store.ErrInvalidFeedbackScore) {
		t.Errorf("score 0: err = %v, want ErrInvalidFeedbackScore", err)
	}
	if err := s.UpsertFeedback(ctx, p.ID, "r1", 1, str("helpful"), nil); err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err := s.DB().QueryRow(ctx, "SELECT created_at FROM request_feedback").Scan(&created); err != nil {
		t.Fatal(err)
	}
	// A second row of the same request_id does not make a second feedback, and
	// the last write replaces score, label and key but keeps created_at.
	logRequest(t, s, p.ID, "r1")
	if err := s.UpsertFeedback(ctx, p.ID, "r1", -1, nil, nil); err != nil {
		t.Fatal(err)
	}
	var score int16
	var label *string
	var created2, updated time.Time
	if err := s.DB().QueryRow(ctx, "SELECT score, label, created_at, updated_at FROM request_feedback").
		Scan(&score, &label, &created2, &updated); err != nil {
		t.Fatal(err)
	}
	if n := feedbackCount(t, s); n != 1 || score != -1 || label != nil || !created2.Equal(created) || !updated.After(created) {
		t.Errorf("rows %d score %d label %v created %v/%v updated %v", n, score, label, created, created2, updated)
	}
}

// Deleting the project takes its feedback with it.
func TestFeedbackGoesWithItsProject(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	logRequest(t, s, p.ID, "r1")
	if err := s.UpsertFeedback(ctx, p.ID, "r1", 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if n := feedbackCount(t, s); n != 0 {
		t.Errorf("%d feedback rows left after the project was deleted", n)
	}
}

func TestFeedbackSummary(t *testing.T) {
	ctx := context.Background()
	s, p, conn := newRAGSettingsProject(t)
	other, _, err := s.CreateProject(ctx, &store.Project{Name: "other", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}

	empty, err := s.FeedbackSummary(ctx, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Days != 30 || empty.Total != 0 || empty.Ratio != nil || empty.Labels == nil || empty.Daily == nil ||
		len(empty.Labels) != 0 || len(empty.Daily) != 0 {
		t.Errorf("empty = %+v, want zeros, a nil ratio and empty (non-nil) lists", empty)
	}

	rate := func(pid int64, id string, score int, label *string) {
		t.Helper()
		logRequest(t, s, pid, id)
		if err := s.UpsertFeedback(ctx, pid, id, score, label, nil); err != nil {
			t.Fatal(err)
		}
	}
	rate(p.ID, "a", 1, str("helpful"))
	rate(p.ID, "b", 1, str("helpful"))
	rate(p.ID, "c", -1, str("helpful"))
	rate(p.ID, "d", -1, str("wrong"))
	rate(p.ID, "e", 1, nil)
	rate(p.ID, "old", -1, str("wrong"))
	rate(other.ID, "x", -1, str("noise"))
	// "old" was first received 40 days ago, outside the default window.
	if _, err := s.DB().Exec(ctx, "UPDATE request_feedback SET created_at = now() - interval '40 days' WHERE request_id = 'old'"); err != nil {
		t.Fatal(err)
	}

	got, err := s.FeedbackSummary(ctx, p.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Positive != 3 || got.Negative != 2 || got.Total != 5 || got.Ratio == nil || *got.Ratio != 0.6 {
		t.Errorf("totals = %+v ratio %v", got, got.Ratio)
	}
	if len(got.Labels) != 2 || got.Labels[0] != (store.FeedbackLabelCount{Label: "helpful", Positive: 2, Negative: 1}) ||
		got.Labels[1] != (store.FeedbackLabelCount{Label: "wrong", Positive: 0, Negative: 1}) {
		t.Errorf("labels = %+v, want helpful then wrong, the unlabelled one only in the totals", got.Labels)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if len(got.Daily) != 1 || got.Daily[0] != (store.FeedbackDay{Day: today, Positive: 3, Negative: 2}) {
		t.Errorf("daily = %+v", got.Daily)
	}

	// A longer window takes the older one in; the cap is MaxFeedbackDays.
	long, err := s.FeedbackSummary(ctx, p.ID, 60)
	if err != nil || long.Days != 60 || long.Total != 6 || len(long.Daily) != 2 {
		t.Errorf("60 days = %+v (%v)", long, err)
	}
	if capped, err := s.FeedbackSummary(ctx, p.ID, 100000); err != nil || capped.Days != store.MaxFeedbackDays {
		t.Errorf("cap: %+v (%v)", capped, err)
	}
	// Another project's feedback is not counted.
	if o, err := s.FeedbackSummary(ctx, other.ID, 30); err != nil || o.Total != 1 || o.Negative != 1 {
		t.Errorf("other = %+v (%v)", o, err)
	}
}

// The label breakdown is bounded however many labels clients invent.
func TestFeedbackSummaryBoundsLabels(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	if _, err := s.DB().Exec(ctx, `INSERT INTO request_logs (project_id, model_name, status_code, request_id)
		SELECT $1, 'm', 200, 'r' || g FROM generate_series(1, 80) g`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(ctx, `INSERT INTO request_feedback (project_id, request_id, score, label)
		SELECT $1, 'r' || g, 1, 'label' || g FROM generate_series(1, 80) g`, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.FeedbackSummary(ctx, p.ID, 30)
	if err != nil || got.Total != 80 || len(got.Labels) != store.MaxFeedbackLabels {
		t.Errorf("total %d labels %d (%v), want 80 and %d", got.Total, len(got.Labels), err, store.MaxFeedbackLabels)
	}
}

// Retention trims feedback by created_at, in batches like the other tables.
func TestRetentionDeletesOldFeedback(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	for _, id := range []string{"old", "fresh"} {
		logRequest(t, s, p.ID, id)
		if err := s.UpsertFeedback(ctx, p.ID, id, 1, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(ctx, "UPDATE request_feedback SET created_at = now() - interval '100 days' WHERE request_id = 'old'"); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredBatch(ctx, store.RetainRequestFeedback, time.Now().AddDate(0, 0, -90), 10)
	if err != nil || n != 1 || feedbackCount(t, s) != 1 {
		t.Errorf("deleted %d (%v), %d rows left, want 1 and 1", n, err, feedbackCount(t, s))
	}
	if store.RetainRequestFeedback.String() != "request_feedback" {
		t.Errorf("String() = %q", store.RetainRequestFeedback.String())
	}
}
