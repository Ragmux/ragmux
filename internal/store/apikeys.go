package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
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
	// AllowedCIDRs and AllowedOrigins are the key's network allowlists
	// (ADR-039), in the canonical form NormalizeCIDRs and NormalizeOrigins
	// produce. Empty means no restriction; they are never nil on a key read
	// from the store, so the JSON shows [] and not null.
	AllowedCIDRs   []string `json:"allowed_cidrs"`
	AllowedOrigins []string `json:"allowed_origins"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// Retired reports whether the key is revoked or past its expiry at t.
func (k *APIKey) Retired(t time.Time) bool {
	return k.RevokedAt != nil || (k.ExpiresAt != nil && !k.ExpiresAt.After(t))
}

//nolint:gosec // G101: a SELECT column list, not a credential.
const apiKeyCols = `k.id, k.kind, k.name, k.key_prefix, k.user_id, k.created_by, k.scopes, k.default_project_id,
	k.rate_limit_rpm, k.rate_limit_tpm, k.budget_daily_tokens, k.budget_monthly_tokens,
	k.expires_at, k.revoked_at, k.last_used_at, k.created_at, k.updated_at,
	ARRAY(SELECT project_id FROM api_key_projects WHERE api_key_id = k.id ORDER BY project_id),
	k.allowed_cidrs::text[], k.allowed_origins`

func scanAPIKey(row interface{ Scan(...any) error }, extra ...any) (*APIKey, error) {
	k := &APIKey{}
	var created, updated time.Time
	dst := []any{&k.ID, &k.Kind, &k.Name, &k.KeyPrefix, &k.UserID, &k.CreatedBy, &k.Scopes, &k.DefaultProjectID,
		&k.RateLimitRPM, &k.RateLimitTPM, &k.BudgetDailyTokens, &k.BudgetMonthlyTokens,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &created, &updated, &k.ProjectIDs,
		&k.AllowedCIDRs, &k.AllowedOrigins}
	if err := row.Scan(append(dst, extra...)...); err != nil {
		return nil, scanErr(err)
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if k.ProjectIDs == nil {
		k.ProjectIDs = []int64{}
	}
	if k.AllowedCIDRs == nil {
		k.AllowedCIDRs = []string{}
	}
	if k.AllowedOrigins == nil {
		k.AllowedOrigins = []string{}
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
		rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens, expires_at,
		allowed_cidrs, allowed_origins)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::text[]::cidr[], $15) RETURNING id`,
		k.Kind, k.Name, HashToken(key), keyPrefix(key), k.UserID, k.CreatedBy, scopes, k.DefaultProjectID,
		k.RateLimitRPM, k.RateLimitTPM, k.BudgetDailyTokens, k.BudgetMonthlyTokens, k.ExpiresAt,
		nonNilStrings(k.AllowedCIDRs), nonNilStrings(k.AllowedOrigins)).Scan(&id)
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
		expires_at=$8, allowed_cidrs=$9::text[]::cidr[], allowed_origins=$10, updated_at=now() WHERE id=$11`,
		k.Name, scopes, k.DefaultProjectID, k.RateLimitRPM, k.RateLimitTPM,
		k.BudgetDailyTokens, k.BudgetMonthlyTokens, k.ExpiresAt,
		nonNilStrings(k.AllowedCIDRs), nonNilStrings(k.AllowedOrigins), k.ID)
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

// RevokeUserManagementKeys revokes the account's management keys and reports
// how many were still live.
//
// A changed password kills every session, which is how someone reacts to a
// suspected leak -- but a management key minted from that session outlives it
// and can still act on the account, so it has to go with them. Gateway keys
// are left alone deliberately: they cannot touch the management API, and
// killing an application's credentials on a routine password change costs
// more than it protects.
func (s *Store) RevokeUserManagementKeys(ctx context.Context, userID int64) (int64, error) {
	res, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now(), updated_at = now()
		 WHERE user_id = $1 AND kind = $2 AND revoked_at IS NULL`, userID, KindManagement)
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
	return s.deleteAllExpired(ctx, RetainAPIKeys, cutoff)
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

