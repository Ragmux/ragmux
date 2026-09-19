// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
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
	// IngestLease is how long a replica owns a document it claimed without
	// renewing the claim. The worker renews it every third of this while
	// the job runs, so it bounds how long a crashed replica's document
	// stays untouchable, not how long a job may take.
	IngestLease time.Duration
	// IngestPollInterval is how often the ingestion dispatcher looks for
	// claimable documents when nothing wakes it.
	IngestPollInterval time.Duration
	// IngestMaxAttempts caps how often one document may be claimed before
	// the retention job marks it failed. It stops a document that kills the
	// process from becoming a cluster-wide crash loop.
	IngestMaxAttempts int
	// MaxPendingDocuments is the cluster-wide ingestion backlog an upload
	// is still accepted into; beyond it the upload gets 503.
	MaxPendingDocuments int
	// MaxUploadBytes caps a single document upload.
	MaxUploadBytes int64
	// LoginRateLimitPerMin caps failed logins per minute from one IP.
	LoginRateLimitPerMin int
	// LoginUserLimitPerMin caps failed logins per minute for one username.
	LoginUserLimitPerMin int
	// LoginLockoutFailures failures within LoginLockoutMinutes lock a username out.
	LoginLockoutFailures int
	LoginLockoutMinutes  int
	// TrustedProxyCIDRs limits TrustProxyHeaders to connections from these
	// networks. Empty means every peer is trusted, which is only safe when the
	// gateway cannot be reached without going through the proxy.
	TrustedProxyCIDRs []*net.IPNet
	// TrustProxyHeaders enables X-Forwarded-For / X-Real-IP as the client
	// address for login limits and audit entries.
	TrustProxyHeaders bool
	// LogRetentionDays is how long request logs are kept; 0 keeps them forever.
	LogRetentionDays int
	// AuditRetentionDays is how long audit entries are kept; 0 keeps them forever.
	AuditRetentionDays int
	// AllowPrivateUpstreams lets provider base URLs point at loopback,
	// link-local and private networks. Off by default (SSRF protection).
	AllowPrivateUpstreams bool
	// PrivateUpstreamAllowlist lists hostnames (lower-case) that may resolve
	// to private addresses even when AllowPrivateUpstreams is false.
	PrivateUpstreamAllowlist map[string]bool
	// StreamMaxDuration bounds one streaming provider response end to end.
	StreamMaxDuration time.Duration
	// StreamMaxBytes caps the bytes read from one streaming response.
	StreamMaxBytes int64
	// ImageFetch lets the gateway download an image_url a chat request
	// carries, for providers whose upstream cannot fetch one itself (Gemini,
	// Ollama). Off, those requests are refused as they were before.
	ImageFetch bool
	// ImageFetchMaxBytes caps one fetched image.
	ImageFetchMaxBytes int64
	// ImageFetchTimeout bounds one image fetch.
	ImageFetchTimeout time.Duration
	// ImageFetchMaxPerRequest caps how many images one chat request may pull,
	// so a single request cannot fan out.
	ImageFetchMaxPerRequest int
	// ImageCacheEntries is the size of the fetched-image cache; 0 disables it.
	ImageCacheEntries int
	// ImageCacheTTL is how long a fetched image may be reused.
	ImageCacheTTL time.Duration
	// MaxChunksPerDocument fails ingestion of documents that split into more
	// chunks than this, bounding memory and embedding cost per document.
	MaxChunksPerDocument int
	// MaxDocumentsPerStore and MaxBytesPerStore are instance-wide ceilings
	// on what a RAG store may hold; 0 means unlimited. A store's own
	// max_documents / max_bytes can only lower them.
	MaxDocumentsPerStore int
	MaxBytesPerStore     int64
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	dbURL, err := envOrFile("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	secretKey, err := envOrFile("SECRET_KEY")
	if err != nil {
		return Config{}, err
	}
	c := Config{
		DatabaseURL:     dbURL,
		DBMaxConns:      10,
		SecretKeyHex:    secretKey,
		DataDir:         env("DATA_DIR", "/app/data"),
		Port:            8765,
		AdminUser:       env("ADMIN_USER", "admin"),
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
		LogLevel:        env("LOG_LEVEL", "info"),
		SessionTTL:      24 * time.Hour,
		UpstreamTimeout: 5 * time.Minute,
		IngestWorkers:   2,
		MaxUploadBytes:  50 << 20,

		IngestLease:         2 * time.Minute,
		IngestPollInterval:  5 * time.Second,
		IngestMaxAttempts:   5,
		MaxPendingDocuments: 1024,

		LoginRateLimitPerMin: 10,
		LoginUserLimitPerMin: 5,
		LoginLockoutFailures: 20,
		LoginLockoutMinutes:  15,

		LogRetentionDays:   90,
		AuditRetentionDays: 365,
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL (or DATABASE_URL_FILE) is required (e.g. postgres://user:pass@host:5432/ragmux?sslmode=disable)")
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
	for _, v := range []struct {
		name string
		dst  *time.Duration
		min  time.Duration
	}{
		// The lease is renewed at a third of its length, so it must stay
		// comfortably above the round trip of one renewal.
		{"INGEST_LEASE", &c.IngestLease, 3 * time.Second},
		{"INGEST_POLL_INTERVAL", &c.IngestPollInterval, 100 * time.Millisecond},
	} {
		if raw := os.Getenv(v.name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d < v.min {
				return c, fmt.Errorf("invalid %s %q (minimum %s)", v.name, raw, v.min)
			}
			*v.dst = d
		}
	}
	for _, v := range []struct {
		name string
		dst  *int
	}{
		{"INGEST_MAX_ATTEMPTS", &c.IngestMaxAttempts},
		{"MAX_PENDING_DOCUMENTS", &c.MaxPendingDocuments},
	} {
		if raw := os.Getenv(v.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return c, fmt.Errorf("invalid %s %q", v.name, raw)
			}
			*v.dst = n
		}
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
	if v := os.Getenv("TRUSTED_PROXY_CIDRS"); v != "" {
		for _, raw := range strings.Split(v, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if !strings.Contains(raw, "/") {
				if strings.Contains(raw, ":") {
					raw += "/128"
				} else {
					raw += "/32"
				}
			}
			_, n, err := net.ParseCIDR(raw)
			if err != nil {
				return c, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q", raw)
			}
			c.TrustedProxyCIDRs = append(c.TrustedProxyCIDRs, n)
		}
	}
	c.AllowPrivateUpstreams = os.Getenv("ALLOW_PRIVATE_UPSTREAMS") == "true"
	c.PrivateUpstreamAllowlist = map[string]bool{}
	for _, h := range strings.Split(os.Getenv("PRIVATE_UPSTREAM_ALLOWLIST"), ",") {
		if h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
			c.PrivateUpstreamAllowlist[h] = true
		}
	}
	c.StreamMaxDuration = 30 * time.Minute
	if v := os.Getenv("STREAM_MAX_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("invalid STREAM_MAX_DURATION %q", v)
		}
		c.StreamMaxDuration = d
	}
	c.StreamMaxBytes = 256 << 20
	if v := os.Getenv("STREAM_MAX_BYTES_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid STREAM_MAX_BYTES_MB %q", v)
		}
		c.StreamMaxBytes = int64(n) << 20
	}
	// Image fetching is on by default: without it a Gemini or Ollama
	// connection cannot answer a request carrying an ordinary image URL at
	// all, and the fetch goes through the same SSRF-guarded client as every
	// other outbound call.
	c.ImageFetch = os.Getenv("IMAGE_FETCH") != "false"
	c.ImageFetchMaxBytes = 8 << 20
	if v := os.Getenv("IMAGE_FETCH_MAX_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid IMAGE_FETCH_MAX_MB %q", v)
		}
		c.ImageFetchMaxBytes = int64(n) << 20
	}
	c.ImageFetchTimeout = 10 * time.Second
	if v := os.Getenv("IMAGE_FETCH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("invalid IMAGE_FETCH_TIMEOUT %q", v)
		}
		c.ImageFetchTimeout = d
	}
	c.ImageFetchMaxPerRequest = 8
	if v := os.Getenv("IMAGE_FETCH_MAX_PER_REQUEST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid IMAGE_FETCH_MAX_PER_REQUEST %q", v)
		}
		c.ImageFetchMaxPerRequest = n
	}
	c.ImageCacheEntries = 64
	if v := os.Getenv("IMAGE_CACHE_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("invalid IMAGE_CACHE_ENTRIES %q (0 disables)", v)
		}
		c.ImageCacheEntries = n
	}
	c.ImageCacheTTL = 10 * time.Minute
	if v := os.Getenv("IMAGE_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("invalid IMAGE_CACHE_TTL %q", v)
		}
		c.ImageCacheTTL = d
	}
	c.MaxChunksPerDocument = 20000
	if v := os.Getenv("MAX_CHUNKS_PER_DOCUMENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid MAX_CHUNKS_PER_DOCUMENT %q", v)
		}
		c.MaxChunksPerDocument = n
	}
	if v := os.Getenv("MAX_DOCUMENTS_PER_STORE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("invalid MAX_DOCUMENTS_PER_STORE %q", v)
		}
		c.MaxDocumentsPerStore = n
	}
	if v := os.Getenv("MAX_BYTES_PER_STORE_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > math.MaxInt64>>20 {
			return c, fmt.Errorf("invalid MAX_BYTES_PER_STORE_MB %q", v)
		}
		c.MaxBytesPerStore = int64(n) << 20
	}
	return c, nil
}

// envOrFile returns the trimmed value of NAME, or the trimmed content of the
// file named by NAME_FILE when NAME is unset (the Docker/Compose secrets
// convention).
func envOrFile(name string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, nil
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// PortFromEnv reads PORT (default 8765). It is separate from Load so the
// container healthcheck can probe the server without a full configuration.
func PortFromEnv() (int, error) {
	v := os.Getenv("PORT")
	if v == "" {
		return 8765, nil
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
