package store

import (
	"context"
	"strings"
	"testing"
)

func TestNewKeyIsReturnedOnceAndStoredAsDigest(t *testing.T) {
	repo := newTestStore(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "alice", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Token, "crk_") {
		t.Fatalf("token = %q", key.Token)
	}
	rows, err := repo.ListClientKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join([]string{rows[0].Token, rows[0].Name}, " "), key.Token) || rows[0].Token != "" {
		t.Fatal("plaintext token persisted or listed")
	}
	var digest []byte
	if err := repo.db.QueryRow("SELECT token_digest FROM client_keys WHERE id = ?", key.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if string(digest) == key.Token || len(digest) == 0 {
		t.Fatal("invalid token digest")
	}
}

func TestSettlementDisablesKeyAtTokenOrAmountLimitAndIsIdempotent(t *testing.T) {
	repo := newTestStore(t)
	p, _ := repo.CreateProvider(context.Background(), NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "s"})
	price := int64(100)
	m, _ := repo.CreateModel(context.Background(), NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	g, _ := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupModels(context.Background(), g.ID, []int64{m.ID})
	key, _ := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1, TokenLimit: ptrInt64(10), AmountLimitMicroyuan: ptrInt64(1000)})
	_ = repo.ReplaceKeyGroups(context.Background(), key.ID, []int64{g.ID})
	r, err := repo.ReserveForKey(context.Background(), key.ID, m.ID, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleKey(context.Background(), r.ID, 10, 1000, "completed"); err != nil {
		t.Fatal(err)
	}
	current, _ := repo.GetClientKey(context.Background(), key.ID)
	if current.Enabled || current.DisabledReason != "quota_exhausted" {
		t.Fatalf("key remains active: %#v", current)
	}
	if err := repo.SettleKey(context.Background(), r.ID, 10, 1000, "completed"); err == nil {
		t.Fatal("double settlement accepted")
	}
}

func ptrInt64(v int64) *int64 { return &v }
