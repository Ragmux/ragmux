-- Cross-replica document ingestion: a claim with a lease, so two replicas
-- polling the same queue cannot pick up the same document, and a replica
-- that dies mid-flight releases its work when the lease expires.
ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS claimed_by TEXT,
    ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;

-- The claim query orders by id over the claimable rows only; a partial
-- index keeps that cheap next to a documents table that is mostly 'ready'.
CREATE INDEX IF NOT EXISTS idx_documents_claimable
    ON documents (id) WHERE status IN ('pending','processing');

-- instance_settings holds small instance-wide state. Its first use is the
-- secret-key canary: a value only the right SECRET_KEY can open, so a replica
-- with a diverged key fails at boot instead of on every provider call.
CREATE TABLE IF NOT EXISTS instance_settings (
    key        TEXT PRIMARY KEY,
    value      BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
