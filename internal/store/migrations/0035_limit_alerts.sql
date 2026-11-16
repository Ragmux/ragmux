-- Limit and budget warning webhooks (ADR-036). Add-only (ADR-001): two new
-- tables and two columns with a constant default, so a row written before this
-- migration, or by a replica that predates it, keeps meaning what it always
-- meant -- no spend recorded, alerts off. PostgreSQL 11+ adds a NOT NULL
-- column with a constant default without rewriting the table.
--
--   project_usage.cost_micros         estimated spend in USD millionths of the
--   project_usage.unpriced_requests   requests counted in the row, and how many
--                                     of those had a model with no price (they
--                                     add 0 to cost_micros). Only the day and
--                                     month rows carry them; the minute row
--                                     stays 0. Written by the same upsert as the
--                                     token counters.
--   project_alert_settings            per project: whether the alert job looks
--                                     at it, and its monthly money budget in
--                                     USD millionths (0 = none). No row means
--                                     alerts are off. The money budget only
--                                     ever warns; it never refuses a request.
--   alert_events                      one row per alert sent, and what makes the
--                                     alert once-only: the UNIQUE key is the
--                                     window the limit belongs to plus the limit
--                                     value, so raising a budget and crossing it
--                                     again is a new event while a repeated
--                                     check of the same state is not. subject_id
--                                     is text: a project id, a key id or an end
--                                     user as the client sent it. The delivery
--                                     is queued in webhook_deliveries in the
--                                     same transaction that inserts the row.
--                                     The janitor deletes the rows of windows
--                                     that are over: an end user's after two
--                                     days or two months (the end-user counters'
--                                     retention), the rest after LOG_RETENTION_DAYS
--                                     (never the current or previous window).
--
-- Rollback (manual, nothing else depends on these objects):
--   DROP TABLE alert_events;
--   DROP TABLE project_alert_settings;
--   ALTER TABLE project_usage DROP COLUMN unpriced_requests, DROP COLUMN cost_micros;
--
-- Lock order and lock timeout. The runner applies this file in one
-- transaction, and ALTER TABLE takes an ACCESS EXCLUSIVE lock that it holds to
-- the commit. project_usage is read and upserted by every gateway request, so a
-- lock request on it that waits behind a long statement stops the requests that
-- queue up behind it, on every replica, for as long as that statement runs.
--
-- Two measures keep that short:
--   * project_usage is altered last, after the two CREATE TABLEs: the only
--     lock taken before it is the one the foreign keys take on projects
--     (SHARE ROW EXCLUSIVE, which readers do not queue behind), so while the
--     migration waits for anything, no request-path table is held;
--   * SET LOCAL lock_timeout (below) bounds the wait itself: a migration that
--     cannot get its lock within 2 seconds fails instead of queueing, and the
--     requests behind it go on.
--
-- What a timeout does. The statement fails with "canceling statement due to
-- lock timeout" (SQLSTATE 55P03), the transaction rolls back (nothing is half
-- applied; the migration is not recorded) and store.Open returns the error, so
-- the new process exits. Replicas still on the old version are unaffected.
-- There is no retry in the runner: the process manager starts the new replica
-- again. The operator steps are in docs/scaling.md (the same ones as for 0033).
SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS project_alert_settings (
    project_id                BIGINT      PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    enabled                   BOOLEAN     NOT NULL DEFAULT false,
    budget_monthly_usd_micros BIGINT      NOT NULL DEFAULT 0 CHECK (budget_monthly_usd_micros >= 0),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS alert_events (
    id           BIGSERIAL   PRIMARY KEY,
    project_id   BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    subject_kind TEXT        NOT NULL CHECK (subject_kind IN ('project', 'key', 'end_user')),
    subject_id   TEXT        NOT NULL,
    limit_kind   TEXT        NOT NULL CHECK (limit_kind IN
                     ('budget_daily_tokens', 'budget_monthly_tokens', 'budget_monthly_usd', 'unpriced')),
    threshold    SMALLINT    NOT NULL CHECK (threshold BETWEEN 0 AND 100),
    window_start TIMESTAMPTZ NOT NULL,
    limit_value  BIGINT      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, subject_kind, subject_id, limit_kind, threshold, window_start, limit_value)
);

-- Retention deletes closed windows by window_start.
CREATE INDEX IF NOT EXISTS idx_alert_events_window ON alert_events (window_start);

ALTER TABLE project_usage
    ADD COLUMN IF NOT EXISTS cost_micros BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS unpriced_requests INT NOT NULL DEFAULT 0;
