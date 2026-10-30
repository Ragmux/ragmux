-- ragmux:no-transaction
--
-- Duplicate lookup of the content hash (0025): "a ready document of this store
-- with this hash". Built CONCURRENTLY so uploads are not blocked while it is
-- built; CREATE INDEX CONCURRENTLY cannot run inside a transaction, which is
-- what the marker on the first line tells migrate(). The runner repairs an
-- interrupted (INVALID) build by itself; see 0017.
--
-- Partial: rows without a hash never match a lookup, so they stay out of the
-- index. Per store, never across stores: dedup does not cross a store
-- boundary.
--
-- Keep exactly one statement in a no-transaction migration.
--
-- Rollback (manual): DROP INDEX CONCURRENTLY IF EXISTS idx_documents_store_content_sha256;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_documents_store_content_sha256
    ON documents (rag_store_id, content_sha256) WHERE content_sha256 IS NOT NULL;
