// Package store owns Civic Relay's encrypted, metadata-only SQLite state.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"civic-ai-relay/internal/secret"
	_ "modernc.org/sqlite"
)

// Store is a bounded SQLite connection pool and its encryption context.
type Store struct {
	db  *sql.DB
	box *secret.Box
}

// Open creates or opens a relay database, enables safe pragmas, and applies all
// numbered schema migrations transactionally.
func Open(path string, box *secret.Box) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	if box == nil {
		return nil, errors.New("secret box is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// PathEscape keeps spaces, #, and other filename characters from being
	// interpreted as URI syntax while modernc.org/sqlite decodes the path.
	dsn := "file:" + url.PathEscape(filepath.ToSlash(abs)) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	closeOnError := func(e error) (*Store, error) {
		_ = db.Close()
		return nil, e
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return closeOnError(fmt.Errorf("set sqlite busy timeout: %w", err))
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return closeOnError(fmt.Errorf("enable sqlite foreign keys: %w", err))
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return closeOnError(fmt.Errorf("enable sqlite WAL: %w", err))
	}
	s := &Store{db: db, box: box}
	if err := s.applySchema(ctx); err != nil {
		return closeOnError(err)
	}
	return s, nil
}

// JournalMode reports SQLite's active journal mode in lower-case.
func (s *Store) JournalMode(ctx context.Context) string {
	if s == nil || s.db == nil {
		return ""
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return ""
	}
	return strings.ToLower(mode)
}

// Close releases all SQLite connections.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
