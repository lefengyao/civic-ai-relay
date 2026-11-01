package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Model struct {
	ID                        int64
	ProviderID                int64
	PublicName                string
	UpstreamName              string
	InputPriceMicroyuan       *int64
	OutputPriceMicroyuan      *int64
	CachedInputPriceMicroyuan *int64 // 缓存命中输入单价；nil 表示按普通输入价计费
	Enabled                   bool
}

type NewModel struct {
	ProviderID                int64
	PublicName                string
	UpstreamName              string
	InputPriceMicroyuan       *int64
	OutputPriceMicroyuan      *int64
	CachedInputPriceMicroyuan *int64
	Enabled                   bool
}

type UpdateModel struct {
	ProviderID                *int64
	PublicName                string
	UpstreamName              string
	InputPriceMicroyuan       *int64
	OutputPriceMicroyuan      *int64
	CachedInputPriceMicroyuan *int64
	Enabled                   *bool
}

type ModelGroup struct {
	ID        int64
	Name      string
	Enabled   bool
	RateMilli int64 // 计费倍率，千分比表示：1000 = 1.0x
	// 分组额度（取代旧全局额度，v18）：nil = 未设 = 不限。判定在预留事务内
	// 从本行读取（ledger.go ReserveRequest），统计与扣减同口径。
	TokenLimit5H      *int64
	AmountLimit5H     *int64 // 微元
	TokenLimitDaily   *int64
	AmountLimitDaily  *int64 // 微元
	TokenLimitWeekly  *int64
	AmountLimitWeekly *int64 // 微元
	PeriodLimitDays   *int
	TokenLimitPeriod  *int64
	AmountLimitPeriod *int64 // 微元
	// PeriodLimitStart 为 RFC3339 文本；PeriodLimitDays>0 且为空时由
	// UpdateModelGroup 自动填 now（沿用 internal/config/settings.go 的既有行为）。
	PeriodLimitStart *string
	// BoundKeys 绑定到本组的客户端密钥数。删组前要让运维看到影响面。
	BoundKeys int64
}

type NewModelGroup struct {
	Name      string
	Enabled   bool
	RateMilli int64 // 0 视为默认 1000
}

type UpdateModelGroup struct {
	Name      string
	Enabled   *bool
	RateMilli *int64
	// 分组额度：nil = 保持原值；非 nil = 设为该值（0 = 不限），负数拒绝。
	// 前端编辑弹窗总是显式传全量，所以「改名」不会误清额度、
	// 「清空输入框」就是显式设为不限。
	TokenLimit5H      *int64
	AmountLimit5H     *int64
	TokenLimitDaily   *int64
	AmountLimitDaily  *int64
	TokenLimitWeekly  *int64
	AmountLimitWeekly *int64
	PeriodLimitDays   *int
	TokenLimitPeriod  *int64
	AmountLimitPeriod *int64
	PeriodStart       *string // RFC3339；"" = 清除起点
}

const DefaultGroupRateMilli int64 = 1000

