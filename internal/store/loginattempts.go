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

// CountFailedLoginAttemptsForPair counts failures since the given time for
// the username coming from the IP address (the lockout key).
func (s *Store) CountFailedLoginAttemptsForPair(ctx context.Context, username, ip string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM login_attempts
		WHERE NOT success AND created_at >= $3 AND username = $1 AND ip = $2`,
		username, ip, since.UTC()).Scan(&n)
	return n, err
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
// username from the IP address since t, or the zero time when there is none.
func (s *Store) OldestFailedLoginAttempt(ctx context.Context, username, ip string, since time.Time) (time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `SELECT MIN(created_at) FROM login_attempts
		WHERE username = $1 AND ip = $2 AND NOT success AND created_at >= $3`, username, ip, since.UTC()).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// CountFailedLoginAttemptsSince counts every failure recorded since t.
func (s *Store) CountFailedLoginAttemptsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM login_attempts WHERE NOT success AND created_at >= $1",
		since.UTC()).Scan(&n)
	return n, err
}

// FailedLoginPair is a username/address pair with its failures since a
// point in time and the time of the oldest counted failure.
type FailedLoginPair struct {
	Username string
	IP       string
	Failures int
	Oldest   time.Time
}

// FailedLoginPairsSince returns the username/address pairs with at least
// min failures since t, most failures first.
func (s *Store) FailedLoginPairsSince(ctx context.Context, since time.Time, minFailures int) ([]FailedLoginPair, error) {
	rows, err := s.pool.Query(ctx, `SELECT username, ip, COUNT(*), MIN(created_at) FROM login_attempts
		WHERE NOT success AND created_at >= $1 GROUP BY username, ip HAVING COUNT(*) >= $2
		ORDER BY COUNT(*) DESC, username, ip`, since.UTC(), minFailures)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FailedLoginPair{}
	for rows.Next() {
		var p FailedLoginPair
		if err := rows.Scan(&p.Username, &p.IP, &p.Failures, &p.Oldest); err != nil {
			return nil, err
		}
		p.Oldest = p.Oldest.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}
