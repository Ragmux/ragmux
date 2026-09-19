package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestFirstRunSetup(t *testing.T) {
	e := newEnvWith(t, testdb.Config(t), false, nil)
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }

	r := e.call("GET", "/admin/api/setup", nil, "x")
	if r["needs_setup"] != true {
		t.Fatalf("fresh schema should need setup: %v", r)
	}
	// While setup is pending the wizard gets the facts its first screen shows.
	for _, k := range []string{"migrations_version", "secret_key_source", "database_role",
		"has_connections", "has_projects"} {
		if _, ok := r[k]; !ok {
			t.Errorf("setup status is missing %s while setup is pending: %v", k, r)
		}
	}
	if r := e.call("POST", "/admin/api/login", map[string]any{"username": "admin", "password": "password123"}, "x"); status(r) != 401 {
		t.Errorf("login before setup: %v", r)
	}
	if r := e.call("GET", "/admin/api/me", nil, "x"); status(r) != 401 {
		t.Errorf("me before setup: %v", r)
	}

	// Validation: username charset and length, password length.
	for _, c := range []map[string]any{
		{"username": "ab", "password": "correct-horse-battery"},
		{"username": "Admin", "password": "correct-horse-battery"},
		{"username": "root user", "password": "correct-horse-battery"},
		{"username": "root", "password": "short-pass1"},
		{"username": "root", "password": strings.Repeat("p", 73)},
	} {
		if r := e.call("POST", "/admin/api/setup", c, "x"); status(r) != 400 {
			t.Errorf("setup %v should be rejected: %v", c, r)
		}
	}
	if n, _ := e.store.CountUsers(context.Background()); n != 0 {
		t.Fatalf("rejected setups created users: %d", n)
	}

	// Two concurrent valid requests: exactly one creates the account.
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			r := e.call("POST", "/admin/api/setup", map[string]any{"username": name, "password": "correct-horse-battery"}, "x")
			results <- status(r)
		}(name)
	}
	wg.Wait()
	close(results)
	got := map[int]int{}
	for st := range results {
		got[st]++
	}
	if got[201] != 1 || got[409] != 1 {
		t.Fatalf("concurrent setup statuses: %v, want one 201 and one 409", got)
	}
	users, _ := e.store.ListUsers(context.Background())
	if len(users) != 1 || users[0].Role != "admin" || !users[0].IsActive {
		t.Fatalf("users after setup: %+v", users)
	}

	// The setup response logs the browser in: a cookie session that reaches
	// authenticated routes. Replay it on a fresh schema rather than reuse the
	// race above, so the cookie can be inspected.
	e2 := newEnvWith(t, testdb.Config(t), false, nil)
	req, _ := http.NewRequest("POST", e2.srv.URL+"/admin/api/setup", strings.NewReader(`{"username":"owner","password":"correct-horse-battery","bearer":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeJSON(t, resp, &body)
	if resp.StatusCode != 201 || body["token"] == nil {
		t.Fatalf("setup: %d %v", resp.StatusCode, body)
	}
	token, _ := body["token"].(string)
	user, _ := body["user"].(map[string]any)
	if user["username"] != "owner" || user["role"] != "admin" {
		t.Errorf("setup user: %v", user)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("session cookie not set: %v", resp.Cookies())
	}
	req, _ = http.NewRequest("GET", e2.srv.URL+"/admin/api/me", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, resp, &body)
	if resp.StatusCode != 200 || body["username"] != "owner" {
		t.Errorf("cookie from setup: %d %v", resp.StatusCode, body)
	}
	if r := e2.call("GET", "/admin/api/users", nil, token); status(r) != 200 {
		t.Errorf("bearer token from setup: %v", r)
	}

	// Setup is closed for good, and the login works with the chosen password.
	if r := e2.call("POST", "/admin/api/setup", map[string]any{"username": "another", "password": "correct-horse-battery"}, "x"); status(r) != 409 {
		t.Errorf("second setup: %v", r)
	}
	// Setup is done, so the unauthenticated endpoint stops publishing the rest:
	// only needs_setup survives (_status is the harness's own field).
	if done := e2.call("GET", "/admin/api/setup", nil, "x"); done["needs_setup"] != false || len(done) != 2 {
		t.Errorf("setup status after setup should be needs_setup alone: %v", done)
	}
	if n, _ := e2.store.CountUsers(context.Background()); n != 1 {
		t.Errorf("users after second setup: %d", n)
	}
	session := e2.login("owner", "correct-horse-battery")
	audit := e2.call("GET", "/admin/api/audit?action=setup.complete", nil, session)
	if list, _ := audit["entries"].([]any); len(list) != 1 {
		t.Errorf("setup.complete audit rows: %v", audit)
	}

	// The setup endpoint shares the login limiter: repeated bad attempts from
	// one address are throttled with Retry-After.
	e3 := newEnvWith(t, testdb.Config(t), false, nil)
	var last map[string]any
	var hdr http.Header
	for i := 0; i < 12; i++ {
		last, hdr = e3.callRaw("POST", "/admin/api/setup", map[string]any{"username": "x", "password": "correct-horse-battery"}, "x")
		if status(last) == 429 {
			break
		}
	}
	if status(last) != 429 || hdr.Get("Retry-After") == "" {
		t.Errorf("setup should be rate limited by address: %v %v", last, hdr)
	}
	if byUser, _, _ := e3.store.CountFailedLoginAttempts(context.Background(), "owner", "", time.Now().Add(-time.Hour)); byUser != 0 {
		t.Errorf("setup attempts must not count against a real username: %d", byUser)
	}
}

// TestBootstrapPathKeepsWorking covers the unattended install: an account
// created before the first request (as ADMIN_PASSWORD does) closes setup.
func TestBootstrapPathKeepsWorking(t *testing.T) {
	cfg := testdb.Config(t)
	st := testdb.OpenWith(t, cfg)
	h, _ := auth.HashPassword("preset-password-1")
	if _, err := st.CreateFirstUser(context.Background(), "ops", h, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFirstUser(context.Background(), "ops2", h, "admin"); err != store.ErrSetupDone {
		t.Fatalf("second CreateFirstUser: %v", err)
	}
	e := newEnvWith(t, cfg, false, nil)
	if r := e.call("GET", "/admin/api/setup", nil, "x"); r["needs_setup"] != false {
		t.Errorf("setup should be closed: %v", r)
	}
	if r := e.call("POST", "/admin/api/setup", map[string]any{"username": "another", "password": "correct-horse-battery"}, "x"); int(r["_status"].(float64)) != 409 {
		t.Errorf("setup after bootstrap: %v", r)
	}
	e.login("ops", "preset-password-1")
}

func decodeJSON(t *testing.T, resp *http.Response, v *map[string]any) {
	t.Helper()
	defer resp.Body.Close()
	*v = nil
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
