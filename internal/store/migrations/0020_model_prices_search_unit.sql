-- Search-unit price of a rerank API (Cohere bills a rerank call per search
-- unit, not per token). Additive and nullable: NULL means the provider has no
-- per-search-unit charge, so every existing row keeps meaning what it did.
-- The figure is in the row's currency per search unit.
ALTER TABLE model_prices
    ADD COLUMN IF NOT EXISTS per_search_unit NUMERIC(14,6) NULL CHECK (per_search_unit >= 0);
