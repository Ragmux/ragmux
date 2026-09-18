-- Per-store quotas: an editor can otherwise fill the database and run up the
-- embedding bill without limit. 0 means unlimited; the instance-wide
-- MAX_DOCUMENTS_PER_STORE / MAX_BYTES_PER_STORE_MB ceilings apply on top.
ALTER TABLE rag_stores
    ADD COLUMN IF NOT EXISTS max_documents INT    NOT NULL DEFAULT 0 CHECK (max_documents >= 0),
    ADD COLUMN IF NOT EXISTS max_bytes     BIGINT NOT NULL DEFAULT 0 CHECK (max_bytes >= 0);
