package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// pgSearchBackend answers the lexical half with ParadeDB's BM25 index. The
// vector half, the fusion and the column contract are the pgvector ones; all
// that changes is how the lexical candidates are found and ordered.
type pgSearchBackend struct{}

func (pgSearchBackend) Name() string { return BackendPgSearch }

func (pgSearchBackend) Available(caps Capabilities) bool { return caps.PgSearch }

func (pgSearchBackend) Prepare(ctx context.Context, s *Store) error { return s.ensureBM25Index(ctx) }

// bm25Index is the name of the one global BM25 index over chunks.
const bm25Index = "idx_chunks_bm25"

// pgSearchTokenizer is the ParadeDB analyser for the BM25 index.
//
// The default is ParadeDB's "default" tokenizer: unicode word split plus
// lowercasing, with no stemming and no stop-word removal. That is the
// closest match to what the pgvector path does -- chunks.tsv is generated
// with the 'simple' configuration, which also neither stems nor drops stop
// words -- so switching a store between the two backends changes the ranking
// quality without changing which words match. Operators who want stemming
// ("en_stem") opt into it knowingly, for the whole instance: the index is
// global, so the tokenizer cannot be a per-store setting.
//
// The value is validated by config.Load, which is the only place it enters
// the process; it is the one part of the index DDL that is interpolated.
func (s *Store) pgSearchTokenizer() string {
	if s.pgSearchTok == "" {
		return "default"
	}
	return s.pgSearchTok
}

// bm25IndexDDL builds the CREATE INDEX statement for the global BM25 index.
//
// One global index, not one partial index per store. A per-store partial
// index multiplies the index count by the number of stores and each BM25
// index carries a full segment tree of its own, so the disk and the write
// amplification grow with the store count rather than with the corpus. The
// store is a fast numeric field inside the one index instead, and every
// query filters on it with paradedb.term.
func (s *Store) bm25IndexDDL() string {
	return fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON chunks
USING bm25 (id, content, rag_store_id, document_id)
WITH (key_field = 'id', text_fields = '{"content":{"tokenizer":{"type":"%s"},"record":"position"}}',
      numeric_fields = '{"rag_store_id":{"fast":true},"document_id":{"fast":true}}')`,
		bm25Index, s.pgSearchTokenizer())
}

// bm25BuildLockWait bounds how long a search waits for another replica's
// index build before giving up and answering from the pgvector backend.
const bm25BuildLockWait = 15 * time.Second

// ensureBM25Index creates the global BM25 index the first time a search
// needs it, serialised across replicas with an advisory lock and remembered
// in-process afterwards.
//
// It is built here rather than in the migration for two reasons. A server
// that gains pg_search later -- an operator swapping the image for the
// ParadeDB one -- still gets the index without re-running migrations. And a
// server that never gains it never pays for one: a BM25 index is maintained
// on every chunk insert, so an installation that only ever uses the pgvector
// backend would otherwise carry the whole ingest cost of a feature it does
// not use.
//
// The build runs on the caller's context, which for the first search after
// a switch is an HTTP request: on a large corpus that request can be the one
// that waits. The index survives, so it is paid once.
func (s *Store) ensureBM25Index(ctx context.Context) error {
	if s.bm25Ready.Load() {
		return nil
	}
	// CREATE INDEX IF NOT EXISTS is not safe to race: two sessions both pass
	// the existence check and the loser fails on pg_class's unique index. The
	// advisory lock is what makes it safe, and it is taken with a retry
	// rather than a blocking wait so a search never hangs on it -- if the
	// lock does not come free within the deadline this one query falls back
	// to pgvector and the next one finds the finished index.
	deadline := time.Now().Add(bm25BuildLockWait)
	for {
		release, ok, err := s.TryAdvisoryLock(ctx, int64(lockBM25Index))
		if err != nil {
			return err
		}
		if ok {
			defer release()
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another replica is still building %s", bm25Index)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if _, err := s.pool.Exec(ctx, s.bm25IndexDDL()); err != nil {
		return fmt.Errorf("create %s: %w", bm25Index, err)
	}
	s.bm25Ready.Store(true)
	s.log.Info("bm25 index ready", "index", bm25Index, "tokenizer", s.pgSearchTokenizer())
	return nil
}

func (pgSearchBackend) HybridQuery(p SearchParams) (string, []any) {
	// FTSConfig is deliberately absent: the BM25 index is global and was
	// tokenised once at index time, so a per-store text search configuration
	// has nothing left to select. The admin search response reports the
	// store's fts_config next to the effective backend so the setting is
	// visibly inert rather than silently so.
	//
	// ftsQuery is not applied either: paradedb.match tokenises the string
	// itself and OR-joins the terms, so the " or " rewriting the tsquery
	// path needs would land in the index as the literal term "or".
	args := []any{p.Vector, p.StoreID, p.Candidates, p.MaxDistance}
	lex := `
				SELECT c.id AS chunk_id, paradedb.score(c.id) AS lex_score,
				       ROW_NUMBER() OVER (ORDER BY paradedb.score(c.id) DESC, c.id) AS rank
				FROM chunks c
				WHERE c.id @@@ paradedb.boolean(must => ARRAY[
				          paradedb.term('rag_store_id', $2::bigint),
				          paradedb.match('content', $5::text)])
				ORDER BY paradedb.score(c.id) DESC, c.id
				LIMIT $3`
	if strings.TrimSpace(p.Query) == "" {
		// A blank query has no terms to match. paradedb.match on an empty
		// string is not a documented no-op, so the lexical leg is replaced
		// by an empty relation and the search degrades to the vector order,
		// which is what the tsquery path does through numnode(query) > 0.
		lex = `
				SELECT NULL::bigint AS chunk_id, 0::float8 AS lex_score, 0::bigint AS rank
				WHERE false`
	} else {
		args = append(args, p.Query)
	}
	sql := fmt.Sprintf(`
			WITH v AS (
				SELECT e.chunk_id, ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM %[1]s e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3),
			f AS (%[3]s),
			m AS (
				SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
				       COALESCE(v.rank, 0) AS vrank, COALESCE(f.rank, 0) AS frank,
				       COALESCE(f.lex_score, 0) AS lex_score,
				       COALESCE(1.0/(%[2]d+v.rank), 0) + COALESCE(1.0/(%[2]d+f.rank), 0) AS score
				FROM v FULL OUTER JOIN f ON v.chunk_id = f.chunk_id)
			SELECT m.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       e.embedding <=> $1::vector AS distance, m.score, m.vrank, m.frank, m.lex_score
			FROM m
			JOIN chunks c ON c.id = m.chunk_id
			JOIN documents d ON d.id = c.document_id
			JOIN %[1]s e ON e.chunk_id = m.chunk_id
			WHERE ($4::float8 = 0 OR (e.embedding <=> $1::vector) <= $4::float8)
			ORDER BY m.score DESC, distance, m.chunk_id
			LIMIT $3`, p.VecTable, rrfK, lex)
	return sql, args
}
