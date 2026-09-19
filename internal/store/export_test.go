package store

import (
	"context"
	"fmt"
	"io/fs"
	"time"
)

// KeyVersionBound is exported for tests that inspect key_version.
const KeyVersionBound = keyVersionBound

// InsertLegacyConnection stores a connection the way releases before
// key_version did: sealed without associated data and marked version 0.
func (s *Store) InsertLegacyConnection(ctx context.Context, name, key string) (int64, error) {
	enc, err := s.cipher.encrypt(key, nil)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO model_connections
		(name, provider_type, base_url, api_key_enc, key_version, model_name)
		VALUES ($1, 'openai', '', $2, 0, 'm') RETURNING id`, name, enc).Scan(&id)
	return id, err
}

// BackdateRequestLog moves a request log's created_at so tests can populate
// earlier windows.
func (s *Store) BackdateRequestLog(ctx context.Context, id int64, t time.Time) error {
	_, err := s.pool.Exec(ctx, "UPDATE request_logs SET created_at = $2 WHERE id = $1", id, t.UTC())
	return err
}

// TableConstraints returns "name: definition" for every constraint on a
// table, so a test can check what the migrations actually left behind.
func (s *Store) TableConstraints(ctx context.Context, table string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT conname || ': ' || pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = to_regclass($1) ORDER BY conname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// LatestMigration is the highest embedded migration version. Tests that
// check a freshly opened schema is fully migrated read it from the embedded
// files rather than from a literal, so adding a migration does not make an
// unrelated test fail on a number.
func LatestMigration() int {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		panic(err)
	}
	highest := 0
	for _, e := range entries {
		var version int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &version); err == nil && version > highest {
			highest = version
		}
	}
	return highest
}

// SwapSearchBackend registers b under name and returns the function that
// puts the original back.
//
// It is how the tests reach the "the lexical index went away" path on a
// server that has no ParadeDB. The state that path exists for -- an index
// this process built and something then dropped -- cannot be produced on a
// server that never had pg_search at all, so the backend that fails is
// injected instead. The map is package state and the store tests do not run
// in parallel, so the swap is serialised by the test framework itself.
func SwapSearchBackend(name string, b SearchBackend) func() {
	prev := searchBackends[name]
	searchBackends[name] = b
	return func() { searchBackends[name] = prev }
}

// BM25Ready reports the cached "this process has built the BM25 index" flag
// that Prepare short-circuits on.
func (s *Store) BM25Ready() bool { return s.bm25Ready.Load() }

// SetBM25Ready stands in for a successful index build on a server that
// cannot perform one.
func (s *Store) SetBM25Ready(ready bool) { s.bm25Ready.Store(ready) }

// EnsureBM25Index is what the pg_search backend's Prepare calls.
func (s *Store) EnsureBM25Index(ctx context.Context) error { return s.ensureBM25Index(ctx) }
