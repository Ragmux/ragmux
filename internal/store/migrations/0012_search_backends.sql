-- Pluggable search and rerank backends per RAG store.
--
-- search_backend picks how the lexical half of a hybrid search is answered:
-- 'pgvector' is PostgreSQL's own full-text search (the tsvector column and
-- its GIN index this schema has always carried), 'pg_search' is ParadeDB's
-- BM25 index. Switching never touches the chunks: both are derived from
-- chunks.content, so no reprocessing is needed in either direction.
--
-- rerank_backend picks who reorders the candidates: the project's own chat
-- model ('llm', the previous and still default behaviour) or a dedicated
-- rerank API reached through a model_connections row.
--
-- There is deliberately NO CHECK (rerank_backend = 'llm' OR
-- rerank_connection_id IS NOT NULL). ON DELETE SET NULL exists so deleting a
-- Cohere connection does not cascade into the store, and that SET NULL would
-- violate such a constraint: the operator's DELETE would fail with a foreign
-- key error naming a column they never heard of. The pairing is enforced on
-- write instead (validateRAG rejects an API backend without a matching
-- connection), and on read a NULL connection behind an API backend means
-- "reranker unavailable" -- skipped and logged, exactly like a rerank that
-- fails mid-flight.
ALTER TABLE rag_stores
    ADD COLUMN IF NOT EXISTS search_backend TEXT NOT NULL DEFAULT 'pgvector'
        CHECK (search_backend IN ('pgvector','pg_search')),
    ADD COLUMN IF NOT EXISTS rerank_backend TEXT NOT NULL DEFAULT 'llm'
        CHECK (rerank_backend IN ('llm','cohere','voyage')),
    ADD COLUMN IF NOT EXISTS rerank_connection_id BIGINT REFERENCES model_connections(id) ON DELETE SET NULL;
