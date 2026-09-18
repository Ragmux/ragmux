package store

import (
	"context"
	"time"
)

// RecordLoginAttempt stores one login attempt for rate limiting.
func (s *Store) RecordLoginAttempt(ctx context.Context, username, ip string, success bool) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO login_attempts (username, ip, success) VALUES ($1, $2, $3)",
		username, ip, success)
	return err
}

// CountFailedLoginAttempts counts failures since the given time, separately
// for the username and for the IP address.
func (s *Store) CountFailedLoginAttempts(ctx context.Context, username, ip string, since time.Time) (byUser, byIP int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE username = $1),
		COUNT(*) FILTER (WHERE ip = $2)
		FROM login_attempts WHERE NOT success AND created_at >= $3 AND (username = $1 OR ip = $2)`,
		username, ip, since.UTC()).Scan(&byUser, &byIP)
	return byUser, byIP, err
}

// DeleteLoginAttemptsBefore purges attempts older than t and reports how
// many rows were removed.
func (s *Store) DeleteLoginAttemptsBefore(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM login_attempts WHERE created_at < $1", t.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// OldestFailedLoginAttempt returns the time of the oldest failure for the
// username since t, or the zero time when there is none.
func (s *Store) OldestFailedLoginAttempt(ctx context.Context, username string, since time.Time) (time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `SELECT MIN(created_at) FROM login_attempts
		WHERE username = $1 AND NOT success AND created_at >= $2`, username, since.UTC()).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}
