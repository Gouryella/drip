// Package store provides the SQLite-backed control plane for Drip: accounts,
// client credentials, tunnel reservations, live sessions, admin users and the
// audit log. The tunnel data plane never touches this package on the hot path;
// it is consulted at registration time only.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a uniqueness constraint would be violated.
	ErrConflict = errors.New("already exists")
)

// Store owns the database handle and exposes typed accessors.
type Store struct {
	db *sql.DB
}

// Open opens (creating it if needed) the SQLite database at path and applies
// any pending migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}

	cleanPath := filepath.Clean(path)
	if dir := filepath.Dir(cleanPath); dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create database directory: %w", err)
		}
	}

	// WAL keeps readers (admin panel) from blocking writers (registrations).
	// busy_timeout avoids spurious SQLITE_BUSY under concurrent registration.
	dsn := cleanPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// SQLite handles one writer at a time; a small pool avoids lock churn while
	// still allowing concurrent reads under WAL.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to reach database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying handle for callers that need custom queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

// migrate applies pending migrations inside a transaction each.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("failed to create migration table: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("failed to read schema version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		version := i + 1

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d failed: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to record migration %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %d: %w", version, err)
		}
	}

	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("failed to read schema version: %w", err)
	}
	return v, nil
}

// unixPtr converts a nullable unix timestamp column into a *time.Time.
func unixPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// nullString converts an optional string into a driver-friendly value.
func nullString(v *string) interface{} {
	if v == nil || *v == "" {
		return nil
	}
	return *v
}

// strPtr converts a nullable text column into a *string.
func strPtr(v sql.NullString) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	s := v.String
	return &s
}

// isUniqueViolation reports whether err is a SQLite uniqueness failure.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
