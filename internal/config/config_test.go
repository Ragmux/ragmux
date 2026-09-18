package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
