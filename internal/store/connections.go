package store

import (
	"context"
	"fmt"
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
	if err := row.Scan(&c.ID, &c.Name, &c.ProviderType, &c.BaseURL, &enc, &c.ModelName, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO model_connections
		(name, provider_type, base_url, api_key_enc, model_name) VALUES (?, ?, ?, ?, ?)`,
		c.Name, c.ProviderType, c.BaseURL, enc, c.ModelName)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetConnection(ctx, id)
}

// UpdateConnection replaces mutable fields. An empty APIKey keeps the old one.
func (s *Store) UpdateConnection(ctx context.Context, c *ModelConnection) (*ModelConnection, error) {
	if c.APIKey == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE model_connections SET name=?, provider_type=?, base_url=?,
			model_name=?, updated_at=? WHERE id=?`,
			c.Name, c.ProviderType, c.BaseURL, c.ModelName, now(), c.ID)
		if err != nil {
			return nil, err
		}
	} else {
		enc, err := s.cipher.encrypt(c.APIKey)
		if err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE model_connections SET name=?, provider_type=?, base_url=?,
			api_key_enc=?, model_name=?, updated_at=? WHERE id=?`,
			c.Name, c.ProviderType, c.BaseURL, enc, c.ModelName, now(), c.ID)
		if err != nil {
			return nil, err
		}
	}
	return s.GetConnection(ctx, c.ID)
}

// GetConnection fetches one connection with its decrypted key.
func (s *Store) GetConnection(ctx context.Context, id int64) (*ModelConnection, error) {
	return s.scanConn(s.db.QueryRowContext(ctx, "SELECT "+connCols+" FROM model_connections WHERE id = ?", id))
}

// ListConnections returns all connections ordered by name.
func (s *Store) ListConnections(ctx context.Context) ([]*ModelConnection, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+connCols+" FROM model_connections ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ModelConnection
	for rows.Next() {
		c, err := s.scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if out == nil {
		out = []*ModelConnection{}
	}
	return out, rows.Err()
}

// DeleteConnection removes a connection; fails if projects or stores use it.
func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM model_connections WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
