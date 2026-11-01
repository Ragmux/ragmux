-- Tool governance and sessions (ADR-032). Add-only (ADR-001): every column has
-- a default that reproduces the behaviour of a row written before this
-- migration, or by a replica that predates it, so such a row means what it
-- always meant -- no tool lists, no session cap, no session.
--
--   projects.tool_allow        glob patterns a request's tools must match;
--                              empty means every tool is allowed.
--   projects.tool_deny         glob patterns whose tools are stripped from the
--                              request; deny always wins over allow.
--   projects.session_token_cap prompt+completion tokens one session may spend;
--                              0 is unlimited.
--   request_logs.session_id    the X-Ragmux-Session value of the request, NULL
--                              when the client sent none. Never a metric label
--                              or a span attribute (PRD rule 10).
--   request_logs.tool_names    the tool names the model called in the answer,
--                              sorted and de-duplicated; never arguments or
--                              content. NULL when it called none.
--   request_logs.tools_denied  how many tools the project's lists stripped.
--                              INT NOT NULL DEFAULT 0 is a constant default,
--                              so PostgreSQL 11+ adds it without rewriting the
--                              table.
--   session_usage              one running total per (project, session): the
--                              session cap reads it with a single-row lookup
--                              and the admin session list reads it without a
--                              GROUP BY over request_logs. Rows go with their
--                              project and are trimmed by the retention job.
--
-- Rollback (manual, nothing else depends on these objects):
--   DROP TABLE session_usage;
--   ALTER TABLE request_logs DROP COLUMN tools_denied, DROP COLUMN tool_names,
--       DROP COLUMN session_id;
--   ALTER TABLE projects DROP COLUMN session_token_cap, DROP COLUMN tool_deny,
--       DROP COLUMN tool_allow;
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS tool_allow TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS tool_deny TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS session_token_cap BIGINT NOT NULL DEFAULT 0;

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS session_id TEXT NULL,
    ADD COLUMN IF NOT EXISTS tool_names TEXT[] NULL,
    ADD COLUMN IF NOT EXISTS tools_denied INT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS session_usage (
    project_id        BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    session_id        TEXT        NOT NULL,
    requests          BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    first_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, session_id)
);

-- The session list orders by last_seen within a project, and retention deletes
-- by last_seen across all of them.
CREATE INDEX IF NOT EXISTS idx_session_usage_last_seen ON session_usage (project_id, last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_session_usage_retention ON session_usage (last_seen);
