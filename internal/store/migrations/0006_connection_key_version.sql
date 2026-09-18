-- key_version records how a connection's api_key_enc was sealed:
--   0: AES-GCM without associated data (releases before 0.2.3)
--   1: AES-GCM with the connection id as associated data, so a blob moved
--      to another row no longer decrypts.
-- Rows at version 0 are re-sealed by the gateway on its next start.
ALTER TABLE model_connections ADD COLUMN IF NOT EXISTS key_version SMALLINT NOT NULL DEFAULT 0;
