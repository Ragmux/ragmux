package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// User is a dashboard/management account.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	CreatedAt    string `json:"created_at"`
}

const userCols = "id, username, password_hash, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var created time.Time
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created); err != nil {
		return nil, scanErr(err)
	}
	u.CreatedAt = ts(created)
	return u, nil
}

// CountUsers returns the number of registered users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CreateUser inserts a user with an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		"INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING "+userCols, username, passwordHash))
}

// GetUser fetches a user by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE id = $1", id))
}

// GetUserByUsername fetches a user by login name.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE username = $1", username))
}

// UpdateUserPassword replaces the stored hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.pool.Exec(ctx, "UPDATE users SET password_hash = $1 WHERE id = $2", passwordHash, id)
	return err
}

// HashToken produces the stable lookup hash for session and API tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession stores a session for the given raw token.
func (s *Store) CreateSession(ctx context.Context, userID int64, token string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx,
		"INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, $3)",
		userID, HashToken(token), time.Now().UTC().Add(ttl))
	return err
}

// UserBySession resolves a raw session token to its user, if still valid.
func (s *Store) UserBySession(ctx context.Context, token string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.password_hash, u.created_at
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.token_hash = $1 AND se.expires_at > now()`, HashToken(token)))
}

// DeleteSession revokes a raw session token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", HashToken(token))
	return err
}

// PurgeExpiredSessions removes stale sessions.
func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= now()")
	return err
}
