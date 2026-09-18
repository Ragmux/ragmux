-- Per-project rate limits and token budgets. Zero means unlimited.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS rate_limit_rpm        INT    NOT NULL DEFAULT 0 CHECK (rate_limit_rpm >= 0),
    ADD COLUMN IF NOT EXISTS rate_limit_tpm        INT    NOT NULL DEFAULT 0 CHECK (rate_limit_tpm >= 0),
    ADD COLUMN IF NOT EXISTS budget_daily_tokens   BIGINT NOT NULL DEFAULT 0 CHECK (budget_daily_tokens >= 0),
    ADD COLUMN IF NOT EXISTS budget_monthly_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_monthly_tokens >= 0);

-- Usage counters per UTC minute / day / month. Rows are upserted by the
-- gateway on every request and purged by the hourly retention job.
CREATE TABLE IF NOT EXISTS project_usage (
    project_id        BIGINT      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    period            TEXT        NOT NULL CHECK (period IN ('minute', 'day', 'month')),
    period_start      TIMESTAMPTZ NOT NULL,
    requests          INT         NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, period, period_start)
);
CREATE INDEX IF NOT EXISTS idx_project_usage_period ON project_usage(period, period_start);
