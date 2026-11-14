package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// poolStoreFixture 造两个供应商、两个已定价模型，供模型池测试复用。
func poolStoreFixture(t *testing.T) (*Store, int64, int64, int64, int64) {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	ids := make([]int64, 0, 4)
	for i, name := range []string{"provider-a", "provider-b"} {
		provider, err := repo.CreateProvider(ctx, NewProvider{Name: name, BaseURL: "https://" + name + ".example", APIKey: "fake"})
		if err != nil {
			t.Fatal(err)
		}
		price := int64(1000 * (i + 1))
		model, err := repo.CreateModel(ctx, NewModel{
			ProviderID: provider.ID, PublicName: "model-" + string(rune('a'+i)),
			UpstreamName: "upstream-" + name, InputPriceMicroyuan: &price,
			OutputPriceMicroyuan: &price, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, provider.ID, model.ID)
	}
	return repo, ids[0], ids[2], ids[1], ids[3]
}

func TestModelPoolTablesMigrated(t *testing.T) {
	repo, _, _, _, _ := poolStoreFixture(t)
	ctx := context.Background()
	for _, table := range []string{"model_pools", "model_pool_members"} {
		var count int
		if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing table %s", table)
		}
	}
	var applied int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=19`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatal("migration v19 was not recorded")
	}
}

// 生产库停在 v18：模拟「只有前 18 条迁移的旧库启动时补上 v19」，
// 确认是纯新增、不动既有数据，且补完后池功能立刻可用。
func TestModelPoolMigrationUpgradesExistingV18Database(t *testing.T) {
	repo, _, _, modelA, _ := poolStoreFixture(t)
	ctx := context.Background()
	for _, statement := range []string{
		`DROP TABLE model_pool_members`,
		`DROP TABLE model_pools`,
		`DELETE FROM schema_migrations WHERE version=19`,
	} {
		if _, err := repo.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.applySchema(ctx); err != nil {
		t.Fatalf("v18 -> v19 upgrade failed: %v", err)
	}
	var models int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM models WHERE id=?`, modelA).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if models != 1 {
		t.Fatal("existing model data was lost during upgrade")
	}
	if _, err := repo.CreateModelPool(ctx, NewModelPool{Name: "upgraded-pool", Members: []PoolMemberInput{{ModelID: modelA, Enabled: true}}}); err != nil {
		t.Fatalf("pools unusable after upgrade: %v", err)
	}
}

