package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIKey is a user-owned credential. Gateway keys authenticate /v1 calls and
// carry their own limits and project grants; management keys authenticate
// /admin/api calls and carry neither.
type APIKey struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	UserID    int64  `json:"user_id"`
	// CreatedBy is the account that issued the key, nil once that account is
	// deleted. It differs from UserID when an admin mints a key for someone.
	CreatedBy *int64   `json:"created_by"`
	Scopes    []string `json:"scopes"`
	// DefaultProjectID picks a project when the request names none; nil for
	// management keys.
	DefaultProjectID *int64 `json:"default_project_id"`
	// Limits; zero means the project ceiling alone applies.
	RateLimitRPM        int        `json:"rate_limit_rpm"`
	RateLimitTPM        int        `json:"rate_limit_tpm"`
	BudgetDailyTokens   int64      `json:"budget_daily_tokens"`
	BudgetMonthlyTokens int64      `json:"budget_monthly_tokens"`
	ExpiresAt           *time.Time `json:"expires_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
	LastUsedAt          *time.Time `json:"last_used_at"`
	// ProjectIDs is the grant set of a gateway key.
	ProjectIDs []int64 `json:"project_ids"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

// Retired reports whether the key is revoked or past its expiry at t.
func (k *APIKey) Retired(t time.Time) bool {
	return k.RevokedAt != nil || (k.ExpiresAt != nil && !k.ExpiresAt.After(t))
}

const apiKeyCols = `k.id, k.kind, k.name, k.key_prefix, k.user_id, k.created_by, k.scopes, k.default_project_id,
	k.rate_limit_rpm, k.rate_limit_tpm, k.budget_daily_tokens, k.budget_monthly_tokens,
	k.expires_at, k.revoked_at, k.last_used_at, k.created_at, k.updated_at,
	ARRAY(SELECT project_id FROM api_key_projects WHERE api_key_id = k.id ORDER BY project_id)`

