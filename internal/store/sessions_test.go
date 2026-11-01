package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func newSessionsProject(t *testing.T) (*store.Store, *store.Project) {
	t.Helper()
	s, p, _ := newRAGSettingsProject(t)
	return s, p
}

// Migrations 0029 and 0030 are recorded, the columns carry the defaults that
// reproduce the old behaviour, and the session index is valid and partial.
func TestToolsSessionsMigrationsApply(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)
	for _, v := range []int{29, 30} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded", v)
		}
	}
	if len(p.ToolAllow) != 0 || len(p.ToolDeny) != 0 || p.ToolAllow == nil || p.ToolDeny == nil || p.SessionTokenCap != 0 {
		t.Errorf("project defaults = allow %#v deny %#v cap %d, want empty non-nil lists and 0", p.ToolAllow, p.ToolDeny, p.SessionTokenCap)
	}
	var valid *bool
	var def string
	if err := s.DB().QueryRow(ctx, `SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('idx_request_logs_session')),
		(SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = to_regclass('idx_request_logs_session'))`).Scan(&valid, &def); err != nil {
		t.Fatal(err)
	}
	if valid == nil || !*valid {
		t.Errorf("idx_request_logs_session: valid = %v", valid)
	}
	if !strings.Contains(def, "session_id IS NOT NULL") {
		t.Errorf("idx_request_logs_session predicate = %q, want a partial index", def)
	}
}

func TestProjectToolSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)

	p.ToolAllow, p.ToolDeny, p.SessionTokenCap = []string{"search", "mcp__docs__*"}, []string{"mcp__github__*"}, 50000
	got, err := s.UpdateProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ToolAllow, []string{"search", "mcp__docs__*"}) || !reflect.DeepEqual(got.ToolDeny, []string{"mcp__github__*"}) || got.SessionTokenCap != 50000 {
		t.Errorf("round trip = %+v", got)
	}

	// A Project built by hand leaves the lists unset (nil): they are kept.
	kept, err := s.UpdateProject(ctx, &store.Project{ID: p.ID, Name: p.Name, ModelConnectionID: p.ModelConnectionID, SessionTokenCap: 50000})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.ToolAllow) != 2 || len(kept.ToolDeny) != 1 {
		t.Errorf("nil lists did not keep the stored ones: allow %v deny %v", kept.ToolAllow, kept.ToolDeny)
	}

	// An empty non-nil list clears.
	cleared, err := s.UpdateProject(ctx, &store.Project{ID: p.ID, Name: p.Name, ModelConnectionID: p.ModelConnectionID,
		ToolAllow: []string{}, ToolDeny: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ToolAllow == nil || len(cleared.ToolAllow) != 0 || len(cleared.ToolDeny) != 0 || cleared.SessionTokenCap != 0 {
		t.Errorf("cleared = %+v", cleared)
	}

	// Create with lists.
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c2", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	made, _, err := s.CreateProject(ctx, &store.Project{Name: "with-lists", ModelConnectionID: conn.ID,
		ToolDeny: []string{"shell"}, SessionTokenCap: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(made.ToolDeny, []string{"shell"}) || made.ToolAllow == nil || made.SessionTokenCap != 7 {
		t.Errorf("created = %+v", made)
	}
}

func TestSessionUsageCounts(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)

	u, err := s.GetSessionUsage(ctx, p.ID, "run-1")
	if err != nil || u.Tokens != 0 || u.Requests != 0 {
		t.Fatalf("unknown session = %+v (%v), want zeros", u, err)
	}
	if _, err := s.GetSession(ctx, p.ID, "run-1"); err == nil {
		t.Error("GetSession of an unknown session must be ErrNotFound")
	}

	for _, c := range [][2]int{{10, 2}, {20, 3}, {-5, -1}} {
		if err := s.RecordSessionUsage(ctx, p.ID, "run-1", c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordSessionUsage(ctx, p.ID, "run-2", 1, 1); err != nil {
		t.Fatal(err)
	}
	u, err = s.GetSessionUsage(ctx, p.ID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	// The negative counts floor at zero: a total never goes down.
	if u.Requests != 3 || u.PromptTokens != 30 || u.CompletionTokens != 5 || u.Tokens != 35 {
		t.Errorf("run-1 = %+v", u)
	}

	// Another project's session of the same name is another session.
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c3", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateProject(ctx, &store.Project{Name: "other", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSessionUsage(ctx, other.ID, "run-1", 100, 100); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetSessionUsage(ctx, p.ID, "run-1"); u.Tokens != 35 {
		t.Errorf("run-1 of the first project = %d tokens, want 35", u.Tokens)
	}

	// Concurrent writers do not lose counts.
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() { done <- s.RecordSessionUsage(ctx, p.ID, "run-3", 1, 0) }()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if u, _ := s.GetSessionUsage(ctx, p.ID, "run-3"); u.Requests != 20 || u.PromptTokens != 20 {
		t.Errorf("run-3 = %+v, want 20 requests and 20 prompt tokens", u)
	}
}

func TestListSessionsOrdersByLastSeen(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.RecordSessionUsage(ctx, p.ID, id, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(ctx, `UPDATE session_usage SET last_seen = now() - interval '2 hours' WHERE session_id = 'c'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(ctx, `UPDATE session_usage SET last_seen = now() - interval '1 hour' WHERE session_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, p.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range list {
		ids = append(ids, u.SessionID)
	}
	if strings.Join(ids, ",") != "b,a,c" {
		t.Errorf("order = %v, want b,a,c (most recently active first)", ids)
	}
	page, err := s.ListSessions(ctx, p.ID, 1, 1)
	if err != nil || len(page) != 1 || page[0].SessionID != "a" {
		t.Errorf("page = %+v (%v), want the second session, a", page, err)
	}
}

func TestRequestLogSessionAndToolColumns(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)
	if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "with",
		SessionID: "run-1", ToolNames: []string{"b", "a", "b"}, ToolsDenied: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "without"}); err != nil {
		t.Fatal(err)
	}
	var sid *string
	var names []string
	var denied int
	if err := s.DB().QueryRow(ctx, `SELECT session_id, tool_names, tools_denied FROM request_logs WHERE request_id = 'with'`).Scan(&sid, &names, &denied); err != nil {
		t.Fatal(err)
	}
	if sid == nil || *sid != "run-1" || !reflect.DeepEqual(names, []string{"a", "b"}) || denied != 2 {
		t.Errorf("stored = %v %v %d", sid, names, denied)
	}
	if err := s.DB().QueryRow(ctx, `SELECT session_id, tool_names, tools_denied FROM request_logs WHERE request_id = 'without'`).Scan(&sid, &names, &denied); err != nil {
		t.Fatal(err)
	}
	if sid != nil || names != nil || denied != 0 {
		t.Errorf("a row without them stores NULL, NULL and 0; got %v %v %d", sid, names, denied)
	}

	rows, err := s.SessionRequests(ctx, p.ID, "run-1", 0)
	if err != nil || len(rows) != 1 || rows[0].RequestID != "with" || rows[0].SessionID != "run-1" ||
		!reflect.DeepEqual(rows[0].ToolNames, []string{"a", "b"}) || rows[0].ToolsDenied != 2 {
		t.Errorf("session rows = %+v (%v)", rows, err)
	}
	if rows, _ := s.SessionRequests(ctx, p.ID, "nope", 0); len(rows) != 0 {
		t.Errorf("unknown session has %d rows", len(rows))
	}
	recent, err := s.RecentRequests(ctx, store.MetricsFilter{ProjectID: &p.ID}, 10)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent = %d (%v)", len(recent), err)
	}
}

