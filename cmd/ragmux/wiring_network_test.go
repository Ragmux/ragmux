package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// The key allowlists (ADR-039) are only as good as the client address, and
// that address is realIP's. These tests prove the pieces main wires together:
// the unverified-proxy setting, its start-up warning, and the end-to-end
// behaviour of realIP in front of the gateway's check.

func TestProxyHeadersUnverified(t *testing.T) {
	trusted := cidrs(t, "10.0.0.0/8")
	for _, c := range []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"headers off, no networks", config.Config{}, false},
		{"headers off, networks set", config.Config{TrustedProxyCIDRs: trusted}, false},
		{"headers on, no networks: X-Forwarded-For accepted from anyone", config.Config{TrustProxyHeaders: true}, true},
		{"headers on, networks set", config.Config{TrustProxyHeaders: true, TrustedProxyCIDRs: trusted}, false},
	} {
		if got := proxyHeadersUnverified(c.cfg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeCounter stands in for the store: it counts the queries asked of it.
type fakeCounter struct {
	calls          atomic.Int32
	keys, projects int64
	err            error
}

func (f *fakeCounter) CountCIDRRestricted(context.Context) (int64, int64, error) {
	f.calls.Add(1)
	return f.keys, f.projects, f.err
}

func TestWarnUnverifiedProxyKeys(t *testing.T) {
	run := func(c *fakeCounter, unverified bool) string {
		var buf strings.Builder
		warnUnverifiedProxyKeys(context.Background(), c, slog.New(slog.NewTextHandler(&buf, nil)), unverified)
		return buf.String()
	}

	// The flag off: no query and no line, whatever the database holds.
	c := &fakeCounter{keys: 3, projects: 1}
	if out := run(c, false); out != "" || c.calls.Load() != 0 {
		t.Errorf("flag off: %d queries, output %q; want neither", c.calls.Load(), out)
	}
	// On, with a CIDR list somewhere: one warning, naming the setting and the counts.
	c = &fakeCounter{keys: 2, projects: 1}
	out := run(c, true)
	if c.calls.Load() != 1 || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "TRUSTED_PROXY_CIDRS") ||
		!strings.Contains(out, "keys_with_cidrs=2") || !strings.Contains(out, "projects_with_cidrs=1") {
		t.Errorf("flag on, lists present: %d queries, output %q", c.calls.Load(), out)
	}
	// A project key alone is enough.
	if out := run(&fakeCounter{projects: 1}, true); !strings.Contains(out, "level=WARN") {
		t.Errorf("project key only: %q", out)
	}
	// On, but no list anywhere: nothing to say.
	c = &fakeCounter{}
	if out := run(c, true); out != "" || c.calls.Load() != 1 {
		t.Errorf("flag on, no lists: %d queries, output %q", c.calls.Load(), out)
	}
	// A failing query is a warning of its own, never a panic or a stop.
	out = run(&fakeCounter{err: errors.New("db is down")}, true)
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "db is down") {
		t.Errorf("failed count: %q", out)
	}
}

// The same against the real store: the query runs and counts what the admin
// API stored.
func TestWarnUnverifiedProxyKeysAgainstTheStore(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	warnUnverifiedProxyKeys(ctx, st, log, true)
	if buf.Len() != 0 {
		t.Fatalf("nothing stored, yet: %q", buf.String())
	}
	if _, _, err := st.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID,
		KeyAllowedCIDRs: []string{"203.0.113.0/24"}}); err != nil {
		t.Fatal(err)
	}
	warnUnverifiedProxyKeys(ctx, st, log, true)
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "projects_with_cidrs=1") {
		t.Errorf("a project key with a CIDR list: %q", out)
	}
}

// main must call both helpers, once, and must not touch the admin.Admin{...}
// literal (Faz 05 owns it): the flag is assigned after it. A helper nothing
// calls would pass every test above.
func TestMainWiresTheProxyHeaderCheck(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, want := range []string{"adm.ProxyHeadersUnverified = proxyHeadersUnverified(cfg)", "warnUnverifiedProxyKeys("} {
		if n := strings.Count(text, want); n != 1 {
			t.Errorf("main.go has %d occurrences of %q, want 1", n, want)
		}
	}
	if strings.Contains(text, "ProxyHeadersUnverified:") {
		t.Error("main.go sets ProxyHeadersUnverified inside the admin.Admin literal")
	}
	if a, b := strings.Index(text, "adm.ProxyHeadersUnverified ="), strings.Index(text, "chi.NewRouter()"); a < 0 || b < 0 || a > b {
		t.Error("the flag must be assigned before the router is built")
	}
}

// realIP in front of the gateway's allowlist check, on a real store. With
// TRUSTED_PROXY_CIDRS set a forged X-Forwarded-For from outside the proxy
// network is ignored; with it empty, the same forgery gets through, which is
// exactly what the start-up and admin warnings are for.
func TestKeyCIDRListBehindRealIP(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := st.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID,
		KeyAllowedCIDRs: []string{"203.0.113.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := &gateway.Gateway{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	status := func(trusted string, peer, xff string) int {
		t.Helper()
		r := chi.NewRouter()
		var nets = cidrs(t)
		if trusted != "" {
			nets = cidrs(t, trusted)
		}
		r.Use(realIP(nets))
		r.Route("/v1", gw.Routes)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.RemoteAddr = peer
		req.Header.Set("Authorization", "Bearer "+key)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	// Verified proxies (TRUSTED_PROXY_CIDRS set).
	if got := status("10.0.0.0/8", "10.0.0.2:5555", "203.0.113.5"); got != http.StatusOK {
		t.Errorf("client in the list, through our proxy: %d", got)
	}
	if got := status("10.0.0.0/8", "10.0.0.2:5555", "192.0.2.1"); got != http.StatusForbidden {
		t.Errorf("client outside the list, through our proxy: %d", got)
	}
	if got := status("10.0.0.0/8", "10.0.0.2:5555", "203.0.113.5, 192.0.2.1"); got != http.StatusForbidden {
		t.Errorf("forged left entry must not count: %d", got)
	}
	if got := status("10.0.0.0/8", "198.51.100.1:5555", "203.0.113.5"); got != http.StatusForbidden {
		t.Errorf("a forged X-Forwarded-For from outside the proxy network must be ignored: %d", got)
	}
	// Unverified (TRUST_PROXY_HEADERS on, TRUSTED_PROXY_CIDRS empty): the forgery works.
	if got := status("", "198.51.100.1:5555", "203.0.113.5"); got != http.StatusOK {
		t.Errorf("unverified proxy headers: %d (the list is expected to be bypassable here)", got)
	}
}