func normalizeRateMilli(v int64) (int64, error) {
	if v == 0 {
		return DefaultGroupRateMilli, nil
	}
	if v < 0 {
		return 0, errors.New("group rate multiplier must be positive")
	}
	return v, nil
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
	if err := validatePrice(in.CachedInputPriceMicroyuan); err != nil {
		return Model{}, err
	}
	name, upstream := strings.TrimSpace(in.PublicName), strings.TrimSpace(in.UpstreamName)
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO models(provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,cached_input_price_microyuan,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?,?)`, in.ProviderID, name, upstream, in.InputPriceMicroyuan, in.OutputPriceMicroyuan, in.CachedInputPriceMicroyuan, boolInt(in.Enabled), now, now)
	if err != nil {
		return Model{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Model{}, err
	}
	return Model{ID: id, ProviderID: in.ProviderID, PublicName: name, UpstreamName: upstream, InputPriceMicroyuan: in.InputPriceMicroyuan, OutputPriceMicroyuan: in.OutputPriceMicroyuan, CachedInputPriceMicroyuan: in.CachedInputPriceMicroyuan, Enabled: in.Enabled}, nil
}

func (s *Store) UpdateModel(ctx context.Context, id int64, in UpdateModel) (Model, error) {
	var m Model
	var enabled int
	var input, output, cached sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,cached_input_price_microyuan,enabled FROM models WHERE id=?`, id).Scan(&m.ProviderID, &m.PublicName, &m.UpstreamName, &input, &output, &cached, &enabled); err != nil {
		return Model{}, err
	}
	m.ID = id
	m.InputPriceMicroyuan, m.OutputPriceMicroyuan, m.CachedInputPriceMicroyuan = nullableInt(input), nullableInt(output), nullableInt(cached)
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
	if in.CachedInputPriceMicroyuan != nil {
		if err := validatePrice(in.CachedInputPriceMicroyuan); err != nil {
			return Model{}, err
		}
		m.CachedInputPriceMicroyuan = in.CachedInputPriceMicroyuan
	}
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE models SET provider_id=?,public_name=?,upstream_name=?,input_price_microyuan=?,output_price_microyuan=?,cached_input_price_microyuan=?,enabled=?,updated_at_utc=? WHERE id=?`, m.ProviderID, m.PublicName, m.UpstreamName, m.InputPriceMicroyuan, m.OutputPriceMicroyuan, m.CachedInputPriceMicroyuan, boolInt(m.Enabled), nowUTC(), id); err != nil {
		return Model{}, err
	}
	return m, nil
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,cached_input_price_microyuan,enabled FROM models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]Model, 0)
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
	row := s.db.QueryRowContext(ctx, `SELECT id,provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,cached_input_price_microyuan,enabled FROM models WHERE id=?`, id)
	return scanModel(row)
}

func (s *Store) GetModelByPublicName(ctx context.Context, name string) (Model, error) {
	return scanModel(s.db.QueryRowContext(ctx, "SELECT id,provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,cached_input_price_microyuan,enabled FROM models WHERE public_name=?", name))
}

// DeleteModel removes a model. Group memberships and reservations cascade;
// historical requests keep their billing amount but lose the model reference.
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("model ID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteModelGroup removes a group. Its provider memberships cascade; bound
// client keys survive but become unbound (group_id → NULL via ON DELETE SET
// NULL), so they can no longer authorize any model until rebound.
// models themselves are untouched.
func (s *Store) DeleteModelGroup(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("group ID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateImportedModel(ctx context.Context, in NewModel) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO models(provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?)", in.ProviderID, in.PublicName, in.UpstreamName, nil, nil, 0, nowUTC(), nowUTC())
	return err
}

type scanner interface{ Scan(...any) error }

func scanModel(src scanner) (Model, error) {
	var m Model
	var input, output, cached sql.NullInt64
	var enabled int
	if err := src.Scan(&m.ID, &m.ProviderID, &m.PublicName, &m.UpstreamName, &input, &output, &cached, &enabled); err != nil {
		return Model{}, err
	}
	m.InputPriceMicroyuan, m.OutputPriceMicroyuan, m.CachedInputPriceMicroyuan, m.Enabled = nullableInt(input), nullableInt(output), nullableInt(cached), enabled == 1
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
	rate, err := normalizeRateMilli(in.RateMilli)
	if err != nil {
		return ModelGroup{}, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO model_groups(name,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?)`, name, 1, rate, nowUTC(), nowUTC())
	if err != nil {
		return ModelGroup{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelGroup{}, err
	}
	return ModelGroup{ID: id, Name: name, Enabled: true, RateMilli: rate}, nil
}

func (s *Store) UpdateModelGroup(ctx context.Context, id int64, in UpdateModelGroup) (ModelGroup, error) {
	var g ModelGroup
	var enabled int
	var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
	var ps sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT name,enabled,rate_milli,token_limit_5h,amount_limit_5h,token_limit_daily,amount_limit_daily,token_limit_weekly,amount_limit_weekly,period_limit_days,token_limit_period,amount_limit_period,period_limit_start FROM model_groups WHERE id=?`, id).Scan(&g.Name, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps); err != nil {
		return ModelGroup{}, err
	}
	g.ID, g.Enabled = id, enabled == 1
	g.TokenLimit5H, g.AmountLimit5H = nullableInt(t5h), nullableInt(a5h)
	g.TokenLimitDaily, g.AmountLimitDaily = nullableInt(td), nullableInt(ad)
	g.TokenLimitWeekly, g.AmountLimitWeekly = nullableInt(tw), nullableInt(aw)
	g.TokenLimitPeriod, g.AmountLimitPeriod = nullableInt(tp), nullableInt(ap)
	if pd.Valid {
		days := int(pd.Int64)
		g.PeriodLimitDays = &days
	}
	if ps.Valid && ps.String != "" {
		start := ps.String
		g.PeriodLimitStart = &start
	}
	if strings.TrimSpace(in.Name) != "" {
		g.Name = strings.TrimSpace(in.Name)
	}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if in.RateMilli != nil {
		rate, err := normalizeRateMilli(*in.RateMilli)
		if err != nil {
			return ModelGroup{}, err
		}
		g.RateMilli = rate
	}
	for _, pair := range []struct {
		dst **int64
		src *int64
	}{
		{&g.TokenLimit5H, in.TokenLimit5H}, {&g.AmountLimit5H, in.AmountLimit5H},
		{&g.TokenLimitDaily, in.TokenLimitDaily}, {&g.AmountLimitDaily, in.AmountLimitDaily},
		{&g.TokenLimitWeekly, in.TokenLimitWeekly}, {&g.AmountLimitWeekly, in.AmountLimitWeekly},
		{&g.TokenLimitPeriod, in.TokenLimitPeriod}, {&g.AmountLimitPeriod, in.AmountLimitPeriod},
	} {
		if pair.src != nil {
			if err := validateLimit(pair.src, "group quota limit"); err != nil {
				return ModelGroup{}, err
			}
			value := *pair.src
			*pair.dst = &value
		}
	}
	if in.PeriodLimitDays != nil {
		if *in.PeriodLimitDays < 0 {
			return ModelGroup{}, errors.New("group quota limit must be non-negative")
		}
		days := *in.PeriodLimitDays
		g.PeriodLimitDays = &days
	}
	// 周期起点：显式传入就照设（"" = 清除）；没传且当前为空、而天数 > 0 时
	// 自动填 now——否则「只填了天数」的周期会因起点为零而静默不生效
	//（periodQuotaTransition 对 PeriodStart.IsZero() 直接返回零值）。
	if in.PeriodStart != nil {
		if *in.PeriodStart == "" {
			g.PeriodLimitStart = nil
		} else {
			start := *in.PeriodStart
			g.PeriodLimitStart = &start
		}
	}
	if g.PeriodLimitDays != nil && *g.PeriodLimitDays > 0 && g.PeriodLimitStart == nil && in.PeriodStart == nil {
		started := time.Now().UTC().Format(time.RFC3339)
		g.PeriodLimitStart = &started
	}
	// 周期不再生效时清掉残留起点，避免「天数归零、起点还在」的历史脏值
	// 在下次设回天数时被当成旧周期的起点。
	if g.PeriodLimitDays == nil || *g.PeriodLimitDays == 0 {
		g.PeriodLimitStart = nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_groups SET name=?,enabled=?,rate_milli=?,token_limit_5h=?,amount_limit_5h=?,token_limit_daily=?,amount_limit_daily=?,token_limit_weekly=?,amount_limit_weekly=?,period_limit_days=?,token_limit_period=?,amount_limit_period=?,period_limit_start=?,updated_at_utc=? WHERE id=?`, g.Name, boolInt(g.Enabled), g.RateMilli, g.TokenLimit5H, g.AmountLimit5H, g.TokenLimitDaily, g.AmountLimitDaily, g.TokenLimitWeekly, g.AmountLimitWeekly, g.PeriodLimitDays, g.TokenLimitPeriod, g.AmountLimitPeriod, g.PeriodLimitStart, nowUTC(), id); err != nil {
		return ModelGroup{}, err
	}
	return g, nil
}

