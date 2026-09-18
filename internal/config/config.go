// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the gateway reads at startup.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string (required).
	DatabaseURL string
	// DBMaxConns caps the connection pool size.
	DBMaxConns int
	// SecretKeyHex is the AES-256 key for provider credentials as 64 hex
	// characters. Empty means fall back to DATA_DIR/secret.key.
	SecretKeyHex string
	// DataDir is only used for the secret.key fallback.
	DataDir       string
	Port          int
	AdminUser     string
	AdminPassword string
	LogLevel      string
	CORSOrigins   []string
	// SessionTTL controls how long a dashboard login stays valid.
	SessionTTL time.Duration
	// UpstreamTimeout bounds a single non-streaming provider call.
	UpstreamTimeout time.Duration
	// IngestWorkers is the number of concurrent document ingestion jobs.
	IngestWorkers int
	// MaxUploadBytes caps a single document upload.
	MaxUploadBytes int64
	// LoginRateLimitPerMin caps failed logins per minute from one IP.
	LoginRateLimitPerMin int
	// LoginUserLimitPerMin caps failed logins per minute for one username.
	LoginUserLimitPerMin int
	// LoginLockoutFailures failures within LoginLockoutMinutes lock a username out.
	LoginLockoutFailures int
	LoginLockoutMinutes  int
	// TrustProxyHeaders enables X-Forwarded-For / X-Real-IP as the client
	// address for login limits and audit entries.
	TrustProxyHeaders bool
	// LogRetentionDays is how long request logs are kept; 0 keeps them forever.
	LogRetentionDays int
	// AuditRetentionDays is how long audit entries are kept; 0 keeps them forever.
	AuditRetentionDays int
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      10,
		SecretKeyHex:    strings.TrimSpace(os.Getenv("SECRET_KEY")),
		DataDir:         env("DATA_DIR", "/app/data"),
		Port:            8080,
		AdminUser:       env("ADMIN_USER", "admin"),
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
		LogLevel:        env("LOG_LEVEL", "info"),
		SessionTTL:      24 * time.Hour,
		UpstreamTimeout: 5 * time.Minute,
		IngestWorkers:   2,
		MaxUploadBytes:  50 << 20,

		LoginRateLimitPerMin: 10,
		LoginUserLimitPerMin: 5,
		LoginLockoutFailures: 20,
		LoginLockoutMinutes:  15,

		LogRetentionDays:   90,
		AuditRetentionDays: 365,
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required (e.g. postgres://user:pass@host:5432/ragmux?sslmode=disable)")
	}
	if c.SecretKeyHex != "" {
		key, err := hex.DecodeString(c.SecretKeyHex)
		if err != nil || len(key) != 32 {
			return c, errors.New("SECRET_KEY must be 64 hex characters (32 bytes); generate one with: openssl rand -hex 32")
		}
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid DB_MAX_CONNS %q", v)
		}
		c.DBMaxConns = n
	}
	p, err := PortFromEnv()
	if err != nil {
		return c, err
	}
	c.Port = p
	c.TrustProxyHeaders = os.Getenv("TRUST_PROXY_HEADERS") == "true"
	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				c.CORSOrigins = append(c.CORSOrigins, o)
			}
		}
	}
	if v := os.Getenv("SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("invalid SESSION_TTL %q: %w", v, err)
		}
		c.SessionTTL = d
	}
	if v := os.Getenv("UPSTREAM_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("invalid UPSTREAM_TIMEOUT %q: %w", v, err)
		}
		c.UpstreamTimeout = d
	}
	if v := os.Getenv("INGEST_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid INGEST_WORKERS %q", v)
		}
		c.IngestWorkers = n
	}
	if v := os.Getenv("MAX_UPLOAD_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid MAX_UPLOAD_MB %q", v)
		}
		c.MaxUploadBytes = int64(n) << 20
	}
	for _, v := range []struct {
		name string
		dst  *int
	}{
		{"LOGIN_RATE_LIMIT_PER_MIN", &c.LoginRateLimitPerMin},
		{"LOGIN_USER_LIMIT_PER_MIN", &c.LoginUserLimitPerMin},
		{"LOGIN_LOCKOUT_FAILURES", &c.LoginLockoutFailures},
		{"LOGIN_LOCKOUT_MINUTES", &c.LoginLockoutMinutes},
		{"LOG_RETENTION_DAYS", &c.LogRetentionDays},
		{"AUDIT_RETENTION_DAYS", &c.AuditRetentionDays},
	} {
		if raw := os.Getenv(v.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return c, fmt.Errorf("invalid %s %q (0 disables)", v.name, raw)
			}
			*v.dst = n
		}
	}
	return c, nil
}

// PortFromEnv reads PORT (default 8080). It is separate from Load so the
// container healthcheck can probe the server without a full configuration.
func PortFromEnv() (int, error) {
	v := os.Getenv("PORT")
	if v == "" {
		return 8080, nil
	}
	p, err := strconv.Atoi(v)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid PORT %q", v)
	}
	return p, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
