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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/bm25"
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
	// RerankTimeout overrides both reranker timeouts: the LLM backend's
	// default 10 s (a full chat completion) and an API backend's 5 s (a
	// single scoring call). Zero keeps each default.
	RerankTimeout time.Duration
	// PgSearchTokenizer names the ParadeDB analyser for the BM25 index.
	// The index is global, so it cannot be a per-store setting.
	PgSearchTokenizer string
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
	// ImageFetchMaxConcurrent caps the image fetches the whole process runs
	// at once, which is what bounds the fan-out across requests; it is also
	// the image transport's per-host connection ceiling.
	ImageFetchMaxConcurrent int
	// ImageCacheEntries is the size of the fetched-image cache; 0 disables it.
	ImageCacheEntries int
	// ImageCacheMaxBytes is the fetched-image cache's byte ceiling. It is
	// derived from ImageFetchMaxBytes unless IMAGE_CACHE_MAX_MB sets it.
	ImageCacheMaxBytes int64
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

	// MetricsEnabled serves the Prometheus endpoint at all.
	MetricsEnabled bool
	// MetricsToken is the bearer token /metrics requires. It may only be
	// empty when MetricsListen binds a loopback address.
	MetricsToken string
	// MetricsListen, when set, moves /metrics onto a second HTTP server on
	// that address and keeps it off the main router entirely, so no
	// reverse-proxy rule can expose it by accident.
	MetricsListen string
	// MetricsMaxSeries caps the registry's label combinations.
	MetricsMaxSeries int

	// TracingEnabled turns the OTLP exporter on. It defaults to true when
	// an endpoint is configured; an explicit TRACING_ENABLED=false wins.
	TracingEnabled bool
	// TracingTrustIncoming honours a client's traceparent header. Off by
	// default: a client that is trusted can pin every request into one
	// trace and force the sampled flag on all of it.
	TracingTrustIncoming bool
	// OTLPEndpoint is the collector's OTLP/HTTP base URL (or the full
	// traces URL).
	OTLPEndpoint string
	// OTLPHeaders are sent with every export request (auth for a vendor
	// endpoint, usually nothing for a local Collector).
	OTLPHeaders map[string]string
	// ServiceName and ResourceAttrs describe this process to the collector.
	ServiceName   string
	ResourceAttrs map[string]string
	// TraceSampleRatio is the head sampling probability for new traces.
	TraceSampleRatio float64
}

