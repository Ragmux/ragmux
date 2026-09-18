-- Retrieval settings per RAG store. Full-text indexing of chunks always uses
-- the 'simple' configuration (see chunks.tsv); fts_config only chooses the
-- configuration used to parse the search query.
ALTER TABLE rag_stores
    ADD COLUMN IF NOT EXISTS search_mode       TEXT    NOT NULL DEFAULT 'hybrid' CHECK (search_mode IN ('vector', 'hybrid')),
    ADD COLUMN IF NOT EXISTS fts_config        TEXT    NOT NULL DEFAULT 'simple',
    ADD COLUMN IF NOT EXISTS rerank            BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS rerank_candidates INT     NOT NULL DEFAULT 15 CHECK (rerank_candidates BETWEEN 1 AND 100),
    ADD COLUMN IF NOT EXISTS max_distance      REAL    NOT NULL DEFAULT 0 CHECK (max_distance >= 0 AND max_distance <= 2),
    ADD COLUMN IF NOT EXISTS contextual_chunks BOOLEAN NOT NULL DEFAULT true;

-- The text that was actually embedded when contextual chunks are enabled
-- (title/section prefix + content). NULL when the plain content was embedded.
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS embed_text TEXT;
