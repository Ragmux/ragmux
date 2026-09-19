package pricing

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CacheTTL is how long a loaded price table is served before it is read
// again. Prices change by hand, so a minute of staleness after an edit is
// cheap; a dashboard edit calls Invalidate and does not wait for it.
const CacheTTL = 60 * time.Second

// Cache serves the price table from memory. Every completed request looks a
// price up, so the table is read once a minute rather than per request.
type Cache struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	log  *slog.Logger

	mu       sync.RWMutex
	table    *Table
	loadedAt time.Time
}

// NewCache builds a cache over the given pool.
func NewCache(pool *pgxpool.Pool, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}
	return &Cache{pool: pool, ttl: CacheTTL, log: log}
}

// Lookup resolves a provider type and model name to a price. It reports
// false when no row matches, when the table cannot be read at all, or when
// the cache has no pool; the caller then records no cost.
func (c *Cache) Lookup(ctx context.Context, providerType, model string) (Price, bool) {
	if c == nil {
		return Price{}, false
	}
	t := c.current()
	if t == nil {
		return Price{}, false
	}
	return t.Lookup(providerType, model)
}

// Invalidate drops the cached table so the next lookup reloads it. The
// price handlers call it after every mutation.
func (c *Cache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.table, c.loadedAt = nil, time.Time{}
	c.mu.Unlock()
}

// current returns a fresh table, reloading it when the TTL has passed.
func (c *Cache) current() *Table {
	c.mu.RLock()
	t, at := c.table, c.loadedAt
	c.mu.RUnlock()
	if t != nil && time.Since(at) < c.ttl {
		return t
	}
	if c.pool == nil {
		return nil
	}
	// The lookup runs on the request's own deadline budget; a slow database
	// must not hold a completion open.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := loadRows(ctx, c.pool)
	if err != nil {
		// Keep serving the previous table rather than silently pricing
		// everything at zero, but back off so a broken database is not
		// queried once per request.
		c.log.Warn("load price table", "err", err)
		c.mu.Lock()
		c.loadedAt = time.Now()
		t = c.table
		c.mu.Unlock()
		return t
	}
	fresh := NewTable(rows)
	c.mu.Lock()
	c.table, c.loadedAt = fresh, time.Now()
	c.mu.Unlock()
	return fresh
}

// loadRows reads model_prices. The lookup path lives here rather than in
// the store package so the cache owns the one query it repeats; the admin
// CRUD over the same table is in internal/store/prices.go.
func loadRows(ctx context.Context, pool *pgxpool.Pool) ([]Row, error) {
	rows, err := pool.Query(ctx, `SELECT id, provider_type, model_pattern,
		input_per_mtok::float8, output_per_mtok::float8,
		COALESCE(cache_write_per_mtok, input_per_mtok)::float8,
		COALESCE(cache_read_per_mtok, input_per_mtok)::float8,
		currency, source FROM model_prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.ProviderType, &r.Pattern, &r.Price.Input, &r.Price.Output,
			&r.Price.CacheWrite, &r.Price.CacheRead, &r.Price.Currency, &r.Price.Source); err != nil {
			return nil, err
		}
		r.Price.Pattern = r.Pattern
		out = append(out, r)
	}
	return out, rows.Err()
}