func TestModelPoolCRUDAndMemberOrder(t *testing.T) {
	repo, _, _, modelA, modelB := poolStoreFixture(t)
	ctx := context.Background()
	created, err := repo.CreateModelPool(ctx, NewModelPool{Name: "pool-a", Members: []PoolMemberInput{
		{ModelID: modelA, Enabled: true}, {ModelID: modelB, Enabled: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Enabled || len(created.Members) != 2 {
		t.Fatalf("unexpected created pool: %+v", created)
	}
	if created.Members[0].ModelID != modelA || created.Members[0].SortOrder != 0 || !created.Members[0].Enabled {
		t.Fatalf("first member not in priority order: %+v", created.Members[0])
	}
	if created.Members[1].ModelID != modelB || created.Members[1].Enabled {
		t.Fatalf("second member state wrong: %+v", created.Members[1])
	}
	candidates, err := repo.PoolCandidateIDs(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0] != modelA {
		t.Fatalf("disabled member must not be a candidate: %v", candidates)
	}

	disabled := false
	members := []PoolMemberInput{{ModelID: modelB, Enabled: true}, {ModelID: modelA, Enabled: true}}
	updated, err := repo.UpdateModelPool(ctx, created.ID, UpdateModelPool{Name: "pool-b", Enabled: &disabled, Members: &members})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "pool-b" || updated.Enabled || len(updated.Members) != 2 || updated.Members[0].ModelID != modelB {
		t.Fatalf("update did not apply: %+v", updated)
	}
	candidates, err = repo.PoolCandidateIDs(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("disabled pool must have no candidates, got %v", candidates)
	}
	// 空成员数组 = 显式清空，而不是「不动」。
	empty := []PoolMemberInput{}
	cleared, err := repo.UpdateModelPool(ctx, created.ID, UpdateModelPool{Members: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Members) != 0 {
		t.Fatalf("members not cleared: %+v", cleared.Members)
	}

	listed, err := repo.ListModelPools(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %+v, err=%v", listed, err)
	}
	if err := repo.DeleteModelPool(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteModelPool(ctx, created.ID); err != sql.ErrNoRows {
		t.Fatalf("second delete = %v, want sql.ErrNoRows", err)
	}
}

func TestModelPoolNameCollision(t *testing.T) {
	repo, providerA, _, modelA, _ := poolStoreFixture(t)
	ctx := context.Background()
	if _, err := repo.CreateModelPool(ctx, NewModelPool{Name: "model-a"}); err == nil {
		t.Fatal("pool name colliding with an existing model must be rejected")
	}
	if _, err := repo.CreateModelPool(ctx, NewModelPool{Name: "dup", Members: []PoolMemberInput{{ModelID: modelA, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModelPool(ctx, NewModelPool{Name: "dup"}); err == nil {
		t.Fatal("duplicate pool name must be rejected")
	}
	price := int64(1)
	if _, err := repo.CreateModel(ctx, NewModel{ProviderID: providerA, PublicName: "dup", UpstreamName: "up", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price}); err == nil {
		t.Fatal("model name colliding with a pool must be rejected")
	}
}

func TestModelPoolAuthorizedEntriesAndCascade(t *testing.T) {
	repo, providerA, providerB, modelA, modelB := poolStoreFixture(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerA, providerB}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateModelPool(ctx, NewModelPool{Name: "pool-model", Members: []PoolMemberInput{
		{ModelID: modelA, Enabled: true}, {ModelID: modelB, Enabled: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// v27：池必须被本组包含才对外可见（不再靠「成员在授权集合里」推断）。
	if err := repo.ReplaceGroupPools(ctx, group.ID, []int64{pool.ID}); err != nil {
		t.Fatal(err)
	}
	authorized, err := repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 3 {
		t.Fatalf("want 2 real models + 1 pool entry, got %d: %+v", len(authorized), authorized)
	}
	virtual := authorized[2]
	if virtual.PoolID != pool.ID || virtual.ID >= 0 || virtual.PublicName != "pool-model" {
		t.Fatalf("virtual pool entry is wrong: %+v", virtual)
	}
	// 池里挂了不属于该分组渠道的模型时，虚拟条目仍然成立，但候选必须被裁掉。
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerB}); err != nil {
		t.Fatal(err)
	}
	authorized, err = repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 2 || authorized[1].PoolID != pool.ID {
		t.Fatalf("scope change not reflected: %+v", authorized)
	}
	candidates, err := repo.PoolCandidateIDs(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	allowed := make(map[int64]bool)
	for _, m := range authorized {
		if m.ID > 0 {
			allowed[m.ID] = true
		}
	}
	if len(candidates) != 2 || !allowed[modelB] || allowed[modelA] {
		t.Fatalf("unexpected candidate/authorized sets: candidates=%v allowed=%v", candidates, allowed)
	}
	// 停用的池不能出现在 /v1/models。
	disabled := false
	if _, err := repo.UpdateModelPool(ctx, pool.ID, UpdateModelPool{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	authorized, err = repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 1 {
		t.Fatalf("disabled pool must be hidden, got %+v", authorized)
	}
	// 删除模型只清掉成员资格，池本身与历史账目不受影响。
	if err := repo.DeleteModel(ctx, modelA); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetModelPool(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Members) != 1 || after.Members[0].ModelID != modelB {
		t.Fatalf("member cascade failed: %+v", after.Members)
	}
}
