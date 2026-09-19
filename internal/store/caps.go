package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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

// TryAdvisoryLock takes a session-scoped advisory lock held for the caller's
// whole critical section. It returns ok=false when another replica holds the
// lock; release drops the lock and returns the connection to the pool.
//
// The connection is pinned on purpose: pg_try_advisory_lock issued through
// pool.Exec would land on an arbitrary pooled connection and the lock would
// be released the moment that connection was reused for something else.
func (s *Store) TryAdvisoryLock(ctx context.Context, key int64) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection for advisory lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("take advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// A background context: the caller's context is usually cancelled by
		// the time a shutdown path releases the lock, and an unlock that does
		// not run leaves the lock held until the connection drops.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key); err != nil {
			s.log.Warn("release advisory lock", "key", key, "err", err)
		}
		conn.Release()
	}, true, nil
}
