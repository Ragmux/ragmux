package store

import (
	"context"
	"time"
)

// login_attempts.username is always lower case (T11): Record writes it that
// way and every count and delete below compares with lower($1), so "ali",
// "ALI" and "Ali" are one counter, as they are one account (the unique index
// on lower(users.username)). The second-factor gate (VerifySecondFactor) writes
// and counts with the same spelling; a different one would silently turn its
// lock off. The stage column keeps the two counters apart: the password
// counters below only see 'password' rows, the second factor's lock only 'mfa'.
const (
	loginStagePassword = "password"
	loginStageMFA      = "mfa"
)

// RecordLoginAttempt stores one password-stage login attempt for rate
// limiting.
func (s *Store) RecordLoginAttempt(ctx context.Context, username, ip string, success bool) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO login_attempts (username, ip, success, stage) VALUES (lower($1), $2, $3, $4)",
		username, ip, success, loginStagePassword)
	return err
}

// CountFailedLoginAttempts counts failures since the given time, separately
// for the username and for the IP address.
func (s *Store) CountFailedLoginAttempts(ctx context.Context, username, ip string, since time.Time) (byUser, byIP int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE username = lower($1)),
		COUNT(*) FILTER (WHERE ip = $2)
		FROM login_attempts WHERE stage = 'password' AND NOT success AND created_at >= $3 AND (username = lower($1) OR ip = $2)`,
		username, ip, since.UTC()).Scan(&byUser, &byIP)
	return byUser, byIP, err
}

// CountFailedLoginAttemptsForPair counts failures since the given time for
// the username coming from the IP address (the lockout key).
func (s *Store) CountFailedLoginAttemptsForPair(ctx context.Context, username, ip string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM login_attempts
		WHERE stage = 'password' AND NOT success AND created_at >= $3 AND username = lower($1) AND ip = $2`,
		username, ip, since.UTC()).Scan(&n)
	return n, err
}

// DeleteLoginAttemptsBefore purges attempts older than t batch by batch
// and reports how many rows were removed.
func (s *Store) DeleteLoginAttemptsBefore(ctx context.Context, t time.Time) (int64, error) {
	return s.deleteAllExpired(ctx, RetainLoginAttempts, t)
}

// OldestFailedLoginAttempt returns the time of the oldest failure for the
// username from the IP address since t, or the zero time when there is none.
func (s *Store) OldestFailedLoginAttempt(ctx context.Context, username, ip string, since time.Time) (time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `SELECT MIN(created_at) FROM login_attempts
		WHERE stage = 'password' AND username = lower($1) AND ip = $2 AND NOT success AND created_at >= $3`,
		username, ip, since.UTC()).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// CountFailedLoginAttemptsSince counts every password-stage failure recorded
// since t. Second-factor failures are not counted: the admin summary describes
// the password limiter, and a wrong TOTP code is not a password attack.
func (s *Store) CountFailedLoginAttemptsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM login_attempts WHERE stage = 'password' AND NOT success AND created_at >= $1",
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
		WHERE stage = 'password' AND NOT success AND created_at >= $1 GROUP BY username, ip HAVING COUNT(*) >= $2
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
