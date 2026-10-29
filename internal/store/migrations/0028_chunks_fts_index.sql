-- ragmux:no-transaction
--
-- GIN index over chunks.fts (0027), the per-store-language full-text column.
-- Built CONCURRENTLY so a large chunks table is not locked against writes while
-- the index is built; CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction, which is what the marker on the first line tells migrate(). The
-- runner repairs an interrupted (INVALID) build by itself; see 0017.
--
-- Partial: only stores whose language is not 'simple' fill the column, so the
-- default installation (every store on 'simple') pays no index space and no
-- write cost for it.
--
-- Keep exactly one statement in a no-transaction migration.
--
-- Rollback (manual): DROP INDEX CONCURRENTLY IF EXISTS idx_chunks_fts;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chunks_fts
    ON chunks USING gin (fts) WHERE fts IS NOT NULL;
