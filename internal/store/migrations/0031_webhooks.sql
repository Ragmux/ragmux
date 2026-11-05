-- Outbound webhooks (ADR-033). Add-only (ADR-001): two new tables, nothing
-- existing is touched, so the indexes are created with them (the tables are
-- empty; no CONCURRENTLY needed).
--
--   webhook_endpoints   at most one endpoint per project and purpose. The
--                       secret is sealed with the instance key and the row id
--                       as associated data (AAD "ragmux:webhook_endpoint:<id>",
--                       key_version 1, see store/sealed.go). prev_secret_enc
--                       keeps the previous secret until prev_expires_at so a
--                       receiver can switch during a rotation. disabled_at is
--                       set when the receiver answers 410 Gone.
--   webhook_deliveries  the outbox of asynchronous webhooks (limit alerts).
--                       Synchronous calls (guardrail) never write here: they
--                       carry request content. payload is the alert body only.
--                       next_attempt_at doubles as the claim lease: a worker
--                       that picks a row pushes it into the future, so a crash
--                       leaves the row to be retried rather than stuck.
--                       last_error_class is a closed set; an upstream response
--                       body is never stored. Rows are trimmed by the
--                       retention job (LOG_RETENTION_DAYS).
--
-- Rollback (manual, nothing else depends on these tables):
--   DROP TABLE webhook_deliveries; DROP TABLE webhook_endpoints;
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id              BIGSERIAL PRIMARY KEY,
    project_id      BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    purpose         TEXT        NOT NULL CHECK (purpose IN ('guardrail', 'alerts')),
    url             TEXT        NOT NULL CHECK (octet_length(url) BETWEEN 1 AND 2048),
    secret_enc      BYTEA       NOT NULL,
    key_version     SMALLINT    NOT NULL DEFAULT 1,
    prev_secret_enc BYTEA       NULL,
    prev_expires_at TIMESTAMPTZ NULL,
    disabled_at     TIMESTAMPTZ NULL,
    created_by      BIGINT      NULL REFERENCES users (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, purpose)
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id               BIGSERIAL PRIMARY KEY,
    endpoint_id      BIGINT      NOT NULL REFERENCES webhook_endpoints (id) ON DELETE CASCADE,
    event_type       TEXT        NOT NULL,
    dedupe_key       TEXT        NOT NULL,
    payload          JSONB       NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts         INT         NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status      INT         NULL,
    last_error_class TEXT        NULL CHECK (last_error_class IN ('timeout', 'network', 'blocked', 'http_4xx', 'http_5xx')),
    last_duration_ms INT         NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at     TIMESTAMPTZ NULL,
    UNIQUE (endpoint_id, dedupe_key)
);

-- The worker's poll: pending rows that are due, oldest first.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due
    ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
-- Retention deletes by created_at.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_created
    ON webhook_deliveries (created_at);
