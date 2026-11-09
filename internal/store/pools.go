package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// AuthorizedPoolEntries 把「至少一个成员对该 Key 可用」的启用池变成虚拟模型条目，
// 追加到授权列表。它是 /v1/models 与管理端「接入信息」的共同口径：客户端能看到的
// 池名，一定真的能调（且不会把未授权渠道里的成员暴露出去）。
//
// 虚拟条目刻意用负 ID + PoolID 标记：它绝不能被拿去做预留（ReserveRequest 要求
// model_id > 0），Begin 会先解析成真实成员。
func (s *Store) AuthorizedPoolEntries(ctx context.Context, authorized []Model) ([]Model, error) {
	allowed := make(map[int64]bool, len(authorized))
	usedNames := make(map[string]bool, len(authorized))
	for _, model := range authorized {
		if model.ID > 0 {
			allowed[model.ID] = true
			usedNames[model.PublicName] = true
		}
	}
	pools, err := s.queryPools(ctx, "WHERE p.enabled=1")
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(pools))
	for _, pool := range pools {
		if usedNames[pool.Name] {
			continue // 真实模型同名时以真实模型为准，避免 /v1/models 出现重复 ID
		}
		usable := false
		for _, member := range pool.Members {
			if member.Enabled && allowed[member.ModelID] {
				usable = true
				break
			}
		}
		if !usable {
			continue
		}
		out = append(out, Model{ID: -pool.ID, PublicName: pool.Name, Enabled: true, PoolID: pool.ID})
	}
	return out, nil
}
