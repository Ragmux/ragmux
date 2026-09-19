-- Model prices and the cache/cost columns of the request log.
--
-- model_prices is seeded from the embedded table on start. A row an operator
-- edits flips to source 'user' and is never overwritten by a later upgrade;
-- a 'builtin' row is refreshed only when the shipped builtin_version grows.
CREATE TABLE IF NOT EXISTS model_prices (
    id BIGSERIAL PRIMARY KEY,
    provider_type TEXT NOT NULL,
    model_pattern TEXT NOT NULL,
    input_per_mtok NUMERIC(14,6) NOT NULL DEFAULT 0,
    output_per_mtok NUMERIC(14,6) NOT NULL DEFAULT 0,
    cache_write_per_mtok NUMERIC(14,6) NULL,
    cache_read_per_mtok NUMERIC(14,6) NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    source TEXT NOT NULL DEFAULT 'builtin' CHECK (source IN ('builtin','user')),
    builtin_version INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_type, model_pattern)
);

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS cached_prompt_tokens INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_write_tokens INT NOT NULL DEFAULT 0,
    -- cost_micros is USD x 1e6 so SUM() stays exact in BIGINT
    ADD COLUMN IF NOT EXISTS cost_micros BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cost_source TEXT NOT NULL DEFAULT 'none';
