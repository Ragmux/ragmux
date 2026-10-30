package store

import (
	"context"
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

// LockNoTxMigrations is the advisory lock key applyOutsideTx serialises on,
// for tests that hold it.
const LockNoTxMigrations = lockNoTxMigrations

// SearchUnderSettings runs the statement of one pgvector search inside a
// transaction that first runs the given setup statements (SET LOCAL ..., or
// DDL: the transaction is rolled back), and returns
// the hits together with the plan the server chose for it. It exists to
// force the planner onto the HNSW index (enable_seqscan = off and friends) on
// tables far too small for it to choose that by itself, so a test can show
// what the index scan -- the thing iterative scans are for -- returns.
func (s *Store) SearchUnderSettings(ctx context.Context, storeID int64, query string, queryVec []float32,
	opts SearchOptions, settings ...string) ([]SearchHit, string, error) {
	r, err := s.GetRAGStore(ctx, storeID)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, set := range settings {
		if _, err := tx.Exec(ctx, set); err != nil {
			return nil, "", err
		}
	}
	sql, args := searchStatement(searchBackends[BackendPgvector], r, query, queryVec, opts, s.useHalfvecOrder(r.Dimensions))
	// pgvector.Vector binds as text for EXPLAIN just as it does for the query.
	prows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		return nil, "", err
	}
	plan := ""
	for prows.Next() {
		var line string
		if err := prows.Scan(&line); err != nil {
			return nil, "", err
		}
		plan += line + "\n"
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, plan, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChunkID, &h.DocumentID, &h.Index, &h.Content, &h.Filename, &h.Section, &h.Page,
			&h.Distance, &h.Score, &h.VectorRank, &h.FTSRank, &h.LexScore); err != nil {
			return nil, plan, err
		}
		hits = append(hits, h)
	}
	return hits, plan, rows.Err()
}

// LockVecReindex is the advisory lock key of the halfvec index builds.
const LockVecReindex = lockVecReindex

// SetCaps replaces the detected server features and returns the function that
// puts them back, so a test can act as a server with an older pgvector.
func (s *Store) SetCaps(c Capabilities) func() {
	prev := s.caps
	s.caps = c
	return func() { s.caps = prev }
}

// SetIndexMissingTTL changes how old the index-missing count may get before a
// read recounts it, and returns the function that restores the default.
func (s *Store) SetIndexMissingTTL(d time.Duration) func() {
	prev := s.indexMissingTTL
	s.indexMissingTTL = d
	return func() { s.indexMissingTTL = prev }
}
