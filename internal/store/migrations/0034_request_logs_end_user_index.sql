-- ragmux:no-transaction
--
-- Index behind the per-end-user views of the request log (the admin end-user
-- lookup and anonymization): the request rows of one end user within a
-- project, by time. Built CONCURRENTLY so the large request_logs table is not
-- locked against writes while it is built; CREATE INDEX CONCURRENTLY cannot
-- run inside a transaction, which is what the marker on the first line tells
-- migrate(). The runner repairs an interrupted (INVALID) build by itself; see
-- 0017.
--
-- Partial: only requests that named an end user pay for the index, which is
-- none of them on an installation that never sends one. There is no index on
-- tags: nothing filters by them yet.
--
-- Keep exactly one statement in a no-transaction migration.
--
-- Rollback (manual): DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_end_user;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_request_logs_end_user
    ON request_logs (project_id, end_user, created_at) WHERE end_user IS NOT NULL;
