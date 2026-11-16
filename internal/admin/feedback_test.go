package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

// GET /admin/api/projects/{id}/feedback/summary (ADR-042).
func TestFeedbackSummaryEndpoint(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	rate := func(pid int64, id string, score int, label *string) {
		t.Helper()
		if err := e.st.InsertRequestLog(ctx, &store.RequestLog{ProjectID: pid, ModelName: "m", StatusCode: 200, RequestID: id}); err != nil {
			t.Fatal(err)
		}
		if err := e.st.UpsertFeedback(ctx, pid, id, score, label, nil); err != nil {
			t.Fatal(err)
		}
	}
	helpful, wrong := "helpful", "wrong_answer"
	rate(e.proj.ID, "a", 1, &helpful)
	rate(e.proj.ID, "b", 1, &helpful)
	rate(e.proj.ID, "c", 1, nil)
	rate(e.proj.ID, "d", -1, &wrong)
	rate(e.other.ID, "x", -1, &wrong)

	viewer := e.user("viewer", "viewer", e.proj.ID)
	admin := e.user("root", "admin")
	outsider := e.user("outsider", "viewer", e.other.ID)
	pid := itoa(e.proj.ID)
	path := "/api/projects/" + pid + "/feedback/summary"

	// A member reads the counts, the ratio, the labels and the daily series.
	status, out := e.do(http.MethodGet, path+"?days=7", nil, viewer.token)
	if status != http.StatusOK || out["days"] != float64(7) || out["positive"] != float64(3) || out["negative"] != float64(1) ||
		out["total"] != float64(4) || out["ratio"] != 0.75 {
		t.Fatalf("summary: %d %v", status, out)
	}
	labels, _ := out["labels"].([]any)
	if len(labels) != 2 {
		t.Fatalf("labels = %v", out["labels"])
	}
	first, _ := labels[0].(map[string]any)
	if first["label"] != "helpful" || first["positive"] != float64(2) || first["negative"] != float64(0) {
		t.Errorf("first label = %v", first)
	}
	if daily, _ := out["daily"].([]any); len(daily) != 1 {
		t.Errorf("daily = %v", out["daily"])
	}
	// The default window is 30 days.
	if _, out := e.do(http.MethodGet, path, nil, viewer.token); out["days"] != float64(30) {
		t.Errorf("default days = %v", out["days"])
	}

	// Admins read any project; a non-member and an anonymous caller do not.
	if status, _ := e.do(http.MethodGet, path, nil, admin.token); status != http.StatusOK {
		t.Errorf("admin: %d", status)
	}
	if status, _ := e.do(http.MethodGet, path, nil, outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member: %d, want 404", status)
	}
	if status, _ := e.do(http.MethodGet, path, nil, "not-a-token"); status != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", status)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects/999999/feedback/summary", nil, admin.token); status != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", status)
	}
	// Nothing to write here: the summary is read-only.
	if status, _ := e.do(http.MethodPost, path, map[string]any{}, admin.token); status == http.StatusOK {
		t.Errorf("POST answered %d", status)
	}

	// A management key needs the read scope (the read group).
	mint := func(name string, scopes ...string) string {
		t.Helper()
		_, raw, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: name, UserID: viewer.ID, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if status, _ := e.do(http.MethodGet, path, nil, mint("reader", store.ScopeRead)); status != http.StatusOK {
		t.Errorf("read key: %d", status)
	}
	if status, _ := e.do(http.MethodGet, path, nil, mint("keys-only", store.ScopeKeys)); status != http.StatusForbidden {
		t.Errorf("key without the read scope: %d, want 403", status)
	}

	// Another project's summary counts that project's own feedback only.
	_, empty := e.do(http.MethodGet, "/api/projects/"+itoa(e.other.ID)+"/feedback/summary?days=1", nil, outsider.token)
	if empty["total"] != float64(1) {
		t.Errorf("other project = %v", empty)
	}
	// No feedback is a zero summary with a null ratio, not an error.
	solo := e.user("solo", "viewer", e.proj.ID)
	if _, err := e.st.DB().Exec(ctx, "DELETE FROM request_feedback WHERE project_id = $1", e.proj.ID); err != nil {
		t.Fatal(err)
	}
	status, none := e.do(http.MethodGet, path, nil, solo.token)
	if status != http.StatusOK || none["total"] != float64(0) || none["ratio"] != nil {
		t.Errorf("empty: %d %v", status, none)
	}
}
