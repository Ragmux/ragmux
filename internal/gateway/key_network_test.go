package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/store"
)

// The per-key network allowlists (ADR-039). The checks run in identify, so
// these tests drive the real router with a hand-set RemoteAddr: that is the
// value the realIP middleware would have put there behind a proxy.

// netCall serves one request through /v1 as if it came from remote ("ip" or
// "ip:port") and returns the recorded answer.
func (e *env) netCall(method, path, key, remote string, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := chi.NewRouter()
	r.Route("/v1", e.gw.Routes)
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"claude-x","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func denial(t *testing.T, rec *httptest.ResponseRecorder) (typ, code string) {
	t.Helper()
	var out struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not an error body (%d): %s", rec.Code, rec.Body.String())
	}
	if out.Error.Code != nil {
		code = *out.Error.Code
	}
	return out.Error.Type, code
}

func (e *env) requestLogCount() int {
	e.t.Helper()
	var n int
	if err := e.st.DB().QueryRow(context.Background(), "SELECT count(*) FROM request_logs").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestKeyCIDRAllowlist(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	_, raw := e.userKey("netted", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.AllowedCIDRs = []string{"203.0.113.0/24", "198.51.100.7/32", "2001:db8::/32"}
	})
	for _, c := range []struct {
		remote string
		ok     bool
	}{
		{"203.0.113.50:4000", true}, {"198.51.100.7:1", true}, {"198.51.100.7", true}, {"[2001:db8::9]:80", true},
		{"[::ffff:203.0.113.5]:80", true}, // IPv4-mapped counts as IPv4
		{"198.51.100.8:1", false}, {"192.0.2.1:9", false}, {"[2001:dead::1]:80", false}, {"garbage", false},
	} {
		rec := e.netCall(http.MethodGet, "/v1/models", raw, c.remote, nil)
		if c.ok && rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s, want it admitted", c.remote, rec.Code, rec.Body.String())
		}
		if !c.ok {
			typ, code := denial(t, rec)
			if rec.Code != http.StatusForbidden || typ != "permission_error" || code != "ip_not_allowed" {
				t.Errorf("%s: %d %s/%s, want 403 permission_error/ip_not_allowed", c.remote, rec.Code, typ, code)
			}
		}
	}
	// An unknown key is still 401, from any address, so the list tells nothing about which keys exist.
	if rec := e.netCall(http.MethodGet, "/v1/models", "sk-user-doesnotexist", "192.0.2.1:9", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", rec.Code)
	}
	if rec := e.netCall(http.MethodGet, "/v1/models", "", "192.0.2.1:9", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	// The four refusals above were counted, by reason and nothing else.
	mustContain(t, reg.Text(), `ragmux_gateway_key_denied_total{reason="ip_not_allowed"} 4`)
	if strings.Contains(reg.Text(), `reason="origin_not_allowed"`) {
		t.Error("an origin series appeared without an origin refusal")
	}
	if n := e.requestLogCount(); n != 0 {
		t.Errorf("%d request_logs rows after refusals, want 0", n)
	}
}

func TestKeyEmptyListsKeepTodaysBehaviour(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	_, raw := e.userKey("open", []int64{e.proj.ID}, nil)
	for _, remote := range []string{"192.0.2.1:9", "[2001:db8::1]:80", "garbage"} {
		for _, hdr := range []map[string]string{nil, {"Origin": "https://anything.example"}, {"Origin": "null"}} {
			if rec := e.netCall(http.MethodGet, "/v1/models", raw, remote, hdr); rec.Code != http.StatusOK {
				t.Errorf("%s %v: %d", remote, hdr, rec.Code)
			}
			if rec := e.netCall(http.MethodGet, "/v1/models", e.key, remote, hdr); rec.Code != http.StatusOK {
				t.Errorf("project key %s %v: %d", remote, hdr, rec.Code)
			}
		}
	}
	if strings.Contains(reg.Text(), "ragmux_gateway_key_denied_total{") {
		t.Errorf("a denial was counted:\n%s", reg.Text())
	}
}