// Session totals follow the request log retention window.
func TestSessionUsageRetention(t *testing.T) {
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
	for _, id := range []string{"old", "fresh"} {
		if err := s.RecordSessionUsage(ctx, p.ID, id, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(ctx, `UPDATE session_usage SET last_seen = now() - interval '100 days' WHERE session_id = 'old'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredBatch(ctx, store.RetainSessionUsage, time.Now().AddDate(0, 0, -90), 10)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredBatch = (%d, %v), want (1, nil)", n, err)
	}
	if u, _ := s.GetSessionUsage(ctx, p.ID, "fresh"); u.Requests != 1 {
		t.Error("the fresh session was removed")
	}
	if got := store.RetainSessionUsage.String(); got != "session_usage" {
		t.Errorf("String() = %q", got)
	}
	// A deleted project takes its sessions along.
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM session_usage`).Scan(&left); err != nil || left != 0 {
		t.Errorf("session_usage rows after project delete = %d (%v)", left, err)
	}
}

func TestSessionRequestsLimitIsCappedNotReset(t *testing.T) {
	ctx := context.Background()
	s, p := newSessionsProject(t)
	for i := 0; i < store.DefaultSessionRequests+5; i++ {
		if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, SessionID: "run-1"}); err != nil {
			t.Fatal(err)
		}
	}
	for limit, want := range map[int]int{0: store.DefaultSessionRequests, -3: store.DefaultSessionRequests, 7: 7, 5000: store.DefaultSessionRequests + 5} {
		rows, err := s.SessionRequests(ctx, p.ID, "run-1", limit)
		if err != nil || len(rows) != want {
			t.Errorf("limit %d: %d rows (%v), want %d", limit, len(rows), err, want)
		}
	}
}
