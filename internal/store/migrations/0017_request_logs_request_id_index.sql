-- ragmux:no-transaction
--
-- Lookup of a request log row by the id the client was given. Built
-- CONCURRENTLY so a large request_logs table is not locked against writes
-- while the index is built; CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction, which is what the marker on the first line tells migrate().
--
-- Keep exactly one statement in a no-transaction migration: it is sent as a
-- single simple-protocol query, and a second statement would make the server
-- wrap the pair in an implicit transaction block, which CONCURRENTLY refuses.
--
-- If a build is interrupted PostgreSQL leaves an INVALID index behind. The
-- runner drops an invalid index of this name before building, and does not
-- record the migration unless the index is valid afterwards, so the next
-- start repairs it by itself. Nothing is needed from the operator.
--
-- Partial: rows written before 0016 have no id and never match a lookup.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_request_logs_request_id
    ON request_logs (request_id) WHERE request_id IS NOT NULL;
