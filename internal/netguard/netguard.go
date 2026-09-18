// Package netguard keeps outbound provider calls away from private and local
// networks. Provider base URLs are entered by dashboard admins, so without
// it the gateway could be pointed at the metadata service, the database or
// any other host reachable from the container (SSRF).
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Error reports that a host only resolves to disallowed addresses. It is a
// distinct type so callers can recognise the policy decision with errors.As.
type Error struct {
	Host string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream host %q resolves to a private or local address; "+
		"set ALLOW_PRIVATE_UPSTREAMS=true or add it to PRIVATE_UPSTREAM_ALLOWLIST", e.Host)
}

// RedirectError reports a redirect the client refused to follow.
type RedirectError struct {
	Reason string
}

func (e *RedirectError) Error() string { return "upstream redirect rejected: " + e.Reason }

var disallowedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{
		"0.0.0.0/8",      // "this" network
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // carrier-grade NAT
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local (cloud metadata services live here)
		"172.16.0.0/12",  // RFC 1918
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // RFC 1918
		"198.18.0.0/15",  // benchmarking
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, broadcast
		"::/128",         // unspecified
		"::1/128",        // loopback
		"64:ff9b::/96",   // NAT64: embeds an IPv4 address
		"fc00::/7",       // unique local
		"fe80::/10",      // link-local
		"ff00::/8",       // multicast
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// IsDisallowedIP reports whether ip belongs to a loopback, link-local,
// multicast, unspecified, private or otherwise non-public range. IPv4-mapped
// IPv6 addresses are classified by their embedded IPv4 address.
func IsDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsPrivate() {
		return true
	}
	for _, n := range disallowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostAllowed reports whether policy lets host through without an address
// check: private upstreams are globally allowed, or the hostname is listed.
func hostAllowed(host string, allowPrivate bool, allowHosts map[string]bool) bool {
	return allowPrivate || allowHosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// SafeDialContext returns a DialContext for http.Transport that resolves the
// target host itself, drops disallowed addresses and dials the surviving IP
// literal. Dialing the literal (instead of the name) means a DNS answer
// cannot be swapped for a private address between the check and the
// connect (DNS rebinding). Only the TCP dial is replaced; TLS still runs in
// the transport with the ServerName taken from the request URL.
//
// allowPrivate disables the filter entirely; allowHosts lists hostnames
// (lower-case, exact match) that bypass it.
func SafeDialContext(base *net.Dialer, allowPrivate bool, allowHosts map[string]bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{Timeout: 15 * time.Second}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if hostAllowed(host, allowPrivate, allowHosts) {
			return base.DialContext(ctx, network, addr)
		}
		ips, err := resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
}

// resolve returns the public addresses of host, or *Error when it has none.
// An IP literal is checked directly.
func resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if IsDisallowedIP(ip) {
			return nil, &Error{Host: host}
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range addrs {
		if !IsDisallowedIP(a.IP) {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, &Error{Host: host}
	}
	return ips, nil
}

// CheckHost resolves host once and returns *Error when every address it
// currently resolves to is disallowed by policy. It is meant for validating
// a base URL at save time; a lookup failure is not an error here because the
// dial-time check in SafeDialContext remains authoritative.
func CheckHost(ctx context.Context, host string, allowPrivate bool, allowHosts map[string]bool) error {
	if hostAllowed(host, allowPrivate, allowHosts) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := resolve(ctx, host)
	var ne *Error
	if errors.As(err, &ne) {
		return ne
	}
	return nil
}

// HostIsPrivate classifies a base URL host the way the dialer would: true
// for "localhost", for an IP literal in a disallowed range, and for a
// hostname whose every current address is disallowed. A lookup failure is
// returned as an error so the caller can record "unknown" rather than a
// guess. Policy (allowlists) plays no part: this is classification only.
func HostIsPrivate(ctx context.Context, host string) (bool, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true, nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return IsDisallowedIP(ip), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := resolve(ctx, host)
	var ne *Error
	if errors.As(err, &ne) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// CheckRedirect is an http.Client CheckRedirect policy: at most maxHops
// redirects, http/https only and never to a different host, so an upstream
// cannot bounce the gateway to somewhere its base URL was not allowed to
// reach.
func CheckRedirect(maxHops int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxHops {
			return &RedirectError{Reason: fmt.Sprintf("more than %d redirects", maxHops)}
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return &RedirectError{Reason: fmt.Sprintf("unsupported scheme %q", req.URL.Scheme)}
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return &RedirectError{Reason: "target host differs from the request host"}
		}
		return nil
	}
}

// ParseAllowlist turns a comma-separated list of hostnames into the set
// SafeDialContext and CheckHost expect (lower-case, trimmed, empty dropped).
func ParseAllowlist(s string) map[string]bool {
	out := map[string]bool{}
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
			out[h] = true
		}
	}
	return out
}
