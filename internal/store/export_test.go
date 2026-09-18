package store

import (
	"context"
	"time"
)

// KeyVersionBound is exported for tests that inspect key_version.
const KeyVersionBound = keyVersionBound

// InsertLegacyConnection stores a connection the way releases before
// key_version did: sealed without associated data and marked version 0.
func (s *Store) InsertLegacyConnection(ctx context.Context, name, key string) (int64, error) {
	enc, err := s.cipher.encrypt(key, nil)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO model_connections
		(name, provider_type, base_url, api_key_enc, key_version, model_name)
		VALUES ($1, 'openai', '', $2, 0, 'm') RETURNING id`, name, enc).Scan(&id)
	return id, err
}

// BackdateRequestLog moves a request log's created_at so tests can populate
// earlier windows.
func (s *Store) BackdateRequestLog(ctx context.Context, id int64, t time.Time) error {
	_, err := s.pool.Exec(ctx, "UPDATE request_logs SET created_at = $2 WHERE id = $1", id, t.UTC())
	return err
}
