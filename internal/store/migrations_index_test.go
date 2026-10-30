package store_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

const requestIDIndex = "idx_request_logs_request_id"

// indexState reads the validity and uniqueness of the request id index.
func indexState(t *testing.T, s *store.Store) (found, valid, unique bool) {
	t.Helper()
	var v, u *bool
	err := s.DB().QueryRow(context.Background(),
		`SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)),
		        (SELECT indisunique FROM pg_index WHERE indexrelid = to_regclass($1))`, requestIDIndex).Scan(&v, &u)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return false, false, false
	}
	return true, *v, *u
}

func migrationRecorded(t *testing.T, s *store.Store, version int) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(context.Background(), "SELECT count(*) FROM schema_migrations WHERE version = $1", version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// TestInterruptedIndexBuildIsRepaired leaves behind exactly what an
// interrupted CREATE INDEX CONCURRENTLY does -- an INVALID index of the right
// name -- and checks the next start rebuilds it instead of recording
// migration 0017 on top of it.
func TestInterruptedIndexBuildIsRepaired(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if found, valid, _ := indexState(t, s); !found || !valid {
		t.Fatalf("fresh database: index found=%v valid=%v, want a valid index", found, valid)
	}

	// A unique build over duplicate values fails and leaves an INVALID index.
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "dup"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		"DROP INDEX " + requestIDIndex,
		"CREATE UNIQUE INDEX CONCURRENTLY " + requestIDIndex + " ON request_logs (request_id)",
	} {
		_, err := s.DB().Exec(ctx, stmt)
		if stmt[:6] == "DROP I" && err != nil {
			t.Fatal(err)
		}
	}
	if found, valid, unique := indexState(t, s); !found || valid || !unique {
		t.Fatalf("setup: index found=%v valid=%v unique=%v, want an invalid unique index", found, valid, unique)
	}
	if _, err := s.DB().Exec(ctx, "DELETE FROM schema_migrations WHERE version = 17"); err != nil {
		t.Fatal(err)
	}

	s2 := testdb.OpenWith(t, cfg)
	found, valid, unique := indexState(t, s2)
	if !found || !valid || unique {
		t.Errorf("after restart: index found=%v valid=%v unique=%v, want a valid non-unique index", found, valid, unique)
	}
	if !migrationRecorded(t, s2, 17) {
		t.Error("migration 17 was not recorded after the rebuild")
	}
}

// TestNoTxMigrationWaitsForTheBuildLock holds the lock another replica's index
// build would hold and checks Open waits for it, then finishes the migration.
func TestNoTxMigrationWaitsForTheBuildLock(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if _, err := s.DB().Exec(ctx, "DELETE FROM schema_migrations WHERE version = 17"); err != nil {
		t.Fatal(err)
	}

	holder, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", store.LockNoTxMigrations); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		s2, err := store.Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil {
			err = s2.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Open returned while the build lock was held: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if migrationRecorded(t, s, 17) {
		t.Fatal("migration 17 recorded while another instance held the build lock")
	}

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", store.LockNoTxMigrations); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open after the lock was released: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Open did not finish after the build lock was released")
	}
	if !migrationRecorded(t, s, 17) {
		t.Error("migration 17 not recorded after the lock was released")
	}
}

// TestDocumentMetadataMigrationsApply: 0023 adds the column with its default,
// 0024 builds the GIN index outside a transaction, and both are recorded.
func TestDocumentMetadataMigrationsApply(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for _, v := range []int{23, 24} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded", v)
		}
	}
	var valid *bool
	var def string
	if err := s.DB().QueryRow(ctx, `SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('idx_documents_metadata')),
		(SELECT column_default FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'documents' AND column_name = 'metadata')`).Scan(&valid, &def); err != nil {
		t.Fatal(err)
	}
	if valid == nil || !*valid {
		t.Errorf("idx_documents_metadata: valid = %v", valid)
	}
	if !strings.Contains(def, "'{}'") {
		t.Errorf("documents.metadata default = %q", def)
	}
}
