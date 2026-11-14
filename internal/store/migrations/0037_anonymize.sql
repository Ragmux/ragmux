-- KVKK anonymisation of a user (ADR-040). Add-only (ADR-001): the one added
-- column is nullable, so a row written before this migration, or by a replica
-- that predates it, keeps meaning what it always meant -- an account that has
-- not been anonymised. PostgreSQL adds a nullable column without rewriting the
-- table.
--
--   users.anonymized_at   when the account's personal fields were made
--                         irreversible. NULL = not anonymised. A second
--                         anonymisation of the same account is refused while
--                         this is set.
--
-- Rollback (manual, nothing else depends on this column):
--   ALTER TABLE users DROP COLUMN anonymized_at;
-- Dropping it forgets which accounts were anonymised; the pseudonyms stay.
--
-- Lock timeout. The gateway resolves every key through users, and the ALTER
-- below takes an ACCESS EXCLUSIVE lock that the runner's transaction holds to
-- the commit. SET LOCAL lock_timeout bounds the wait for that lock: a migration
-- that cannot get it within 2 seconds fails (SQLSTATE 55P03), the transaction
-- rolls back and store.Open returns the error, instead of queueing every
-- request behind it. Retry the restart if the lock timed out. Only one table is
-- touched, so there is no lock order to keep.
SET LOCAL lock_timeout = '2s';

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS anonymized_at TIMESTAMPTZ NULL;
