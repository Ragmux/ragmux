// Package store is the embedded persistence layer: SQLite (via a cgo-free wasm
// build) with the sqlite-vec extension for vector search. Everything lives
// under a single data directory so a volume mount captures all state.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Registers the sqlite-vec enabled wasm build of SQLite.
	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

func init() {
	// The sqlite-vec wasm build uses atomic instructions; enable the threads
	// feature so wazero can validate and run it.
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
}

// Store wraps the database handle plus filesystem locations.
type Store struct {
	db      *sql.DB
	DataDir string
	// UploadsDir holds raw uploaded documents.
	UploadsDir string
	// VecAvailable reports whether the sqlite-vec extension loaded. When it
	// did not, similarity search falls back to a brute-force scan in Go.
	VecAvailable bool
	cipher       *cipher
	log          *slog.Logger
}

// Open initialises the data directory, opens the database, runs migrations
// and loads (or creates) the secret key used to encrypt provider credentials.
func Open(ctx context.Context, dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	uploads := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploads, 0o750); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}

	dsn := "file:" + filepath.Join(dataDir, "ragmux.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer; keep the pool small to avoid lock churn.
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db, DataDir: dataDir, UploadsDir: uploads, log: log}

	var vecVersion string
	if err := db.QueryRowContext(ctx, "SELECT vec_version()").Scan(&vecVersion); err == nil {
		s.VecAvailable = true
		log.Info("sqlite-vec loaded", "version", vecVersion)
	} else {
		log.Warn("sqlite-vec unavailable; using brute-force vector search", "err", err)
	}

	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}

	c, err := loadOrCreateCipher(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		db.Close()
		return nil, err
	}
	s.cipher = c
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for callers that need transactions.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	applied := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_version")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("bad migration name %q", name)
		}
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version(version, applied_at) VALUES (?, ?)",
			version, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.log.Info("applied migration", "name", name)
	}
	return nil
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func scanErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