func TestKeyOriginAllowlist(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	_, raw := e.userKey("browser", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.AllowedOrigins = []string{"https://app.example.com", "https://*.example.org", "http://localhost:3000"}
	})
	ok := func(origin string) {
		t.Helper()
		if rec := e.netCall(http.MethodGet, "/v1/models", raw, "203.0.113.1:1", map[string]string{"Origin": origin}); rec.Code != http.StatusOK {
			t.Errorf("origin %q: %d %s, want it admitted", origin, rec.Code, rec.Body.String())
		}
	}
	deny := func(hdr map[string]string, label string) {
		t.Helper()
		rec := e.netCall(http.MethodGet, "/v1/models", raw, "203.0.113.1:1", hdr)
		typ, code := denial(t, rec)
		if rec.Code != http.StatusForbidden || typ != "permission_error" || code != "origin_not_allowed" {
			t.Errorf("%s: %d %s/%s, want 403 permission_error/origin_not_allowed", label, rec.Code, typ, code)
		}
	}
	ok("https://app.example.com")
	ok("https://a.example.org")       // a subdomain
	ok("https://a.b.example.org")     // at any depth
	ok("http://localhost:3000")       // an explicit port
	ok("https://app.example.com:443") // the default port is the same origin
	deny(map[string]string{"Origin": "https://example.org"}, "the apex of a wildcard")
	deny(map[string]string{"Origin": "https://evilexample.org"}, "a look-alike of a wildcard")
	deny(map[string]string{"Origin": "http://app.example.com"}, "another scheme")
	deny(map[string]string{"Origin": "https://app.example.com:8443"}, "another port")
	deny(map[string]string{"Origin": "https://evil.example"}, "another site")
	deny(map[string]string{"Origin": "null"}, "an opaque origin")
	deny(nil, "no Origin header at all")
	mustContain(t, reg.Text(), `ragmux_gateway_key_denied_total{reason="origin_not_allowed"} 7`)
	if n := e.requestLogCount(); n != 0 {
		t.Errorf("%d request_logs rows after refusals, want 0", n)
	}

	// Two lists: the address is checked first, then the Origin.
	_, both := e.userKey("both", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.AllowedCIDRs = []string{"203.0.113.0/24"}
		k.AllowedOrigins = []string{"https://app.example.com"}
	})
	hdr := map[string]string{"Origin": "https://app.example.com"}
	if rec := e.netCall(http.MethodGet, "/v1/models", both, "203.0.113.1:1", hdr); rec.Code != http.StatusOK {
		t.Errorf("both right: %d", rec.Code)
	}
	if _, code := denial(t, e.netCall(http.MethodGet, "/v1/models", both, "192.0.2.1:1", hdr)); code != "ip_not_allowed" {
		t.Errorf("wrong address: %q", code)
	}
	if _, code := denial(t, e.netCall(http.MethodGet, "/v1/models", both, "203.0.113.1:1", nil)); code != "origin_not_allowed" {
		t.Errorf("no Origin: %q", code)
	}
}

// The project's own key has no api_keys row: its lists are the project's
// key_allowed_* columns, and a user key of the same project does not inherit
// them.
func TestProjectKeyUsesProjectColumns(t *testing.T) {
	e := newEnv(t, func(p *store.Project) {
		p.KeyAllowedCIDRs = []string{"203.0.113.0/24"}
		p.KeyAllowedOrigins = []string{"https://site.example.com"}
	})
	reg := e.withMetrics()
	good := map[string]string{"Origin": "https://site.example.com"}
	if rec := e.netCall(http.MethodGet, "/v1/models", e.key, "203.0.113.9:1", good); rec.Code != http.StatusOK {
		t.Fatalf("project key from the list: %d %s", rec.Code, rec.Body.String())
	}
	if _, code := denial(t, e.netCall(http.MethodGet, "/v1/models", e.key, "192.0.2.1:1", good)); code != "ip_not_allowed" {
		t.Errorf("project key, wrong address: %q", code)
	}
	if _, code := denial(t, e.netCall(http.MethodGet, "/v1/models", e.key, "203.0.113.9:1", nil)); code != "origin_not_allowed" {
		t.Errorf("project key, no Origin: %q", code)
	}
	// A user key granted that project carries only its own (empty) lists.
	_, raw := e.userKey("free", []int64{e.proj.ID}, nil)
	if rec := e.netCall(http.MethodGet, "/v1/models", raw, "192.0.2.1:1", nil); rec.Code != http.StatusOK {
		t.Errorf("a user key must not inherit the project key's lists: %d", rec.Code)
	}
	mustContain(t, reg.Text(), `ragmux_gateway_key_denied_total{reason="ip_not_allowed"} 1`,
		`ragmux_gateway_key_denied_total{reason="origin_not_allowed"} 1`)
	if n := e.requestLogCount(); n != 0 {
		t.Errorf("%d request_logs rows after refusals, want 0", n)
	}
}

