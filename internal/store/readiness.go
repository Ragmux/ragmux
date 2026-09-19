package store

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync"
)

// headMigration is the newest migration version embedded in this binary,
// read once from the same file names migrate() applies.
var headMigration = sync.OnceValue(func() int {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return 0
	}
	head := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err == nil && v > head {
			head = v
		}
	}
	return head
})

// HeadMigration reports the schema version this binary embeds. A readiness
// probe compares it against MAX(version) in schema_migrations: during a
// rolling upgrade a replica whose database has not reached its own head yet
// is not ready to serve, and neither is an old replica left behind by a
// newer one's migration.
func HeadMigration() int { return headMigration() }

// AppliedMigration reports the newest migration recorded in the database, or
// 0 when none has been applied.
func (s *Store) AppliedMigration(ctx context.Context) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, "SELECT COALESCE((SELECT MAX(version) FROM schema_migrations), 0)").Scan(&v)
	return v, err
}

// DocumentStatusCounts groups the documents table by status. It backs the
// ragmux_documents gauge, which is cached on purpose: this is a full count
// and must not run once per scrape.
func (s *Store) DocumentStatusCounts(ctx context.Context) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, "SELECT status, count(*) FROM documents GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Every status is seeded at zero so a drained queue reads as 0 rather
	// than as a series that stopped being exported, which an alert on
	// absence would read as "the exporter broke".
	out := map[string]float64{DocPending: 0, DocProcessing: 0, DocReady: 0, DocFailed: 0}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = float64(n)
	}
	return out, rows.Err()
}

// DegradedRAGStores counts the RAG stores configured for a search backend
// this server cannot provide. They keep working -- the search falls back to
// pgvector -- so this is reported as informational degradation and never as
// a failed readiness probe.
func (s *Store) DegradedRAGStores(ctx context.Context) (int, error) {
	if s.caps.PgSearch {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM rag_stores WHERE search_backend = $1", BackendPgSearch).Scan(&n)
	return n, err
}