// Load reads configuration from the environment, applying defaults.
// pgSearchTokenizerPattern bounds the tokenizer name. It is the only part of
// the BM25 index DDL that is not a bind parameter, so it is validated before
// it can reach a statement.
var pgSearchTokenizerPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

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
		PgSearchTokenizer: "default",
		DatabaseURL:       dbURL,
		DBMaxConns:        10,
		SecretKeyHex:      secretKey,
		DataDir:           env("DATA_DIR", "/app/data"),
		Port:              8765,
		AdminUser:         env("ADMIN_USER", "admin"),
		AdminPassword:     os.Getenv("ADMIN_PASSWORD"),
		LogLevel:          env("LOG_LEVEL", "info"),
		SessionTTL:        24 * time.Hour,
		UpstreamTimeout:   5 * time.Minute,
		IngestWorkers:     2,
		MaxUploadBytes:    50 << 20,

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
	if v := os.Getenv("RERANK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("invalid RERANK_TIMEOUT %q: %w", v, err)
		}
		if d <= 0 {
			return c, fmt.Errorf("RERANK_TIMEOUT must be positive, got %q", v)
		}
		c.RerankTimeout = d
	}
	if v := strings.TrimSpace(os.Getenv("PG_SEARCH_TOKENIZER")); v != "" {
		// Interpolated into the index DDL, so the shape is checked here
		// rather than trusted at the point of use.
		if !pgSearchTokenizerPattern.MatchString(v) {
			return c, fmt.Errorf("invalid PG_SEARCH_TOKENIZER %q: expected a name like \"default\" or \"en_stem\"", v)
		}
		// A "<code>_stem" name is translated to a Snowball language from a
		// table in the store package, so one that is not in it can only fail.
		// Caught here it is a startup error; left alone it would be a warning
		// on every search and a store silently stuck on the pgvector
		// fallback for the life of the process.
		if err := bm25.ValidateTokenizer(v); err != nil {
			return c, fmt.Errorf("invalid PG_SEARCH_TOKENIZER: %w", err)
		}
		c.PgSearchTokenizer = v
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
	// IMAGE_FETCH_MAX_PER_REQUEST bounds one request and nothing across
	// them: this is the process-wide ceiling, and the image transport's
	// per-host connection limit is set from it too.
	c.ImageFetchMaxConcurrent = 16
	if v := os.Getenv("IMAGE_FETCH_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid IMAGE_FETCH_MAX_CONCURRENT %q", v)
		}
		c.ImageFetchMaxConcurrent = n
	}
	c.ImageCacheEntries = 64
	if v := os.Getenv("IMAGE_CACHE_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("invalid IMAGE_CACHE_ENTRIES %q (0 disables)", v)
		}
		c.ImageCacheEntries = n
	}
	// The cache holds base64, which is a third larger than the bytes
	// IMAGE_FETCH_MAX_MB caps. A fixed 64 MiB ceiling therefore turned the
	// cache off silently the moment an operator raised the per-image limit
	// past it: every entry was too large to store, so nothing ever was. The
	// default now holds at least two images of the largest size the fetcher
	// will accept, and a ceiling too small to hold even one is refused at
	// startup rather than discovered as a cache that never hits.
	oneImage := Base64Len(c.ImageFetchMaxBytes)
	c.ImageCacheMaxBytes = 64 << 20
	if floor := 2 * oneImage; floor > c.ImageCacheMaxBytes {
		c.ImageCacheMaxBytes = floor
	}
	// The derivation follows IMAGE_FETCH_MAX_MB and must not follow it
	// anywhere: IMAGE_FETCH_MAX_MB=512 would otherwise hand a memory-limited
	// container a 1.3 GiB cache nobody asked for. Past this an operator who
	// wants a larger one says so, and startup says the cache will not hold
	// images that size.
	if c.ImageCacheMaxBytes > maxDerivedImageCacheBytes {
		c.ImageCacheMaxBytes = maxDerivedImageCacheBytes
	}
	if v := os.Getenv("IMAGE_CACHE_MAX_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid IMAGE_CACHE_MAX_MB %q", v)
		}
		if int64(n)<<20 < oneImage {
			return c, fmt.Errorf("IMAGE_CACHE_MAX_MB %q cannot hold one IMAGE_FETCH_MAX_MB image "+
				"(%d MiB of base64), so nothing would ever be cached", v, (oneImage+(1<<20)-1)>>20)
		}
		c.ImageCacheMaxBytes = int64(n) << 20
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
	if err := loadMetrics(&c); err != nil {
		return c, err
	}
	if err := loadTracing(&c); err != nil {
		return c, err
	}
	return c, nil
}

