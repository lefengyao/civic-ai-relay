package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 池是**模型级**成员（v27），对外只有一个名字（v0.14.1）：组包含一个池之后，
// 客户端在 /v1/models 里只看到池名。池成员模型既不单列、也不能按自己的名字调用，
// 同一渠道下没被池选中的模型同样进不来 —— 那正是 v0.13.3「勾池 = 勾渠道」的老毛病。
func TestGroupPoolExposesOnlyPoolName(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	price := int64(100)
	other, err := repo.CreateModel(ctx, NewModel{
		ProviderID: providerID, PublicName: "m-unlisted", UpstreamName: "m-unlisted",
		InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateModelPool(ctx, NewModelPool{Name: "p1", Members: []PoolMemberInput{{ModelID: modelID, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	// 组一个渠道都不挂，只包含池。
	if err := repo.ReplaceGroupPools(ctx, agg.ID, []int64{pool.ID}); err != nil {
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
	if len(models) != 1 || models[0].PoolID != pool.ID || models[0].PublicName != pool.Name {
		t.Fatalf("组只包含一个池时，对外应该只有池名一个条目，实际：%#v", models)
	}
	for _, m := range models {
		if m.ID == modelID || m.ID == other.ID {
			t.Fatalf("池成员/池外模型不应该作为独立模型对外：%#v", models)
		}
	}
	// 成员仍然是池的内部候选：Begin 靠它把池名解析成真实模型。
	members, err := repo.PoolUsableModels(ctx, pool.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != modelID {
		t.Fatalf("池候选应该只有被勾选的那个成员，实际：%#v", members)
	}
}

// 池必须真的被这个 Key 的组包含才对外可见：全局所有启用池都列出来，等于
// 把别组的池名泄漏给客户端，客户端调过去还会 404（v0.14.1 之前就是这么算的）。
func TestGroupPoolScopeIsPerGroup(t *testing.T) {
	ctx := context.Background()
	repo, _, modelID := setupGroupScope(t)
	attached, err := repo.CreateModelPool(ctx, NewModelPool{Name: "attached", Members: []PoolMemberInput{{ModelID: modelID, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModelPool(ctx, NewModelPool{Name: "other-group", Members: []PoolMemberInput{{ModelID: modelID, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupPools(ctx, agg.ID, []int64{attached.ID}); err != nil {
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
	if len(models) != 1 || models[0].PublicName != "attached" {
		t.Fatalf("只应看到本组包含的池，实际：%#v", models)
	}
}

// 池成员渠道被别的组独占也不影响：池不改渠道归属，只是把模型借给本组。
func TestGroupPoolDoesNotTouchProviderOwnership(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	owner, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "owner", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, owner.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateModelPool(ctx, NewModelPool{Name: "p1", Members: []PoolMemberInput{{ModelID: modelID, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupPools(ctx, agg.ID, []int64{pool.ID}); err != nil {
		t.Fatalf("渠道已被成员组占用时，池仍然应该可以挂到聚合组：%v", err)
	}
	ids, err := repo.GroupProviderIDs(ctx, agg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("池不应该写入 group_providers，实际 %v", ids)
	}
	stored, err := repo.GetProvider(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GroupID != owner.ID {
		t.Fatalf("渠道归属被池改动了：got %d want %d", stored.GroupID, owner.ID)
	}
}

// 只有聚合组能包含池；成员组不对外，挂上去没有任何入口。停用的池也要挡住。
func TestReplaceGroupPoolsValidates(t *testing.T) {
	ctx := context.Background()
	repo, _, modelID := setupGroupScope(t)
	pool, err := repo.CreateModelPool(ctx, NewModelPool{Name: "p1", Members: []PoolMemberInput{{ModelID: modelID, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "member", Kind: GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	err = repo.ReplaceGroupPools(ctx, member.ID, []int64{pool.ID})
	if err == nil || !strings.Contains(err.Error(), "聚合") {
		t.Fatalf("成员组包含池应被拒绝，got %v", err)
	}
	agg, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "agg", Kind: GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := repo.UpdateModelPool(ctx, pool.ID, UpdateModelPool{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupPools(ctx, agg.ID, []int64{pool.ID}); err == nil {
		t.Fatal("停用的池不应该能挂进组")
	}
	if err := repo.ReplaceGroupPools(ctx, agg.ID, []int64{99999}); err == nil {
		t.Fatal("不存在的池应该报错")
	}
	// 清空是合法的：组可以退化成只靠渠道/成员组。
	if err := repo.ReplaceGroupPools(ctx, agg.ID, nil); err != nil {
		t.Fatalf("清空池失败：%v", err)
	}
}
