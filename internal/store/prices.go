package store

import (
	"context"
	"errors"
	"time"
)

// ErrBuiltinPrice is returned when a built-in price row is deleted. Built-in
// rows can be edited or reset but never removed: a deleted one would come
// back at the next upgrade, and remembering that it was deleted would need a
// tombstone column that nothing else in the schema needs.
var ErrBuiltinPrice = errors.New("built-in price rows cannot be deleted")

// Price row origins, mirroring the model_prices CHECK constraint.
const (
	PriceSourceBuiltin = "builtin"
	PriceSourceUser    = "user"
)

// ModelPrice is one row of the price table: what a provider charges per
// million tokens for the models matching model_pattern ("*" is a wildcard).
// The cache prices are absolute, not multipliers of the input price, and are
// nullable: an absent one falls back to input_per_mtok.
type ModelPrice struct {
	ID                int64    `json:"id"`
	ProviderType      string   `json:"provider_type"`
	ModelPattern      string   `json:"model_pattern"`
	InputPerMTok      float64  `json:"input_per_mtok"`
	OutputPerMTok     float64  `json:"output_per_mtok"`
	CacheWritePerMTok *float64 `json:"cache_write_per_mtok"`
	CacheReadPerMTok  *float64 `json:"cache_read_per_mtok"`
	Currency          string   `json:"currency"`
	Source            string   `json:"source"`
	BuiltinVersion    int      `json:"builtin_version"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

const priceColumns = `id, provider_type, model_pattern, input_per_mtok::float8, output_per_mtok::float8,
	cache_write_per_mtok::float8, cache_read_per_mtok::float8, currency, source, builtin_version,
	created_at, updated_at`

// scanPrice reads one row in priceColumns order.
func scanPrice(row interface{ Scan(...any) error }) (*ModelPrice, error) {
	p := &ModelPrice{}
	var created, updated time.Time
	if err := row.Scan(&p.ID, &p.ProviderType, &p.ModelPattern, &p.InputPerMTok, &p.OutputPerMTok,
		&p.CacheWritePerMTok, &p.CacheReadPerMTok, &p.Currency, &p.Source, &p.BuiltinVersion,
		&created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = ts(created), ts(updated)
	return p, nil
}

// ListModelPrices returns the whole table, grouped by provider type and
// ordered so the most specific pattern of a provider comes first.
func (s *Store) ListModelPrices(ctx context.Context) ([]*ModelPrice, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+priceColumns+` FROM model_prices
		ORDER BY provider_type, model_pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ModelPrice{}
	for rows.Next() {
		p, err := scanPrice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetModelPrice reads one row.
func (s *Store) GetModelPrice(ctx context.Context, id int64) (*ModelPrice, error) {
	p, err := scanPrice(s.pool.QueryRow(ctx, `SELECT `+priceColumns+` FROM model_prices WHERE id = $1`, id))
	if err != nil {
		return nil, scanErr(err)
	}
	return p, nil
}

// CreateModelPrice adds an operator-supplied row. It is always a "user" row:
// upgrades leave those alone.
func (s *Store) CreateModelPrice(ctx context.Context, p *ModelPrice) (*ModelPrice, error) {
	out, err := scanPrice(s.pool.QueryRow(ctx, `INSERT INTO model_prices
		(provider_type, model_pattern, input_per_mtok, output_per_mtok,
		 cache_write_per_mtok, cache_read_per_mtok, currency, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'user') RETURNING `+priceColumns,
		p.ProviderType, p.ModelPattern, p.InputPerMTok, p.OutputPerMTok,
		p.CacheWritePerMTok, p.CacheReadPerMTok, p.Currency))
	if err != nil {
		return nil, scanErr(err)
	}
	return out, nil
}

// UpdateModelPrice overwrites a row's prices and marks it as the operator's.
// A built-in row edited this way stops being refreshed by upgrades, which is
// the point: a deliberate price is not something a release should undo.
func (s *Store) UpdateModelPrice(ctx context.Context, p *ModelPrice) (*ModelPrice, error) {
	out, err := scanPrice(s.pool.QueryRow(ctx, `UPDATE model_prices SET
		input_per_mtok = $2, output_per_mtok = $3, cache_write_per_mtok = $4,
		cache_read_per_mtok = $5, currency = $6, source = 'user', updated_at = now()
		WHERE id = $1 RETURNING `+priceColumns,
		p.ID, p.InputPerMTok, p.OutputPerMTok, p.CacheWritePerMTok, p.CacheReadPerMTok, p.Currency))
	if err != nil {
		return nil, scanErr(err)
	}
	return out, nil
}

// ResetModelPrice restores a row to the built-in values in b and makes it a
// "builtin" row again, so later upgrades pick it up.
func (s *Store) ResetModelPrice(ctx context.Context, id int64, b *ModelPrice) (*ModelPrice, error) {
	out, err := scanPrice(s.pool.QueryRow(ctx, `UPDATE model_prices SET
		input_per_mtok = $2, output_per_mtok = $3, cache_write_per_mtok = $4,
		cache_read_per_mtok = $5, currency = $6, source = 'builtin',
		builtin_version = $7, updated_at = now()
		WHERE id = $1 RETURNING `+priceColumns,
		id, b.InputPerMTok, b.OutputPerMTok, b.CacheWritePerMTok, b.CacheReadPerMTok,
		b.Currency, b.BuiltinVersion))
	if err != nil {
		return nil, scanErr(err)
	}
	return out, nil
}

// DeleteModelPrice removes an operator-supplied row. A built-in row is
// refused with ErrBuiltinPrice; reset it instead.
func (s *Store) DeleteModelPrice(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM model_prices WHERE id = $1 AND source = 'user'", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing was deleted: either the row does not exist or it is built-in.
	if _, err := s.GetModelPrice(ctx, id); err != nil {
		return err
	}
	return ErrBuiltinPrice
}