func (s *Store) ListModelGroups(ctx context.Context) ([]ModelGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.enabled,g.rate_milli,g.token_limit_5h,g.amount_limit_5h,g.token_limit_daily,g.amount_limit_daily,g.token_limit_weekly,g.amount_limit_weekly,g.period_limit_days,g.token_limit_period,g.amount_limit_period,g.period_limit_start,(SELECT count(*) FROM client_keys k WHERE k.group_id=g.id) FROM model_groups g ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]ModelGroup, 0)
	for rows.Next() {
		var g ModelGroup
		var enabled int
		var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
		var ps sql.NullString
		var bound int64
		if err := rows.Scan(&g.ID, &g.Name, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps, &bound); err != nil {
			return nil, err
		}
		g.Enabled = enabled == 1
		g.TokenLimit5H, g.AmountLimit5H = nullableInt(t5h), nullableInt(a5h)
		g.TokenLimitDaily, g.AmountLimitDaily = nullableInt(td), nullableInt(ad)
		g.TokenLimitWeekly, g.AmountLimitWeekly = nullableInt(tw), nullableInt(aw)
		g.TokenLimitPeriod, g.AmountLimitPeriod = nullableInt(tp), nullableInt(ap)
		if pd.Valid {
			days := int(pd.Int64)
			g.PeriodLimitDays = &days
		}
		if ps.Valid && ps.String != "" {
			start := ps.String
			g.PeriodLimitStart = &start
		}
		g.BoundKeys = bound
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountKeysInGroup 数出绑定到某分组的密钥数，供删除确认框告知影响面：
// 一对一之后删组会把该组所有密钥变成未绑、立即无法调用（spec 5.1）。
func (s *Store) CountKeysInGroup(ctx context.Context, groupID int64) (int64, error) {
	if groupID <= 0 {
		return 0, errors.New("group ID is required")
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE group_id=?`, groupID).Scan(&n)
	return n, err
}

// GroupProviderIDs returns the provider (channel) IDs currently assigned to a
// group, ordered by provider ID. Used by the administration console to prefill
// group editing. A group manages providers; authorized models are derived from
// the providers it contains.
func (s *Store) GroupProviderIDs(ctx context.Context, groupID int64) ([]int64, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT provider_id FROM group_providers WHERE group_id=? ORDER BY provider_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceGroupProviders(ctx context.Context, groupID int64, providerIDs []int64) error {
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
	seen := make(map[int64]struct{}, len(providerIDs))
	for _, id := range providerIDs {
		if id <= 0 {
			return errors.New("invalid provider ID")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate provider ID")
		}
		seen[id] = struct{}{}
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM providers WHERE id=?`, id).Scan(&enabled); err != nil {
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("provider %d is disabled", id)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_providers WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for _, id := range providerIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_providers(group_id,provider_id) VALUES (?,?)`, groupID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
