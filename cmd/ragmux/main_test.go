package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/web"
)

func cidrs(t *testing.T, list ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestRealIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted []*net.IPNet
		remote  string
		xff     []string
		realIP  string
		want    string
	}{
		{"no headers keeps peer", nil, "10.0.0.1:1234", nil, "", "10.0.0.1:1234"},
		{"last xff entry wins", nil, "10.0.0.1:1234", []string{"1.1.1.1, 2.2.2.2"}, "", "2.2.2.2"},
		{"repeated header fields are joined", nil, "10.0.0.1:1234", []string{"1.1.1.1", "3.3.3.3"}, "", "3.3.3.3"},
		{"x-real-ip only without xff", nil, "10.0.0.1:1234", []string{"1.1.1.1"}, "9.9.9.9", "1.1.1.1"},
		{"x-real-ip fallback", nil, "10.0.0.1:1234", nil, "9.9.9.9", "9.9.9.9"},
		{"garbage keeps peer", nil, "10.0.0.1:1234", []string{"1.1.1.1, evil"}, "", "10.0.0.1:1234"},
		{"garbage real ip keeps peer", nil, "10.0.0.1:1234", nil, "evil", "10.0.0.1:1234"},
		{"ipv6 entry", nil, "10.0.0.1:1234", []string{"2001:db8::1"}, "", "2001:db8::1"},
		{"untrusted peer ignores headers", cidrs(t, "10.0.0.0/8"), "203.0.113.5:1234", []string{"1.1.1.1"}, "9.9.9.9", "203.0.113.5:1234"},
		{"trusted peer, client behind two of our proxies", cidrs(t, "10.0.0.0/8"), "10.0.0.2:1234", []string{"6.6.6.6, 1.1.1.1, 10.0.0.9"}, "", "1.1.1.1"},
		{"trusted peer, forged left entries ignored", cidrs(t, "10.0.0.0/8"), "10.0.0.2:1234", []string{"6.6.6.6, 1.1.1.1"}, "", "1.1.1.1"},
		{"all hops trusted falls back to the leftmost", cidrs(t, "10.0.0.0/8"), "10.0.0.2:1234", []string{"10.0.0.7, 10.0.0.9"}, "", "10.0.0.7"},
		{"trusted peer with x-real-ip only", cidrs(t, "10.0.0.0/8"), "10.0.0.2:1234", nil, "1.1.1.1", "1.1.1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := realIP(tc.trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Errorf("RemoteAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDashboardCSPMatchesEmbeddedScript(t *testing.T) {
	page, err := fs.ReadFile(web.FS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if n := strings.Count(html, "<script"); n != 1 {
		t.Fatalf("dashboard has %d <script> tags, the CSP hash covers exactly one", n)
	}
	for _, attr := range []string{" onclick=", " onsubmit=", " onchange=", " oninput=", " onload=", "javascript:"} {
		if strings.Contains(html, attr) {
			t.Errorf("inline handler %q would be blocked by the CSP", attr)
		}
	}
	body := html[strings.Index(html, "<script>")+len("<script>") : strings.Index(html, "</script>")]
	sum := sha256.Sum256([]byte(body))
	want := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])

	hash, err := dashboardScriptHash(web.FS)
	if err != nil || hash != want {
		t.Fatalf("hash = %q err %v, want %q", hash, err, want)
	}

	r := chi.NewRouter()
	r.Use(securityHeaders(hash))
	adm := &admin.Admin{WebFS: web.FS}
	r.Route("/admin", adm.Routes)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "script-src '"+hash+"'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("status %d csp %q", resp.StatusCode, csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("hardening headers missing: %v", resp.Header)
	}

	// The JSON API gets the generic headers but no CSP.
	resp2, err := http.Get(srv.URL + "/admin/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("Content-Security-Policy") != "" || resp2.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("api headers: %v", resp2.Header)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Without ADMIN_PASSWORD nothing is created: setup happens in the dashboard.
	st := testdb.Open(t)
	if err := bootstrapAdmin(ctx, st, config.Config{AdminUser: "admin"}, log); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUsers(ctx); n != 0 {
		t.Fatalf("bootstrap without a password created %d user(s)", n)
	}

	// With it the account is created once, as an active admin, and later
	// starts leave the table alone.
	cfg := config.Config{AdminUser: "ops", AdminPassword: "preset-password-1"}
	for i := 0; i < 2; i++ {
		if err := bootstrapAdmin(ctx, st, cfg, log); err != nil {
			t.Fatal(err)
		}
	}
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("users after bootstrap: %v %+v", err, users)
	}
	u := users[0]
	if u.Username != "ops" || u.Role != "admin" || !u.IsActive || !auth.CheckPassword(u.PasswordHash, "preset-password-1") {
		t.Errorf("bootstrapped user: %+v", u)
	}
	// An existing table is never touched, even with a different ADMIN_USER.
	if err := bootstrapAdmin(ctx, st, config.Config{AdminUser: "other", AdminPassword: "another-password-1"}, log); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Errorf("bootstrap on a populated table created users: %d", n)
	}
}

