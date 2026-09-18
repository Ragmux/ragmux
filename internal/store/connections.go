package store

import (
	"context"
	"fmt"
	"time"
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

const connCols = "id, name, provider_type, base_url, api_key_enc, model_name, created_at, updated_at"

func (s *Store) scanConn(row interface{ Scan(...any) error }) (*ModelConnection, error) {
	c := &ModelConnection{}
	var enc []byte
	var created, updated time.Time
	if err := row.Scan(&c.ID, &c.Name, &c.ProviderType, &c.BaseURL, &enc, &c.ModelName, &created, &updated); err != nil {
		return nil, scanErr(err)
	}
	c.CreatedAt, c.UpdatedAt = ts(created), ts(updated)
	key, err := s.cipher.decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt api key for connection %d: %w", c.ID, err)
	}
	c.APIKey = key
	c.APIKeyMasked = MaskKey(key)
	return c, nil
}

// CreateConnection stores a new provider connection, encrypting the key.
func (s *Store) CreateConnection(ctx context.Context, c *ModelConnection) (*ModelConnection, error) {
	enc, err := s.cipher.encrypt(c.APIKey)
	if err != nil {
		return nil, err
	}
	return s.scanConn(s.pool.QueryRow(ctx, `INSERT INTO model_connections
		(name, provider_type, base_url, api_key_enc, model_name) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+connCols, c.Name, c.ProviderType, c.BaseURL, enc, c.ModelName))
}

// UpdateConnection replaces mutable fields. An empty APIKey keeps the old one.
func (s *Store) UpdateConnection(ctx context.Context, c *ModelConnection) (*ModelConnection, error) {
	if c.APIKey == "" {
		_, err := s.pool.Exec(ctx, `UPDATE model_connections SET name=$1, provider_type=$2, base_url=$3,
			model_name=$4, updated_at=now() WHERE id=$5`,
			c.Name, c.ProviderType, c.BaseURL, c.ModelName, c.ID)
		if err != nil {
			return nil, err
		}
	} else {
		enc, err := s.cipher.encrypt(c.APIKey)
		if err != nil {
			return nil, err
		}
		_, err = s.pool.Exec(ctx, `UPDATE model_connections SET name=$1, provider_type=$2, base_url=$3,
			api_key_enc=$4, model_name=$5, updated_at=now() WHERE id=$6`,
			c.Name, c.ProviderType, c.BaseURL, enc, c.ModelName, c.ID)
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

// ReencryptConnections re-encrypts every stored provider key with the key
// given as 64 hex characters, in one transaction, and verifies each row
// decrypts with the new key before committing. It returns the number of
// rows rewritten. The store keeps using its current key; restart the
// gateway with the new SECRET_KEY afterwards.
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
	rows, err := tx.Query(ctx, "SELECT id, api_key_enc FROM model_connections ORDER BY id FOR UPDATE")
	if err != nil {
		return 0, err
	}
	type row struct {
		id  int64
		enc []byte
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.enc); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range todo {
		plain, err := s.cipher.decrypt(r.enc)
		if err != nil {
			return 0, fmt.Errorf("decrypt api key for connection %d with the current key: %w", r.id, err)
		}
		enc, err := newCipher.encrypt(plain)
		if err != nil {
			return 0, err
		}
		if back, err := newCipher.decrypt(enc); err != nil || back != plain {
			return 0, fmt.Errorf("verify re-encrypted key for connection %d: %w", r.id, err)
		}
		if _, err := tx.Exec(ctx, "UPDATE model_connections SET api_key_enc = $1 WHERE id = $2", enc, r.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(todo), nil
}
