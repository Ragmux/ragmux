-- ragmux:no-transaction
--
-- GIN index behind the metadata filter's containment predicates (@>). Built
-- CONCURRENTLY so uploads are not blocked while it is built; CREATE INDEX
-- CONCURRENTLY cannot run inside a transaction, which is what the marker on the
-- first line tells migrate(). The runner repairs an interrupted (INVALID) build
-- by itself; see 0017.
--
-- jsonb_path_ops: smaller and faster than the default operator class and enough
-- here, because the filter only ever asks @> (it never asks key existence).
--
-- Keep exactly one statement in a no-transaction migration.
--
-- Rollback (manual): DROP INDEX CONCURRENTLY IF EXISTS idx_documents_metadata;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_documents_metadata
    ON documents USING gin (metadata jsonb_path_ops);
