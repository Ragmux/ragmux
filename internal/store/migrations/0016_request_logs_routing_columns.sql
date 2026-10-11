-- Columns that make one /v1 request traceable and that routing, retry and the
-- other endpoints write into. All nullable and add-only (ADR-001): old rows
-- keep NULL, and nothing that reads request_logs today has to know about them.
--
--   request_id     the id returned to the client in X-Request-Id
--   endpoint       which /v1 endpoint served the row, e.g. chat.completions
--   requested_model the model string the client sent, as it sent it
--   route          the route that picked the targets (NULL until routing exists)
--   attempt        total upstream attempts made for the request
--   fallback_from  connection id of the first target, when a fallback served
--                  the request (ADR-020). BIGINT to match model_connections.id;
--                  no foreign key, so deleting a connection never touches logs.
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS request_id      TEXT,
    ADD COLUMN IF NOT EXISTS endpoint        TEXT,
    ADD COLUMN IF NOT EXISTS requested_model TEXT,
    ADD COLUMN IF NOT EXISTS route           TEXT,
    ADD COLUMN IF NOT EXISTS attempt         SMALLINT,
    ADD COLUMN IF NOT EXISTS fallback_from   BIGINT;
