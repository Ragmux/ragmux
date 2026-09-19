package store

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Capabilities are the optional server features detected at Open. They are
// read once: an extension installed while the gateway runs takes effect on
// the next restart, the same rule that applies to migrations.
type Capabilities struct {
	// PgSearch reports that ParadeDB's pg_search extension is installed and
	// speaks the query API this version uses.
	PgSearch bool
	// PgSearchVersion is the installed extension version, empty when absent.
	PgSearchVersion string
}

// Caps reports the optional server features detected at Open.
func (s *Store) Caps() Capabilities { return s.caps }

// detectCapabilities asks the server which optional extensions it carries.
// A present extension is additionally probed with a smoke query: a pg_search
// whose query API we do not speak is downgraded to absent here, so the
// mismatch becomes a logged capability downgrade at boot instead of a 500 on
// the first hybrid search.
func detectCapabilities(ctx context.Context, conn *pgx.Conn, log *slog.Logger) Capabilities {
	var caps Capabilities
	err := conn.QueryRow(ctx,
		`SELECT COALESCE((SELECT extversion FROM pg_extension WHERE extname = 'pg_search'), '')`,
	).Scan(&caps.PgSearchVersion)
	if err != nil {
		log.Warn("detect search backends: query pg_extension", "err", err)
		return Capabilities{}
	}
	if caps.PgSearchVersion != "" {
		caps.PgSearch = true
		if err := pgSearchSmokeTest(ctx, conn); err != nil {
			log.Warn("pg_search is installed but its query API is not the one this version speaks; "+
				"rag stores configured for it fall back to the pgvector backend",
				"pg_search_version", caps.PgSearchVersion, "err", err)
			caps.PgSearch = false
		}
	}
	log.Info("search backends",
		"pgvector", true,
		"pg_search", caps.PgSearch,
		"pg_search_version", caps.PgSearchVersion)
	return caps
}

// pgSearchSmokeTest checks that the installed pg_search speaks the query API
// the hybrid search is written against.
//
// It builds the query object and nothing else. That one statement exercises
// every function, overload and named argument search_pgsearch.go uses --
// paradedb.boolean(must => ARRAY[...]), paradedb.term on a bigint field,
// paradedb.match on a text field -- and it touches no table and no index.
//
// Running an actual @@@ search here would be a better probe and is not
// possible: pg_search refuses the operator until the table carries a BM25
// index ("`chunks` does not contain a `USING bm25` index"), and that index is
// built lazily on the first search of a store configured for this backend.
// A probe that needed it would downgrade every ParadeDB server that has not
// run such a search yet -- which is every server, because the downgrade is
// what stops the index from ever being built. The operator and
// paradedb.score are checked in the catalogue instead.
func pgSearchSmokeTest(ctx context.Context, conn *pgx.Conn) error {
	var query string
	err := conn.QueryRow(ctx,
		`SELECT paradedb.boolean(must => ARRAY[paradedb.term('rag_store_id', $1::bigint),
		                                       paradedb.match('content', $2::text)])::text`,
		int64(0), "ragmux").Scan(&query)
	if err != nil {
		return err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		        WHERE n.nspname = 'paradedb' AND p.proname = 'score')
		AND EXISTS (SELECT 1 FROM pg_operator WHERE oprname = '@@@')`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("paradedb.score or the @@@ operator is missing")
	}
	return nil
}

// TryAdvisoryLock takes the session-level advisory lock for key without
// waiting. It reports whether the lock was acquired and returns the
// function that releases it; release is nil when ok is false.
//
// The lock pins the connection it was taken on: a session advisory lock
// belongs to its connection, so the pool must not hand that connection to
// anybody else before the unlock. release returns it.
//
// The key is combined with the current schema, so two Ragmux instances
// sharing one database in separate schemas elect their own leader (and so
// tests, which are isolated by schema, do not fight over one lock).
func (s *Store) TryAdvisoryLock(ctx context.Context, key int64) (func(), bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var scoped int64
	if err := conn.QueryRow(ctx, "SELECT hashtext(current_schema() || ':' || $1)::bigint",
		strconv.FormatInt(key, 10)).Scan(&scoped); err != nil {
		conn.Release()
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", scoped).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// The caller's context is usually already cancelled by the time a
		// leader shuts down, so the unlock gets its own budget. Dropping
		// the connection would release the lock anyway.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", scoped); err != nil {
			s.log.Warn("release advisory lock", "key", key, "err", err)
		}
		conn.Release()
	}, true, nil
}
