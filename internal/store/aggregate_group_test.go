package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// v22 之前建立的组没有 kind 列，升级后必须一律是 normal——聚合是新建能力，
// 不能让任何既有组因为默认值被当成「包含 0 个成员」，否则其可用模型会全空。
func TestMigrationV22DefaultsToNormal(t *testing.T) {
	repo, providerID, _ := setupGroupScope(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	// 显式把 kind 写回默认值，模拟从 v21 升级上来的行。
	if _, err := repo.db.ExecContext(ctx, `UPDATE model_groups SET kind='normal' WHERE id=?`, group.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetModelGroup(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Kind != GroupKindNormal {
		t.Fatalf("kind = %q, want normal", stored.Kind)
	}
	var columns int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('key_reservations') WHERE name='source_group_id'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("key_reservations.source_group_id 列数 = %d", columns)
	}
}

// 聚合组包含成员组之后，成员组的渠道与模型必须出现在授权集合里；
// 这正是「模型组可以包含其他模型组」的全部意义。
func TestAggregateGroupAuthorizesMemberModels(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	// 聚合组自己一个渠道都不挂，模型全部来自成员组。
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	models, err := repo.UsableModelsForKey(ctx, key.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		if m.ID == modelID {
			found = true
		}
	}
	if !found {
		t.Fatalf("聚合组看不到成员组模型：%#v", models)
	}
}

// 只允许一层：聚合组不能包含聚合组，也不能包含自己。否则额度归属与环检测
// 都会变得不可控（这是设计上有意砍掉的能力，不是遗漏）。
func TestAggregateGroupRejectsNestedAggregate(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setupGroupScope(t)
	inner, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "inner", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "outer", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupCompositions(ctx, outer.ID, []int64{inner.ID}); err == nil {
		t.Fatal("聚合组作为成员必须被拒绝")
	}
	if err := repo.ReplaceGroupCompositions(ctx, outer.ID, []int64{outer.ID}); err == nil {
		t.Fatal("聚合组包含自己必须被拒绝")
	}
	// 普通组不能声明成员（前端选错类型时也要在服务端挡住）。
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "plain", GroupIDs: []int64{inner.ID}}); err == nil {
		t.Fatal("普通模型组带成员必须被拒绝")
	}
}

// 「两个组都扣」的核心：成员组额度用满时，即使聚合组还很宽松，请求也必须被拒。
// 只判定 Key 所属组等于只扣了一边，老板明确否掉了那种实现。
func TestAggregateGroupMemberQuotaAlsoBlocks(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	// 聚合组不限，成员组当日额度只够一次请求。
	if _, err := repo.UpdateModelGroup(ctx, agg.ID, UpdateModelGroup{}); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err := repo.UpdateModelGroup(ctx, member.ID, UpdateModelGroup{TokenLimitDaily: &one}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "agg-1", 600)); err != nil {
		t.Fatalf("首次请求应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "agg-2", 600))
	if err == nil {
		t.Fatal("成员组额度用满后必须拒绝")
	}
	quota, ok := err.(*QuotaError)
	if !ok || quota.GroupID != member.ID || quota.Scope != "daily" {
		t.Fatalf("错误应指向成员组当日额度，实际 %#v", err)
	}
}

// 成员组的用量视图必须包含「被聚合组带出去」的流量，否则成员组看起来一直
// 没用量、额度却莫名被咬住——这是两套口径最容易出的岔子。
func TestAggregateGroupUsageVisibleOnMember(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	reservation, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "usage-1", 600))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, reservation.ID, 100, 50, 0, 1234, "completed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	five, _, _, err := repo.GroupUsageBuckets(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if five[agg.ID][1] != 1234 {
		t.Fatalf("聚合组金额 = %d, want 1234", five[agg.ID][1])
	}
	if five[member.ID][1] != 1234 {
		t.Fatalf("成员组金额 = %d, want 1234（被聚合的流量要同步计入）", five[member.ID][1])
	}
	overview, err := repo.GroupOverview(ctx, member.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	daily, ok := overview["daily"].(map[string]any)
	if !ok {
		t.Fatalf("overview.daily 形状不对：%#v", overview["daily"])
	}
	if daily["used_microyuan"].(int64) != 1234 {
		t.Fatalf("成员组当日金额 = %v, want 1234", daily["used_microyuan"])
	}
	// 账本只有一行：双行会让所有 SUM 翻倍、结算改多行，是计费系统最容易埋雷的地方。
	var rows int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM key_reservations WHERE request_id=?`, "usage-1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("账本行数 = %d, want 1", rows)
	}
	var sourceGroup int64
	if err := repo.db.QueryRowContext(ctx, `SELECT source_group_id FROM key_reservations WHERE request_id=?`, "usage-1").Scan(&sourceGroup); err != nil {
		t.Fatal(err)
	}
	if sourceGroup != member.ID {
		t.Fatalf("source_group_id = %d, want %d", sourceGroup, member.ID)
	}
}

// 聚合组自己的渠道优先于成员组：同一模型两处都可达时，来源组记为聚合组
// 自己，避免「明明自己挂着却去扣成员组的额度」这种反直觉行为。
func TestAggregateGroupPrefersOwnProvider(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, agg.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "own-1", 600)); err != nil {
		t.Fatal(err)
	}
	var sourceGroup int64
	if err := repo.db.QueryRowContext(ctx, `SELECT source_group_id FROM key_reservations WHERE request_id=?`, "own-1").Scan(&sourceGroup); err != nil {
		t.Fatal(err)
	}
	if sourceGroup != agg.ID {
		t.Fatalf("source_group_id = %d, want 聚合组自己 %d", sourceGroup, agg.ID)
	}
}

// 停用的成员组不能继续通过聚合组授权模型：否则「停用」在聚合场景下形同虚设。
func TestAggregateGroupSkipsDisabledMember(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := repo.UpdateModelGroup(ctx, member.ID, UpdateModelGroup{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	models, err := repo.UsableModelsForKey(ctx, key.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ID == modelID {
			t.Fatal("停用成员组的模型不应继续授权")
		}
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "disabled-1", 600)); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("停用成员组后预留应拒绝，实际 %v", err)
	}
}
