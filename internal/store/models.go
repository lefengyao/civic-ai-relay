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
	// PoolID 只在「故障转移供应商」的虚拟条目上非零（见 pools.go）。真实模型
	// 一律为 0；虚拟条目的 ID 为负数，不会参与预留。
	PoolID int64
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

// 分组类型（v22 引入，v23 收紧语义）。
//
//   - normal（成员组）：只有「渠道 + 独立额度」，**不对外**——不能被客户端 Key
//     绑定，也不参与任何请求的授权。它的全部意义是作为成员被聚合组包含。
//   - aggregate（聚合组）：唯一可对外的一类组。除自有渠道外还能包含若干成员组，
//     合并它们的渠道与模型；Key 只能绑定聚合组。只允许一层（聚合组不能包含聚合
//     组），避免环与额度归属的递归歧义。
//
// 这样拆开是为了消除「两个组都能被 Key 绑定，到底扣谁的额度」的混淆：对外永远
// 只有聚合组，成员组只是「有自己额度的供应商」。
const (
	GroupKindNormal    = "normal"
	GroupKindAggregate = "aggregate"
)

// NormalizeGroupKind 把外部输入收敛成合法值。
//
// 空串按 aggregate 处理：**可对外是默认，成员组必须显式声明**。理由是误建一个
// 成员组的代价更大——它建出来什么都能配（渠道、额度、倍率），却永远接不到请求，
// 排查时非常难看出「只是组类型选错了」。反过来，误建一个空聚合组在列表里一眼
// 就能看到「未包含任何组、0 个渠道」。
func NormalizeGroupKind(kind string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "", GroupKindAggregate:
		return GroupKindAggregate, nil
	case GroupKindNormal:
		return GroupKindNormal, nil
	default:
		return "", fmt.Errorf("未知的分组类型 %q（只支持 normal / aggregate）", kind)
	}
}

// ErrProviderGrouped 表示「这条渠道已经属于另一个组」（v24）。
//
// 一个渠道只能属于一个组。渠道是「上游地址 + 凭据」这个实体的唯一标识，同时挂进
// 两个组会让同一个上游账号受两份额度约束，而一笔请求的账目只能记到一个组上——
// 用量、额度、概览三者必然对不上（其中最隐蔽的一种：某个组显示「从未使用」，实际
// 每个请求都在跑它挂的渠道）。要接同一家供应商的第二个账号，应该新建一条渠道。
var ErrProviderGrouped = errors.New("一个渠道只能属于一个组")

// ProviderGroupConflict 说明冲突具体落在谁身上：哪条渠道、已经归哪个组。
// 文案直接给运维看（管理端放进 message），所以带上名字而不只是 ID。
type ProviderGroupConflict struct {
	ProviderID   int64
	ProviderName string
	GroupID      int64
	GroupName    string
}

func (e *ProviderGroupConflict) Error() string {
	name := e.ProviderName
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("#%d", e.ProviderID)
	}
	group := e.GroupName
	if strings.TrimSpace(group) == "" {
		group = fmt.Sprintf("#%d", e.GroupID)
	}
	return fmt.Sprintf("渠道「%s」已属于分组「%s」，一个渠道只能属于一个组；请先从原分组移除，或为同一上游新建一条渠道", name, group)
}

func (e *ProviderGroupConflict) Unwrap() error { return ErrProviderGrouped }

type ModelGroup struct {
	ID        int64
	Name      string
	Kind      string // normal | aggregate
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
	// 成员组恒为 0：Key 只能绑聚合组（v23）。
	BoundKeys int64
	// ParentCount 是「包含本组的聚合组数量」。成员组靠它说明自己还有没有被用到；
	// 聚合组恒为 0（聚合组不能作为成员）。
	ParentCount int
	// Members / MemberIDs 仅在聚合组上非空：它包含的成员组。
	MemberIDs []int64
	Members   []ModelGroupBrief
	// Providers 是组「自己能直接用的渠道数」：成员组 = 自己挂的渠道，聚合组
	// 只算自有渠道，不含成员组带来的（成员组各自有计数）。
	Providers int
}

// ModelGroupBrief 是嵌在聚合组里的成员组摘要，供列表与编辑弹窗回显，
// 避免前端为了显示成员名再去拉一遍全部分组。
type ModelGroupBrief struct {
	ID        int64  `json:"ID"`
	Name      string `json:"Name"`
	Enabled   bool   `json:"Enabled"`
	RateMilli int64  `json:"RateMilli"`
	Providers int    `json:"Providers"`
}

