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
--
-- Lock order and lock timeout. The runner applies this file in one
-- transaction, and every ALTER TABLE takes an ACCESS EXCLUSIVE lock that it
-- holds to the commit. The gateway reads the project on every request and
-- writes a request_logs row at the end of every request, so a lock request
-- that waits behind a long read (a CSV export, a summary) stops the requests
-- that queue up behind it, on every replica, for as long as that read runs.
-- request_logs is altered first, so nothing is held on projects while the
-- migration waits for it, and SET LOCAL lock_timeout bounds the wait itself.
-- On a timeout the statement fails (SQLSTATE 55P03), the transaction rolls back
-- whole and store.Open returns the error; the process manager starts the
-- replica again and the migration runs from the start. See 0033 and
-- docs/scaling.md for what the operator does.
SET LOCAL lock_timeout = '2s';

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS guardrail_blocked_stage TEXT NULL,
    ADD COLUMN IF NOT EXISTS guardrail_redactions INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS guardrail_flags INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS guardrail_rules TEXT[] NULL,
    ADD COLUMN IF NOT EXISTS guardrail_webhook TEXT NULL;

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS guardrails JSONB NULL;
