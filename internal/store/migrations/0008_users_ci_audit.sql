-- Usernames become case-insensitive: login matches lower(username) and the
-- unique index below refuses two accounts that differ only by case. Existing
-- data that already collides makes the migration (and so the start-up) fail
-- with the colliding names, because there is no safe automatic rename; see
-- docs/users-and-limits.md for the manual fix.
DO $$
DECLARE
    dup TEXT;
BEGIN
    SELECT string_agg(name, ', ' ORDER BY name) INTO dup
      FROM (SELECT lower(username) AS name FROM users GROUP BY lower(username) HAVING count(*) > 1) d;
    IF dup IS NOT NULL THEN
        RAISE EXCEPTION 'usernames that differ only by case exist (%); rename or delete the duplicates before upgrading, see docs/users-and-limits.md#case-insensitive-usernames', dup;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_lower ON users (lower(username));

-- The audit log is filtered by action prefix and by time window.
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs (action text_pattern_ops, id);
