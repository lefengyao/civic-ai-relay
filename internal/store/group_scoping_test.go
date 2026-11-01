package store

import (
	"context"
	"strings"
	"testing"
)

func setupGroupScope(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	model, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return repo, provider.ID, model.ID
}

// 未绑分组的密钥没有可授权的模型，预留必须被拒——与旧版「无 key_groups 行 →
// 计价查询空」走同一个错误出口。
func TestReserveRejectsUnboundKey(t *testing.T) {
	repo, _, modelID := setupGroupScope(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, modelID, "unbound", 600))
	if err == nil || !strings.Contains(err.Error(), "model is not authorized for key") {
		t.Fatalf("err = %v", err)
	}
}

// 切换分组不能搬历史：账目行的 group_id 记录请求发生时的归属。
func TestSwitchingGroupDoesNotMoveHistory(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-a", 600)); err != nil {
		t.Fatal(err)
	}
	var inA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&inA); err != nil {
		t.Fatal(err)
	}
	if inA != 1 {
		t.Fatalf("A 组预留数 = %d", inA)
	}

	if err := repo.SetKeyGroup(ctx, key.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	// 历史行不动
	var stillA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&stillA); err != nil {
		t.Fatal(err)
	}
	if stillA != 1 {
		t.Fatalf("切换分组搬动了历史：A 组行数变成 %d", stillA)
	}
	// 新请求记到新组
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-b", 600)); err != nil {
		t.Fatal(err)
	}
	var inB int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupB.ID).Scan(&inB); err != nil {
		t.Fatal(err)
	}
	if inB != 1 {
		t.Fatalf("B 组预留数 = %d", inB)
	}
}

// 计价倍率取自唯一所属组（走预留事务里的报价查询，不只是 KeyModelRateMilli）。
func TestReserveQuotesRateFromSingleGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	premium, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "premium", RateMilli: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, premium.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, premium.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "quoted", 600))
	if err != nil {
		t.Fatal(err)
	}
	if res.QuoteRateMilli != 1500 {
		t.Fatalf("QuoteRateMilli = %d, want 1500", res.QuoteRateMilli)
	}
}