// x-api-key (the Anthropic SDKs' header, ADR-028) reaches the same check, and
// so does the refusal's Anthropic-shaped body on /v1/messages.
func TestKeyAllowlistOnMessagesAndXAPIKey(t *testing.T) {
	e := newEnv(t, nil)
	_, raw := e.userKey("anthropic-client", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.AllowedCIDRs = []string{"203.0.113.0/24"}
	})
	rec := e.netCall(http.MethodPost, "/v1/messages", "", "192.0.2.1:1", map[string]string{"x-api-key": raw, "anthropic-version": "2023-06-01"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("x-api-key from outside the list: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Type != "error" ||
		out.Error.Type != "permission_error" || out.Error.Code != "ip_not_allowed" {
		t.Errorf("messages refusal body = %s (%v)", rec.Body.String(), err)
	}
	// The same key, sent as x-api-key from inside the list, passes identify and
	// the model listing answers 200.
	rec = e.netCall(http.MethodGet, "/v1/models", "", "203.0.113.9:1", map[string]string{"x-api-key": raw})
	if rec.Code != http.StatusOK {
		t.Errorf("x-api-key from inside the list: %d %s", rec.Code, rec.Body.String())
	}
	// An unknown x-api-key is 401, whatever the address.
	if rec := e.netCall(http.MethodPost, "/v1/messages", "", "192.0.2.1:1", map[string]string{"x-api-key": "sk-user-nope"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown x-api-key: %d", rec.Code)
	}
	// The refusals above wrote no request_logs row. /v1/messages is a route
	// that does write one for a request it serves, so the control below shows
	// the count can move at all; without it "0" would hold for any route.
	if n := e.requestLogCount(); n != 0 {
		t.Fatalf("%d request_logs rows after refused /v1/messages calls, want 0", n)
	}
	served := e.netCall(http.MethodPost, "/v1/messages", "", "203.0.113.9:1", map[string]string{"x-api-key": raw, "anthropic-version": "2023-06-01"})
	if served.Code != http.StatusOK {
		t.Fatalf("/v1/messages from inside the list: %d %s", served.Code, served.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.requestLogCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.requestLogCount(); n != 1 {
		t.Errorf("%d request_logs rows after one served and several refused calls, want exactly 1", n)
	}
}

// /mcp is authenticated by MCPAuth, which runs identify: the lists apply there
// too, and the refusal is handed to the MCP layer with the same code.
func TestKeyAllowlistOnMCP(t *testing.T) {
	e := newEnv(t, nil)
	_, raw := e.userKey("mcp-client", []int64{e.proj.ID}, func(k *store.APIKey) {
		k.AllowedCIDRs = []string{"203.0.113.0/24"}
		k.AllowedOrigins = []string{"https://app.example.com"}
	})
	var refusedStatus int
	var refusedCode string
	reached := false
	handler := e.gw.MCPAuth(func(w http.ResponseWriter, status int, code, msg string) {
		refusedStatus, refusedCode = status, code
		w.WriteHeader(status)
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(remote string, hdr map[string]string) int {
		refusedStatus, refusedCode, reached = 0, "", false
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.RemoteAddr = remote
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	good := map[string]string{"Authorization": "Bearer " + raw, "Origin": "https://app.example.com"}
	if code := call("203.0.113.9:1", good); code != http.StatusNoContent || !reached {
		t.Errorf("inside: %d reached=%v", code, reached)
	}
	if code := call("192.0.2.1:1", good); code != http.StatusForbidden || reached || refusedCode != "ip_not_allowed" || refusedStatus != http.StatusForbidden {
		t.Errorf("wrong address: %d reached=%v %d %q", code, reached, refusedStatus, refusedCode)
	}
	noOrigin := map[string]string{"x-api-key": raw}
	if code := call("203.0.113.9:1", noOrigin); code != http.StatusForbidden || reached || refusedCode != "origin_not_allowed" {
		t.Errorf("no Origin (x-api-key): %d reached=%v %q", code, reached, refusedCode)
	}
	unknown := map[string]string{"Authorization": "Bearer sk-user-nope"}
	if code := call("192.0.2.1:1", unknown); code != http.StatusUnauthorized || reached {
		t.Errorf("unknown key: %d reached=%v", code, reached)
	}
}

// A refusal is logged with the key's id and nothing that identifies the
// credential, the address or the origin as a label.
func TestKeyDenialIsLoggedWithTheKeyID(t *testing.T) {
	e := newEnv(t, nil)
	var buf bytes.Buffer
	e.gw.Log = slog.New(slog.NewTextHandler(&buf, nil))
	k, raw := e.userKey("logged", []int64{e.proj.ID}, func(k *store.APIKey) { k.AllowedCIDRs = []string{"203.0.113.0/24"} })
	if rec := e.netCall(http.MethodGet, "/v1/models", raw, "192.0.2.1:1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	line := buf.String()
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "key="+strconv.FormatInt(k.ID, 10)) || !strings.Contains(line, "reason=ip_not_allowed") {
		t.Errorf("log line = %q", line)
	}
	if strings.Contains(line, raw) {
		t.Errorf("the credential reached the log: %q", line)
	}
}
