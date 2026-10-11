-- Named routes inside a project and the weighted targets behind them
-- (ADR-017). Add-only (ADR-001): projects.model_connection_id stays and keeps
-- serving every project that has no route.
--
--   project_routes   one row per route name; kind says which endpoint family
--                    the route serves (only chat is resolved by the gateway
--                    today), strategy how a target is picked.
--   route_targets    the connections behind a route. connection_id is RESTRICT
--                    on purpose: deleting a connection that a route still
--                    uses must fail rather than leave a route with no target.
--                    model, when set, replaces the connection's model_name for
--                    requests sent through that target; NULL keeps it.
--
-- Rollback (manual, nothing else depends on these tables):
--   DROP TABLE route_targets; DROP TABLE project_routes;
CREATE TABLE IF NOT EXISTS project_routes (
    id         BIGSERIAL PRIMARY KEY,
    project_id BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 256),
    kind       TEXT        NOT NULL DEFAULT 'chat' CHECK (kind IN ('chat', 'embeddings', 'rerank')),
    strategy   TEXT        NOT NULL DEFAULT 'weighted' CHECK (strategy IN ('weighted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS route_targets (
    id            BIGSERIAL PRIMARY KEY,
    route_id      BIGINT      NOT NULL REFERENCES project_routes (id) ON DELETE CASCADE,
    connection_id BIGINT      NOT NULL REFERENCES model_connections (id) ON DELETE RESTRICT,
    model         TEXT,
    weight        INT         NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 1000),
    priority      INT         NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 100),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_route_targets_route ON route_targets (route_id);
CREATE INDEX IF NOT EXISTS idx_route_targets_connection ON route_targets (connection_id);

-- NULLS NOT DISTINCT needs PostgreSQL 15, so the NULL model is folded to ''.
CREATE UNIQUE INDEX IF NOT EXISTS idx_route_targets_unique
    ON route_targets (route_id, connection_id, COALESCE(model, ''));
