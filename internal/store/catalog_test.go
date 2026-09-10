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

func TestGroupRejectsDisabledOrUnpricedModelsAndAuthorizationUsesUnion(t *testing.T) {
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(context.Background(), NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	basic, err := repo.CreateModel(context.Background(), NewModel{ProviderID: provider.ID, PublicName: "basic", UpstreamName: "basic", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := repo.CreateModel(context.Background(), NewModel{ProviderID: provider.ID, PublicName: "advanced", UpstreamName: "advanced", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	unpriced, err := repo.CreateModel(context.Background(), NewModel{ProviderID: provider.ID, PublicName: "unpriced", UpstreamName: "unpriced", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	groupA, _ := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "basic-group"})
	groupB, _ := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "advanced-group"})
	if err := repo.ReplaceGroupModels(context.Background(), groupA.ID, []int64{basic.ID, unpriced.ID}); err == nil {
		t.Fatal("unpriced model accepted")
	}
	if err := repo.ReplaceGroupModels(context.Background(), groupA.ID, []int64{basic.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupModels(context.Background(), groupB.ID, []int64{advanced.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "alice", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(context.Background(), key.ID, []int64{groupA.ID, groupB.ID}); err != nil {
		t.Fatal(err)
	}
	models, err := repo.AuthorizedModels(context.Background(), key.Token)
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
}

func TestAtomicJoinReplacementOnlyChangesPassedRows(t *testing.T) {
	repo := newTestStore(t)
	p, _ := repo.CreateProvider(context.Background(), NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "s"})
	price := int64(1)
	m1, _ := repo.CreateModel(context.Background(), NewModel{ProviderID: p.ID, PublicName: "m1", UpstreamName: "m1", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	m2, _ := repo.CreateModel(context.Background(), NewModel{ProviderID: p.ID, PublicName: "m2", UpstreamName: "m2", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	g, _ := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "g"})
	if err := repo.ReplaceGroupModels(context.Background(), g.ID, []int64{m1.ID, m2.ID}); err != nil {
		t.Fatal(err)
	}
	key, _ := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err := repo.ReplaceKeyGroups(context.Background(), key.ID, []int64{g.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(context.Background(), key.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	models, err := repo.AuthorizedModels(context.Background(), key.Token)
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
		if err := repo.ReplaceGroupModels(ctx, gid, []int64{m.ID}); err != nil {
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
	// 单个 key 同时属于两个分组：取最大倍率
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(ctx, key.ID, []int64{cheap.ID, premium.ID}); err != nil {
		t.Fatal(err)
	}
	rate, err := repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1500 {
		t.Fatalf("rate = %d, want 1500", rate)
	}
	// 禁用分组不参与倍率
	if _, err := repo.UpdateModelGroup(ctx, premium.ID, UpdateModelGroup{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	rate, err = repo.KeyModelRateMilli(ctx, key.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 800 {
		t.Fatalf("rate after disable = %d, want 800", rate)
	}
	// 无分组的模型回落默认倍率
	other, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m2", UpstreamName: "m2", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
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

func intPtr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool  { return &v }
