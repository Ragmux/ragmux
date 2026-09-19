package pricing

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed writes the built-in price table into model_prices. It is safe to run
// on every start and reports how many rows it inserted or refreshed.
//
// The upsert never touches a row an operator edited: the dashboard flips
// such a row's source to "user", and the WHERE clause only refreshes rows
// that are still "builtin" and older than the shipped version. Builtin rows
// cannot be deleted either (only edited or reset), which is what keeps a
// deleted row from resurrecting on the next upgrade without a tombstone
// column to remember it by.
func Seed(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int64, error) {
	if log == nil {
		log = slog.Default()
	}
	f, err := loadBuiltin()
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var changed int64
	for _, m := range f.Models {
		// An absent cache price is stored as NULL, not as the input price:
		// the column means "this provider charges the input rate", and a
		// later version may fill it in without looking like an edit.
		tag, err := tx.Exec(ctx, `INSERT INTO model_prices
			(provider_type, model_pattern, input_per_mtok, output_per_mtok,
			 cache_write_per_mtok, cache_read_per_mtok, currency, source, builtin_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'builtin', $8)
			ON CONFLICT (provider_type, model_pattern) DO UPDATE SET
				input_per_mtok = EXCLUDED.input_per_mtok,
				output_per_mtok = EXCLUDED.output_per_mtok,
				cache_write_per_mtok = EXCLUDED.cache_write_per_mtok,
				cache_read_per_mtok = EXCLUDED.cache_read_per_mtok,
				currency = EXCLUDED.currency,
				builtin_version = EXCLUDED.builtin_version,
				updated_at = now()
			WHERE model_prices.source = 'builtin' AND model_prices.builtin_version < EXCLUDED.builtin_version`,
			m.ProviderType, m.Model, m.Input, m.Output, m.CacheWrite, m.CacheRead, f.Currency, f.Version)
		if err != nil {
			return 0, err
		}
		changed += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if changed > 0 {
		log.Info("price table seeded", "rows", changed, "version", f.Version, "models", len(f.Models))
	}
	return changed, nil
}
