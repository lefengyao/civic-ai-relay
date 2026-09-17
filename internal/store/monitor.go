package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 分组可用性状态。
//
//   - ok / degraded：还能服务（degraded 表示组内部分渠道不可达，走那些渠道的
//     模型会失败，但其他模型正常）；
//   - upstream_unreachable：所有启用渠道都探不通，整组无法服务；
//   - config_unavailable：配置层面就不成立（无启用渠道、或渠道下没有已启用且
//     已定价的模型），跟上游通不通无关；
//   - disabled：运维主动停用。这不算故障，界面用灰色区分，避免一片红。
const (
	GroupStatusOK                  = "ok"
	GroupStatusDegraded            = "degraded"
	GroupStatusUpstreamUnreachable = "upstream_unreachable"
	GroupStatusConfigUnavailable   = "config_unavailable"
	GroupStatusDisabled            = "disabled"
)

// MonitorProvider 是监测快照里的一条渠道。
type MonitorProvider struct {
	ID           int64
	Name         string
	Enabled      bool
	PricedModels int // 该渠道下「已启用且已定价」的模型数量
}

// GroupMonitorTarget 是一个待监测的分组及其构成，由一次联表查询取全，
// 避免在探活循环里反复查库。
type GroupMonitorTarget struct {
	GroupID   int64
	GroupName string
	Enabled   bool
	Providers []MonitorProvider
}

// PricedModelCount 统计组内**启用渠道**下已启用且已定价的模型数量。
// 停用渠道下的模型不会授权给客户端，所以不计入。
func (t GroupMonitorTarget) PricedModelCount() int {
	total := 0
	for _, p := range t.Providers {
		if p.Enabled {
			total += p.PricedModels
		}
	}
	return total
}

// EnabledProviders 返回组内启用中的渠道。
func (t GroupMonitorTarget) EnabledProviders() []MonitorProvider {
	out := make([]MonitorProvider, 0, len(t.Providers))
	for _, p := range t.Providers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// GroupMonitorResult 是一轮监测的结果。Detail 会原样显示在管理台，因此
// 绝不能写入上游 Key 或任何凭据。
type GroupMonitorResult struct {
	GroupID         int64  `json:"group_id"`
	GroupName       string `json:"group_name"`
	Status          string `json:"status"`
	Detail          string `json:"detail"`
	TotalProviders  int    `json:"total_providers"`
	FailedProviders int    `json:"failed_providers"`
	LatencyMS       int64  `json:"latency_ms"`
	Trigger         string `json:"trigger"`
	CreatedAtUTC    string `json:"created_at_utc"`
}

// GroupMonitorTargets 一次查出所有分组及其渠道构成，供监测循环使用。
func (s *Store) GroupMonitorTargets(ctx context.Context) ([]GroupMonitorTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,g.enabled,p.id,COALESCE(p.name,''),COALESCE(p.enabled,0),
		(SELECT COUNT(*) FROM models m WHERE m.provider_id=p.id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL)
		FROM model_groups g
		LEFT JOIN group_providers gp ON gp.group_id=g.id
		LEFT JOIN providers p ON p.id=gp.provider_id
		ORDER BY g.id, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GroupMonitorTarget, 0)
	position := make(map[int64]int)
	for rows.Next() {
		var groupID int64
		var groupName string
		var groupEnabled int
		var providerID sql.NullInt64
		var providerName string
		var providerEnabled int
		var priced int
		if err := rows.Scan(&groupID, &groupName, &groupEnabled, &providerID, &providerName, &providerEnabled, &priced); err != nil {
			return nil, err
		}
		idx, ok := position[groupID]
		if !ok {
			out = append(out, GroupMonitorTarget{GroupID: groupID, GroupName: groupName, Enabled: groupEnabled == 1, Providers: make([]MonitorProvider, 0)})
			idx = len(out) - 1
			position[groupID] = idx
		}
		// 没有任何渠道的分组在 LEFT JOIN 下会得到一行全 NULL，跳过即可，
		// 它的 ProviderCount 自然是 0。
		if providerID.Valid {
			out[idx].Providers = append(out[idx].Providers, MonitorProvider{
				ID: providerID.Int64, Name: providerName, Enabled: providerEnabled == 1, PricedModels: priced,
			})
		}
	}
	return out, rows.Err()
}