// Reasons a key's network allowlist refuses a request (ADR-039). They are the
// "code" of the 403 the gateway answers and the values of
// ragmux_gateway_key_denied_total{reason}.
const (
	DenyIPNotAllowed     = "ip_not_allowed"
	DenyOriginNotAllowed = "origin_not_allowed"
)

// MaxKeyNetworkEntries bounds each of a key's two allowlists.
const MaxKeyNetworkEntries = 32

// NormalizeCIDRs validates a CIDR allowlist and returns it in the form the
// store keeps: a bare address becomes /32 or /128, an IPv4-mapped IPv6 network
// (::ffff:a.b.c.d/n, n >= 96) becomes the IPv4 network it stands for, and
// repeated entries collapse. A network with bits set to the right of its mask
// is refused rather than silently widened: 10.0.0.5/24 is more likely a typo
// than a request for 10.0.0.0/24. The result is never nil.
func NormalizeCIDRs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		p, err := parseCIDREntry(raw)
		if err != nil {
			return nil, err
		}
		s := p.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) > MaxKeyNetworkEntries {
		return nil, fmt.Errorf("at most %d CIDR entries are allowed", MaxKeyNetworkEntries)
	}
	return out, nil
}

func parseCIDREntry(raw string) (netip.Prefix, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return netip.Prefix{}, errors.New("an empty CIDR entry")
	}
	var p netip.Prefix
	if strings.Contains(v, "/") {
		var err error
		if p, err = netip.ParsePrefix(v); err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a CIDR network or an IP address", raw)
		}
	} else {
		a, err := netip.ParseAddr(v)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a CIDR network or an IP address", raw)
		}
		if a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q: an address with a zone is not allowed", raw)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is4In6() {
		// An IPv4-mapped network counts as the IPv4 one. A mask shorter than
		// 96 bits reaches beyond the mapped range and has no IPv4 reading.
		if p.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("%q: an IPv4-mapped network needs a mask of at least /96", raw)
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("%q has bits set to the right of its mask; did you mean %s", raw, p.Masked())
	}
	return p, nil
}

// hostLabels is the shape of a host name in an allowed origin: dot-separated
// labels of lower-case ASCII letters, digits and hyphens. A browser sends an
// internationalised name in punycode, so that is what an entry must hold.
var hostLabels = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// origin is a parsed allowed-origin entry or request Origin.
type origin struct {
	scheme   string
	host     string // for a wildcard entry, the part after "*."
	port     string // "" for the scheme's default
	wildcard bool
}

// parseOrigin parses scheme://host[:port], lower-cased, with the scheme's
// default port dropped (a browser leaves it out of the Origin header). With
// wildcard set a leading "*." on the host is accepted. It does not accept a
// path, query, fragment or user info.
func parseOrigin(raw string, wildcard bool) (origin, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = strings.TrimSuffix(v, "/")
	scheme, rest, ok := strings.Cut(v, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return origin{}, errors.New(`must look like https://example.com (scheme http or https, then host)`)
	}
	if rest == "" || strings.ContainsAny(rest, "/?#@ \t") {
		return origin{}, errors.New("must not carry a path, query, fragment or user info")
	}
	o := origin{scheme: scheme}
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return origin{}, errors.New("an IPv6 host needs a closing bracket")
		}
		a, err := netip.ParseAddr(rest[1:end])
		if err != nil || !a.Is6() || a.Zone() != "" {
			return origin{}, errors.New("is not a valid IPv6 host")
		}
		rest = rest[end+1:]
		o.host = "[" + a.String() + "]"
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return origin{}, errors.New("unexpected text after the host")
			}
			o.port = rest[1:]
		}
	} else {
		host, port, _ := strings.Cut(rest, ":")
		if strings.Contains(port, ":") {
			return origin{}, errors.New("is not a valid host and port")
		}
		o.port = port
		if strings.HasPrefix(host, "*.") {
			if !wildcard {
				return origin{}, errors.New("a wildcard is not valid here")
			}
			o.wildcard = true
			host = strings.TrimPrefix(host, "*.")
			// "*.com" would admit every site of a TLD; a registrable name
			// has at least one dot after the wildcard.
			if !strings.Contains(host, ".") {
				return origin{}, errors.New(`a wildcard needs a domain with a dot after it, such as https://*.example.com`)
			}
		}
		if !hostLabels.MatchString(host) || len(host) > 253 {
			return origin{}, errors.New("host is not a valid lower-case ASCII host name")
		}
		o.host = host
	}
	if o.port != "" {
		n, err := strconv.Atoi(o.port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != o.port {
			return origin{}, errors.New("port must be a number between 1 and 65535")
		}
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			o.port = ""
		}
	}
	return o, nil
}

