-- Per-key IP and Origin allowlists (ADR-039). Add-only (ADR-001): every column
-- is NOT NULL with a constant default, so a key or project written before this
-- migration, or by a replica that predates it, keeps the behaviour it always
-- had -- two empty lists are "no restriction". PostgreSQL 11+ adds such a
-- column without rewriting the table.
--
--   api_keys.allowed_cidrs          networks (IPv4 and IPv6; a single address is
--                                   stored as /32 or /128) a gateway key may be
--                                   used from. Empty: from anywhere.
--   api_keys.allowed_origins        browser origins (scheme://host[:port], or
--                                   https://*.example.com) a gateway key may be
--                                   used from. Empty: any, and none needed.
--                                   When it is not empty a request without an
--                                   Origin header is refused.
--   projects.key_allowed_cidrs      the same two lists for the project's own
--   projects.key_allowed_origins    "sk-proj-" key, which has no api_keys row.
--
-- There is no project ceiling and no intersection: each key carries its own
-- lists. Their shape (at most 32 entries, canonical form) is checked by the
-- admin API and the gateway reads them as stored; the CIDR type refuses a
-- malformed network here. A replica on an older binary does not read the
-- columns, so it applies no allowlist (docs/users-and-limits.md says so).
--
-- Rollback (manual, nothing else depends on these objects):
--   ALTER TABLE api_keys DROP COLUMN allowed_origins, DROP COLUMN allowed_cidrs;
--   ALTER TABLE projects DROP COLUMN key_allowed_origins, DROP COLUMN key_allowed_cidrs;
--
-- Lock order and lock timeout. The gateway reads both tables on every request,
-- and each ALTER TABLE below takes an ACCESS EXCLUSIVE lock that the runner's
-- transaction holds to the commit. SET LOCAL lock_timeout bounds the wait for
-- a lock: a migration that cannot get it within 2 seconds fails (SQLSTATE
-- 55P03), the transaction rolls back and store.Open returns the error, instead
-- of queueing every request behind a long read. projects is altered first: it
-- is the table the dashboard's usage and stats queries join, so it is the one
-- that can show the longer read, and while the migration waits for it nothing
-- else is held. api_keys is then locked with projects already held, which is
-- the shorter wait. What the operator does when it times out is in
-- docs/scaling.md; 0033 explains the mechanism at length.
SET LOCAL lock_timeout = '2s';

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS key_allowed_cidrs CIDR[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS key_allowed_origins TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS allowed_cidrs CIDR[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS allowed_origins TEXT[] NOT NULL DEFAULT '{}';
