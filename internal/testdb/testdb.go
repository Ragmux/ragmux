// Package testdb opens isolated PostgreSQL schemas for tests. Every call
// creates a fresh schema on the database named by TEST_DATABASE_URL and
// drops it when the test ends, so tests can run in parallel against one
// server (the CI service container or `make dev-db`).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/store"
)

// EnvVar names the connection string tests read.
const EnvVar = "TEST_DATABASE_URL"

// Config returns an OpenConfig pointing at a brand-new schema (dropped on
// cleanup) with a random SECRET_KEY. Opening several stores from the same
// config sees the same data, which is how tests simulate a restart. The test
// is skipped when TEST_DATABASE_URL is unset.
func Config(t *testing.T) store.OpenConfig {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s is not set; start a pgvector Postgres (make dev-db) and export it to run this test", EnvVar)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	schema := "test_" + randHex(6)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvVar, err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create test schema: %v", err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Logf("cleanup: connect: %v", err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
	})

	// The vector extension lives in public, so it must stay on the path.
	q := u.Query()
	q.Set("options", "-c search_path="+schema+",public")
	// libpq-style parsing does not treat "+" as a space, so encode it as %20.
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return store.OpenConfig{
		DatabaseURL:  u.String(),
		MaxConns:     4,
		SecretKeyHex: randHex(32),
		DataDir:      t.TempDir(),
	}
}

// Open opens a store on a fresh schema and closes it when the test ends.
func Open(t *testing.T) *store.Store {
	t.Helper()
	return OpenWith(t, Config(t))
}

// OpenWith opens a store for an existing test config (for restart tests).
func OpenWith(t *testing.T, cfg store.OpenConfig) *store.Store {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s, err := store.Open(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// RequirePgSearch skips the test unless the server behind s carries a usable
// ParadeDB pg_search. CI runs the suite against a plain pgvector image on
// purpose -- that job is what proves the fallback path -- so everything that
// needs BM25 is capability-gated rather than assumed.
func RequirePgSearch(t *testing.T, s *store.Store) {
	t.Helper()
	if !s.Caps().PgSearch {
		t.Skipf("pg_search is not available on this server; start a ParadeDB Postgres (make dev-db-paradedb) to run this test")
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
