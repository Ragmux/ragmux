-- Connection test outcomes are kept on the row so the dashboard can show when
-- a connection was last verified without running the test again, and
-- upstream_private caches whether the base URL points at a private or local
-- address (NULL: unknown or never computed; the default provider URL counts
-- as public).
ALTER TABLE model_connections
    ADD COLUMN IF NOT EXISTS last_test_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS last_test_ok BOOL NULL,
    ADD COLUMN IF NOT EXISTS last_test_latency_ms INT NULL,
    ADD COLUMN IF NOT EXISTS last_test_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS upstream_private BOOL NULL;

-- Ingestion progress: page_count is known for PDFs only, progress_percent
-- moves 0 -> 10 (parsed) -> 20 (chunked) -> 100 (ready) as batches embed.
ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS page_count INT NULL,
    ADD COLUMN IF NOT EXISTS progress_percent SMALLINT NOT NULL DEFAULT 0;
