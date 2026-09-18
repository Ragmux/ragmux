package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelConnection describes one upstream LLM (or embedding) endpoint.
type ModelConnection struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	// APIKey is populated only for internal use; JSON output masks it.
	APIKey       string `json:"-"`
	APIKeyMasked string `json:"api_key_masked"`
	ModelName    string `json:"model_name"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// ValidProviderTypes lists the supported provider identifiers.
var ValidProviderTypes = []string{"openai", "anthropic", "gemini", "deepseek", "ollama", "custom_openai"}

// IsValidProviderType checks membership in ValidProviderTypes.
func IsValidProviderType(t string) bool {
	for _, v := range ValidProviderTypes {
		if v == t {
			return true
		}
	}
	return false
}

// MaskKey renders a credential as a short, non-reversible hint.
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:3] + "..." + k[len(k)-4:]
}

const connCols = "id, name, provider_type, base_url, api_key_enc, key_version, model_name, created_at, updated_at"

func (s *Store) scanConn(row interface{ Scan(...any) error }) (*ModelConnection, error) {
	c := &ModelConnection{}
	var enc []byte
	var version int16
	var created, updated time.Time
	if err := row.Scan(&c.ID, &c.Name, &c.ProviderType, &c.BaseURL, &enc, &version, &c.ModelName, &created, &updated); err != nil {
		return nil, scanErr(err)
	}
	c.CreatedAt, c.UpdatedAt = ts(created), ts(updated)
	key, err := s.decryptKey(c.ID, version, enc)
	if err != nil {
		return nil, err
	}
	c.APIKey = key
	c.APIKeyMasked = MaskKey(key)
	return c, nil
}

// decryptKey opens a stored credential according to its key version.
func (s *Store) decryptKey(id int64, version int16, enc []byte) (string, error) {
	aad, err := aadFor(version, id)
	if err != nil {
		return "", fmt.Errorf("connection %d: %w", id, err)
	}
	key, err := s.cipher.decrypt(enc, aad)
	if err != nil {
		return "", fmt.Errorf("decrypt api key for connection %d: %w", id, err)
	}
	return key, nil
}

// CreateConnection stores a new provider connection. The row is inserted
// first so its id is known, then the key is sealed with that id as
// associated data; both happen in one transaction.
func (s *Store) CreateConnection(ctx context.Context, c *ModelConnection) (*ModelConnection, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO model_connections
		(name, provider_type, base_url, api_key_enc, key_version, model_name) VALUES ($1, $2, $3, NULL, $4, $5)
		RETURNING id`, c.Name, c.ProviderType, c.BaseURL, keyVersionBound, c.ModelName).Scan(&id); err != nil {
		return nil, err
	}
	enc, err := s.cipher.encrypt(c.APIKey, connectionAAD(id))
	if err != nil {
		return nil, err
	}
	out, err := s.scanConn(tx.QueryRow(ctx, "UPDATE model_connections SET api_key_enc = $1 WHERE id = $2 RETURNING "+connCols, enc, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateConnection replaces mutable fields. An empty APIKey keeps the old one
// (and its key version); a new key is sealed at the current version.
func (s *Store) UpdateConnection(ctx context.Context, c *ModelConnection) (*ModelConnection, error) {
	if c.APIKey == "" {
		_, err := s.pool.Exec(ctx, `UPDATE model_connections SET name=$1, provider_type=$2, base_url=$3,
			model_name=$4, updated_at=now() WHERE id=$5`,
			c.Name, c.ProviderType, c.BaseURL, c.ModelName, c.ID)
		if err != nil {
			return nil, err
		}
	} else {
		enc, err := s.cipher.encrypt(c.APIKey, connectionAAD(c.ID))
		if err != nil {
			return nil, err
		}
		_, err = s.pool.Exec(ctx, `UPDATE model_connections SET name=$1, provider_type=$2, base_url=$3,
			api_key_enc=$4, key_version=$5, model_name=$6, updated_at=now() WHERE id=$7`,
			c.Name, c.ProviderType, c.BaseURL, enc, keyVersionBound, c.ModelName, c.ID)
		if err != nil {
			return nil, err
		}
	}
	return s.GetConnection(ctx, c.ID)
}

// GetConnection fetches one connection with its decrypted key.
func (s *Store) GetConnection(ctx context.Context, id int64) (*ModelConnection, error) {
	return s.scanConn(s.pool.QueryRow(ctx, "SELECT "+connCols+" FROM model_connections WHERE id = $1", id))
}

// ListConnections returns all connections ordered by name.
func (s *Store) ListConnections(ctx context.Context) ([]*ModelConnection, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+connCols+" FROM model_connections ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ModelConnection{}
	for rows.Next() {
		c, err := s.scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection removes a connection; fails if projects or stores use it.
func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM model_connections WHERE id = $1", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// encRow is a credential row as read for re-sealing.
type encRow struct {
	id      int64
	version int16
	enc     []byte
}

// lockedRows reads the credential rows matching where (FOR UPDATE) inside tx.
func lockedRows(ctx context.Context, tx pgx.Tx, where string) ([]encRow, error) {
	rows, err := tx.Query(ctx, "SELECT id, api_key_enc, key_version FROM model_connections "+where+" ORDER BY id FOR UPDATE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []encRow
	for rows.Next() {
		var r encRow
		if err := rows.Scan(&r.id, &r.enc, &r.version); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resealRows decrypts each row with the store's cipher at its recorded
// version and writes it back sealed by to with the connection id as
// associated data, verifying the result before the update.
func (s *Store) resealRows(ctx context.Context, tx pgx.Tx, to *cipher, todo []encRow) error {
	for _, r := range todo {
		plain, err := s.decryptKey(r.id, r.version, r.enc)
		if err != nil {
			return err
		}
		aad := connectionAAD(r.id)
		enc, err := to.encrypt(plain, aad)
		if err != nil {
			return err
		}
		if back, err := to.decrypt(enc, aad); err != nil || back != plain {
			return fmt.Errorf("verify re-encrypted key for connection %d: %w", r.id, err)
		}
		if _, err := tx.Exec(ctx, "UPDATE model_connections SET api_key_enc = $1, key_version = $2 WHERE id = $3",
			enc, keyVersionBound, r.id); err != nil {
			return err
		}
	}
	return nil
}

// ReencryptConnections re-encrypts every stored provider key with the key
// given as 64 hex characters, in one transaction, and verifies each row
// decrypts with the new key before committing. Rows come out at the current
// key version. It returns the number of rows rewritten. The store keeps
// using its current key; restart the gateway with the new SECRET_KEY
// afterwards.
func (s *Store) ReencryptConnections(ctx context.Context, newKeyHex string) (int, error) {
	newCipher, _, err := loadCipher(newKeyHex, "", s.log)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	todo, err := lockedRows(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	if err := s.resealRows(ctx, tx, newCipher, todo); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(todo), nil
}

// UpgradeConnectionKeys re-seals every credential still stored without
// associated data (key_version 0) with the current key and the connection
// id bound in, in one transaction, and returns how many rows changed. It is
// safe to run on every start and from several replicas at once: rows are
// locked and only version-0 rows are touched.
func (s *Store) UpgradeConnectionKeys(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	todo, err := lockedRows(ctx, tx, fmt.Sprintf("WHERE key_version = %d", keyVersionLegacy))
	if err != nil {
		return 0, err
	}
	if len(todo) == 0 {
		return 0, nil
	}
	if err := s.resealRows(ctx, tx, s.cipher, todo); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(todo), nil
}
