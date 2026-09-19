package store

import (
	"context"
	"fmt"
	"strings"
)

// pgvectorBackend answers the lexical half with PostgreSQL's own full-text
// search: the chunks.tsv column generated with the 'simple' configuration
// and its GIN index. It needs no extension beyond pgvector itself, so it is
// always available and is what every store falls back to.
type pgvectorBackend struct{}

func (pgvectorBackend) Name() string { return BackendPgvector }

func (pgvectorBackend) Available(Capabilities) bool { return true }

// Prepare is a no-op: the tsvector column and its index come from the
// migrations, so there is nothing to build lazily.
func (pgvectorBackend) Prepare(context.Context, *Store) error { return nil }

// Invalidate is a no-op for the same reason: Prepare caches nothing.
func (pgvectorBackend) Invalidate(*Store) {}

func (pgvectorBackend) HybridQuery(p SearchParams) (string, []any) {
	// An empty or stop-word-only query yields a tsquery with numnode = 0;
	// the full-text side is then simply empty instead of an error.
	//
	// The twelfth column is a constant 0: ts_rank_cd is not a BM25 score and
	// is not comparable across queries or configurations, so exposing it as
	// SearchHit.LexScore would invite exactly the cross-backend comparison it
	// cannot support.
	sql := fmt.Sprintf(`
			WITH q AS (SELECT websearch_to_tsquery($5::text::regconfig, $6::text) AS query),
			v AS (
				SELECT e.chunk_id, ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM %[1]s e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3),
			f AS (
				SELECT c.id AS chunk_id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.tsv, q.query) DESC, c.id) AS rank
				FROM chunks c, q
				WHERE c.rag_store_id = $2 AND numnode(q.query) > 0 AND c.tsv @@ q.query
				ORDER BY ts_rank_cd(c.tsv, q.query) DESC, c.id
				LIMIT $3),
			m AS (
				SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
				       COALESCE(v.rank, 0) AS vrank, COALESCE(f.rank, 0) AS frank,
				       COALESCE(1.0/(%[2]d+v.rank), 0) + COALESCE(1.0/(%[2]d+f.rank), 0) AS score
				FROM v FULL OUTER JOIN f ON v.chunk_id = f.chunk_id)
			SELECT m.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       e.embedding <=> $1::vector AS distance, m.score, m.vrank, m.frank, 0::float8
			FROM m
			JOIN chunks c ON c.id = m.chunk_id
			JOIN documents d ON d.id = c.document_id
			JOIN %[1]s e ON e.chunk_id = m.chunk_id
			WHERE ($4::float8 = 0 OR (e.embedding <=> $1::vector) <= $4::float8)
			ORDER BY m.score DESC, distance, m.chunk_id
			LIMIT $3`, p.VecTable, rrfK)
	return sql, []any{p.Vector, p.StoreID, p.Candidates, p.MaxDistance, p.FTSConfig, ftsQuery(p.Query)}
}

// ftsQuery prepares free text for websearch_to_tsquery. That function ANDs
// all words, which makes natural-language questions ("what is the zyxquux
// protocol") match only chunks containing every word. Plain queries are
// therefore OR-ed; ts_rank_cd still ranks chunks matching more terms higher.
// Queries using websearch syntax (quotes, "or", a leading "-") are passed
// through unchanged.
//
// It lives with the backend that needs it: pg_search tokenises and OR-joins
// inside paradedb.match, so rewriting the query for it would inject the
// literal word "or" into the BM25 terms.
func ftsQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" || strings.Contains(q, `"`) {
		return q
	}
	words := strings.Fields(q)
	for _, w := range words {
		if strings.EqualFold(w, "or") || strings.HasPrefix(w, "-") {
			return q
		}
	}
	return strings.Join(words, " or ")
}
