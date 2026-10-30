package store

import (
	"context"
	"fmt"
)

// lockFTSRebuild is the high half of the per-store advisory lock key a rebuild
// batch takes; the store id is XORed into the key (see ftsRebuildLockKey).
const lockFTSRebuild int32 = 0x72616704

// ftsRebuildLockKey is the bigint advisory lock key of one store's rebuild.
// XOR with a constant is a bijection, so two store ids never share a key, however
// large; the two-int form of the lock functions would have needed the id cut to
// 32 bits.
func ftsRebuildLockKey(storeID int64) int64 { return (int64(lockFTSRebuild) << 32) ^ storeID }

// DefaultFTSRebuildBatch is how many chunks one rebuild batch rewrites.
const DefaultFTSRebuildBatch = 5000

// FTSRebuildResult is what one RebuildFTSBatch did.
type FTSRebuildResult struct {
	// Rows is the number of chunks the batch rewrote.
	Rows int
	// Done: nothing is left to do for the store -- the batch finished the
	// rebuild, or there was no rebuild to run (a 'simple' store, an already
	// applied language, a store that is gone).
	Done bool
	// Busy: another replica holds the store's rebuild lock; try again later.
	Busy bool
}

// StoresNeedingFTSRebuild lists the stores whose chunks.fts has to be (re)built:
// a language other than 'simple' that is not applied yet. It is what a
// restarted process uses to pick the work up again at the cursor the dead one
// left.
func (s *Store) StoresNeedingFTSRebuild(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM rag_stores
		WHERE language <> 'simple' AND fts_language_applied IS DISTINCT FROM language ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RebuildFTSBatch rewrites chunks.fts for up to limit chunks of a store, in id
// order after the store's cursor, with the store's current language, and moves
// the cursor. The batch that finds nothing left (or fewer rows than limit)
// marks the language applied, which is the moment searches switch from
// tsv/'simple' to fts (RAGStore.lexicalPlan).
//
// One transaction does it all: a per-store advisory lock (tried, not waited
// for, so a second replica skips the store), then the store row under the
// lock ingest and a language change take (FOR NO KEY UPDATE). The language and
// the applied state are read under that lock, so a batch never writes a
// language the store no longer has -- a language change waits for the batch
// and the next batch sees the new language -- and an ingest never interleaves
// with it. Chunks an ingest wrote after the cursor are simply rewritten with
// the same configuration; chunks it wrote before are covered by the cursor
// having been reset to 0 by the language change.
func (s *Store) RebuildFTSBatch(ctx context.Context, storeID int64, limit int) (FTSRebuildResult, error) {
	if limit <= 0 {
		limit = DefaultFTSRebuildBatch
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FTSRebuildResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var got bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1::bigint)", ftsRebuildLockKey(storeID)).Scan(&got); err != nil {
		return FTSRebuildResult{}, err
	}
	if !got {
		return FTSRebuildResult{Busy: true}, nil
	}
	var language string
	var applied *string
	var cursor int64
	err = tx.QueryRow(ctx, `SELECT language, fts_language_applied, fts_rebuild_cursor FROM rag_stores
		WHERE id = $1 FOR NO KEY UPDATE`, storeID).Scan(&language, &applied, &cursor)
	if err != nil {
		if scanErr(err) == ErrNotFound {
			return FTSRebuildResult{Done: true}, nil
		}
		return FTSRebuildResult{}, err
	}
	if language == LanguageSimple || (applied != nil && *applied == language) {
		return FTSRebuildResult{Done: true}, nil
	}
	cfg, ok := LanguageConfig(language)
	if !ok {
		return FTSRebuildResult{}, fmt.Errorf("rag store %d has language %q, which maps to no text search configuration", storeID, language)
	}

	var rows int
	var maxID int64
	if err := tx.QueryRow(ctx, `WITH batch AS (
			SELECT id FROM chunks WHERE rag_store_id = $1 AND id > $2 ORDER BY id LIMIT $3),
		upd AS (
			UPDATE chunks c SET fts = to_tsvector($4::regconfig, c.content)
			FROM batch WHERE c.id = batch.id RETURNING c.id)
		SELECT COUNT(*), COALESCE(MAX(id), 0) FROM upd`, storeID, cursor, limit, cfg).Scan(&rows, &maxID); err != nil {
		return FTSRebuildResult{}, err
	}
	done := rows < limit
	if done {
		_, err = tx.Exec(ctx, `UPDATE rag_stores SET fts_language_applied = language, fts_rebuild_cursor = $2 WHERE id = $1`,
			storeID, max(cursor, maxID))
	} else {
		_, err = tx.Exec(ctx, "UPDATE rag_stores SET fts_rebuild_cursor = $2 WHERE id = $1", storeID, maxID)
	}
	if err != nil {
		return FTSRebuildResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FTSRebuildResult{}, err
	}
	return FTSRebuildResult{Rows: rows, Done: done}, nil
}
