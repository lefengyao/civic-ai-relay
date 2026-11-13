package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// v22 的 kind 列必须有默认值，老库升级后才能读出来（聚合是 v22 新建的能力，
// 不能让任何既有组因为列缺失而被当成「包含 0 个成员」，否则其可用模型会全空）。
// 注意 v23 会把「没有被任何聚合组包含」的 normal 组改成 aggregate —— 那是语义
// 收紧的补丁，与这里的列默认值不是一回事：列默认值保证能读，v23 保证能对外。
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
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "plain", Kind: GroupKindNormal, GroupIDs: []int64{inner.ID}}); err == nil {
		t.Fatal("普通模型组带成员必须被拒绝")
	}
}

// 「两个组都扣」的核心：成员组额度用满时，即使聚合组还很宽松，请求也必须被拒。
// 只判定 Key 所属组等于只扣了一边，老板明确否掉了那种实现。
func TestAggregateGroupMemberQuotaAlsoBlocks(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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

// v24：一个渠道只能属于一个组，所以「聚合组自己也挂这条渠道、同时成员组也挂它」
// 这种双归属在保存时就会被拒——它正是过去「这笔请求到底扣谁额度」说不清的来源。
// 记账侧因此只需读渠道的归属组（见 ledger.go 的 source_group 子查询），不再需要
// 「自有渠道优先 / 取最小成员组 ID」这样的消歧规则。
func TestAggregateCannotAlsoOwnMemberProvider(t *testing.T) {
	ctx := context.Background()
	repo, providerID, _ := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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
	err = repo.ReplaceGroupProviders(ctx, agg.ID, []int64{providerID})
	var conflict *ProviderGroupConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *ProviderGroupConflict", err)
	}
	if conflict.GroupID != member.ID || conflict.GroupName != "member" {
		t.Fatalf("conflict = %#v, want 已属于 member(#%d)", conflict, member.ID)
	}
	// 冲突不能留下半成品：聚合组应当仍只有 0 个自有渠道。
	own, err := repo.GroupProviderIDs(ctx, agg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		t.Fatalf("被拒之后聚合组仍有自有渠道 %v", own)
	}
	// 但成员关系照旧生效：渠道依然通过成员组为聚合组服务。
	ids, err := repo.EffectiveProviderIDs(ctx, agg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != providerID {
		t.Fatalf("聚合组有效渠道 = %v, want [%d]", ids, providerID)
	}
}

// 停用的成员组不能继续通过聚合组授权模型：否则「停用」在聚合场景下形同虚设。
func TestAggregateGroupSkipsDisabledMember(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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

// v23：成员组不对外。绑 Key 必须在写入时就被拒，而不是绑上去以后「静默没有模型」。
func TestMemberGroupCannotBeBoundToKey(t *testing.T) {
	ctx := context.Background()
	repo, providerID, _ := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	err = repo.SetKeyGroup(ctx, key.ID, member.ID)
	if !errors.Is(err, ErrGroupNotCallable) {
		t.Fatalf("绑成员组 err = %v, want ErrGroupNotCallable", err)
	}
	// 拒绝之后不能留下「已绑上」的半成品。
	stored, err := repo.GetClientKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GroupID != nil {
		t.Fatalf("绑定失败却写入了 group_id = %d", *stored.GroupID)
	}
}

// v23：即使历史数据里已经有「Key 绑在成员组」的脏绑定，转发链路的两个出口
// （授权 SQL / 预留事务）也都不许放行 —— 规则要落在读路径上，不能只靠写路径拦。
func TestMemberGroupServesNothingEvenIfLegacyBinding(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 绕过 SetKeyGroup 的校验，模拟从 v22 升级上来的数据。
	if _, err := repo.db.ExecContext(ctx, `UPDATE client_keys SET group_id=? WHERE id=?`, member.ID, key.ID); err != nil {
		t.Fatal(err)
	}
	models, err := repo.UsableModelsForKey(ctx, key.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("成员组不应该授权任何模型，实际 %#v", models)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "legacy-member-1", 600)); err == nil {
		t.Fatal("成员组上的 Key 不允许产生预留")
	}
	// 记账也不能被动过：拒绝发生在写账目之前。
	var rows int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM key_reservations WHERE request_id=?`, "legacy-member-1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("被拒的请求仍写入了 %d 行账目", rows)
	}
}

// 聚合组的倍率要对**成员组提供的模型**同样生效。曾经这里只查自有渠道，
// 从成员组来的模型会掉回默认倍率，与预留事务里的报价（永远取绑定组的倍率）打架。
func TestAggregateRateAppliesToMemberModels(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate, RateMilli: 1500, GroupIDs: []int64{member.ID}})
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
	rate, err := repo.KeyModelRateMilli(ctx, key.ID, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1500 {
		t.Fatalf("rate = %d, want 1500（聚合组倍率）", rate)
	}
}

// 组作用域的管理视图要按「实际能用的渠道」过滤：聚合组自己的渠道 + 成员组的
// 渠道。只取 group_providers 会让聚合组的使用者看到「客户端有模型、渠道列表里
// 却没有这个渠道」——同一份权限在两个界面上说法不一致，最难解释。
func TestEffectiveProviderIDsCoversMemberChannels(t *testing.T) {
	ctx := context.Background()
	repo, providerID, _ := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
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
	ids, err := repo.EffectiveProviderIDs(ctx, agg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != providerID {
		t.Fatalf("聚合组有效渠道 = %v, want [%d]", ids, providerID)
	}
	// 成员组停用后它的渠道就不再被这个聚合组用了，视图必须同步消失。
	res, err := repo.UpdateModelGroup(ctx, member.ID, UpdateModelGroup{Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Enabled {
		t.Fatal("成员组应已停用")
	}
	ids, err = repo.EffectiveProviderIDs(ctx, agg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("成员组停用后聚合组有效渠道 = %v, want 空", ids)
	}
	// 自有渠道那条分支不受影响：编辑页仍然只列自己挂的渠道。
	if own, err := repo.GroupProviderIDs(ctx, member.ID); err != nil || len(own) != 1 {
		t.Fatalf("GroupProviderIDs(member) = %v, %v; want 1 个", own, err)
	}
}

// 聚合组的自有渠道：来源组就是聚合组自己。这是 v24 改写 source_group 子查询后
// 的第一条分支（旧写法是 EXISTS(group_providers WHERE group_id=g.id)）。
func TestAggregateOwnChannelIsItsOwnSourceGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate})
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
		t.Fatalf("source_group_id = %d, want %d（自有渠道应记在聚合组自己头上）", sourceGroup, agg.ID)
	}
}
