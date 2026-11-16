package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// TestEndUserMigrationsApply: 0033 and 0034 are recorded, the project ceilings
// default to unlimited, and the end-user index is valid and partial.
func TestEndUserMigrationsApply(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for _, v := range []int{33, 34} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded", v)
		}
	}
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if p.EndUserRPM != 0 || p.EndUserTPM != 0 || p.EndUserDailyTokens != 0 || p.EndUserMonthlyTokens != 0 {
		t.Errorf("project defaults = %+v, want unlimited", p)
	}
	var valid *bool
	var def, cols string
	if err := s.DB().QueryRow(ctx, `SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('idx_request_logs_end_user')),
		(SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = to_regclass('idx_request_logs_end_user')),
		(SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid = to_regclass('idx_request_logs_end_user'))`).Scan(&valid, &def, &cols); err != nil {
		t.Fatal(err)
	}
	if valid == nil || !*valid {
		t.Errorf("idx_request_logs_end_user: valid = %v", valid)
	}
	if !strings.Contains(def, "end_user IS NOT NULL") || !strings.Contains(cols, "(project_id, end_user, created_at)") {
		t.Errorf("idx_request_logs_end_user = %q (predicate %q), want a partial (project_id, end_user, created_at) index", cols, def)
	}
	// There is deliberately no index on tags.
	var n int
	if err := s.DB().QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'request_logs' AND indexdef LIKE '%tags%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("an index on tags exists (%d, %v)", n, err)
	}
}

// TestMigration0033LeavesProjectsReadableWhileItWaits reproduces the deploy
// scenario behind the lock order of 0033: a long read of request_logs (a CSV
// export, a summary) is running when the migration starts. The migration waits
// for it, and while it does the gateway must still be able to read projects,
// which it does on every request. A migration that altered projects first would
// hold its ACCESS EXCLUSIVE lock through the wait and stop every request.
func TestMigration0033LeavesProjectsReadableWhileItWaits(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	script, err := os.ReadFile("migrations/0033_end_user_tags.sql")
	if err != nil {
		t.Fatal(err)
	}

	reader, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	var n int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}

	mig, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mig.Rollback(ctx) }()
	done := make(chan error, 1)
	go func() {
		_, err := mig.Exec(ctx, string(script))
		done <- err
	}()

	// Wait until the migration is really queued behind the reader.
	waitForTableLockWaiter(t, s, done)

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var projects int
	if err := s.DB().QueryRow(readCtx, "SELECT count(*) FROM projects").Scan(&projects); err != nil {
		t.Errorf("reading projects while the migration waits for request_logs: %v", err)
	}

	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after the reader finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the migration did not finish after the reader did")
	}
	if err := mig.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestMigration0033TimesOutInsteadOfStallingRequests runs the real runner while
