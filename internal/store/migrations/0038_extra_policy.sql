-- Per-project allow and deny lists for the request fields the gateway does not
-- read itself (ADR-041). Add-only (ADR-001): both columns are NOT NULL with a
-- constant default, so a project written before this migration, or by a replica
-- that predates it, keeps the behaviour it always had -- two empty lists forward
-- every field, as before. PostgreSQL 11+ adds such a column without rewriting
-- the table.
--
--   projects.extra_allow   glob patterns; when not empty, only a field whose
--                          name matches one is forwarded upstream.
--   projects.extra_deny    glob patterns; a field whose name matches one is
--                          removed from the request. Deny wins over allow.
--
-- The lists apply to a chat request's unknown fields (what the gateway keeps in
-- Extra) and to the top-level fields of a native /v1/messages body that the
-- gateway has no name for. The pattern grammar is the tool lists' (ADR-032) and
-- is checked by the admin API, not here.
--
-- Rollback (manual, nothing else depends on these objects):
--   ALTER TABLE projects DROP COLUMN extra_deny, DROP COLUMN extra_allow;
--
-- Lock timeout. The gateway reads projects on every request, and the ALTER
-- below takes an ACCESS EXCLUSIVE lock that the runner's transaction holds to
-- the commit. SET LOCAL lock_timeout bounds the wait for that lock: a migration
-- that cannot get it within 2 seconds fails (SQLSTATE 55P03), the transaction
-- rolls back and store.Open returns the error, instead of queueing every
-- request behind a long read. projects is the only table touched, so there is
-- no lock order to keep. What the operator does when it times out is in
-- docs/scaling.md; 0033 explains the mechanism at length.
SET LOCAL lock_timeout = '2s';

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS extra_allow TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS extra_deny TEXT[] NOT NULL DEFAULT '{}';