type NewModelGroup struct {
	Name      string
	Enabled   bool
	RateMilli int64 // 0 视为默认 1000
	// Kind 为空按 aggregate（可对外）；aggregate 时 GroupIDs 是成员组，
	// normal（成员组）不允许带 GroupIDs。
	Kind     string
	GroupIDs []int64
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
	if err := s.ensureModelNameFree(ctx, s.db, name); err != nil {
		return Model{}, err
	}
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
	if rename := strings.TrimSpace(in.PublicName); rename != "" && rename != m.PublicName {
		if err := s.ensureModelNameFree(ctx, s.db, rename); err != nil {
			return Model{}, err
		}
		m.PublicName = rename
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

// DeleteModel removes a model. Group memberships and pool memberships cascade;
// settled ledger rows are kept so group quota is not reset by a deletion.
// Deleting a model with an in-flight request is refused until it settles.
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("model ID is required")
	}
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if err := s.ensureNoActiveReservation(ctx, `SELECT count(*) FROM key_reservations WHERE model_id=? AND status='reserved'`, id); err != nil {
		return err
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

// EffectiveProviderIDs 返回「这个组实际能用到的渠道」：自有渠道 ∪ 处于启用
// 状态的成员组渠道，与授权口径（authorizedModelsSelect）一致。
//
// 与 GroupProviderIDs 的区别：后者是「本组自己挂的渠道」，只该用于编辑。把
// 成员组渠道也列进编辑页会诱导运维把它们保存成自有渠道——v24 之后一个渠道只
// 属于一个组，这样保存会被后端直接拒绝（provider_in_use），而在 v24 之前则会
// 静默复制出一份归属，拆成员关系时拆不干净。组作用域的管理视图必须用这一份，
// 否则聚合组的使用者只看到聚合组自己挂的渠道，成员组带来的渠道像是凭空消失。
func (s *Store) EffectiveProviderIDs(ctx context.Context, groupID int64) ([]int64, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT provider_id FROM group_providers WHERE group_id=?
		UNION
		SELECT gp.provider_id FROM group_compositions gc
			JOIN model_groups cg ON cg.id=gc.child_group_id AND cg.enabled=1 AND cg.kind='normal'
			JOIN group_providers gp ON gp.group_id=gc.child_group_id
			WHERE gc.parent_group_id=?
		ORDER BY 1`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (s *Store) CreateModelGroup(ctx context.Context, in NewModelGroup) (ModelGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ModelGroup{}, errors.New("group name is required")
	}
	rate, err := normalizeRateMilli(in.RateMilli)
	if err != nil {
		return ModelGroup{}, err
	}
	kind, err := NormalizeGroupKind(in.Kind)
	if err != nil {
		return ModelGroup{}, err
	}
	if kind == GroupKindNormal && len(in.GroupIDs) > 0 {
		return ModelGroup{}, errors.New("成员组不能包含其他模型组；需要包含成员组请改用「聚合模型组」类型")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelGroup{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO model_groups(name,kind,enabled,rate_milli,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?)`, name, kind, 1, rate, nowUTC(), nowUTC())
	if err != nil {
		return ModelGroup{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelGroup{}, err
	}
	if kind == GroupKindAggregate {
		if err := replaceGroupCompositionsTx(ctx, tx, id, in.GroupIDs); err != nil {
			return ModelGroup{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ModelGroup{}, err
	}
	return ModelGroup{ID: id, Name: name, Kind: kind, Enabled: true, RateMilli: rate}, nil
}

// replaceGroupCompositionsTx 重建聚合组的成员关系。校验全部在事务内完成：
// 成员必须存在、必须是 normal 组、不能是聚合组自己（防环的第一道墙；因为
// 聚合组不允许作为成员，环在结构上就不可能形成）。
func replaceGroupCompositionsTx(ctx context.Context, tx *sql.Tx, parentID int64, childIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_compositions WHERE parent_group_id=?`, parentID); err != nil {
		return err
	}
	seen := make(map[int64]bool, len(childIDs))
	for _, childID := range childIDs {
		if childID <= 0 {
			return errors.New("成员组 ID 无效")
		}
		if childID == parentID {
			return errors.New("聚合组不能包含自己")
		}
		if seen[childID] {
			continue
		}
		seen[childID] = true
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM model_groups WHERE id=?`, childID).Scan(&kind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("成员组 #%d 不存在", childID)
			}
			return err
		}
		if kind != GroupKindNormal {
			return fmt.Errorf("聚合组只能包含成员组（#%d 是聚合组）", childID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_compositions(parent_group_id,child_group_id) VALUES (?,?)`, parentID, childID); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceGroupCompositions 是给管理端用的成员关系覆盖写。
func (s *Store) ReplaceGroupCompositions(ctx context.Context, parentID int64, childIDs []int64) error {
	if parentID <= 0 {
		return errors.New("group ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM model_groups WHERE id=?`, parentID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}
	if kind != GroupKindAggregate {
		return errors.New("只有聚合模型组可以包含其他模型组")
	}
	if err := replaceGroupCompositionsTx(ctx, tx, parentID, childIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateModelGroup(ctx context.Context, id int64, in UpdateModelGroup) (ModelGroup, error) {
	var g ModelGroup
	var enabled int
	var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
	var ps sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT name,kind,enabled,rate_milli,token_limit_5h,amount_limit_5h,token_limit_daily,amount_limit_daily,token_limit_weekly,amount_limit_weekly,period_limit_days,token_limit_period,amount_limit_period,period_limit_start FROM model_groups WHERE id=?`, id).Scan(&g.Name, &g.Kind, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps); err != nil {
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
	if g.Kind == GroupKindAggregate {
		members, err := s.groupMembers(ctx, id)
		if err != nil {
			return ModelGroup{}, err
		}
		g.Members = members
	}
	return g, nil
}

// groupMembers 读聚合组包含的成员组摘要。普通组直接返回空切片。
func (s *Store) groupMembers(ctx context.Context, groupID int64) ([]ModelGroupBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.enabled,g.rate_milli,
		(SELECT COUNT(*) FROM group_providers gp WHERE gp.group_id=g.id)
		FROM group_compositions gc JOIN model_groups g ON g.id=gc.child_group_id
		WHERE gc.parent_group_id=? ORDER BY g.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelGroupBrief, 0)
	for rows.Next() {
		var m ModelGroupBrief
		var enabled int
		if err := rows.Scan(&m.ID, &m.Name, &enabled, &m.RateMilli, &m.Providers); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// GroupCompositionIDs 返回聚合组的成员组 ID，供编辑弹窗回显与删除影响面计算。
func (s *Store) GroupCompositionIDs(ctx context.Context, groupID int64) ([]int64, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT child_group_id FROM group_compositions WHERE parent_group_id=? ORDER BY child_group_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// AggregatesContaining 返回把 groupID 当作成员包含的聚合组，供删除普通组时
// 提示影响面（删掉成员组会同时改变这些聚合组的可用模型）。
func (s *Store) AggregatesContaining(ctx context.Context, groupID int64) ([]ModelGroupBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.enabled,g.rate_milli,0
		FROM group_compositions gc JOIN model_groups g ON g.id=gc.parent_group_id
		WHERE gc.child_group_id=? ORDER BY g.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelGroupBrief, 0)
	for rows.Next() {
		var m ModelGroupBrief
		var enabled int
		if err := rows.Scan(&m.ID, &m.Name, &enabled, &m.RateMilli, &m.Providers); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListModelGroups(ctx context.Context) ([]ModelGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.kind,g.enabled,g.rate_milli,g.token_limit_5h,g.amount_limit_5h,g.token_limit_daily,g.amount_limit_daily,g.token_limit_weekly,g.amount_limit_weekly,g.period_limit_days,g.token_limit_period,g.amount_limit_period,g.period_limit_start,(SELECT count(*) FROM client_keys k WHERE k.group_id=g.id),(SELECT count(*) FROM group_providers gp WHERE gp.group_id=g.id),(SELECT count(*) FROM group_compositions gc WHERE gc.child_group_id=g.id) FROM model_groups g ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]ModelGroup, 0)
	briefs := make(map[int64]ModelGroupBrief)
	for rows.Next() {
		var g ModelGroup
		var enabled int
		var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
		var ps sql.NullString
		var bound, providers, parents int64
		if err := rows.Scan(&g.ID, &g.Name, &g.Kind, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps, &bound, &providers, &parents); err != nil {
			return nil, err
		}
		g.Enabled = enabled == 1
		g.ParentCount = int(parents)
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
		g.Providers = int(providers)
		g.MemberIDs = make([]int64, 0)
		g.Members = make([]ModelGroupBrief, 0)
		briefs[g.ID] = ModelGroupBrief{ID: g.ID, Name: g.Name, Enabled: g.Enabled, RateMilli: g.RateMilli, Providers: int(providers)}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 一次取全部成员关系，按父组归类——避免每个聚合组各查一次（组多时是 N+1）。
	members := make(map[int64][]ModelGroupBrief)
	rows2, err := s.db.QueryContext(ctx, `SELECT parent_group_id,child_group_id FROM group_compositions ORDER BY parent_group_id,child_group_id`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var parent, child int64
		if err := rows2.Scan(&parent, &child); err != nil {
			return nil, err
		}
		if brief, ok := briefs[child]; ok {
			members[parent] = append(members[parent], brief)
		}
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if list, ok := members[out[i].ID]; ok {
			out[i].Members = list
			for _, m := range list {
				out[i].MemberIDs = append(out[i].MemberIDs, m.ID)
			}
		}
	}
	return out, nil
}

// GetModelGroup 读单个分组。控制台按密钥主体登录时要拿组名做展示，
// 逐个 ListModelGroups 再筛不划算也容易写错。
func (s *Store) GetModelGroup(ctx context.Context, id int64) (ModelGroup, error) {
	if id <= 0 {
		return ModelGroup{}, errors.New("group ID is required")
	}
	var g ModelGroup
	var enabled int
	var t5h, a5h, td, ad, tw, aw, pd, tp, ap sql.NullInt64
	var ps sql.NullString
	var bound, providers, parents int64
	err := s.db.QueryRowContext(ctx, `SELECT g.id,g.name,g.kind,g.enabled,g.rate_milli,g.token_limit_5h,g.amount_limit_5h,g.token_limit_daily,g.amount_limit_daily,g.token_limit_weekly,g.amount_limit_weekly,g.period_limit_days,g.token_limit_period,g.amount_limit_period,g.period_limit_start,(SELECT count(*) FROM client_keys k WHERE k.group_id=g.id),(SELECT count(*) FROM group_providers gp WHERE gp.group_id=g.id),(SELECT count(*) FROM group_compositions gc WHERE gc.child_group_id=g.id) FROM model_groups g WHERE g.id=?`, id).
		Scan(&g.ID, &g.Name, &g.Kind, &enabled, &g.RateMilli, &t5h, &a5h, &td, &ad, &tw, &aw, &pd, &tp, &ap, &ps, &bound, &providers, &parents)
	if err != nil {
		return ModelGroup{}, err
	}
	g.Enabled = enabled == 1
	g.ParentCount = int(parents)
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
	g.Providers = int(providers)
	g.MemberIDs = make([]int64, 0)
	g.Members = make([]ModelGroupBrief, 0)
	if g.Kind == GroupKindAggregate {
		members, err := s.groupMembers(ctx, id)
		if err != nil {
			return ModelGroup{}, err
		}
		g.Members = members
		for _, m := range members {
			g.MemberIDs = append(g.MemberIDs, m.ID)
		}
	}
	return g, nil
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
		return errors.New("分组已停用，启用后才能调整渠道")
	}
	seen := make(map[int64]struct{}, len(providerIDs))
	names := make(map[int64]string, len(providerIDs))
	for _, id := range providerIDs {
		if id <= 0 {
			return errors.New("invalid provider ID")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate provider ID")
		}
		seen[id] = struct{}{}
		var enabled int
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name,enabled FROM providers WHERE id=?`, id).Scan(&name, &enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("渠道 #%d 不存在", id)
			}
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("渠道「%s」已停用，停用渠道不能入组", name)
		}
		// v24：一个渠道只能属于一个组。校验放在事务里，和随后的先删后插保持同一
		// 视图；同组重复保存是幂等的，所以只排除 groupID 自己。
		if err := checkProviderUnowned(ctx, tx, groupID, id, name); err != nil {
			return err
		}
		names[id] = name
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_providers WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for _, id := range providerIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_providers(group_id,provider_id) VALUES (?,?)`, groupID, id); err != nil {
			// 预检与插入之间被别的请求抢走了同一条渠道：唯一索引会在这里挡住。
			// 把约束错误翻译成同一句可读的冲突说明，别让运维看到 SQLite 原文。
			if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
				if conflict := checkProviderUnowned(ctx, tx, groupID, id, names[id]); conflict != nil {
					return conflict
				}
			}
			// 另一种并发形态：预检时拿的是旧快照，写的时候另一个连接已经提交，
			// WAL 下这里报的是 SQLITE_BUSY（"database is locked"），不是唯一约束。
			// 同样是「有人同时改分组」，给一句能照着做的中文，别让运维去搜英文。
			if isBusyError(err) {
				return errors.New("保存冲突：另一个操作正在同时修改分组或渠道，请稍后重试")
			}
			return err
		}
	}
	return tx.Commit()
}

// checkProviderUnowned 断言这条渠道还没有被别的组占用（v24）。
func checkProviderUnowned(ctx context.Context, q rowQuerier, groupID, providerID int64, providerName string) error {
	var ownerID int64
	var ownerName string
	err := q.QueryRowContext(ctx, `SELECT g.id,g.name FROM group_providers gp JOIN model_groups g ON g.id=gp.group_id
		WHERE gp.provider_id=? AND gp.group_id<>? ORDER BY g.id LIMIT 1`, providerID, groupID).Scan(&ownerID, &ownerName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &ProviderGroupConflict{ProviderID: providerID, ProviderName: providerName, GroupID: ownerID, GroupName: ownerName}
}

// isBusyError 识别 SQLite 的写-写冲突（SQLITE_BUSY / SQLITE_LOCKED）。WAL 下
// 事务先读后写时，如果读到的快照已被别的连接提交打断，报的是 "database is
// locked" 而不是唯一约束——它同样是「有人同时在改」，但对运维来说完全不可读。
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy")
}
