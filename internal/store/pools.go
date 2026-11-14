package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 故障转移供应商（模型池）是与 providers 并列的第二类「供应商形式」：
//
//   - 每个池只对应**一个对外模型名**（单一模型类别），例如 "gpt-4o"；
//   - 池下面挂的是**已有供应商下的模型**（可跨供应商），按 sort_order 排优先级；
//   - 客户端请求池名时，网关按顺序挑一个可用成员转发；成员故障时自动切下一个。
//
// 池不是真实模型：它不出现在 models 表里，也不直接参与预留。请求最终一定会落到
// 某个真实成员模型上，账目里记的也是那个成员的 model_id / provider_id。

type ModelPool struct {
	ID      int64
	Name    string
	Enabled bool
	Members []ModelPoolMember
}

type ModelPoolMember struct {
	ModelID      int64
	PublicName   string
	UpstreamName string
	ProviderID   int64
	ProviderName string
	SortOrder    int
	Enabled      bool
}

// PoolMemberInput 的顺序就是优先级顺序（数组下标写入 sort_order）。
type PoolMemberInput struct {
	ModelID int64
	Enabled bool
}

type NewModelPool struct {
	Name    string
	Members []PoolMemberInput
}

type UpdateModelPool struct {
	Name    string
	Enabled *bool
	// nil = 不动成员；非 nil（含空数组）= 全量替换。
	Members *[]PoolMemberInput
}

const poolSelect = `SELECT p.id,p.name,p.enabled,
	mpm.model_id,mpm.sort_order,mpm.enabled,
	m.public_name,m.upstream_name,m.provider_id,pr.name
	FROM model_pools p
	LEFT JOIN model_pool_members mpm ON mpm.pool_id=p.id
	LEFT JOIN models m ON m.id=mpm.model_id
	LEFT JOIN providers pr ON pr.id=m.provider_id `

type poolQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) queryPools(ctx context.Context, where string, args ...any) ([]ModelPool, error) {
	rows, err := s.db.QueryContext(ctx, poolSelect+where+` ORDER BY p.id,mpm.sort_order,mpm.model_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelPool, 0)
	index := make(map[int64]int)
	for rows.Next() {
		var id int64
		var name string
		var enabled int
		var memberID, sortOrder, memberEnabled, providerID sql.NullInt64
		var publicName, upstreamName, providerName sql.NullString
		if err := rows.Scan(&id, &name, &enabled, &memberID, &sortOrder, &memberEnabled,
			&publicName, &upstreamName, &providerID, &providerName); err != nil {
			return nil, err
		}
		pos, ok := index[id]
		if !ok {
			pos = len(out)
			index[id] = pos
			out = append(out, ModelPool{ID: id, Name: name, Enabled: enabled == 1, Members: make([]ModelPoolMember, 0)})
		}
		if !memberID.Valid {
			continue // 空池：LEFT JOIN 补齐的那一行
		}
		member := ModelPoolMember{
			ModelID: memberID.Int64, SortOrder: int(sortOrder.Int64), Enabled: memberEnabled.Int64 == 1,
			PublicName: publicName.String, UpstreamName: upstreamName.String,
			ProviderID: providerID.Int64, ProviderName: providerName.String,
		}
		out[pos].Members = append(out[pos].Members, member)
	}
	return out, rows.Err()
}

func (s *Store) ListModelPools(ctx context.Context) ([]ModelPool, error) {
	return s.queryPools(ctx, "")
}

func (s *Store) GetModelPool(ctx context.Context, id int64) (ModelPool, error) {
	if id <= 0 {
		return ModelPool{}, errors.New("pool ID is required")
	}
	pools, err := s.queryPools(ctx, "WHERE p.id=?", id)
	if err != nil {
		return ModelPool{}, err
	}
	if len(pools) == 0 {
		return ModelPool{}, sql.ErrNoRows
	}
	return pools[0], nil
}

// ensurePoolNameFree 禁止池名与「模型对外名」或「另一个池名」撞车：两者都会出现在
// /v1/models 里，同名会让 Begin 无法判断客户端要的是真实模型还是池。
func (s *Store) ensurePoolNameFree(ctx context.Context, q poolQuerier, name string, excludePoolID int64) error {
	var models, pools int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM models WHERE public_name=?`, name).Scan(&models); err != nil {
		return err
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM model_pools WHERE name=? AND id<>?`, name, excludePoolID).Scan(&pools); err != nil {
		return err
	}
	if models > 0 || pools > 0 {
		return fmt.Errorf("name %q is already used by a model or another pool", name)
	}
	return nil
}

// ensureModelNameFree 是反向保护：新建/改名真实模型时也不能占用池名。
func (s *Store) ensureModelNameFree(ctx context.Context, q poolQuerier, name string) error {
	var pools int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM model_pools WHERE name=?`, name).Scan(&pools); err != nil {
		return err
	}
	if pools > 0 {
		return fmt.Errorf("name %q is already used by a failover pool", name)
	}
	return nil
}