// loadMetrics reads the /metrics settings and refuses the one combination
// that would publish per-project usage and spend to anyone who can reach the
// port.
func loadMetrics(c *Config) error {
	// Only the two spellings TRACING_ENABLED accepts. A silent fallback to
	// false would turn METRICS_ENABLED=1 or =yes into metrics that never
	// appear, which is discovered by missing dashboards rather than by the
	// start-up that could have said so.
	switch strings.TrimSpace(os.Getenv("METRICS_ENABLED")) {
	case "true":
		c.MetricsEnabled = true
	case "false", "":
		c.MetricsEnabled = false
	default:
		return fmt.Errorf("invalid METRICS_ENABLED %q (true or false)", os.Getenv("METRICS_ENABLED"))
	}
	token, err := envOrFile("METRICS_TOKEN")
	if err != nil {
		return err
	}
	c.MetricsToken = token
	c.MetricsListen = strings.TrimSpace(os.Getenv("METRICS_LISTEN"))
	c.MetricsMaxSeries = 5000
	if v := os.Getenv("METRICS_MAX_SERIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid METRICS_MAX_SERIES %q", v)
		}
		c.MetricsMaxSeries = n
	}
	if c.MetricsListen != "" {
		host, _, err := net.SplitHostPort(c.MetricsListen)
		if err != nil {
			return fmt.Errorf("invalid METRICS_LISTEN %q: expected host:port, e.g. 127.0.0.1:9090", c.MetricsListen)
		}
		if !c.MetricsEnabled {
			return errors.New("METRICS_LISTEN is set but METRICS_ENABLED is not true; nothing would listen on it")
		}
		if c.MetricsToken == "" && !isLoopbackHost(host) {
			return errors.New("METRICS_ENABLED without METRICS_TOKEN would publish per-project usage and spend " +
				"unauthenticated; set METRICS_TOKEN or bind METRICS_LISTEN to a loopback address")
		}
		return nil
	}
	if c.MetricsEnabled && c.MetricsToken == "" {
		return errors.New("METRICS_ENABLED without METRICS_TOKEN would publish per-project usage and spend " +
			"unauthenticated; set METRICS_TOKEN or bind METRICS_LISTEN to a loopback address")
	}
	return nil
}

// isLoopbackHost reports whether a METRICS_LISTEN host only accepts
// connections from this machine. An empty host means every interface, which
// is the opposite of loopback.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Base64Len is what n bytes take once base64-encoded, which is the form the
// image cache actually stores and measures.
func Base64Len(n int64) int64 { return (n + 2) / 3 * 4 }

// maxDerivedImageCacheBytes caps the ceiling Load derives from
// IMAGE_FETCH_MAX_MB. IMAGE_CACHE_MAX_MB overrides it in either direction.
const maxDerivedImageCacheBytes = 256 << 20

// loadTracing reads the OTLP settings. The OTEL_* names are the ones the
// OpenTelemetry specification defines, so a collector sidecar that already
// injects them into the environment needs no Ragmux-specific variable.
func loadTracing(c *Config) error {
	c.OTLPEndpoint = strings.TrimSpace(env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))))
	// Tracing follows the endpoint: configuring one is the act of turning it
	// on. An explicit false always wins, so a sidecar that injects the
	// OTEL_* variables can still be switched off with one setting.
	c.TracingEnabled = c.OTLPEndpoint != ""
	switch strings.TrimSpace(os.Getenv("TRACING_ENABLED")) {
	case "true":
		c.TracingEnabled = true
	case "false":
		c.TracingEnabled = false
	case "":
	default:
		return fmt.Errorf("invalid TRACING_ENABLED %q (true or false)", os.Getenv("TRACING_ENABLED"))
	}
	if c.TracingEnabled && c.OTLPEndpoint == "" {
		return errors.New("TRACING_ENABLED=true without OTEL_EXPORTER_OTLP_ENDPOINT " +
			"(or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT); point it at an OpenTelemetry Collector, e.g. http://otel-collector:4318")
	}
	c.TracingTrustIncoming = os.Getenv("TRACING_TRUST_INCOMING") == "true"
	c.ServiceName = env("OTEL_SERVICE_NAME", "ragmux")
	var err error
	if c.OTLPHeaders, err = parseKeyValues("OTEL_EXPORTER_OTLP_HEADERS", os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")); err != nil {
		return err
	}
	if c.ResourceAttrs, err = parseKeyValues("OTEL_RESOURCE_ATTRIBUTES", os.Getenv("OTEL_RESOURCE_ATTRIBUTES")); err != nil {
		return err
	}
	c.TraceSampleRatio = 0.05
	if v := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("invalid OTEL_TRACES_SAMPLER_ARG %q (a ratio between 0 and 1)", v)
		}
		c.TraceSampleRatio = f
	}
	return nil
}

// parseKeyValues parses the "k=v,k2=v2" form both OTEL_EXPORTER_OTLP_HEADERS
// and OTEL_RESOURCE_ATTRIBUTES use. A value may contain "=", so only the
// first one separates.
func parseKeyValues(name, raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid %s entry %q (expected key=value)", name, pair)
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
