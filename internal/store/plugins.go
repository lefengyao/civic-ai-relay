package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Plugin is the metadata Civic keeps for an installed external WASM package.
// The manifest and permission/limit JSON are retained so an operator can audit
// the package without opening the binary again.
type Plugin struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Version          string  `json:"version"`
	Description      string  `json:"description"`
	PackagePath      string  `json:"package_path"`
	WASMHash         string  `json:"wasm_sha256"`
	ManifestJSON     string  `json:"manifest_json"`
	PermissionsJSON  string  `json:"permissions_json"`
	LimitsJSON       string  `json:"limits_json"`
	Enabled          bool    `json:"enabled"`
	Status           string  `json:"status"`
	LastError        string  `json:"last_error,omitempty"`
	InstalledAtUTC   string  `json:"installed_at_utc"`
	UpdatedAtUTC     string  `json:"updated_at_utc"`
	LastStartedAtUTC *string `json:"last_started_at_utc,omitempty"`
	LastStoppedAtUTC *string `json:"last_stopped_at_utc,omitempty"`
	StartCount       int64   `json:"start_count"`
	FailureCount     int64   `json:"failure_count"`
}

func (s *Store) UpsertPlugin(ctx context.Context, plugin Plugin) error {
	if plugin.ID == "" || plugin.PackagePath == "" || plugin.WASMHash == "" {
		return errors.New("plugin id, package path and wasm hash are required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if plugin.InstalledAtUTC == "" {
		plugin.InstalledAtUTC = now
	}
	if plugin.UpdatedAtUTC == "" {
		plugin.UpdatedAtUTC = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO civic_plugins
		(id,name,version,description,package_path,wasm_sha256,manifest_json,permissions_json,limits_json,enabled,status,last_error,installed_at_utc,updated_at_utc,start_count,failure_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, version=excluded.version,
		description=excluded.description, package_path=excluded.package_path, wasm_sha256=excluded.wasm_sha256,
		manifest_json=excluded.manifest_json, permissions_json=excluded.permissions_json, limits_json=excluded.limits_json,
		updated_at_utc=excluded.updated_at_utc, status=CASE WHEN civic_plugins.enabled=1 THEN civic_plugins.status ELSE excluded.status END,
		last_error=excluded.last_error`, plugin.ID, plugin.Name, plugin.Version, plugin.Description, plugin.PackagePath,
		plugin.WASMHash, plugin.ManifestJSON, plugin.PermissionsJSON, plugin.LimitsJSON, boolInt(plugin.Enabled),
		plugin.Status, plugin.LastError, plugin.InstalledAtUTC, plugin.UpdatedAtUTC, plugin.StartCount, plugin.FailureCount)
	if err != nil {
		return fmt.Errorf("upsert plugin: %w", err)
	}
	return nil
}

func (s *Store) ListPlugins(ctx context.Context) ([]Plugin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,version,description,package_path,wasm_sha256,manifest_json,
		permissions_json,limits_json,enabled,status,last_error,installed_at_utc,updated_at_utc,last_started_at_utc,
		last_stopped_at_utc,start_count,failure_count FROM civic_plugins ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plugins := make([]Plugin, 0)
	for rows.Next() {
		plugin, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, rows.Err()
}

func (s *Store) GetPlugin(ctx context.Context, id string) (Plugin, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,version,description,package_path,wasm_sha256,manifest_json,
		permissions_json,limits_json,enabled,status,last_error,installed_at_utc,updated_at_utc,last_started_at_utc,
		last_stopped_at_utc,start_count,failure_count FROM civic_plugins WHERE id=?`, id)
	return scanPlugin(row)
}

func (s *Store) SetPluginEnabled(ctx context.Context, id string, enabled bool, status, lastError string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE civic_plugins SET enabled=?,status=?,last_error=?,updated_at_utc=? WHERE id=?`,
		boolInt(enabled), status, lastError, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RecordPluginStart(ctx context.Context, id string, status, lastError string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE civic_plugins SET status=?,last_error=?,last_started_at_utc=?,
		updated_at_utc=?,start_count=start_count+1 WHERE id=?`, status, lastError,
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RecordPluginStop(ctx context.Context, id string, status, lastError string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE civic_plugins SET status=?,last_error=?,last_stopped_at_utc=?,
		updated_at_utc=? WHERE id=?`, status, lastError, time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RecordPluginFailure(ctx context.Context, id, status, lastError string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE civic_plugins SET status=?,last_error=?,failure_count=failure_count+1,
		updated_at_utc=? WHERE id=?`, status, lastError, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeletePlugin(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM civic_plugins WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanPlugin(row scanner) (Plugin, error) {
	var p Plugin
	var enabled int
	if err := row.Scan(&p.ID, &p.Name, &p.Version, &p.Description, &p.PackagePath, &p.WASMHash,
		&p.ManifestJSON, &p.PermissionsJSON, &p.LimitsJSON, &enabled, &p.Status, &p.LastError,
		&p.InstalledAtUTC, &p.UpdatedAtUTC, &p.LastStartedAtUTC, &p.LastStoppedAtUTC, &p.StartCount, &p.FailureCount); err != nil {
		return Plugin{}, err
	}
	p.Enabled = enabled != 0
	return p, nil
}
