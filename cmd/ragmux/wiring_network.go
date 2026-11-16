package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ragmux/ragmux/internal/config"
)

// proxyHeadersUnverified reports the setting under which a key's CIDR
// allowlist (ADR-039) can be walked around: TRUST_PROXY_HEADERS is on and
// TRUSTED_PROXY_CIDRS is empty, so realIP honours X-Forwarded-For from any
// connection and a client picks the address the gateway sees.
func proxyHeadersUnverified(cfg config.Config) bool {
	return cfg.TrustProxyHeaders && len(cfg.TrustedProxyCIDRs) == 0
}

// cidrRestrictedCounter is the store's count of keys and projects that carry a
// CIDR allowlist.
type cidrRestrictedCounter interface {
	CountCIDRRestricted(ctx context.Context) (keys, projects int64, err error)
}

// warnUnverifiedProxyKeys logs a warning at start-up when the proxy headers
// are unverified and some key or project key holds a CIDR allowlist. It never
// stops the start: refusing would break installations that already run with
// that setting. With unverified false it does not touch the database, and a
// failed count is logged, not returned, for the same reason.
func warnUnverifiedProxyKeys(ctx context.Context, st cidrRestrictedCounter, log *slog.Logger, unverified bool) {
	if !unverified {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys, projects, err := st.CountCIDRRestricted(ctx)
	if err != nil {
		log.Warn("could not check whether keys carry a CIDR allowlist while TRUSTED_PROXY_CIDRS is empty", "err", err)
		return
	}
	if keys == 0 && projects == 0 {
		return
	}
	log.Warn("TRUST_PROXY_HEADERS is on and TRUSTED_PROXY_CIDRS is empty: X-Forwarded-For is accepted from any "+
		"connection, so a client can forge its address and bypass the CIDR allowlists of its keys; "+
		"set TRUSTED_PROXY_CIDRS to the network of your reverse proxy",
		"keys_with_cidrs", keys, "projects_with_cidrs", projects)
}
