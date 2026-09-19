package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadReadsSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("  "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db"), []byte("postgres://u:p@h/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SECRET_KEY", "")
	t.Setenv("DATABASE_URL_FILE", filepath.Join(dir, "db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(dir, "key"))
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.168.1.5,fd00::/8")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://u:p@h/db" || c.SecretKeyHex != key {
		t.Errorf("file values not read: %q %q", c.DatabaseURL, c.SecretKeyHex)
	}
	if len(c.TrustedProxyCIDRs) != 3 || c.TrustedProxyCIDRs[1].String() != "192.168.1.5/32" {
		t.Errorf("cidrs = %v", c.TrustedProxyCIDRs)
	}

	// The plain variable wins over the file.
	t.Setenv("DATABASE_URL", "postgres://plain")
	if c, err := Load(); err != nil || c.DatabaseURL != "postgres://plain" {
		t.Errorf("plain variable should win: %q %v", c.DatabaseURL, err)
	}

	t.Setenv("DATABASE_URL_FILE", filepath.Join(dir, "missing"))
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL_FILE") {
		t.Errorf("unreadable file should be reported, got %v", err)
	}

	t.Setenv("DATABASE_URL", "postgres://plain")
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-network")
	if _, err := Load(); err == nil {
		t.Error("bad CIDR should fail")
	}
}

func TestLoadImageSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@h/db")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ImageFetch || c.ImageFetchMaxBytes != 8<<20 || c.ImageFetchTimeout != 10*time.Second ||
		c.ImageFetchMaxPerRequest != 8 || c.ImageCacheEntries != 64 || c.ImageCacheTTL != 10*time.Minute {
		t.Errorf("defaults = %+v", c)
	}

	t.Setenv("IMAGE_FETCH", "false")
	t.Setenv("IMAGE_FETCH_MAX_MB", "2")
	t.Setenv("IMAGE_FETCH_TIMEOUT", "3s")
	t.Setenv("IMAGE_FETCH_MAX_PER_REQUEST", "1")
	// 0 entries is the documented way to turn caching off, not an error.
	t.Setenv("IMAGE_CACHE_ENTRIES", "0")
	t.Setenv("IMAGE_CACHE_TTL", "45s")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ImageFetch || c.ImageFetchMaxBytes != 2<<20 || c.ImageFetchTimeout != 3*time.Second ||
		c.ImageFetchMaxPerRequest != 1 || c.ImageCacheEntries != 0 || c.ImageCacheTTL != 45*time.Second {
		t.Errorf("overrides = %+v", c)
	}

	for _, bad := range []struct{ key, value string }{
		{"IMAGE_FETCH_MAX_MB", "0"},
		{"IMAGE_FETCH_MAX_MB", "huge"},
		{"IMAGE_FETCH_TIMEOUT", "-1s"},
		{"IMAGE_FETCH_TIMEOUT", "soon"},
		{"IMAGE_FETCH_MAX_PER_REQUEST", "0"},
		{"IMAGE_CACHE_ENTRIES", "-1"},
		{"IMAGE_CACHE_TTL", "0"},
	} {
		t.Run(bad.key+"="+bad.value, func(t *testing.T) {
			t.Setenv(bad.key, bad.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), bad.key) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// The tokenizer is the one part of the BM25 index DDL that is interpolated
// rather than bound, so a value that is not a plain identifier must be
// refused at boot. Falling back to the default instead would leave an
// operator who typed "en-stem" silently running an unstemmed index.
func TestPgSearchTokenizerRejectsInjection(t *testing.T) {
	base := func() { t.Setenv("DATABASE_URL", "postgres://x/y"); t.Setenv("SECRET_KEY", strings.Repeat("a", 64)) }
	for _, bad := range []string{"default'} , x => '", "DROP TABLE", "en stem", "en-stem", "En_Stem", "1stem"} {
		base()
		t.Setenv("PG_SEARCH_TOKENIZER", bad)
		if _, err := Load(); err == nil {
			t.Errorf("PG_SEARCH_TOKENIZER %q was accepted", bad)
		}
	}
	base()
	t.Setenv("PG_SEARCH_TOKENIZER", "en_stem")
	c, err := Load()
	if err != nil {
		t.Fatalf("en_stem rejected: %v", err)
	}
	if c.PgSearchTokenizer != "en_stem" {
		t.Errorf("tokenizer = %q", c.PgSearchTokenizer)
	}
}

func TestRerankTimeoutMustParse(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("SECRET_KEY", strings.Repeat("a", 64))
	for _, bad := range []string{"5", "-1s", "soon"} {
		t.Setenv("RERANK_TIMEOUT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("RERANK_TIMEOUT %q was accepted", bad)
		}
	}
	t.Setenv("RERANK_TIMEOUT", "8s")
	c, err := Load()
	if err != nil {
		t.Fatalf("8s rejected: %v", err)
	}
	if c.RerankTimeout != 8*time.Second {
		t.Errorf("timeout = %v", c.RerankTimeout)
	}
}
