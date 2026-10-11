package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHasNoTxMarker(t *testing.T) {
	cases := map[string]bool{
		"-- ragmux:no-transaction\nCREATE INDEX CONCURRENTLY i ON t (c);": true,
		"-- ragmux:no-transaction\r\nSELECT 1;":                           true,
		"  -- ragmux:no-transaction  \nSELECT 1;":                         true,
		"-- header\n-- ragmux:no-transaction\nSELECT 1;":                  false,
		"ALTER TABLE t ADD COLUMN c TEXT;":                                false,
		"":                                                                false,
	}
	for body, want := range cases {
		if got := hasNoTxMarker([]byte(body)); got != want {
			t.Errorf("hasNoTxMarker(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestConcurrentIndexRE(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON t (c);":                                             "idx_a",
		"create unique index concurrently idx_b on t (c)":                                                     "idx_b",
		"-- note\nCREATE INDEX CONCURRENTLY\n  idx_c\n  ON t (c) WHERE c > 0;":                                "idx_c",
		"-- CREATE INDEX CONCURRENTLY cannot run in a transaction\nCREATE INDEX CONCURRENTLY idx_f ON t (c);": "idx_f",
		"DROP INDEX CONCURRENTLY IF EXISTS idx_d;":                                                            "",
		"CREATE INDEX idx_e ON t (c);":                                                                        "",
	}
	for body, want := range cases {
		got := concurrentIndexName(body)
		if got != want {
			t.Errorf("index name in %q = %q, want %q", body, got, want)
		}
	}
}

func TestClampClientText(t *testing.T) {
	if got := clampClientText("gpt-4o"); got != "gpt-4o" {
		t.Errorf("short value changed: %q", got)
	}
	if got := clampClientText("a\x00b"); got != "ab" {
		t.Errorf("NUL not removed: %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := clampClientText(long); len(got) != 256 {
		t.Errorf("len = %d, want 256", len(got))
	}
	// A multi-byte character straddling the limit is dropped whole.
	got := clampClientText(strings.Repeat("a", 255) + "\u00e9\u00e9")
	if !utf8.ValidString(got) || got != strings.Repeat("a", 255) {
		t.Errorf("cut inside a character: %q", got)
	}
}