// TestDashboardSelectorsResolve guards a class of defect nothing else here can
// see. A querySelector that matches nothing raises no error: it returns null,
// the next line dereferences it, and the feature is dead on click while the
// page looks fine. The first-run wizard shipped that way -- setupCard rendered
// its primary button with no type attribute, [type=submit] requires the
// attribute to be present rather than merely implied, and the submit handlers
// for steps 2 and 3 both did btn.disabled = true on the null before sending
// anything. The drawer and modal helpers set the attribute, so the same
// selector worked everywhere else and the hole stayed invisible.
//
// The checks are deliberately coarse: string matching over the one embedded
// script, no browser, no parser. They are meant to catch a selector that
// cannot possibly match, not to prove that every lookup finds its element at
// runtime.
func TestDashboardSelectorsResolve(t *testing.T) {
	page, err := fs.ReadFile(web.FS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	script := html[strings.Index(html, "<script>")+len("<script>") : strings.Index(html, "</script>")]

	// Every form the dashboard renders carries an explicit submit button. That
	// is the invariant `$('button[type=submit]', form)` depends on, and a form
	// whose only button relies on the implicit default breaks it silently.
	forms := regexp.MustCompile(`(?s)<form\b.*?</form>`).FindAllString(html, -1)
	if len(forms) < 5 {
		t.Fatalf("found %d <form> templates, far fewer than the dashboard has; the scan is broken", len(forms))
	}
	typedSubmit := regexp.MustCompile(`<button[^>]*\stype="submit"`)
	for _, f := range forms {
		if !typedSubmit.MatchString(f) {
			head, _, _ := strings.Cut(f, "\n")
			if len(head) > 120 {
				head = head[:120] + "…"
			}
			t.Errorf("a <form> has no button with an explicit type=\"submit\", "+
				"so button[type=submit] cannot find it:\n\t%s", head)
		}
	}

	// Every literal #id the script looks up is either written in the markup or
	// passed to a template that interpolates it into id="${...}". The second
	// case is why a bare quoted occurrence counts: setupCard is handed
	// 'setupProjectForm' and writes the id itself. Values are collected quoted
	// and unquoted, since the helpers emit both (id="app", id=backendSel).
	written := map[string]bool{}
	idAttr := regexp.MustCompile(`\bid=(?:"([^"]*)"|'([^']*)'|([A-Za-z][\w-]*))`)
	for _, m := range idAttr.FindAllStringSubmatch(html, -1) {
		for _, v := range m[1:] {
			if v != "" {
				written[v] = true
			}
		}
	}
	selector := regexp.MustCompile(`\$\$?\(\s*(?:'([^'\n]*)'|"([^"\n]*)")`)
	idSelector := regexp.MustCompile(`#([A-Za-z][\w-]*)`)
	checked := 0
	for _, m := range selector.FindAllStringSubmatch(script, -1) {
		sel := m[1]
		if sel == "" {
			sel = m[2]
		}
		for _, id := range idSelector.FindAllStringSubmatch(sel, -1) {
			name := id[1]
			checked++
			if written[name] || strings.Contains(script, `'`+name+`'`) {
				continue
			}
			t.Errorf("selector %q looks for id %q, which nothing in the page sets", sel, name)
		}
	}
	if checked < 20 {
		t.Errorf("only %d id selectors examined; the selector scan is not matching the script", checked)
	}
}
