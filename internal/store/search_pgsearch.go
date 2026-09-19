package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/bm25"
)

// pgSearchBackend answers the lexical half with ParadeDB's BM25 index. The
// vector half, the fusion and the column contract are the pgvector ones; all
// that changes is how the lexical candidates are found and ordered.
type pgSearchBackend struct{}

func (pgSearchBackend) Name() string { return BackendPgSearch }

func (pgSearchBackend) Available(caps Capabilities) bool { return caps.PgSearch }

func (pgSearchBackend) Prepare(ctx context.Context, s *Store) error { return s.ensureBM25Index(ctx) }

// Invalidate forgets that the BM25 index was ever built, so the next
// Prepare goes back to the database instead of trusting this process's
// memory.
//
// It is what makes a dropped index recoverable. docs/configuration.md tells
// an operator to change PG_SEARCH_TOKENIZER with
// "DROP INDEX IF EXISTS idx_chunks_bm25;" and does not ask for a restart;
// without this, every replica that had already built the index would keep
// answering Prepare from a cached true, send @@@ at a table that no longer
// carries a BM25 index, and fail every hybrid search until it was restarted.
func (pgSearchBackend) Invalidate(s *Store) { s.bm25Ready.Store(false) }

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
WITH (key_field = 'id', text_fields = '{"content":{"tokenizer":%s,"record":"position"}}',
      numeric_fields = '{"rag_store_id":{"fast":true},"document_id":{"fast":true}}')`,
		bm25Index, bm25.TokenizerJSON(s.pgSearchTokenizer()))
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
	// The lock is taken with a retry rather than a blocking wait so a search
	// never hangs on it: if it does not come free within the deadline this
	// one query falls back to pgvector and the next one finds the finished
	// index.
	deadline := time.Now().Add(bm25BuildLockWait)
	for {
		built, err := s.buildBM25Index(ctx)
		if err != nil {
			return err
		}
		if built {
			return nil
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
}

// buildBM25Index takes the DDL lock and creates the index if it is missing,
// both inside one transaction. It reports false, and changes nothing, when
// another replica holds the lock.
//
// CREATE INDEX IF NOT EXISTS is not safe to race: two sessions both pass the
// existence check and the loser fails on pg_class's unique index. The
// advisory lock is what makes it safe, and it is transaction scoped rather
// than session scoped for two reasons.
//
// A session advisory lock belongs to its connection, and PgBouncer in
// transaction mode hands that connection to somebody else between
// statements, so the lock cannot be held across the DDL. The retention pass
// survives that because every step of it is idempotent (docs/scaling.md);
// this DDL is not. A transaction-scoped lock is held for exactly as long as
// the transaction that also runs the DDL, which is the one unit PgBouncer
// does keep on one server connection.
//
// And it puts the two lazy-DDL paths of this package on one pattern:
// ensureVecTable already builds its tables under pg_advisory_xact_lock.
func (s *Store) buildBM25Index(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	// The second half of the key is the schema, because the index this
	// guards is per-schema: two Ragmux instances sharing one database, and
	// the tests, which isolate themselves by schema, must not queue behind
	// each other's build. Without that they do, and this lock reports the
	// wait as "another replica is still building" after bm25BuildLockWait --
	// a silent downgrade to pgvector rather than the blocking wait
	// ensureVecTable's global key gets away with.
	//
	// hashtext rather than hashtextextended here: pg_try_advisory_xact_lock
	// takes two int4s, so 32 bits is the width the key actually has and
	// narrowing a bigint into it would only add a cast that can overflow.
	var locked bool
	if err := tx.QueryRow(ctx,
		"SELECT pg_try_advisory_xact_lock($1, pg_catalog.hashtext(pg_catalog.current_schema()))",
		lockBM25Index).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	// Read before the DDL: CREATE INDEX IF NOT EXISTS leaves an index that
	// already exists exactly as it was, tokenizer included, so this is the
	// only moment the analyser actually in use can be told apart from the
	// one the configuration asks for.
	want := s.pgSearchTokenizer()
	have, err := bm25IndexTokenizer(ctx, tx)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, s.bm25IndexDDL()); err != nil {
		return false, fmt.Errorf("create %s: %w", bm25Index, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.bm25Ready.Store(true)
	if have != "" && have != want {
		s.log.Warn("the bm25 index was built with a different analyser than PG_SEARCH_TOKENIZER asks for; "+
			"searches use the index's own analyser until the index is dropped and rebuilt",
			"index", bm25Index, "index_tokenizer", have, "configured_tokenizer", want)
	}
	// Report what the index carries, not what the configuration says: an
	// index built by an earlier boot under a different PG_SEARCH_TOKENIZER
	// would otherwise be logged as ready with an analyser no query uses.
	effective := have
	if effective == "" {
		effective = want
	}
	s.log.Info("bm25 index ready", "index", bm25Index, "tokenizer", effective)
	return true, nil
}

// bm25IndexTokenizer reads the analyser the BM25 index on chunks was
// actually built with, or "" when there is no such index.
//
// It looks the index up through pg_index on the chunks table this connection
// resolves, not by schema name. CREATE INDEX puts the index in the schema of
// the table it indexes, so with a multi-entry search_path a check against
// current_schema() would miss the index that the DDL beside it is about to
// touch and quietly disable the drift warning.
func bm25IndexTokenizer(ctx context.Context, tx pgx.Tx) (string, error) {
	var opts []string
	err := tx.QueryRow(ctx, `SELECT COALESCE(c.reloptions, '{}')
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE i.indrelid = to_regclass('chunks') AND c.relname = $1`,
		bm25Index).Scan(&opts)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return tokenizerFromReloptions(opts), nil
}

// tokenizerFromReloptions pulls the analyser name out of an index's
// reloptions. PostgreSQL stores each option as "name=value" and ParadeDB
// keeps text_fields as the JSON document bm25IndexDDL passed it, so the
// analyser is at text_fields={"content":{"tokenizer":{"type":"<name>"}}}.
//
// The name returned is the one an operator writes in PG_SEARCH_TOKENIZER,
// not the one in the JSON, because the caller compares it against exactly
// that; bm25.TokenizerName does that translation.
//
// An empty result means "could not tell", never "no tokenizer": the value
// only ever reaches a log line, so a reloptions shape a future ParadeDB
// renders differently -- or a stemmer language this build has no code for --
// degrades to saying nothing rather than to warning about a drift that is
// not there.
func tokenizerFromReloptions(opts []string) string {
	const prefix = "text_fields="
	for _, o := range opts {
		if !strings.HasPrefix(o, prefix) {
			continue
		}
		var fields map[string]struct {
			Tokenizer json.RawMessage `json:"tokenizer"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(o, prefix)), &fields); err != nil {
			return ""
		}
		raw := fields["content"].Tokenizer
		if len(raw) == 0 {
			return ""
		}
		return bm25.TokenizerName(raw)
	}
	return ""
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
