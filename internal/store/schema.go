package store

import (
	"context"
	"fmt"
)

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{version: 1, statements: []string{
		`CREATE TABLE providers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			base_url TEXT NOT NULL,
			api_key_ciphertext BLOB NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL
		)`,
		`CREATE TABLE models (
			id INTEGER PRIMARY KEY,
			provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			public_name TEXT NOT NULL UNIQUE,
			upstream_name TEXT NOT NULL,
			input_price_microyuan INTEGER,
			output_price_microyuan INTEGER,
			enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL,
			CHECK (input_price_microyuan IS NULL OR input_price_microyuan >= 0),
			CHECK (output_price_microyuan IS NULL OR output_price_microyuan >= 0)
		)`,
		`CREATE TABLE model_groups (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL
		)`,
		`CREATE TABLE group_models (
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
			PRIMARY KEY (group_id, model_id)
		)`,
		`CREATE TABLE client_keys (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			token_digest BLOB NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			concurrency_limit INTEGER NOT NULL CHECK (concurrency_limit > 0),
			token_limit INTEGER,
			amount_limit_microyuan INTEGER,
			disabled_reason TEXT,
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL,
			CHECK (token_limit IS NULL OR token_limit >= 0),
			CHECK (amount_limit_microyuan IS NULL OR amount_limit_microyuan >= 0)
		)`,
		`CREATE TABLE key_groups (
			key_id INTEGER NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			PRIMARY KEY (key_id, group_id)
		)`,
		`CREATE TABLE key_reservations (
			id INTEGER PRIMARY KEY,
			key_id INTEGER NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
			model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
			reserved_tokens INTEGER NOT NULL CHECK (reserved_tokens >= 0),
			reserved_amount_microyuan INTEGER NOT NULL CHECK (reserved_amount_microyuan >= 0),
			charged_tokens INTEGER NOT NULL DEFAULT 0 CHECK (charged_tokens >= 0),
			charged_amount_microyuan INTEGER NOT NULL DEFAULT 0 CHECK (charged_amount_microyuan >= 0),
			status TEXT NOT NULL CHECK (status IN ('reserved', 'completed', 'failed', 'aborted', 'rejected')),
			created_at_utc TEXT NOT NULL,
			finished_at_utc TEXT
		)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY,
			request_id TEXT NOT NULL UNIQUE,
			key_id INTEGER REFERENCES client_keys(id) ON DELETE SET NULL,
			model_id INTEGER REFERENCES models(id) ON DELETE SET NULL,
			provider_id INTEGER REFERENCES providers(id) ON DELETE SET NULL,
			status TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
			output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
			amount_microyuan INTEGER NOT NULL DEFAULT 0 CHECK (amount_microyuan >= 0),
			upstream_status INTEGER,
			streamed INTEGER NOT NULL DEFAULT 0 CHECK (streamed IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			finished_at_utc TEXT
		)`,
		`CREATE INDEX idx_client_keys_enabled ON client_keys(enabled, id)`,
		`CREATE INDEX idx_models_provider_enabled ON models(provider_id, enabled, id)`,
		`CREATE INDEX idx_group_models_model ON group_models(model_id, group_id)`,
		`CREATE INDEX idx_key_groups_group ON key_groups(group_id, key_id)`,
		`CREATE INDEX idx_key_reservations_key_status_time ON key_reservations(key_id, status, created_at_utc)`,
		`CREATE INDEX idx_key_reservations_time ON key_reservations(created_at_utc, status)`,
		`CREATE INDEX idx_requests_time ON requests(created_at_utc)`,
		`CREATE INDEX idx_requests_key_time ON requests(key_id, created_at_utc)`,
	}},
}

func (s *Store) applySchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at_utc TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	for _, migration := range migrations {
		var applied int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("check schema migration %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_utc) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`, migration.version); err != nil {
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
