-- TOTP two-step dashboard login, recovery codes and the user-level MFA lock
-- (ADR-037). Add-only (ADR-001): every added column is nullable or has a
-- constant default, so a row written before this migration, or by a replica
-- that predates it, keeps meaning what it always meant -- no second factor,
-- stage 'password', no failed password tries on the session. PostgreSQL 11+
-- adds a NOT NULL column with a constant default without rewriting the table.
--
--   users.totp_secret_enc         the base32 secret sealed with the instance
--                                 key (AAD ragmux:user_totp:<id>; registered
--                                 in store/sealed.go so rotate-key covers it).
--                                 NULL = no secret.
--   users.totp_key_version        key version the secret was sealed at.
--   users.totp_enabled_at         when the second factor was switched on. A
--                                 secret with this NULL is an abandoned setup
--                                 and does not make the login two-step.
--   users.totp_last_step          the last accepted 30 s step (RFC 6238 5.2):
--                                 a step at or below it is refused, so a code
--                                 passes once.
--   user_recovery_codes           SHA-256 hashes of the ten recovery codes;
--                                 used_at is set when one is spent.
--   login_challenges              the state between the password and the
--                                 second factor: the hash of a one-time token,
--                                 five minutes of life, at most five tries.
--   login_attempts.stage          'password' for the existing counters,
--                                 'mfa' for the second-factor failures. The
--                                 password counters filter on 'password', so a
--                                 foreign MFA lock never shows in a login 429.
--   sessions.password_failures    password tries a session has made against
--                                 its own account (setup, enable, disable,
--                                 recovery-codes, change password); the
--                                 session ends at the fifth wrong one.
--
-- Locks. users, sessions and login_attempts are read on the request path
-- (users on every gateway key resolution, sessions and login_attempts on admin
-- requests), and ADD COLUMN takes ACCESS EXCLUSIVE on its table for an instant.
-- SET LOCAL lock_timeout bounds the wait: a migration that cannot get its lock
-- within 2 seconds fails instead of queueing, and the requests behind it go on.
-- An ACCESS EXCLUSIVE lock is held until the migration commits, so the table read
-- most, users, is altered last: its lock is taken as late as possible and held
-- the shortest. Retry the restart if a lock timed out.
--
-- Rolling back is a code rollback only; the columns and tables stay. Do not
-- return to a binary older than v0.8 once an account has switched TOTP on:
-- that binary does not know the second factor and lets the password in alone.
SET LOCAL lock_timeout = '2s';

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS password_failures SMALLINT NOT NULL DEFAULT 0;

ALTER TABLE login_attempts
    ADD COLUMN IF NOT EXISTS stage TEXT NOT NULL DEFAULT 'password';

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS totp_secret_enc  BYTEA NULL,
    ADD COLUMN IF NOT EXISTS totp_key_version SMALLINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS totp_enabled_at  TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS totp_last_step   BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS user_recovery_codes (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, code_hash)
);

CREATE TABLE IF NOT EXISTS login_challenges (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    bearer     BOOLEAN NOT NULL DEFAULT false,
    attempts   SMALLINT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip         TEXT NOT NULL DEFAULT ''
);
-- The retention job deletes by expires_at; a password change or an admin reset
-- deletes by user.
CREATE INDEX IF NOT EXISTS idx_login_challenges_expires ON login_challenges(expires_at);
CREATE INDEX IF NOT EXISTS idx_login_challenges_user ON login_challenges(user_id);
