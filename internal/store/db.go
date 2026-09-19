// Package store is the persistence layer: PostgreSQL with the pgvector
// extension for vector search. Provider credentials are encrypted with a key
// supplied via SECRET_KEY (or a secret.key file as a development fallback).
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// Advisory lock keys. Application-wide DDL is serialised on these so several
// replicas can start (or create a new embedding table) at the same time.
const (
	lockMigrations int64 = 0x7261676d75780001 // "ragmux" + 1
	// LockJanitor is the leader lock the hourly retention pass takes so only
	// one replica does the work. Exported: internal/maintenance takes it.
	LockJanitor   int64 = 0x7261676d75780002 // "ragmux" + 2
	lockVecTables int32 = 0x72616701
	lockBM25Index int32 = 0x72616702
)

// OpenConfig carries everything Open needs.
type OpenConfig struct {
	// DatabaseURL is a PostgreSQL connection string.
	DatabaseURL string
	// MaxConns caps the pool size (default 10).
	MaxConns int
	// SecretKeyHex is the 32-byte credential key as hex. When empty the key
	// is read from (or created at) DataDir/secret.key.
	SecretKeyHex string
	// DataDir is only used for the secret.key fallback.
	DataDir string
}

// Store wraps the connection pool and the credential cipher.
type Store struct {
	pool *pgxpool.Pool
	// ServerVersion is the PostgreSQL server_version reported at Open.
	ServerVersion string
	// SecretKeySource is "env" or "file", depending on where the key came from.
	SecretKeySource string
	cipher          *cipher
	log             *slog.Logger
	// vecTables remembers which chunk_embeddings_<dims> tables exist.
	vecTables sync.Map
	// caps are the optional server features detected once at Open.
	caps Capabilities
	// bm25Ready remembers that the ParadeDB index has been created.
	bm25Ready atomic.Bool
}

// Open connects to PostgreSQL, ensures the vector extension exists, runs
// migrations and loads the secret key used to encrypt provider credentials.
func Open(ctx context.Context, cfg OpenConfig, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("database URL is empty")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 10
	}

	c, source, err := loadCipher(cfg.SecretKeyHex, cfg.DataDir, log)
	if err != nil {
		return nil, err
	}

	// Schema setup runs on a plain connection before the pool exists: the
	// pool's AfterConnect registers the vector type, which only resolves once
	// the extension has been created.
	conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	var version string
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&version); err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("query server version: %w", err)
	}
	if err := migrate(ctx, conn, log); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	caps := detectCapabilities(ctx, conn, log)
	if err := conn.Close(ctx); err != nil {
		return nil, err
	}

	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxConns < 1 || cfg.MaxConns > math.MaxInt32 {
		return nil, fmt.Errorf("max connections %d out of range", cfg.MaxConns)
	}
	pc.MaxConns = int32(cfg.MaxConns)
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool, ServerVersion: version, SecretKeySource: source, cipher: c, log: log, caps: caps}, nil
}

// Close releases the connection pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// DB exposes the pool for callers that need raw access (health checks).
func (s *Store) DB() *pgxpool.Pool { return s.pool }

// DatabaseInfo describes the connected database for the system endpoint.
type DatabaseInfo struct {
	PostgresVersion   string `json:"postgres_version"`
	PgvectorVersion   string `json:"pgvector_version"`
	PgSearchVersion   string `json:"pg_search_version"`
	MigrationsVersion int    `json:"migrations_version"`
	SizeBytes         int64  `json:"size_bytes"`
}

// BackupInfo sizes the parts of the state an operator backs up: the
// per-dimension vector tables that pg_dump picks up dynamically, the raw
// document bytes kept in the database, and when the schema last changed.
type BackupInfo struct {
	Tables          int        `json:"tables"`
	DocumentsBytes  int64      `json:"documents_bytes"`
	LastMigrationAt *time.Time `json:"last_migration_at"`
}

// DatabaseInfo reports server, extension and migration versions plus size.
func (s *Store) DatabaseInfo(ctx context.Context) (*DatabaseInfo, error) {
	info := &DatabaseInfo{PostgresVersion: s.ServerVersion, PgSearchVersion: s.caps.PgSearchVersion}
	err := s.pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT extversion FROM pg_extension WHERE extname = 'vector'), ''),
		COALESCE((SELECT MAX(version) FROM schema_migrations), 0),
		pg_database_size(current_database())`).
		Scan(&info.PgvectorVersion, &info.MigrationsVersion, &info.SizeBytes)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// BackupInfo counts the chunk_embeddings_<dims> tables in the current
// schema, sums the stored document contents and returns the timestamp of
// the newest applied migration (nil before the first one).
func (s *Store) BackupInfo(ctx context.Context) (*BackupInfo, error) {
	info := &BackupInfo{}
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_tables
		   WHERE schemaname = current_schema() AND tablename ~ '^chunk_embeddings_[0-9]+$'),
		COALESCE((SELECT SUM(octet_length(content)) FROM documents), 0),
		(SELECT MAX(applied_at) FROM schema_migrations)`).
		Scan(&info.Tables, &info.DocumentsBytes, &info.LastMigrationAt)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// migrate applies embedded migrations that have not been recorded yet. Every
// step takes a transaction-scoped advisory lock so concurrent replicas
// serialise instead of racing on DDL.
func migrate(ctx context.Context, conn *pgx.Conn, log *slog.Logger) error {
	err := withLockedTx(ctx, conn, func(tx pgx.Tx) error {
		if err := ensureVectorExtension(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
		if err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

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
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		applied := false
		err = withLockedTx(ctx, conn, func(tx pgx.Tx) error {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
				return err
			}
			applied = true
			return nil
		})
		if err != nil {
			return err
		}
		if applied {
			log.Info("applied migration", "name", name)
		}
	}
	return nil
}

// ensureVectorExtension makes sure pgvector is installed. The check comes
// first because CREATE EXTENSION IF NOT EXISTS is not a no-op for a
// non-superuser role: on some servers it fails with a permission error even
// when the extension already exists. Only a missing extension is created,
// and a permission failure at that point is reported as an operator action.
func ensureVectorExtension(ctx context.Context, tx pgx.Tx) error {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')").Scan(&exists); err != nil {
		return fmt.Errorf("check vector extension: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public"); err != nil {
		if pgCode(err) == "42501" { // insufficient_privilege
			return errors.New(`the vector extension is missing and the database role may not create it; run "CREATE EXTENSION vector" as a superuser`)
		}
		return fmt.Errorf("create vector extension: %w", err)
	}
	return nil
}

func withLockedTx(ctx context.Context, conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockMigrations); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ts renders a database timestamp the way the JSON API exposes it.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func scanErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// IsUniqueViolation reports whether err is a unique constraint failure.
func IsUniqueViolation(err error) bool { return pgCode(err) == "23505" }

// IsForeignKeyViolation reports whether err is a foreign key failure.
func IsForeignKeyViolation(err error) bool { return pgCode(err) == "23503" }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// SetupInfo is the cheap, unauthenticated slice of DatabaseInfo shown on
// the first-run page: the newest applied migration and the database role.
type SetupInfo struct {
	MigrationsVersion int
	DatabaseRole      string
}

// SetupInfo reads the migration version and the connected role in one
// round trip.
func (s *Store) SetupInfo(ctx context.Context) (*SetupInfo, error) {
	info := &SetupInfo{}
	err := s.pool.QueryRow(ctx, "SELECT COALESCE((SELECT MAX(version) FROM schema_migrations), 0), current_user").
		Scan(&info.MigrationsVersion, &info.DatabaseRole)
	if err != nil {
		return nil, err
	}
	return info, nil
}
