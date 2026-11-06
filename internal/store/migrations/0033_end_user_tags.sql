-- End-user identifiers, tags and per-end-user limits (ADR-035). Add-only
-- (ADR-001): every column is nullable or has a constant default, so a row
-- written before this migration, or by a replica that predates it, keeps
-- meaning what it always meant -- no end user, no tags, no end-user ceiling.
-- PostgreSQL 11+ adds a NOT NULL column with a constant default without
-- rewriting the table.
--
--   request_logs.end_user                  the end user the request was made
--                                          for: X-Ragmux-End-User, else the
--                                          body's own field. NULL when the
--                                          request named none. Never a metric
--                                          label or a span attribute (PRD
--                                          rule 10).
--   request_logs.tags                      the request's tags, sorted and
--                                          de-duplicated; NULL when none.
--   projects.end_user_rpm                  requests per minute one end user
--   projects.end_user_tpm                  may make, tokens per minute,
--   projects.end_user_daily_tokens         tokens per UTC day and per UTC
--   projects.end_user_monthly_tokens       month; 0 is unlimited.
--   end_user_usage                         the counters those ceilings are
--                                          compared with, one row per (project,
--                                          end user, window), the shape of
--                                          project_usage and key_usage. Only a
--                                          request that names an end user, to a
--                                          project that sets a ceiling, reads
--                                          or writes it. Rows go with their
--                                          project. The limiter reads only the
--                                          current window and a row holds the
--                                          end user's id as sent, so the
--                                          retention job keeps them shorter
--                                          than the other counters: minute
--                                          rows 2 hours, day rows 2 days, and
--                                          month rows for the current and the
--                                          previous month (internal/limits).
--
-- Rollback (manual, nothing else depends on these objects):
--   DROP TABLE end_user_usage;
--   ALTER TABLE projects DROP COLUMN end_user_monthly_tokens,
--       DROP COLUMN end_user_daily_tokens, DROP COLUMN end_user_tpm,
--       DROP COLUMN end_user_rpm;
--   ALTER TABLE request_logs DROP COLUMN tags, DROP COLUMN end_user;
-- Lock order and lock timeout. The runner applies this file in one
-- transaction, and every ALTER TABLE takes an ACCESS EXCLUSIVE lock that it
-- holds to the commit. The gateway reads the project on every request and
-- writes a request_logs row at the end of every request, so a lock request
-- that waits behind a long read stops the requests that queue up behind it,
-- on every replica, for as long as that read runs.
--
-- Two measures keep that short:
--   * request_logs is altered first, so nothing is held on projects while the
--     migration waits for request_logs;
--   * SET LOCAL lock_timeout (below) bounds the wait itself: a migration that
--     cannot get its lock within 2 seconds fails instead of queueing, and the
--     requests behind it go on.
--
-- What a timeout does. The statement fails with "canceling statement due to
-- lock timeout" (SQLSTATE 55P03), the transaction rolls back (nothing is half
-- applied; the migration is not recorded) and store.Open returns the error, so
-- the new process exits. Replicas still on the old version are unaffected.
-- There is no retry in the runner: the process manager starts the new replica
-- again, and the migration runs from the start. A replica that was waiting for
-- the migration advisory lock takes it next and tries in its turn.
--
-- What the operator does (the query and the details are in docs/scaling.md):
--   1. List the sessions that hold request_logs (pg_locks joined to
--      pg_stat_activity, filtered on the table).
--   2. An active query: wait for it or pg_cancel_backend(pid). A session that is
--      "idle in transaction" is not helped by that -- the lock outlives the
--      query -- and needs pg_terminate_backend(pid).
--   3. Under steady load, where new long reads keep starting, stop the source of
--      that load (an export job, a polling dashboard) first, or the next start
--      times out again.
-- Then let the restart go through. 0032 carries the same measures, and the two
-- run back to back when a v0.7 database is upgraded.
SET LOCAL lock_timeout = '2s';

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS end_user TEXT NULL,
    ADD COLUMN IF NOT EXISTS tags TEXT[] NULL;

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS end_user_rpm INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS end_user_tpm INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS end_user_daily_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS end_user_monthly_tokens BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS end_user_usage (
    project_id        BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    end_user          TEXT        NOT NULL,
    period            TEXT        NOT NULL CHECK (period IN ('minute', 'day', 'month')),
    period_start      TIMESTAMPTZ NOT NULL,
    requests          INT         NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, end_user, period, period_start)
);
-- Retention deletes by window across all projects.
CREATE INDEX IF NOT EXISTS idx_end_user_usage_period ON end_user_usage (period, period_start);
