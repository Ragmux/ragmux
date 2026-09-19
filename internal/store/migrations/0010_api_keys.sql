-- User-owned API keys: "sk-user-" credentials for /v1 and "sk-mgmt-"
-- credentials for /admin/api.
--
-- projects.api_key_hash stays exactly where it is and is not mirrored into
-- this table. That legacy key has no owner, cannot be revoked (only rotated)
-- and cannot expire; giving it a synthetic owner would force user_id, and with
-- it every constraint below, to be nullable for one row per project. The
-- dashboard labels it "the project's default key" instead.
CREATE TABLE IF NOT EXISTS api_keys (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('gateway','management')),
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    default_project_id BIGINT REFERENCES projects(id) ON DELETE SET NULL,
    rate_limit_rpm INT NOT NULL DEFAULT 0 CHECK (rate_limit_rpm >= 0),
    rate_limit_tpm INT NOT NULL DEFAULT 0 CHECK (rate_limit_tpm >= 0),
    budget_daily_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_daily_tokens >= 0),
    budget_monthly_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_monthly_tokens >= 0),
    expires_at TIMESTAMPTZ, revoked_at TIMESTAMPTZ, last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_name_per_user UNIQUE (user_id, name),
    CONSTRAINT api_keys_mgmt_has_no_project CHECK (kind <> 'management' OR default_project_id IS NULL),
    CONSTRAINT api_keys_mgmt_has_no_limits CHECK (kind <> 'management'
        OR (rate_limit_rpm=0 AND rate_limit_tpm=0 AND budget_daily_tokens=0 AND budget_monthly_tokens=0))
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id, kind);
CREATE INDEX IF NOT EXISTS idx_api_keys_retired ON api_keys(revoked_at, expires_at)
    WHERE revoked_at IS NOT NULL OR expires_at IS NOT NULL;

-- Which projects a gateway key may route to. These grants, not
-- project_members, are what /v1 enforces: membership is a dashboard
-- visibility concept, and an admin editing a member list must not silently
-- break a production key.
CREATE TABLE IF NOT EXISTS api_key_projects (
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (api_key_id, project_id)
);
CREATE INDEX IF NOT EXISTS idx_api_key_projects_project ON api_key_projects(project_id);

-- key_usage mirrors project_usage exactly so the same code paths and the same
-- retention windows apply.
CREATE TABLE IF NOT EXISTS key_usage (
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    period TEXT NOT NULL CHECK (period IN ('minute','day','month')),
    period_start TIMESTAMPTZ NOT NULL,
    requests INT NOT NULL DEFAULT 0,
    prompt_tokens BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (api_key_id, period, period_start)
);
CREATE INDEX IF NOT EXISTS idx_key_usage_period ON key_usage(period, period_start);

-- Attribution on the request log. ON DELETE SET NULL plus a denormalised
-- user_id so deleting a key never destroys billing history; the janitor only
-- deletes keys no request log still references, which is what makes that safe.
-- Deliberately no index on either column: an index build on a large
-- request_logs would stall the boot inside migrate()'s transaction, and
-- nothing in v0.4 queries by them.
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS api_key_id BIGINT NULL REFERENCES api_keys(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL;
