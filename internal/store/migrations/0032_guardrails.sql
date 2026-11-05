-- Guardrails (ADR-034). Add-only (ADR-001): every column is nullable or has a
-- constant default, so a row written before this migration, or by a replica
-- that predates it, keeps meaning what it always meant -- no guardrails, none
-- triggered. PostgreSQL 11+ adds a NOT NULL column with a constant default
-- without rewriting the table.
--
--   projects.guardrails                    the project's rules and settings as
--                                          one JSON object (rules, stream
--                                          window, webhook switch). NULL is a
--                                          project without guardrails. The
--                                          shape is validated by the admin API
--                                          and by internal/guardrail.
--   request_logs.guardrail_blocked_stage   the stage a rule blocked the request
--                                          at (pre_request, pre_retrieval or
--                                          post_response); NULL when none did.
--   request_logs.guardrail_redactions      how many matches were replaced.
--   request_logs.guardrail_flags           how many matches were only counted.
--   request_logs.guardrail_rules           the ids of the rules that matched,
--                                          each once, at most 16; NULL when
--                                          none did. A webhook verdict appears
--                                          as "webhook:<reason>".
--   request_logs.guardrail_webhook         what the guardrail webhook did:
--                                          ok, error, timeout or
--                                          skipped_fail_open; NULL when it was
--                                          not asked.
--
-- None of these holds the text that matched, its hash or its position: a
-- TCKN has about 10^9 candidates and its hash is recovered by brute force
-- (PRD rule 10).
--
-- Rollback (manual, nothing else depends on these columns):
--   ALTER TABLE request_logs DROP COLUMN guardrail_webhook, DROP COLUMN guardrail_rules,
--       DROP COLUMN guardrail_flags, DROP COLUMN guardrail_redactions,
--       DROP COLUMN guardrail_blocked_stage;
--   ALTER TABLE projects DROP COLUMN guardrails;
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS guardrails JSONB NULL;

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS guardrail_blocked_stage TEXT NULL,
    ADD COLUMN IF NOT EXISTS guardrail_redactions INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS guardrail_flags INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS guardrail_rules TEXT[] NULL,
    ADD COLUMN IF NOT EXISTS guardrail_webhook TEXT NULL;
