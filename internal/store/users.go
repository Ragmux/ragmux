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

// CountUsers returns the number of registered users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CreateUser inserts a user with an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO users(username, password_hash) VALUES (?, ?)", username, passwordHash)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetUser(ctx, id)
}

// GetUser fetches a user by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, created_at FROM users WHERE id = ?", id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	return u, scanErr(err)
}

// GetUserByUsername fetches a user by login name.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, created_at FROM users WHERE username = ?", username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	return u, scanErr(err)
}

// UpdateUserPassword replaces the stored hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", passwordHash, id)
	return err
}

// HashToken produces the stable lookup hash for session and API tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession stores a session for the given raw token.
func (s *Store) CreateSession(ctx context.Context, userID int64, token string, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions(user_id, token_hash, expires_at) VALUES (?, ?, ?)",
		userID, HashToken(token), time.Now().UTC().Add(ttl).Format(time.RFC3339))
	return err
}

// UserBySession resolves a raw session token to its user, if still valid.
func (s *Store) UserBySession(ctx context.Context, token string) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.password_hash, u.created_at
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.token_hash = ? AND se.expires_at > ?`,
		HashToken(token), time.Now().UTC().Format(time.RFC3339)).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	return u, scanErr(err)
}

// DeleteSession revokes a raw session token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", HashToken(token))
	return err
}

// PurgeExpiredSessions removes stale sessions.
func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?",
		time.Now().UTC().Format(time.RFC3339))
	return err
}
