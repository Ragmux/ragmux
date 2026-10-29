-- Per-store language (ADR-027). The language picks the text search
-- configuration a store is indexed and queried with; rag_stores.fts_config is
-- derived from it and no longer read (the column stays in the schema).
--
-- Add-only (ADR-001), and nothing here rewrites a table:
--   * rag_stores.language starts as 'simple' for every existing store, which is
--     what chunks.tsv (generated with 'simple') has always been. There is no
--     automatic rebuild; a store whose fts_config is not 'simple' gets a
--     startup warning that names the language to set.
--   * chunks.fts is nullable with no default, so PostgreSQL adds it without
--     touching existing rows. It is filled only for stores whose language is
--     not 'simple'; 'simple' stores keep using the generated chunks.tsv.
--   * fts_language_applied is the language chunks.fts was last (re)built for,
--     NULL while a rebuild is pending or running; fts_rebuild_cursor is the
--     highest chunk id the running rebuild has finished, so a rebuild that
--     was interrupted resumes where it stopped.
--
-- The generated chunks.tsv column and its index are not touched.
--
-- Rollback (manual, nothing else depends on the columns):
--   ALTER TABLE chunks DROP COLUMN fts;
--   ALTER TABLE rag_stores DROP COLUMN fts_rebuild_cursor, DROP COLUMN fts_language_applied, DROP COLUMN language;
ALTER TABLE rag_stores ADD COLUMN IF NOT EXISTS language TEXT NOT NULL DEFAULT 'simple';
ALTER TABLE rag_stores ADD COLUMN IF NOT EXISTS fts_language_applied TEXT;
ALTER TABLE rag_stores ADD COLUMN IF NOT EXISTS fts_rebuild_cursor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS fts TSVECTOR;
