-- Per-project RAG behaviour (ADR-024). Add-only (ADR-001): every column has a
-- default that reproduces the behaviour of a project that never set it, so a
-- row written before this migration, or by a replica that predates it, means
-- what it always meant.
--
--   context_placement          where retrieved context lands: 'system' (the
--                              system message, as before) or 'before_last_user'
--                              (a leading text part of the last user message).
--   retrieval_policy           when retrieval runs: 'always' (as before),
--                              'auto' or 'off'. Stored here; acted on later.
--   query_strategy             what is searched for: 'last_user' (as before) or
--                              'condense'. Stored here; acted on later.
--   history_turns_for_query    turns of history 'condense' looks at, 1..10.
--   query_model_connection_id  connection that runs 'condense'; NULL falls back
--                              to the first route target. RESTRICT on purpose:
--                              deleting a connection a project still uses for
--                              this must fail, not silently change the setting.
--
-- The CHECK lists widen in later releases; widening is not the narrowing ADR-001
-- forbids, and the application falls back to the default for a value it does
-- not know. The constraints are named so a later migration can replace them.
--
-- Rollback (manual, nothing else depends on these columns):
--   ALTER TABLE projects
--       DROP COLUMN query_model_connection_id, DROP COLUMN history_turns_for_query,
--       DROP COLUMN query_strategy, DROP COLUMN retrieval_policy,
--       DROP COLUMN context_placement;
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS context_placement TEXT NOT NULL DEFAULT 'system'
        CONSTRAINT projects_context_placement_check CHECK (context_placement IN ('system', 'before_last_user')),
    ADD COLUMN IF NOT EXISTS retrieval_policy TEXT NOT NULL DEFAULT 'always'
        CONSTRAINT projects_retrieval_policy_check CHECK (retrieval_policy IN ('always', 'auto', 'off')),
    ADD COLUMN IF NOT EXISTS query_strategy TEXT NOT NULL DEFAULT 'last_user'
        CONSTRAINT projects_query_strategy_check CHECK (query_strategy IN ('last_user', 'condense')),
    ADD COLUMN IF NOT EXISTS history_turns_for_query INT NOT NULL DEFAULT 3
        CONSTRAINT projects_history_turns_for_query_check CHECK (history_turns_for_query BETWEEN 1 AND 10),
    ADD COLUMN IF NOT EXISTS query_model_connection_id BIGINT NULL
        REFERENCES model_connections (id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_projects_query_model_connection
    ON projects (query_model_connection_id) WHERE query_model_connection_id IS NOT NULL;
