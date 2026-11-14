package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 成员组并发上限（v26）：名额被占满时，第三笔预留必须被拒，而且错误要带够
// 信息让运维知道「是哪个组、在跑几个、上限多少」。
func TestMemberGroupConcurrencyLimitEnforced(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	limit := int64(2)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "夜间池", Kind: GroupKindNormal, ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if member.ConcurrencyLimit == nil || *member.ConcurrencyLimit != limit {
		t.Fatalf("建组返回的并发上限 = %v, want %d", member.ConcurrencyLimit, limit)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "总池", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}

	first, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "conc-1", 600))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "conc-2", 600)); err != nil {
		t.Fatalf("第二个名额应当可用：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "conc-3", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("第三个请求应被并发上限拒绝，得到 %v", err)
	}
	if quota.Code != "group_concurrency_exceeded" {
		t.Fatalf("错误码 = %q, want group_concurrency_exceeded", quota.Code)
	}
	if quota.GroupID != member.ID || quota.Group != "夜间池" {
		t.Fatalf("错误归属 = #%d/%q, want #%d/夜间池", quota.GroupID, quota.Group, member.ID)
	}
	if quota.Used != 2 || quota.Limit != 2 {
		t.Fatalf("错误数字 = %d/%d, want 2/2", quota.Used, quota.Limit)
	}
	// 文案必须说「等前面的结束」，不能套额度那套「欠费/等窗口轮换」——并发是瞬时状态。
	if detail := quota.Detail(); detail == "" || !strings.Contains(detail, "并发已满") {
		t.Fatalf("文案 = %q，应说明并发已满", detail)
	}

	// 结算后名额释放：这正是并发上限与窗口额度的区别。
	if err := repo.SettleRequest(ctx, first.ID, 10, 10, 0, 100, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "conc-4", 600)); err != nil {
		t.Fatalf("结算后应能再占一个名额：%v", err)
	}
}

// 上限按「真正承接请求的组」算：A 组占满不影响 B 组的渠道。
func TestGroupConcurrencyIsCountedPerGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerA, modelA := setupGroupScope(t)
	providerB, modelB := addChannel(t, repo, "channel-b")
	limit := int64(1)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "A 池", Kind: GroupKindNormal, ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "B 池", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupA.ID, []int64{providerA}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupB.ID, []int64{providerB}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "总池", Kind: GroupKindAggregate, GroupIDs: []int64{groupA.ID, groupB.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelA, "per-a1", 600)); err != nil {
		t.Fatal(err)
	}
	// A 组只剩 0 个名额，但请求走的是 B 组渠道。
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelB, "per-b1", 600)); err != nil {
		t.Fatalf("B 组不应被 A 组的并发上限影响：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelA, "per-a2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.GroupID != groupA.ID {
		t.Fatalf("A 组第二个请求应被拒且归属 A，得到 %v", err)
	}
}

// 0 / 未设 = 不限：默认行为必须与 v25 完全一致，否则升级会悄悄限制既有流量。
func TestGroupConcurrencyDefaultsToUnlimited(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "默认池", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if member.ConcurrencyLimit != nil {
		t.Fatalf("未设上限时不该有值：%v", *member.ConcurrencyLimit)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "总池", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, fmt.Sprintf("free-%d", i), 600)); err != nil {
			t.Fatalf("第 %d 个请求不该被拒：%v", i+1, err)
		}
	}

	// 显式设 0 等价于不限，并且落库为 NULL（不留「0 = 不限」的第二种表示）。
	zero := int64(0)
	updated, err := repo.UpdateModelGroup(ctx, member.ID, UpdateModelGroup{ConcurrencyLimit: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ConcurrencyLimit != nil {
		t.Fatalf("填 0 后应归一成 nil，得到 %v", *updated.ConcurrencyLimit)
	}
	var stored any
	if err := repo.db.QueryRowContext(ctx, `SELECT concurrency_limit FROM model_groups WHERE id=?`, member.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("库里应存 NULL，得到 %v", stored)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "free-zero", 600)); err != nil {
		t.Fatalf("填 0 之后仍应不限：%v", err)
	}
}

// 改名等无关更新不能顺手把并发上限清掉（与额度字段同一约定：nil = 保持原值）。
func TestGroupConcurrencySurvivesUnrelatedUpdate(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	limit := int64(3)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "池", Kind: GroupKindNormal, ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{Name: "池-改名"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ConcurrencyLimit == nil || *renamed.ConcurrencyLimit != limit {
		t.Fatalf("改名后并发上限 = %v, want %d", renamed.ConcurrencyLimit, limit)
	}
}

func TestGroupConcurrencyRejectsNegative(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	negative := int64(-1)
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "坏值", Kind: GroupKindNormal, ConcurrencyLimit: &negative}); err == nil {
		t.Fatal("建组时负数应被拒")
	}
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "池", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{ConcurrencyLimit: &negative}); err == nil {
		t.Fatal("改组时负数应被拒")
	}
}

// 组列表要同时给出上限与实时占用，管理台靠它显示「3 / 5」。
func TestListGroupsReportsConcurrencyUsage(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	limit := int64(4)
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "夜间池", Kind: GroupKindNormal, ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "总池", Kind: GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, agg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "usage-1", 600)); err != nil {
		t.Fatal(err)
	}
	groups, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.ID != member.ID {
			continue
		}
		if g.ConcurrencyLimit == nil || *g.ConcurrencyLimit != limit {
			t.Fatalf("列表里的上限 = %v, want %d", g.ConcurrencyLimit, limit)
		}
		if g.ActiveConcurrency != 1 {
			t.Fatalf("在跑 = %d, want 1", g.ActiveConcurrency)
		}
		return
	}
	t.Fatal("列表里没有找到成员组")
}