// SaveGroupMonitorResult 落盘一轮监测结果。
func (s *Store) SaveGroupMonitorResult(ctx context.Context, r GroupMonitorResult) error {
	if r.GroupID <= 0 {
		return errors.New("group ID is required")
	}
	if r.Trigger == "" {
		r.Trigger = "auto"
	}
	if r.TotalProviders < 0 || r.FailedProviders < 0 || r.FailedProviders > r.TotalProviders {
		return errors.New("invalid provider counters")
	}
	created := r.CreatedAtUTC
	if strings.TrimSpace(created) == "" {
		created = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO group_monitor_results(group_id,status,detail,total_providers,failed_providers,latency_ms,trigger,created_at_utc) VALUES (?,?,?,?,?,?,?,?)`,
		r.GroupID, r.Status, r.Detail, r.TotalProviders, r.FailedProviders, r.LatencyMS, r.Trigger, created)
	return err
}

const monitorResultColumns = `r.group_id,g.name,r.status,r.detail,r.total_providers,r.failed_providers,r.latency_ms,r.trigger,r.created_at_utc`

func scanMonitorResult(scan func(...any) error) (GroupMonitorResult, error) {
	var r GroupMonitorResult
	if err := scan(&r.GroupID, &r.GroupName, &r.Status, &r.Detail, &r.TotalProviders, &r.FailedProviders, &r.LatencyMS, &r.Trigger, &r.CreatedAtUTC); err != nil {
		return GroupMonitorResult{}, err
	}
	return r, nil
}

// LatestGroupMonitorResult 返回单个分组最近一次监测结果。
func (s *Store) LatestGroupMonitorResult(ctx context.Context, groupID int64) (GroupMonitorResult, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+monitorResultColumns+` FROM group_monitor_results r JOIN model_groups g ON g.id=r.group_id WHERE r.group_id=? ORDER BY r.id DESC LIMIT 1`, groupID)
	return scanMonitorResult(row.Scan)
}

// LatestGroupMonitorResults 返回每个分组最近一次监测结果，键为分组 ID。
// 管理台分组列表一次拿全，避免每组发一次请求。
func (s *Store) LatestGroupMonitorResults(ctx context.Context) (map[int64]GroupMonitorResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+monitorResultColumns+`
		FROM group_monitor_results r JOIN model_groups g ON g.id=r.group_id
		WHERE r.id=(SELECT MAX(id) FROM group_monitor_results WHERE group_id=r.group_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]GroupMonitorResult)
	for rows.Next() {
		r, err := scanMonitorResult(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[r.GroupID] = r
	}
	return out, rows.Err()
}

// GroupMonitorHistory 返回单个分组的最近若干轮结果，最新的在前。
func (s *Store) GroupMonitorHistory(ctx context.Context, groupID int64, limit int) ([]GroupMonitorResult, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+monitorResultColumns+` FROM group_monitor_results r JOIN model_groups g ON g.id=r.group_id WHERE r.group_id=? ORDER BY r.id DESC LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GroupMonitorResult, 0)
	for rows.Next() {
		r, err := scanMonitorResult(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// monitorRowsPerGroup 是每个分组保留的监测历史上限。监测间隔可以配到 1 分钟，
// 不设上限的话一张小表也会无限长；200 条足够看清「什么时候开始坏的」。
const monitorRowsPerGroup = 200

// PruneGroupMonitorResults 按时间与条数双重裁剪：先删早于 cutoff 的，
// 再对每个分组只保留最新的 monitorRowsPerGroup 条。
func (s *Store) PruneGroupMonitorResults(ctx context.Context, cutoff time.Time) (int64, error) {
	cutoffText := cutoff.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_monitor_results WHERE created_at_utc < ?`, cutoffText); err != nil {
		return 0, err
	}
	// 「保留每组最新 N 条」：删掉那些在它之后已有 >= N 条的行。
	result, err := tx.ExecContext(ctx, `DELETE FROM group_monitor_results WHERE id IN (
		SELECT r1.id FROM group_monitor_results r1
		WHERE (SELECT COUNT(*) FROM group_monitor_results r2 WHERE r2.group_id=r1.group_id AND r2.id>r1.id) >= ?
	)`, monitorRowsPerGroup)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
