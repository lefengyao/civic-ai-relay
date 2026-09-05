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
