package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestProviderKeyIsEncryptedAndNeverListed(t *testing.T) {
	repo := newTestStore(t)
	created, err := repo.CreateProvider(context.Background(), NewProvider{Name: "A", BaseURL: "https://a.example", APIKey: "secret-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.APIKeyConfigured || created.Name != "A" {
		t.Fatalf("created = %#v", created)
	}
	listed, err := repo.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(listed), "secret-a") {
		t.Fatal("provider secret leaked")
	}
	var ciphertext string
	if err := repo.db.QueryRow("SELECT api_key_ciphertext FROM providers WHERE id = ?", created.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "secret-a" || ciphertext == "" {
		t.Fatalf("ciphertext = %q", ciphertext)
	}
}

func TestProviderProtocolDefaultsAndCanBeChanged(t *testing.T) {
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(context.Background(), NewProvider{Name: "default", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil || provider.Protocol != "openai" {
		t.Fatalf("created provider = %#v err=%v", provider, err)
	}
	updated, err := repo.UpdateProvider(context.Background(), provider.ID, UpdateProvider{Protocol: "anthropic"})
	if err != nil || updated.Protocol != "anthropic" {
		t.Fatalf("updated provider = %#v err=%v", updated, err)
	}
	if _, err := repo.UpdateProvider(context.Background(), provider.ID, UpdateProvider{Protocol: "unknown"}); err == nil {
		t.Fatal("unknown provider protocol accepted")
	}
}

func TestProviderURLValidationAndUpdatePreservesOrRotatesSecret(t *testing.T) {
	repo := newTestStore(t)
	if _, err := repo.CreateProvider(context.Background(), NewProvider{Name: "bad", BaseURL: "http://remote.example", APIKey: "x"}); err == nil {
		t.Fatal("remote HTTP URL accepted")
	}
	p, err := repo.CreateProvider(context.Background(), NewProvider{Name: "local", BaseURL: "http://localhost:1234", APIKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := repo.db.QueryRow("SELECT api_key_ciphertext FROM providers WHERE id = ?", p.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateProvider(context.Background(), p.ID, UpdateProvider{Name: "local-2", BaseURL: "http://127.0.0.1:1234"}); err != nil {
		t.Fatal(err)
	}
	var preserved string
	if err := repo.db.QueryRow("SELECT api_key_ciphertext FROM providers WHERE id = ?", p.ID).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if preserved != before {
		t.Fatal("empty API key unexpectedly rotated")
	}
	if _, err := repo.UpdateProvider(context.Background(), p.ID, UpdateProvider{APIKey: "second"}); err != nil {
		t.Fatal(err)
	}
	var rotated string
	if err := repo.db.QueryRow("SELECT api_key_ciphertext FROM providers WHERE id = ?", p.ID).Scan(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated == before {
		t.Fatal("API key rotation did not change ciphertext")
	}
}

func TestAuthorizedModelsFollowSingleGroup(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "main", BaseURL: "https://main.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	off, err := repo.CreateProvider(ctx, NewProvider{Name: "off", BaseURL: "https://off.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateProvider(ctx, off.ID, UpdateProvider{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "basic", UpstreamName: "basic", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "advanced", UpstreamName: "advanced", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "unpriced", UpstreamName: "unpriced", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	groupA, _ := repo.CreateModelGroup(ctx, NewModelGroup{Name: "main"})
	groupB, _ := repo.CreateModelGroup(ctx, NewModelGroup{Name: "spare"})
	if err := repo.ReplaceGroupProviders(ctx, groupA.ID, []int64{off.ID}); err == nil {
		t.Fatal("disabled provider accepted")
	}
	if err := repo.ReplaceGroupProviders(ctx, groupA.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupB.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "alice", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 未绑分组：授权模型为空
	if models, err := repo.AuthorizedModels(ctx, key.Token); err != nil || len(models) != 0 {
		t.Fatalf("unbound models = %#v, err = %v", models, err)
	}
	// 授权模型 = 唯一所属组内渠道下的已定价启用模型；未定价模型不授权
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	models, err := repo.AuthorizedModels(ctx, key.Token)
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
	// 切换分组：授权模型随之变化（组 B 换成挂一个独有渠道来区分）
	providerB, err := repo.CreateProvider(ctx, NewProvider{Name: "only-b", BaseURL: "https://b.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateModel(ctx, NewModel{ProviderID: providerB.ID, PublicName: "b-only", UpstreamName: "b-only", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, groupB.ID, []int64{providerB.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	models, err = repo.AuthorizedModels(ctx, key.Token)
	if err != nil || len(models) != 1 || models[0].PublicName != "b-only" {
		t.Fatalf("after switch models = %#v, err = %v", models, err)
	}
}

func TestAtomicJoinReplacementOnlyChangesPassedRows(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	p, _ := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "s"})
	price := int64(1)
	if _, err := repo.CreateModel(ctx, NewModel{ProviderID: p.ID, PublicName: "m1", UpstreamName: "m1", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModel(ctx, NewModel{ProviderID: p.ID, PublicName: "m2", UpstreamName: "m2", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	g, _ := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "g"})
	if err := repo.ReplaceGroupProviders(context.Background(), g.ID, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	key, _ := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err := repo.SetKeyGroup(context.Background(), key.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	models, err := repo.AuthorizedModels(context.Background(), key.Token)
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, 0); err != nil {
		t.Fatal(err)
	}
	models, err = repo.AuthorizedModels(context.Background(), key.Token)
	if err != nil || len(models) != 0 {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
}

func TestGroupRateMultiplierPersistenceAndKeyModelRate(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	m, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cheap, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "cheap", Enabled: true, RateMilli: 800})
	if err != nil {
		t.Fatal(err)
	}
	premium, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "premium", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if cheap.RateMilli != 800 || premium.RateMilli != DefaultGroupRateMilli {
		t.Fatalf("rates = %d, %d", cheap.RateMilli, premium.RateMilli)
	}
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "bad", RateMilli: -1}); err == nil {
		t.Fatal("negative rate accepted")
	}
	for _, gid := range []int64{cheap.ID, premium.ID} {
		if err := repo.ReplaceGroupProviders(ctx, gid, []int64{provider.ID}); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := repo.UpdateModelGroup(ctx, premium.ID, UpdateModelGroup{RateMilli: intPtr(1500)})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RateMilli != 1500 {
		t.Fatalf("updated rate = %d", updated.RateMilli)
	}
	// 倍率取自唯一所属组
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, premium.ID); err != nil {
		t.Fatal(err)
	}
	rate, err := repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1500 {
		t.Fatalf("rate = %d, want 1500", rate)
	}
	// 所属组停用：没有启用的组可授权，回落默认倍率
	if _, err := repo.UpdateModelGroup(ctx, premium.ID, UpdateModelGroup{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != DefaultGroupRateMilli {
		t.Fatalf("rate after disable = %d, want %d", rate, DefaultGroupRateMilli)
	}
	// 改绑到 cheap：倍率跟随唯一所属组
	if err := repo.SetKeyGroup(ctx, key.ID, cheap.ID); err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 800 {
		t.Fatalf("rate after rebind = %d, want 800", rate)
	}
	// 不属于任何组的渠道：其模型回落默认倍率
	otherProvider, err := repo.CreateProvider(ctx, NewProvider{Name: "p2", BaseURL: "http://localhost:2", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := repo.CreateModel(ctx, NewModel{ProviderID: otherProvider.ID, PublicName: "m2", UpstreamName: "m2", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != DefaultGroupRateMilli {
		t.Fatalf("fallback rate = %d", rate)
	}
}

func intPtr(v int64) *int64   { return &v }
func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
func daysPtr(v int) *int      { return &v }

// 分组额度持久化：nil = 保持原值，0 = 不限，负数拒绝；周期起点自动填充。
func TestGroupQuotaPersistenceAndValidation(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	g, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{TokenLimit5H: intPtr(-1)}); err == nil {
		t.Fatal("负数额度被接受")
	}
	updated, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{
		TokenLimit5H:    intPtr(5000),
		AmountLimit5H:   intPtr(1_000_000),
		PeriodLimitDays: daysPtr(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.TokenLimit5H == nil || *updated.TokenLimit5H != 5000 {
		t.Fatalf("token_limit_5h = %v", updated.TokenLimit5H)
	}
	if updated.PeriodLimitStart == nil || *updated.PeriodLimitStart == "" {
		t.Fatalf("days>0 且起点为空时应自动填 now，实际 %v", updated.PeriodLimitStart)
	}
	// 未提及的字段保持原值（nil = 不动）
	touched, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{TokenLimit5H: intPtr(6000)})
	if err != nil {
		t.Fatal(err)
	}
	if *touched.TokenLimit5H != 6000 || touched.AmountLimit5H == nil || *touched.AmountLimit5H != 1_000_000 {
		t.Fatalf("未提及的额度被误改：5h=%v 金额5h=%v", touched.TokenLimit5H, touched.AmountLimit5H)
	}
	// 0 = 显式设为不限；周期天数归零时起点一并清除
	cleared, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{
		TokenLimit5H:    intPtr(0),
		PeriodLimitDays: daysPtr(0),
		PeriodStart:     strPtr(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.TokenLimit5H == nil || *cleared.TokenLimit5H != 0 {
		t.Fatalf("0 应表示不限，实际 %v", cleared.TokenLimit5H)
	}
	if cleared.PeriodLimitStart != nil && *cleared.PeriodLimitStart != "" {
		t.Fatalf("days=0 应清除起点，实际 %v", cleared.PeriodLimitStart)
	}
	// 列表能读回
	listed, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *ModelGroup
	for i := range listed {
		if listed[i].ID == g.ID {
			found = &listed[i]
		}
	}
	if found == nil || found.TokenLimit5H == nil || *found.TokenLimit5H != 0 {
		t.Fatalf("列表未读回额度：%#v", found)
	}
}
