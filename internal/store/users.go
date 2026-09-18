package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// User is a dashboard/management account.
type User struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	Role         string     `json:"role"`
	IsActive     bool       `json:"is_active"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	CreatedAt    string     `json:"created_at"`
}

const userCols = "id, username, password_hash, role, is_active, last_login_at, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var created time.Time
	var lastLogin *time.Time
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.IsActive, &lastLogin, &created); err != nil {
		return nil, scanErr(err)
	}
	if lastLogin != nil {
		t := lastLogin.UTC()
		u.LastLoginAt = &t
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

// CountActiveAdmins returns how many active users hold the admin role.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE role = 'admin' AND is_active").Scan(&n)
	return n, err
}

// CreateUser inserts a user with an already-hashed password and a role.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		"INSERT INTO users (username, password_hash, role) VALUES ($1, $2, $3) RETURNING "+userCols,
		username, passwordHash, role))
}

// GetUser fetches a user by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE id = $1", id))
}

// GetUserByUsername fetches a user by login name.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE username = $1", username))
}

// ListUsers returns every account ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+userCols+" FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser changes the role and active flag of an account.
func (s *Store) UpdateUser(ctx context.Context, id int64, role string, isActive bool) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		"UPDATE users SET role = $1, is_active = $2 WHERE id = $3 RETURNING "+userCols, role, isActive, id))
}

// DeleteUser removes an account; its sessions and memberships cascade.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM users WHERE id = $1", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserPassword replaces the stored hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.pool.Exec(ctx, "UPDATE users SET password_hash = $1 WHERE id = $2", passwordHash, id)
	return err
}

// TouchLastLogin records a successful login time.
func (s *Store) TouchLastLogin(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, "UPDATE users SET last_login_at = now() WHERE id = $1", id)
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
// Sessions of deactivated accounts do not resolve.
func (s *Store) UserBySession(ctx context.Context, token string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.password_hash, u.role, u.is_active, u.last_login_at, u.created_at
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.token_hash = $1 AND se.expires_at > now() AND u.is_active`, HashToken(token)))
}

// DeleteSession revokes a raw session token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", HashToken(token))
	return err
}

// DeleteUserSessions revokes every session of one user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", userID)
	return err
}

// PurgeExpiredSessions removes sessions that expired before t and reports
// how many rows were removed.
func (s *Store) PurgeExpiredSessions(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= $1", t.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
