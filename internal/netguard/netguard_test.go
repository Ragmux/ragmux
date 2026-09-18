package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.8.8", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "192.0.0.1", "198.18.0.1", "198.19.255.255",
		"0.0.0.0", "224.0.0.1", "255.255.255.255",
		"::1", "::", "fc00::1", "fd12::1", "fe80::1", "ff02::1", "64:ff9b::7f00:1", "64:ff9b::808:808",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
	}
	for _, s := range blocked {
		if !IsDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s should be disallowed", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "100.128.0.1", "192.0.1.1", "198.20.0.1",
		"2606:4700:4700::1111", "::ffff:8.8.8.8", "2001:4860:4860::8888"}
	for _, s := range allowed {
		if IsDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !IsDisallowedIP(nil) {
		t.Error("nil should be disallowed")
	}
}

func TestParseAllowlist(t *testing.T) {
	got := ParseAllowlist(" Ollama, host.docker.internal. ,,")
	if len(got) != 2 || !got["ollama"] || !got["host.docker.internal"] {
		t.Errorf("allowlist = %v", got)
	}
}

func newClient(allowPrivate bool, hosts map[string]bool) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: SafeDialContext(&net.Dialer{Timeout: 2 * time.Second}, allowPrivate, hosts),
	}}
}

func TestSafeDialContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	byName := "http://localhost:" + u.Port() + "/"

	// Blocked by default: the loopback literal and the loopback name.
	for _, target := range []string{srv.URL, byName} {
		_, err := newClient(false, nil).Get(target)
		var ne *Error
		if !errors.As(err, &ne) {
			t.Fatalf("%s: expected *Error, got %v", target, err)
		}
		if !strings.Contains(err.Error(), "PRIVATE_UPSTREAM_ALLOWLIST") {
			t.Errorf("message = %q", err.Error())
		}
	}
	// Allowed globally.
	resp, err := newClient(true, nil).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowPrivate: %v", err)
	}
	_ = resp.Body.Close()
	// Allowed for one hostname only.
	hosts := map[string]bool{"localhost": true}
	resp, err = newClient(false, hosts).Get(byName)
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := newClient(false, hosts).Get(srv.URL); err == nil {
		t.Error("the allowlist must not cover the literal address")
	}
}

func TestCheckHost(t *testing.T) {
	ctx := context.Background()
	var ne *Error
	if err := CheckHost(ctx, "127.0.0.1", false, nil); !errors.As(err, &ne) {
		t.Errorf("literal: %v", err)
	}
	if err := CheckHost(ctx, "localhost", false, nil); !errors.As(err, &ne) {
		t.Errorf("localhost: %v", err)
	}
	if err := CheckHost(ctx, "localhost", false, map[string]bool{"localhost": true}); err != nil {
		t.Errorf("allowlisted: %v", err)
	}
	if err := CheckHost(ctx, "127.0.0.1", true, nil); err != nil {
		t.Errorf("allowPrivate: %v", err)
	}
	if err := CheckHost(ctx, "8.8.8.8", false, nil); err != nil {
		t.Errorf("public literal: %v", err)
	}
	// An unresolvable name is left to the dial-time check.
	if err := CheckHost(ctx, "does-not-exist.invalid", false, nil); err != nil {
		t.Errorf("unresolvable: %v", err)
	}
}

func TestCheckRedirect(t *testing.T) {
	first, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)
	mk := func(u string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		return r
	}
	check := CheckRedirect(3)
	if err := check(mk("https://api.example.com/v2"), []*http.Request{first}); err != nil {
		t.Errorf("same host: %v", err)
	}
	if err := check(mk("https://API.example.com:443/v2"), []*http.Request{first}); err == nil {
		t.Error("host with port differs and must be rejected")
	}
	var re *RedirectError
	if err := check(mk("https://evil.example.com/"), []*http.Request{first}); !errors.As(err, &re) {
		t.Errorf("other host: %v", err)
	}
	if err := check(mk("ftp://api.example.com/"), []*http.Request{first}); !errors.As(err, &re) {
		t.Errorf("scheme: %v", err)
	}
	via := []*http.Request{first, first, first, first}
	if err := check(mk("https://api.example.com/"), via); !errors.As(err, &re) {
		t.Errorf("hops: %v", err)
	}
}
