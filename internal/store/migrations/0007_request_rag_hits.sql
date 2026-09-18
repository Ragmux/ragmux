-- rag_hits records how many retrieved chunks were injected into the prompt,
-- so operators can see whether RAG returns thin or rich context per request
-- rather than only whether it ran at all (rag_used).
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS rag_hits INT NOT NULL DEFAULT 0;
