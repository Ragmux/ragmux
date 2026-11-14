-- Request feedback (ADR-042). Add-only (ADR-001): one new table, nothing else
-- is touched, so a replica that predates this migration never reads or writes
-- it.
--
--   request_feedback   one row per (project, request_id): a score of 1 or -1
--                      and an optional label, sent through POST /v1/feedback.
--                      request_id is the id the client saw in X-Request-Id; it
--                      is not unique (a client may supply its own), so the
--                      feedback hangs on the pair, not on a request_logs row.
--                      A second submission replaces the first (the last writer
--                      wins). The row holds no request or response content and
--                      no free text (PRD rule 10); label is a short token the
--                      gateway validated. api_key_id is the key that sent it,
--                      NULL once that key is deleted. Rows go with their
--                      project, and the retention job trims them by created_at
--                      with LOG_RETENTION_DAYS.
--
-- Rollback (manual, nothing else depends on this table):
--   DROP TABLE request_feedback;
-- Dropping it forgets every score received.
--
-- Lock timeout. The table is new, so the CREATE itself locks nothing a request
-- uses. The two foreign keys do: adding one takes a SHARE ROW EXCLUSIVE lock on
-- the table it references, which the runner's transaction holds to the commit.
-- The gateway reads projects and api_keys on every request, and writes to them
-- (key usage, project edits); a lock request that waits behind a long write
-- stops the requests that queue up behind it. SET LOCAL lock_timeout bounds the
-- wait: a migration that cannot get its locks within 2 seconds fails (SQLSTATE
-- 55P03), the transaction rolls back and store.Open returns the error, instead
-- of queueing every request. Retry the restart; the operator's steps are in
-- docs/scaling.md and in 0033. projects comes first because it is the table
-- the gateway touches on the most paths; api_keys, held next, is the one a
-- request holds shortest. The table is new and empty, so the index below needs
-- no CONCURRENTLY: nothing reads or writes it yet.
SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS request_feedback (
    project_id BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    request_id TEXT        NOT NULL,
    score      SMALLINT    NOT NULL CHECK (score IN (-1, 1)),
    label      TEXT        NULL,
    api_key_id BIGINT      NULL REFERENCES api_keys (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, request_id)
);

-- The foreign key above makes deleting an api_keys row look for the feedback it
-- sent. The retention job deletes retired keys that never made a request, which
-- is what a front end that only rates answers with its own key is, so without
-- this index each deleted key scans the whole table.
CREATE INDEX IF NOT EXISTS idx_request_feedback_api_key
    ON request_feedback (api_key_id) WHERE api_key_id IS NOT NULL;

-- The admin summary reads one project's window.
CREATE INDEX IF NOT EXISTS idx_request_feedback_project_created
    ON request_feedback (project_id, created_at);

-- Retention deletes across all projects in created_at order, in batches; this
-- index lets each batch walk the oldest rows instead of scanning the table
-- (the shape of idx_request_logs_created).
CREATE INDEX IF NOT EXISTS idx_request_feedback_created
    ON request_feedback (created_at);