func (s *Store) CreateModelPool(ctx context.Context, in NewModelPool) (ModelPool, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ModelPool{}, errors.New("pool name is required")
	}
	if err := s.ensurePoolNameFree(ctx, s.db, name, 0); err != nil {
		return ModelPool{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelPool{}, err
	}
	defer tx.Rollback()
	now := nowUTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO model_pools(name,enabled,created_at_utc,updated_at_utc) VALUES (?,1,?,?)`, name, now, now)
	if err != nil {
		return ModelPool{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelPool{}, err
	}
	if err := replacePoolMembers(ctx, tx, id, in.Members); err != nil {
		return ModelPool{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelPool{}, err
	}
	return s.GetModelPool(ctx, id)
}

func (s *Store) UpdateModelPool(ctx context.Context, id int64, in UpdateModelPool) (ModelPool, error) {
	if id <= 0 {
		return ModelPool{}, errors.New("pool ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelPool{}, err
	}
	defer tx.Rollback()
	var name string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT name,enabled FROM model_pools WHERE id=?`, id).Scan(&name, &enabled); err != nil {
		return ModelPool{}, err
	}
	if trimmed := strings.TrimSpace(in.Name); trimmed != "" && trimmed != name {
		if err := s.ensurePoolNameFree(ctx, tx, trimmed, id); err != nil {
			return ModelPool{}, err
		}
		name = trimmed
	}
	if in.Enabled != nil {
		enabled = boolInt(*in.Enabled)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_pools SET name=?,enabled=?,updated_at_utc=? WHERE id=?`, name, enabled, nowUTC(), id); err != nil {
		return ModelPool{}, err
	}
	if in.Members != nil {
		if err := replacePoolMembers(ctx, tx, id, *in.Members); err != nil {
			return ModelPool{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ModelPool{}, err
	}
	return s.GetModelPool(ctx, id)
}

func replacePoolMembers(ctx context.Context, tx *sql.Tx, poolID int64, members []PoolMemberInput) error {
	seen := make(map[int64]struct{}, len(members))
	for i, member := range members {
		if member.ModelID <= 0 {
			return errors.New("invalid pool member model ID")
		}
		if _, ok := seen[member.ModelID]; ok {
			return errors.New("duplicate pool member")
		}
		seen[member.ModelID] = struct{}{}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM models WHERE id=?`, member.ModelID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pool member model %d does not exist", member.ModelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_pool_members(pool_id,model_id,sort_order,enabled) VALUES (?,?,?,?) ON CONFLICT(pool_id,model_id) DO UPDATE SET sort_order=excluded.sort_order,enabled=excluded.enabled`, poolID, member.ModelID, i, boolInt(member.Enabled)); err != nil {
			return err
		}
	}
	// 删除这次没提交的成员（幂等：重复提交同一批不会误删）。
	if len(members) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM model_pool_members WHERE pool_id=?`, poolID)
		return err
	}
	query := `DELETE FROM model_pool_members WHERE pool_id=? AND model_id NOT IN (`
	args := make([]any, 0, len(members)+1)
	args = append(args, poolID)
	for i, member := range members {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, member.ModelID)
	}
	query += ")"
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) DeleteModelPool(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("pool ID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_pools WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PoolCandidateIDs 返回某池启用的成员模型 ID，按优先级顺序。停用成员、被删模型
// 都不在其中；调用方还要与「该 Key 的授权模型」求交，避免绕过分组授权。
func (s *Store) PoolCandidateIDs(ctx context.Context, poolID int64) ([]int64, error) {
	if poolID <= 0 {
		return nil, errors.New("pool ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT mpm.model_id FROM model_pool_members mpm JOIN model_pools p ON p.id=mpm.pool_id AND p.enabled=1 WHERE mpm.pool_id=? AND mpm.enabled=1 ORDER BY mpm.sort_order,mpm.model_id`, poolID)
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