// a long read holds request_logs. The migration must give up after its
// lock_timeout, and the requests that queued behind its lock request -- the
// gateway writes a request_logs row at the end of every request -- must go
// through instead of hanging until the read ends. The failed start then
// succeeds on a retry once the read is over, which is what the process manager
// does for a replica that exited.
func TestMigration0033TimesOutInsteadOfStallingRequests(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The migration is applied again, as on a replica that has not seen it.
	if _, err := s.DB().Exec(ctx, "DELETE FROM schema_migrations WHERE version = 33"); err != nil {
		t.Fatal(err)
	}

	reader, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	var n int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opened := make(chan error, 1)
	go func() {
		s2, err := store.Open(ctx, cfg, log)
		if err == nil {
			err = s2.Close()
		}
		opened <- err
	}()

	// Wait until the migration's lock request is queued behind the reader;
	// the timeouts below count from here.
	started := waitForTableLockWaiter(t, s, nil)

	// A request finishing now writes its row. It queues behind the migration's
	// lock request and must be let through when that gives up.
	insertDone := make(chan error, 1)
	go func() {
		insertDone <- s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "queued"})
	}()

	select {
	case err := <-opened:
		if err == nil {
			t.Fatal("Open succeeded although the reader held request_logs")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("Open failed with %v, want a lock timeout (SQLSTATE 55P03)", err)
		}
		if took := time.Since(started); took > 8*time.Second {
			t.Errorf("Open gave up after %v, want about the 2s lock_timeout", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Open is still waiting: the migration has no lock timeout")
	}
	select {
	case err := <-insertDone:
		if err != nil {
			t.Fatalf("the queued request log write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request log write is still stuck behind the migration after it gave up")
	}
	if took := time.Since(started); took > 8*time.Second {
		t.Errorf("the queued write finished after %v, long after the timeout", took)
	}
	// Nothing was half applied: the failed run recorded nothing.
	if migrationRecorded(t, s, 33) {
		t.Fatal("migration 33 was recorded by a run that timed out")
	}

	// The read ends; the restarted replica goes through.
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s3, err := store.Open(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Open after the read finished: %v", err)
	}
	_ = s3.Close()
	if !migrationRecorded(t, s, 33) {
		t.Error("migration 33 is not recorded after the retry")
	}
}

// waitForTableLockWaiter waits until a backend is queued on a table lock in
// the test's own schema, and returns the moment it saw that. Only such a lock
// counts: the packages of one `go test ./...` run share the database, and their
// migration runs queue on the database-wide migration advisory lock, so a Lock
// wait in pg_stat_activity may well be theirs. For the same reason store.Open
// can spend a while queued on that advisory lock before it reaches the table;
// the deadline allows for it, and a test counts its timeouts from the returned
// moment. If done is not nil and receives first, the waiter never queued.
func waitForTableLockWaiter(t *testing.T, s *store.Store, done <-chan error) time.Time {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var waiting int
		if err := s.DB().QueryRow(context.Background(), `SELECT count(*)
			FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_class c ON c.oid = l.relation
			WHERE l.locktype = 'relation' AND NOT l.granted
			  AND l.database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
			  AND c.relnamespace = pg_catalog.current_schema()::regnamespace`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return time.Now()
		}
		select {
		case err := <-done:
			t.Fatalf("the migration finished (%v) without waiting for the lock holder", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the migration never queued behind the lock holder")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUpgradeFromV07DoesNotStallRequests is the release path: a v0.7 database
// has migrations up to 0030 and the first v0.8 replica runs 0031 to 0034, of
// which the test replays 0032, 0033 and 0034 one after the other, with a long
// read open on request_logs. The first of them to ask for the table must give
// up on its own timeout, with the gateway's two
// queries -- the project read that authenticates a request and the request log
// write that ends it -- going through the whole time, and the retry after the
// read must apply all three.
func TestUpgradeFromV07DoesNotStallRequests(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(ctx, "DELETE FROM schema_migrations WHERE version IN (32, 33, 34)"); err != nil {
		t.Fatal(err)
	}

	reader, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	var n int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opened := make(chan error, 1)
	go func() {
		s2, err := store.Open(ctx, cfg, log)
		if err == nil {
			err = s2.Close()
		}
		opened <- err
	}()
	started := waitForTableLockWaiter(t, s, nil)

	// The project read of a request does not wait for the migration.
	readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	if _, err := s.GetProject(readCtx, p.ID); err != nil {
		t.Errorf("reading the project while the upgrade waits: %v", err)
	}
	cancel()
	// The log write of a request queues, and is let through when the migration
	// gives up.
	insertDone := make(chan error, 1)
	go func() {
		insertDone <- s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "queued"})
	}()

	select {
	case err := <-opened:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("Open = %v, want a lock timeout (SQLSTATE 55P03)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Open is still waiting: the upgrade has no lock timeout")
	}
	select {
	case err := <-insertDone:
		if err != nil {
			t.Fatalf("the queued request log write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request log write is still stuck behind the upgrade after it gave up")
	}
	if took := time.Since(started); took > 8*time.Second {
		t.Errorf("the write finished after %v, long after the 2s timeout", took)
	}
	for _, v := range []int{32, 33} {
		if migrationRecorded(t, s, v) {
			t.Errorf("migration %d was recorded by a run that timed out", v)
		}
	}

	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s3, err := store.Open(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Open after the read finished: %v", err)
	}
	_ = s3.Close()
	for _, v := range []int{32, 33, 34} {
		if !migrationRecorded(t, s, v) {
			t.Errorf("migration %d is not recorded after the retry", v)
		}
	}
}

// TestMigration0033RollsBackWhenItsSecondStepTimesOut: the request_logs step is
// done when the projects step cannot get its lock. Nothing of 0033 may remain,
// and the retry applies it whole.
func TestMigration0033RollsBackWhenItsSecondStepTimesOut(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	for _, stmt := range []string{
		"DROP TABLE end_user_usage",
		"ALTER TABLE request_logs DROP COLUMN end_user, DROP COLUMN tags",
		`ALTER TABLE projects DROP COLUMN end_user_rpm, DROP COLUMN end_user_tpm,
			DROP COLUMN end_user_daily_tokens, DROP COLUMN end_user_monthly_tokens`,
		"DELETE FROM schema_migrations WHERE version = 33",
	} {
		if _, err := s.DB().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	reader, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	var n int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&n); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opened := make(chan error, 1)
	go func() {
		s2, err := store.Open(ctx, cfg, log)
		if err == nil {
			err = s2.Close()
		}
		opened <- err
	}()
	waitForTableLockWaiter(t, s, nil)
	select {
	case err := <-opened:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("Open = %v, want a lock timeout (SQLSTATE 55P03)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Open is still waiting for projects")
	}
	var cols, tables int
	if err := s.DB().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
			AND table_name = 'request_logs' AND column_name IN ('end_user', 'tags')),
		(SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()
			AND table_name = 'end_user_usage')`).Scan(&cols, &tables); err != nil {
		t.Fatal(err)
	}
	if cols != 0 || tables != 0 || migrationRecorded(t, s, 33) {
		t.Errorf("a timed-out 0033 left request_logs columns=%d end_user_usage=%d recorded=%v", cols, tables, migrationRecorded(t, s, 33))
	}

	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s3, err := store.Open(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Open after the read finished: %v", err)
	}
	_ = s3.Close()
	if !migrationRecorded(t, s, 33) {
		t.Error("migration 33 is not recorded after the retry")
	}
}
