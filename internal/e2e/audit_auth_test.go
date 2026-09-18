package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/testdb"
)

// TestAuditAndAuthHardening covers the v0.3 audit and account surface: audit
// filters, totals and the NDJSON export, the login security summary, the
// attempt counters in login failures, session details in /me, user
// statistics and project memberships on create, the unauthenticated setup
// facts, the admin-only database versions and case-insensitive usernames.
func TestAuditAndAuthHardening(t *testing.T) {
	e := newEnvWith(t, testdb.Config(t), true, func(a *admin.Admin) {
		// A small lockout so the test reaches it without waiting a minute.
		a.Limiter = &auth.LoginLimiter{Store: a.Store, PerIP: 100, PerUser: 5, LockoutFailures: 3, LockoutWindow: 15 * time.Minute}
	})
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	errOf := func(r map[string]any) map[string]any { m, _ := r["error"].(map[string]any); return m }

	// Setup facts are readable without a session and do not name users.
	setup := e.call("GET", "/admin/api/setup", nil, "x")
	if setup["needs_setup"] != false || setup["migrations_version"].(float64) < 8 || setup["secret_key_source"] != "env" || setup["database_role"] == "" {
		t.Errorf("setup status: %v", setup)
	}
	for _, k := range []string{"users", "username", "postgres_version"} {
		if _, ok := setup[k]; ok {
			t.Errorf("setup status leaks %s: %v", k, setup)
		}
	}

	// /me describes the session: bearer here, cookie below.
	me := e.call("GET", "/admin/api/me", nil, "")
	exp, err := time.Parse(time.RFC3339, fmt.Sprint(me["session_expires_at"]))
	if err != nil || exp.Before(time.Now().Add(50*time.Minute)) || me["session_bearer"] != true {
		t.Errorf("/me session fields: %v (%v)", me, err)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/api/login", strings.NewReader(`{"username":"ADMIN","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var loginBody map[string]any
	json.NewDecoder(resp.Body).Decode(&loginBody)
	resp.Body.Close()
	if resp.StatusCode != 200 || loginBody["user"].(map[string]any)["username"] != "admin" {
		t.Fatalf("login with a different casing should succeed and keep the stored casing: %d %v", resp.StatusCode, loginBody)
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/admin/api/me", nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cookieMe map[string]any
	json.NewDecoder(resp.Body).Decode(&cookieMe)
	resp.Body.Close()
	if cookieMe["session_bearer"] != false || cookieMe["session_expires_at"] == nil {
		t.Errorf("cookie /me: %v", cookieMe)
	}

	// Users: memberships on create, statistics in the listing, 422 for an
	// unknown project and 409 for a name that differs only by case.
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "m", "provider_type": "ollama", "model_name": "x"}, "")
	connID := int64(conn["id"].(float64))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "P", "model_connection_id": connID}, "")
	projID := int64(proj["project"].(map[string]any)["id"].(float64))
	if r := e.call("POST", "/admin/api/users", map[string]any{"username": "viewer", "password": "viewerpass", "project_ids": []int64{projID, 99999}}, ""); status(r) != 422 {
		t.Errorf("unknown project id: %v", r)
	}
	if r := e.call("GET", "/admin/api/users", nil, ""); len(r["_list"].([]any)) != 1 {
		t.Errorf("failed create must not leave a user behind: %v", r)
	}
	vw := e.call("POST", "/admin/api/users", map[string]any{"username": "viewer", "password": "viewerpass", "project_ids": []int64{projID}}, "")
	if status(vw) != 201 {
		t.Fatalf("create viewer with projects: %v", vw)
	}
	if r := e.call("POST", "/admin/api/users", map[string]any{"username": "Viewer", "password": "viewerpass"}, ""); status(r) != 409 || !strings.Contains(fmt.Sprint(errOf(r)["message"]), "case-insensitive") {
		t.Errorf("case-insensitive duplicate: %v", r)
	}
	if r := e.call("GET", fmt.Sprintf("/admin/api/projects/%d/members", projID), nil, ""); len(r["_list"].([]any)) != 2 {
		t.Errorf("project members after create: %v", r)
	}
	viewerTok := e.login("VIEWER", "viewerpass")
	e.login("viewer", "viewerpass")
	users := e.call("GET", "/admin/api/users", nil, "")["_list"].([]any)
	stats := map[string]map[string]any{}
	for _, u := range users {
		m := u.(map[string]any)
		stats[m["username"].(string)] = m
	}
	if stats["viewer"]["project_count"] != float64(1) || stats["viewer"]["active_sessions"] != float64(2) ||
		stats["admin"]["project_count"] != float64(1) || stats["admin"]["active_sessions"] != float64(2) {
		t.Errorf("user statistics: %v", stats)
	}
	if r := e.call("GET", "/admin/api/users/lite", nil, ""); len(r["_list"].([]any)) != 2 {
		t.Errorf("lite listing: %v", r)
	}

	// System: database versions are admin-only.
	sys := e.call("GET", "/admin/api/system", nil, "")
	if db := sys["database"].(map[string]any); db["postgres_version"] == nil || db["pgvector_version"] == nil {
		t.Errorf("admin system info: %v", sys)
	}
	sys = e.call("GET", "/admin/api/system", nil, viewerTok)
	db := sys["database"].(map[string]any)
	if _, ok := db["postgres_version"]; ok || db["pgvector_version"] != nil || db["migrations_version"] == nil || db["size_bytes"] == nil || sys["version"] == nil {
		t.Errorf("viewer system info: %v", sys)
	}

	// Failed logins: the same counters for a wrong password and an unknown
	// username, then the lockout with "locked": true.
	failLogin := func(username string) (map[string]any, http.Header) {
		t.Helper()
		return e.callRaw("POST", "/admin/api/login", map[string]any{"username": username, "password": "wrong"}, "x")
	}
	for i, want := range []float64{4, 3, 2} {
		known, _ := failLogin("viewer")
		unknown, _ := failLogin("ghost")
		if status(known) != 401 || status(unknown) != 401 {
			t.Fatalf("attempt %d: %v %v", i+1, known, unknown)
		}
		ek, eu := errOf(known), errOf(unknown)
		if ek["attempts_remaining"] != want || eu["attempts_remaining"] != want {
			t.Errorf("attempt %d: attempts_remaining known=%v unknown=%v, want %v", i+1, ek["attempts_remaining"], eu["attempts_remaining"], want)
		}
		delete(ek, "attempts_remaining")
		delete(eu, "attempts_remaining")
		if fmt.Sprint(ek) != fmt.Sprint(eu) {
			t.Errorf("attempt %d: bodies differ: %v vs %v", i+1, ek, eu)
		}
	}
	for _, username := range []string{"viewer", "ghost"} {
		r, hdr := failLogin(username)
		if status(r) != 429 || errOf(r)["locked"] != true || errOf(r)["type"] != "rate_limited" || hdr.Get("Retry-After") == "" {
			t.Errorf("lockout for %s: %v %v", username, r, hdr)
		}
	}

	// Login security summary: counts and the lockouts the limiter enforces.
	sec := e.call("GET", "/admin/api/security/logins", nil, "")
	if sec["failed_last_hour"] != float64(6) || sec["failed_last_24h"] != float64(6) {
		t.Errorf("failure counters: %v", sec)
	}
	locks := sec["active_lockouts"].([]any)
	if len(locks) != 2 {
		t.Fatalf("active lockouts: %v", locks)
	}
	for _, l := range locks {
		m := l.(map[string]any)
		until, err := time.Parse(time.RFC3339, fmt.Sprint(m["until"]))
		if err != nil || until.Before(time.Now().Add(14*time.Minute)) || m["ip"] != "127.0.0.1" || (m["username"] != "viewer" && m["username"] != "ghost") {
			t.Errorf("lockout entry: %v (%v)", m, err)
		}
	}
	if r := e.call("GET", "/admin/api/security/logins", nil, viewerTok); status(r) != 403 {
		t.Errorf("viewer security summary: %v", r)
	}

	// Audit: totals, has_more, the new filters and validation.
	page := e.call("GET", "/admin/api/audit?limit=2&action=login.", nil, "")
	if len(page["entries"].([]any)) != 2 || page["has_more"] != true || page["total"] != float64(12) {
		t.Errorf("audit page: entries=%d has_more=%v total=%v", len(page["entries"].([]any)), page["has_more"], page["total"])
	}
	adminID := int64(me["id"].(float64))
	byActor := e.call("GET", fmt.Sprintf("/admin/api/audit?actor_user_id=%d&action=user.", adminID), nil, "")
	if byActor["total"] != float64(1) || byActor["has_more"] != false {
		t.Errorf("audit by actor: %v", byActor)
	}
	if d := byActor["entries"].([]any)[0].(map[string]any)["details"].(map[string]any); d["project_ids"] == nil {
		t.Errorf("user.create details should carry project_ids: %v", d)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if r := e.call("GET", "/admin/api/audit?since="+future, nil, ""); r["total"] != float64(0) || len(r["entries"].([]any)) != 0 {
		t.Errorf("since in the future: %v", r)
	}
	if r := e.call("GET", "/admin/api/audit?until="+past, nil, ""); r["total"] != float64(0) {
		t.Errorf("until in the past: %v", r)
	}
	all := e.call("GET", "/admin/api/audit?since="+past+"&until="+future, nil, "")
	if all["total"].(float64) < 12 || all["has_more"] != false {
		t.Errorf("since/until window: %v", all)
	}
	for _, q := range []string{"since=yesterday", "until=2026-09-18", "actor_user_id=x"} {
		if r := e.call("GET", "/admin/api/audit?"+q, nil, ""); status(r) != 400 {
			t.Errorf("audit?%s should be 400: %v", q, r)
		}
	}
	if r := e.call("GET", "/admin/api/audit/export", nil, viewerTok); status(r) != 403 {
		t.Errorf("viewer export: %v", r)
	}

	// Export: NDJSON attachment with every matching row, audited.
	req, _ = http.NewRequest("GET", e.srv.URL+"/admin/api/audit/export?action=login.", nil)
	req.Header.Set("Authorization", "Bearer "+e.session)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="ragmux-audit-`) {
		t.Fatalf("export response: %d %v", resp.StatusCode, resp.Header)
	}
	lines := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil || !strings.HasPrefix(row["action"].(string), "login.") {
			t.Fatalf("export line %d: %v %s", lines+1, err, sc.Text())
		}
		lines++
	}
	if lines != 12 {
		t.Errorf("exported %d rows, want 12", lines)
	}
	exported := e.call("GET", "/admin/api/audit?action=audit.exported", nil, "")
	if exported["total"] != float64(1) {
		t.Fatalf("audit.exported entry: %v", exported)
	}
	entry := exported["entries"].([]any)[0].(map[string]any)
	if entry["actor_user_id"] != float64(adminID) || entry["details"].(map[string]any)["action"] != "login." {
		t.Errorf("audit.exported entry: %v", entry)
	}
}
