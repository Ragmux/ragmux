package pricing

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed writes the built-in price table into model_prices. It is safe to run
// on every start and reports how many rows it inserted or refreshed.
//
// The upsert never touches a row an operator edited: the dashboard flips
// such a row's source to "user", and the WHERE clause only refreshes rows
// that are still "builtin" and older than the shipped version. An operator
// cannot delete a builtin row through the API either (only edit or reset
// it), which is what keeps a deleted row from resurrecting on the next
// upgrade without a tombstone column to remember it by.
//
// Dropping an entry from prices.json does retire it, though: retireRows
// removes the rows the shipped table no longer lists, so a price that
// turned out to be wrong for a whole provider does not live on in every
// existing install with no way to get rid of it.
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
	retired, err := retireRows(ctx, tx, f)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if changed > 0 {
		log.Info("price table seeded", "rows", changed, "version", f.Version, "models", len(f.Models))
	}
	if retired > 0 {
		log.Info("price rows retired", "rows", retired, "version", f.Version)
	}
	return changed, nil
}

// retireRows deletes the builtin rows the shipped table no longer lists.
//
// Two guards keep it narrow. Only rows still marked "builtin" go: an
// operator who edited one owns it now, and their number survives as a
// "user" row. And only rows at or below the shipped version go, so starting
// an older binary against a database seeded by a newer one does not delete
// the models the newer table added.
func retireRows(ctx context.Context, tx pgx.Tx, f *builtinFile) (int64, error) {
	types := make([]string, len(f.Models))
	patterns := make([]string, len(f.Models))
	for i, m := range f.Models {
		types[i], patterns[i] = m.ProviderType, m.Model
	}
	tag, err := tx.Exec(ctx, `DELETE FROM model_prices m
		WHERE m.source = 'builtin' AND m.builtin_version <= $1
		  AND NOT EXISTS (SELECT 1 FROM unnest($2::text[], $3::text[]) AS s(provider_type, model_pattern)
		                  WHERE s.provider_type = m.provider_type AND s.model_pattern = m.model_pattern)`,
		f.Version, types, patterns)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
