// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the gateway reads at startup.
type Config struct {
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
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	c := Config{
		DataDir:         env("DATA_DIR", "/app/data"),
		Port:            8080,
		AdminUser:       env("ADMIN_USER", "admin"),
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
		LogLevel:        env("LOG_LEVEL", "info"),
		SessionTTL:      24 * time.Hour,
		UpstreamTimeout: 5 * time.Minute,
		IngestWorkers:   2,
		MaxUploadBytes:  50 << 20,
	}
	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p <= 0 || p > 65535 {
			return c, fmt.Errorf("invalid PORT %q", v)
		}
		c.Port = p
	}
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
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
