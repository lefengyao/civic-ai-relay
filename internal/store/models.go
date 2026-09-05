package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Model struct {
	ID                   int64
	ProviderID           int64
	PublicName           string
	UpstreamName         string
	InputPriceMicroyuan  *int64
	OutputPriceMicroyuan *int64
	Enabled              bool
}

type NewModel struct {
	ProviderID           int64
	PublicName           string
	UpstreamName         string
	InputPriceMicroyuan  *int64
	OutputPriceMicroyuan *int64
	Enabled              bool
}

type UpdateModel struct {
	ProviderID           *int64
	PublicName           string
	UpstreamName         string
	InputPriceMicroyuan  *int64
	OutputPriceMicroyuan *int64
	Enabled              *bool
}

type ModelGroup struct {
	ID      int64
	Name    string
	Enabled bool
}

type NewModelGroup struct {
	Name    string
	Enabled bool
}

type UpdateModelGroup struct {
	Name    string
	Enabled *bool
}

func validatePrice(v *int64) error {
	if v != nil && *v < 0 {
		return errors.New("price must be non-negative")
	}
	return nil
}

func (s *Store) CreateModel(ctx context.Context, in NewModel) (Model, error) {
	if in.ProviderID <= 0 || strings.TrimSpace(in.PublicName) == "" || strings.TrimSpace(in.UpstreamName) == "" {
		return Model{}, errors.New("provider, public name, and upstream name are required")
	}
	if err := validatePrice(in.InputPriceMicroyuan); err != nil {
		return Model{}, err
	}
	if err := validatePrice(in.OutputPriceMicroyuan); err != nil {
		return Model{}, err
	}
	name, upstream := strings.TrimSpace(in.PublicName), strings.TrimSpace(in.UpstreamName)
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO models(provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?)`, in.ProviderID, name, upstream, in.InputPriceMicroyuan, in.OutputPriceMicroyuan, boolInt(in.Enabled), now, now)
	if err != nil {
		return Model{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Model{}, err
	}
	return Model{ID: id, ProviderID: in.ProviderID, PublicName: name, UpstreamName: upstream, InputPriceMicroyuan: in.InputPriceMicroyuan, OutputPriceMicroyuan: in.OutputPriceMicroyuan, Enabled: in.Enabled}, nil
}

func (s *Store) UpdateModel(ctx context.Context, id int64, in UpdateModel) (Model, error) {
	var m Model
	var enabled int
	var input, output sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled FROM models WHERE id=?`, id).Scan(&m.ProviderID, &m.PublicName, &m.UpstreamName, &input, &output, &enabled); err != nil {
		return Model{}, err
	}
	m.ID = id
	m.InputPriceMicroyuan, m.OutputPriceMicroyuan = nullableInt(input), nullableInt(output)
	m.Enabled = enabled == 1
	if in.ProviderID != nil {
		if *in.ProviderID <= 0 {
			return Model{}, errors.New("invalid provider ID")
		}
		m.ProviderID = *in.ProviderID
	}
	if strings.TrimSpace(in.PublicName) != "" {
		m.PublicName = strings.TrimSpace(in.PublicName)
	}
	if strings.TrimSpace(in.UpstreamName) != "" {
		m.UpstreamName = strings.TrimSpace(in.UpstreamName)
	}
	if in.InputPriceMicroyuan != nil {
		if err := validatePrice(in.InputPriceMicroyuan); err != nil {
			return Model{}, err
		}
		m.InputPriceMicroyuan = in.InputPriceMicroyuan
	}
	if in.OutputPriceMicroyuan != nil {
		if err := validatePrice(in.OutputPriceMicroyuan); err != nil {
			return Model{}, err
		}
		m.OutputPriceMicroyuan = in.OutputPriceMicroyuan
	}
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE models SET provider_id=?,public_name=?,upstream_name=?,input_price_microyuan=?,output_price_microyuan=?,enabled=?,updated_at_utc=? WHERE id=?`, m.ProviderID, m.PublicName, m.UpstreamName, m.InputPriceMicroyuan, m.OutputPriceMicroyuan, boolInt(m.Enabled), nowUTC(), id); err != nil {
		return Model{}, err
	}
	return m, nil
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled FROM models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetModel(ctx context.Context, id int64) (Model, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled FROM models WHERE id=?`, id)
	return scanModel(row)
}

type scanner interface{ Scan(...any) error }

func scanModel(src scanner) (Model, error) {
	var m Model
	var input, output sql.NullInt64
	var enabled int
	if err := src.Scan(&m.ID, &m.ProviderID, &m.PublicName, &m.UpstreamName, &input, &output, &enabled); err != nil {
		return Model{}, err
	}
	m.InputPriceMicroyuan, m.OutputPriceMicroyuan, m.Enabled = nullableInt(input), nullableInt(output), enabled == 1
	return m, nil
}
func nullableInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) CreateModelGroup(ctx context.Context, in NewModelGroup) (ModelGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ModelGroup{}, errors.New("group name is required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO model_groups(name,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?)`, name, 1, nowUTC(), nowUTC())
	if err != nil {
		return ModelGroup{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelGroup{}, err
	}
	return ModelGroup{ID: id, Name: name, Enabled: true}, nil
}

func (s *Store) UpdateModelGroup(ctx context.Context, id int64, in UpdateModelGroup) (ModelGroup, error) {
	var g ModelGroup
	var enabled int
	if err := s.db.QueryRowContext(ctx, `SELECT name,enabled FROM model_groups WHERE id=?`, id).Scan(&g.Name, &enabled); err != nil {
		return ModelGroup{}, err
	}
	g.ID, g.Enabled = id, enabled == 1
	if strings.TrimSpace(in.Name) != "" {
		g.Name = strings.TrimSpace(in.Name)
	}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_groups SET name=?,enabled=?,updated_at_utc=? WHERE id=?`, g.Name, boolInt(g.Enabled), nowUTC(), id); err != nil {
		return ModelGroup{}, err
	}
	return g, nil
}

func (s *Store) ListModelGroups(ctx context.Context) ([]ModelGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,enabled FROM model_groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelGroup
	for rows.Next() {
		var g ModelGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &enabled); err != nil {
			return nil, err
		}
		g.Enabled = enabled == 1
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceGroupModels(ctx context.Context, groupID int64, modelIDs []int64) error {
	if groupID <= 0 {
		return errors.New("group ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var groupEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM model_groups WHERE id=?`, groupID).Scan(&groupEnabled); err != nil {
		return err
	}
	if groupEnabled != 1 {
		return errors.New("model group is disabled")
	}
	seen := make(map[int64]struct{}, len(modelIDs))
	for _, id := range modelIDs {
		if id <= 0 {
			return errors.New("invalid model ID")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate model ID")
		}
		seen[id] = struct{}{}
		var enabled int
		var input, output sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT enabled,input_price_microyuan,output_price_microyuan FROM models WHERE id=?`, id).Scan(&enabled, &input, &output); err != nil {
			return err
		}
		if enabled != 1 || !input.Valid || !output.Valid {
			return fmt.Errorf("model %d is disabled or unpriced", id)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_models WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for _, id := range modelIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_models(group_id,model_id) VALUES (?,?)`, groupID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