func (o origin) String() string {
	s := o.scheme + "://"
	if o.wildcard {
		s += "*."
	}
	s += o.host
	if o.port != "" {
		s += ":" + o.port
	}
	return s
}

// NormalizeOrigins validates an Origin allowlist and returns it in the form
// the store keeps: scheme://host[:port] or scheme://*.domain[:port], lower
// case, no trailing slash, the scheme's default port dropped, repeated entries
// collapsed. A lone "*" is refused. The result is never nil.
func NormalizeOrigins(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		if strings.TrimSpace(raw) == "*" {
			return nil, errors.New(`"*" alone is not allowed; name the origins`)
		}
		o, err := parseOrigin(raw, true)
		if err != nil {
			return nil, fmt.Errorf("origin %q: %s", raw, err.Error())
		}
		s := o.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) > MaxKeyNetworkEntries {
		return nil, fmt.Errorf("at most %d origin entries are allowed", MaxKeyNetworkEntries)
	}
	return out, nil
}

// CheckKeyNetwork applies a key's allowlists (ADR-039) to a request and
// returns "" when it may proceed, else DenyIPNotAllowed or
// DenyOriginNotAllowed. clientIP is the request's remote address as the
// gateway sees it ("ip" or "ip:port"; behind TRUST_PROXY_HEADERS the address
// the proxy reported); origins holds every Origin header value of the request.
//
// An empty list restricts nothing. A non-empty one fails closed: an address
// that does not parse is refused, and so is a request without an Origin, with
// two different ones, or with one no entry matches ("null" included).
func CheckKeyNetwork(cidrs, allowedOrigins []string, clientIP string, origins []string) string {
	if len(cidrs) > 0 && !ipAllowed(cidrs, clientIP) {
		return DenyIPNotAllowed
	}
	if len(allowedOrigins) > 0 && !originAllowed(allowedOrigins, origins) {
		return DenyOriginNotAllowed
	}
	return ""
}

func ipAllowed(cidrs []string, remote string) bool {
	var ip netip.Addr
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		ip = ap.Addr()
	} else if a, err := netip.ParseAddr(remote); err == nil {
		ip = a
	} else {
		return false
	}
	ip = ip.WithZone("").Unmap()
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			continue // not a value the store can hold (the column is CIDR)
		}
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func originAllowed(allowed, got []string) bool {
	if len(got) != 1 {
		// No Origin, or several: a browser sends exactly one.
		return false
	}
	o, err := parseOrigin(got[0], false)
	if err != nil {
		return false
	}
	for _, e := range allowed {
		a, err := parseOrigin(e, true)
		if err != nil || a.scheme != o.scheme || a.port != o.port {
			continue
		}
		switch {
		case !a.wildcard && a.host == o.host:
			return true
		case a.wildcard && strings.HasSuffix(o.host, "."+a.host):
			return true
		}
	}
	return false
}

// CountCIDRRestricted counts the live gateway keys and the projects whose own
// key carries a CIDR allowlist (ADR-039). The boot check for an unverified
// proxy-header setting uses it: it needs to know whether any such list exists,
// not what it holds.
func (s *Store) CountCIDRRestricted(ctx context.Context) (keys, projects int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM api_keys WHERE kind = 'gateway' AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > now()) AND cardinality(allowed_cidrs) > 0),
		(SELECT count(*) FROM projects WHERE cardinality(key_allowed_cidrs) > 0)`).Scan(&keys, &projects)
	return keys, projects, err
}