// GroupPoolIDs 返回聚合组直接包含的故障转移供应商（池）ID（v27）。
func (s *Store) GroupPoolIDs(ctx context.Context, groupID int64) ([]int64, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pool_id FROM group_pools WHERE group_id=? ORDER BY pool_id`, groupID)
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

// ReplaceGroupPools 全量替换聚合组包含的池。
//
// 池是**模型级**成员：这里只写 group_pools，绝不碰 group_providers。同一个渠道
// 可以既属于别的组、又被本组的池引用——渠道归属不变，本组拿到的只是池里那几个
// 模型的调用权（authorizedModelsSelect 的第三个分支）。
//
// 只允许聚合组：成员组不对外，Key 绑不上它，把池挂上去没有任何入口。
func (s *Store) ReplaceGroupPools(ctx context.Context, groupID int64, poolIDs []int64) error {
	if groupID <= 0 {
		return errors.New("group ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind string
	var groupEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT kind,enabled FROM model_groups WHERE id=?`, groupID).Scan(&kind, &groupEnabled); err != nil {
		return err
	}
	if kind != GroupKindAggregate {
		return errors.New("只有聚合模型组可以包含故障转移供应商")
	}
	if groupEnabled != 1 {
		return errors.New("分组已停用，启用后才能调整故障转移供应商")
	}
	seen := make(map[int64]struct{}, len(poolIDs))
	for _, id := range poolIDs {
		if id <= 0 {
			return errors.New("invalid pool ID")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate pool ID")
		}
		seen[id] = struct{}{}
		var name string
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT name,enabled FROM model_pools WHERE id=?`, id).Scan(&name, &enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("故障转移供应商 #%d 不存在", id)
			}
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("故障转移供应商「%s」已停用，停用的池不能入组", name)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_pools WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for _, id := range poolIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_pools(group_id,pool_id) VALUES (?,?)`, groupID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuthorizedPoolEntries 把「本 Key 的分组确实包含、且至少一个成员此刻可路由」的
// 池变成虚拟模型条目，追加到授权列表。
//
// 这是池的唯一对外口径：池在 /v1/models 里只以**一个**名字出现，成员模型不单列。
// 成员能在池内被路由，但不能按自己的名字直接调用 —— 否则客户端只要知道成员名
// 就能绕过故障转移，池的成员清单也等于公开了（v0.14.1）。
//
// 成员「可路由」= 成员启用 + 模型启用且已定价 + 渠道启用 +（now 非零时）渠道在
// 生效时段内。now 为零值表示「不按时段过滤」，管理台「接入信息」用这种口径。
//
// 虚拟条目刻意用负 ID + PoolID 标记：它绝不能被拿去做预留（ReserveRequest 要求
// model_id > 0），Begin 会先解析成真实成员。
func (s *Store) AuthorizedPoolEntries(ctx context.Context, keyWhere string, keyArgs []any, authorized []Model, now time.Time) ([]Model, error) {
	usedNames := make(map[string]bool, len(authorized))
	for _, model := range authorized {
		if model.ID > 0 {
			usedNames[model.PublicName] = true
		}
	}
	query := `SELECT p.id,p.name,m.provider_id
		FROM client_keys k
		JOIN model_groups g ON g.id=k.group_id AND g.enabled=1 AND g.kind='aggregate'
		JOIN group_pools gpl ON gpl.group_id=g.id
		JOIN model_pools p ON p.id=gpl.pool_id AND p.enabled=1
		JOIN model_pool_members pm ON pm.pool_id=p.id AND pm.enabled=1
		JOIN models m ON m.id=pm.model_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL
		JOIN providers pr ON pr.id=m.provider_id AND pr.enabled=1
		WHERE ` + keyWhere + ` ORDER BY p.id,pm.sort_order,pm.model_id`
	rows, err := s.db.QueryContext(ctx, query, keyArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type poolRow struct {
		id   int64
		name string
	}
	order := make([]poolRow, 0, 4)
	index := make(map[int64]int, 4)
	members := make(map[int64][]int64, 4)
	seenProvider := make(map[int64]map[int64]bool, 4)
	for rows.Next() {
		var poolID, providerID int64
		var name string
		if err := rows.Scan(&poolID, &name, &providerID); err != nil {
			return nil, err
		}
		if _, ok := index[poolID]; !ok {
			index[poolID] = len(order)
			order = append(order, poolRow{id: poolID, name: name})
		}
		if seenProvider[poolID] == nil {
			seenProvider[poolID] = make(map[int64]bool, 2)
		}
		if !seenProvider[poolID][providerID] {
			seenProvider[poolID][providerID] = true
			members[poolID] = append(members[poolID], providerID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 时段过滤：一个池只有「至少一个成员此刻能路由」才对外可见，否则客户端会看到
	// 一个必然失败的池名。
	active := map[int64]bool{}
	if !now.IsZero() && len(order) > 0 {
		ids := make([]int64, 0, len(members))
		for _, row := range order {
			ids = append(ids, members[row.id]...)
		}
		active, err = s.activeProviderIDs(ctx, ids, now)
		if err != nil {
			return nil, err
		}
	}
	out := make([]Model, 0, len(order))
	for _, row := range order {
		if usedNames[row.name] {
			continue // 真实模型同名时以真实模型为准，避免 /v1/models 出现重复 ID
		}
		usable := false
		for _, providerID := range members[row.id] {
			if now.IsZero() {
				usable = true
				break
			}
			// 查不到渠道行时保守放行，与 filterByProviderSchedule 同口径。
			if inWindow, known := active[providerID]; !known || inWindow {
				usable = true
				break
			}
		}
		if !usable {
			continue
		}
		out = append(out, Model{ID: -row.id, PublicName: row.name, Enabled: true, PoolID: row.id})
	}
	return out, nil
}

// PoolUsableModels 返回某池此刻真正可路由的成员模型，按池内优先级排序。
//
// 不再与该 Key 的授权模型求交：池的授权范围就是成员本身（组通过 group_pools
// 引用池），求交反而会让「只挂在池里的成员」永远选不上。这里只校验成员自身能
// 不能调：模型启用且已定价、渠道启用、渠道在生效时段内。
func (s *Store) PoolUsableModels(ctx context.Context, poolID int64, now time.Time) ([]Model, error) {
	if poolID <= 0 {
		return nil, errors.New("pool ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.provider_id,m.public_name,m.upstream_name,m.input_price_microyuan,m.output_price_microyuan,m.cached_input_price_microyuan,m.enabled
		FROM model_pools p
		JOIN model_pool_members pm ON pm.pool_id=p.id AND pm.enabled=1
		JOIN models m ON m.id=pm.model_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL
		JOIN providers pr ON pr.id=m.provider_id AND pr.enabled=1
		WHERE p.id=? AND p.enabled=1 ORDER BY pm.sort_order,pm.model_id`, poolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Model, 0)
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.filterByProviderSchedule(ctx, out, now)
}
