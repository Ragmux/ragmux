-- ragmux:no-transaction
--
-- Index behind the admin session detail: the request rows of one session, by
-- time. Built CONCURRENTLY so the large request_logs table is not locked
-- against writes while it is built; CREATE INDEX CONCURRENTLY cannot run
-- inside a transaction, which is what the marker on the first line tells
-- migrate(). The runner repairs an interrupted (INVALID) build by itself; see
-- 0017.
--
-- Partial: only requests that carried a session pay for the index, which is
-- none of them on an installation that never sends X-Ragmux-Session.
--
-- Keep exactly one statement in a no-transaction migration.
--
-- Rollback (manual): DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_session;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_request_logs_session
    ON request_logs (project_id, session_id, created_at) WHERE session_id IS NOT NULL;