func scanAPIKey(row interface{ Scan(...any) error }, extra ...any) (*APIKey, error) {
	k := &APIKey{}
	var created, updated time.Time
	dst := []any{&k.ID, &k.Kind, &k.Name, &k.KeyPrefix, &k.UserID, &k.CreatedBy, &k.Scopes, &k.DefaultProjectID,
		&k.RateLimitRPM, &k.RateLimitTPM, &k.BudgetDailyTokens, &k.BudgetMonthlyTokens,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &created, &updated, &k.ProjectIDs}
	if err := row.Scan(append(dst, extra...)...); err != nil {
		return nil, scanErr(err)
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if k.ProjectIDs == nil {
		k.ProjectIDs = []int64{}
	}
	for _, t := range []**time.Time{&k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt} {
		if *t != nil {
			utc := (*t).UTC()
			*t = &utc
		}
	}
	k.CreatedAt, k.UpdatedAt = ts(created), ts(updated)
	return k, nil
}

// HasScope reports whether the key carries the scope.
func (k *APIKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// CreateAPIKey inserts a key with its project grants in one transaction and
// returns it plus the plaintext credential, which is never stored.
func (s *Store) CreateAPIKey(ctx context.Context, k *APIKey) (*APIKey, string, error) {
	key, err := GenerateAPIKey(k.Kind)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO api_keys
		(kind, name, key_hash, key_prefix, user_id, created_by, scopes, default_project_id,
		rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		k.Kind, k.Name, HashToken(key), keyPrefix(key), k.UserID, k.CreatedBy, scopes, k.DefaultProjectID,
		k.RateLimitRPM, k.RateLimitTPM, k.BudgetDailyTokens, k.BudgetMonthlyTokens, k.ExpiresAt).Scan(&id)
	if err != nil {
		return nil, "", err
	}
	if err := setKeyProjects(ctx, tx, id, k.ProjectIDs); err != nil {
		return nil, "", err
	}
	out, err := scanAPIKey(tx.QueryRow(ctx, "SELECT "+apiKeyCols+" FROM api_keys k WHERE k.id = $1", id))
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return out, key, nil
}

func setKeyProjects(ctx context.Context, tx pgx.Tx, keyID int64, projectIDs []int64) error {
	if _, err := tx.Exec(ctx, "DELETE FROM api_key_projects WHERE api_key_id = $1", keyID); err != nil {
		return err
	}
	for _, pid := range projectIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO api_key_projects (api_key_id, project_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, keyID, pid); err != nil {
			return err
		}
	}
	return nil
}

// UpdateAPIKey changes the mutable fields and replaces the grant set. The
// credential itself is never changed: a key is revoked and reissued instead.
func (s *Store) UpdateAPIKey(ctx context.Context, k *APIKey) (*APIKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	res, err := tx.Exec(ctx, `UPDATE api_keys SET name=$1, scopes=$2, default_project_id=$3,
		rate_limit_rpm=$4, rate_limit_tpm=$5, budget_daily_tokens=$6, budget_monthly_tokens=$7,
		expires_at=$8, updated_at=now() WHERE id=$9`,
		k.Name, scopes, k.DefaultProjectID, k.RateLimitRPM, k.RateLimitTPM,
		k.BudgetDailyTokens, k.BudgetMonthlyTokens, k.ExpiresAt, k.ID)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if err := setKeyProjects(ctx, tx, k.ID, k.ProjectIDs); err != nil {
		return nil, err
	}
	out, err := scanAPIKey(tx.QueryRow(ctx, "SELECT "+apiKeyCols+" FROM api_keys k WHERE k.id = $1", k.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAPIKey fetches one key.
func (s *Store) GetAPIKey(ctx context.Context, id int64) (*APIKey, error) {
	return scanAPIKey(s.pool.QueryRow(ctx, "SELECT "+apiKeyCols+" FROM api_keys k WHERE k.id = $1", id))
}

// APIKeyFilter narrows ListAPIKeys. A nil UserID means every owner.
type APIKeyFilter struct {
	UserID *int64
	Kind   string
}

// APIKeyListItem is a key plus the owner name the listing shows.
type APIKeyListItem struct {
	*APIKey
	Username string `json:"username"`
}

// ListAPIKeys returns the matching keys, newest first, with their owner name.
func (s *Store) ListAPIKeys(ctx context.Context, f APIKeyFilter) ([]*APIKeyListItem, error) {
	q := "SELECT " + apiKeyCols + ", u.username FROM api_keys k JOIN users u ON u.id = k.user_id WHERE true"
	var args []any
	if f.UserID != nil {
		args = append(args, *f.UserID)
		q += fmt.Sprintf(" AND k.user_id = $%d", len(args))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		q += fmt.Sprintf(" AND k.kind = $%d", len(args))
	}
	rows, err := s.pool.Query(ctx, q+" ORDER BY k.id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKeyListItem{}
	for rows.Next() {
		item := &APIKeyListItem{}
		k, err := scanAPIKey(rows, &item.Username)
		if err != nil {
			return nil, err
		}
		item.APIKey = k
		out = append(out, item)
	}
	return out, rows.Err()
}

// RevokeAPIKey stops a key from resolving. Revocation takes effect on the
// next request: there is no cache in front of the lookup.
func (s *Store) RevokeAPIKey(ctx context.Context, id int64) (*APIKey, error) {
	res, err := s.pool.Exec(ctx, "UPDATE api_keys SET revoked_at = now(), updated_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		// Either unknown or already revoked; the read below tells which.
		k, err := s.GetAPIKey(ctx, id)
		return k, err
	}
	return s.GetAPIKey(ctx, id)
}

// RevokeUserAPIKeys revokes every key of one account and reports how many
// were still live.
func (s *Store) RevokeUserAPIKeys(ctx context.Context, userID int64) (int64, error) {
	res, err := s.pool.Exec(ctx,
		"UPDATE api_keys SET revoked_at = now(), updated_at = now() WHERE user_id = $1 AND revoked_at IS NULL", userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// ErrKeyInUse is returned when a key cannot be deleted because request logs
// still point at it.
var ErrKeyInUse = errors.New("api key is still referenced by request logs")

// DeleteAPIKey removes a key that no request log references. Keys with
// history must be revoked instead, so the attribution on those rows survives.
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx,
		"DELETE FROM api_keys WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM request_logs WHERE api_key_id = $1)", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1)", id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return ErrKeyInUse
	}
	return nil
}

// PurgeRetiredAPIKeys deletes keys revoked or expired before cutoff that no
// request log references. The NOT EXISTS is what makes the ON DELETE SET NULL
// on request_logs.api_key_id safe: attribution is never erased, the row just
// outlives the key it names.
func (s *Store) PurgeRetiredAPIKeys(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.pool.Exec(ctx, `DELETE FROM api_keys
		WHERE (revoked_at < $1 OR expires_at < $1)
		  AND NOT EXISTS (SELECT 1 FROM request_logs WHERE api_key_id = api_keys.id)`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// TouchAPIKeyUsed stamps last_used_at at most once a minute per key, so the
// hot path writes one row per key per minute instead of one per request.
func (s *Store) TouchAPIKeyUsed(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '60 seconds')`, id)
	return err
}

// GatewayKey is a resolved /v1 credential. A project key resolves with Key
// and Owner nil; a user key carries the key row, its live owner and, through
// Key.ProjectIDs, the projects it may route to.
type GatewayKey struct {
	Project *Project
	Key     *APIKey
	Owner   *User
}

// ResolveGatewayKey resolves a plaintext /v1 credential, dispatching on its
// prefix. For a user key everything that can retire it — revocation, expiry
// and the owner's active flag — is part of the one lookup, so there is
// exactly one enforcement point and revocation needs no invalidation.
func (s *Store) ResolveGatewayKey(ctx context.Context, key string) (*GatewayKey, error) {
	switch {
	case strings.HasPrefix(key, ProjectKeyPrefix):
		p, err := s.GetProjectByKey(ctx, key)
		if err != nil {
			return nil, err
		}
		return &GatewayKey{Project: p}, nil
	case strings.HasPrefix(key, GatewayKeyPrefix):
		u := &User{}
		var created time.Time
		var lastLogin *time.Time
		k, err := scanAPIKey(s.pool.QueryRow(ctx, "SELECT "+apiKeyCols+`,
			u.id, u.username, u.password_hash, u.role, u.is_active, u.last_login_at, u.created_at
			FROM api_keys k JOIN users u ON u.id = k.user_id
			WHERE k.key_hash = $1 AND k.kind = 'gateway' AND k.revoked_at IS NULL
			  AND (k.expires_at IS NULL OR k.expires_at > now()) AND u.is_active`, HashToken(key)),
			&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.IsActive, &lastLogin, &created)
		if err != nil {
			return nil, err
		}
		if lastLogin != nil {
			t := lastLogin.UTC()
			u.LastLoginAt = &t
		}
		u.CreatedAt = ts(created)
		return &GatewayKey{Key: k, Owner: u}, nil
	}
	return nil, ErrNotFound
}

// ResolveManagementKey resolves a plaintext "sk-mgmt-" credential with the
// same single-statement rule as ResolveGatewayKey.
func (s *Store) ResolveManagementKey(ctx context.Context, key string) (*APIKey, *User, error) {
	u := &User{}
	var created time.Time
	var lastLogin *time.Time
	k, err := scanAPIKey(s.pool.QueryRow(ctx, "SELECT "+apiKeyCols+`,
		u.id, u.username, u.password_hash, u.role, u.is_active, u.last_login_at, u.created_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1 AND k.kind = 'management' AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > now()) AND u.is_active`, HashToken(key)),
		&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.IsActive, &lastLogin, &created)
	if err != nil {
		return nil, nil, err
	}
	if lastLogin != nil {
		t := lastLogin.UTC()
		u.LastLoginAt = &t
	}
	u.CreatedAt = ts(created)
	return k, u, nil
}

// Why a live key lookup missed, as reported by GetAPIKeyState.
const (
	KeyStateUnknown       = "unknown"
	KeyStateRevoked       = "revoked"
	KeyStateExpired       = "expired"
	KeyStateOwnerInactive = "inactive_owner"
)

// GetAPIKeyState explains a failed resolve. It runs only on a miss, so the
// hot path stays one query, and its answer is only ever shown to a caller
// that already holds the key bytes.
func (s *Store) GetAPIKeyState(ctx context.Context, key string) (string, error) {
	var revoked, expired, ownerActive bool
	err := s.pool.QueryRow(ctx, `SELECT k.revoked_at IS NOT NULL,
		k.expires_at IS NOT NULL AND k.expires_at <= now(), u.is_active
		FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.key_hash = $1`, HashToken(key)).
		Scan(&revoked, &expired, &ownerActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyStateUnknown, nil
	}
	if err != nil {
		return KeyStateUnknown, err
	}
	switch {
	case revoked:
		return KeyStateRevoked, nil
	case expired:
		return KeyStateExpired, nil
	case !ownerActive:
		return KeyStateOwnerInactive, nil
	}
	return KeyStateUnknown, nil
}

// ListProjectsByIDs returns the named projects ordered by name; unknown ids
// are skipped.
func (s *Store) ListProjectsByIDs(ctx context.Context, ids []int64) ([]*Project, error) {
	if len(ids) == 0 {
		return []*Project{}, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT "+projCols+" FROM projects WHERE id = ANY($1) ORDER BY name", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjects(rows)
}
