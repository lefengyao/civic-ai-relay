package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestDeletesCascade verifies that removing a resource cleans up its join rows
// while historical billing rows survive with their amount intact.
func TestDeletesCascade(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)

	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	model, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupModels(ctx, group.ID, []int64{model.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID}); err != nil {
		t.Fatal(err)
	}

	// A settled request, as the ledger keeps it after the key is gone.
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES (?,?,?,?,?,?,?,?,?)`,
		"req-1", key.ID, model.ID, provider.ID, "succeeded", 10, 20, 30, nowUTC()); err != nil {
		t.Fatal(err)
	}

	// Deleting the model drops it from the group but keeps the group itself.
	if err := repo.DeleteModel(ctx, model.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.GroupModelIDs(ctx, group.ID); err != nil {
		t.Fatal(err)
	} else if len(ids) != 0 {
		t.Fatalf("group members after model delete = %v", ids)
	}
	if groups, err := repo.ListModelGroups(ctx); err != nil {
		t.Fatal(err)
	} else if len(groups) != 1 {
		t.Fatalf("group should survive model delete, groups = %v", groups)
	}

	// Deleting the group drops the key's grant but keeps the key.
	if err := repo.DeleteModelGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.KeyGroupIDs(ctx, key.ID); err != nil {
		t.Fatal(err)
	} else if len(ids) != 0 {
		t.Fatalf("key grants after group delete = %v", ids)
	}
	if _, err := repo.GetClientKey(ctx, key.ID); err != nil {
		t.Fatalf("key should survive group delete: %v", err)
	}

	// Deleting the key keeps the ledger row and its amount.
	if err := repo.DeleteClientKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	var orphanKey sql.NullInt64
	var amount int64
	if err := repo.db.QueryRowContext(ctx, `SELECT key_id, amount_microyuan FROM requests WHERE request_id='req-1'`).Scan(&orphanKey, &amount); err != nil {
		t.Fatalf("ledger row vanished: %v", err)
	}
	if amount != 30 {
		t.Fatalf("amount = %d, want 30", amount)
	}
	if orphanKey.Valid {
		t.Fatalf("key_id should be NULL after delete, got %d", orphanKey.Int64)
	}

	// Deleting the provider works; unknown IDs report ErrNoRows.
	if err := repo.DeleteProvider(ctx, provider.ID); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"provider": repo.DeleteProvider(ctx, 9999),
		"model":    repo.DeleteModel(ctx, 9999),
		"group":    repo.DeleteModelGroup(ctx, 9999),
		"key":      repo.DeleteClientKey(ctx, 9999),
	} {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s delete of missing id = %v, want ErrNoRows", name, err)
		}
	}
	for name, err := range map[string]error{
		"provider": repo.DeleteProvider(ctx, 0),
		"model":    repo.DeleteModel(ctx, -1),
		"group":    repo.DeleteModelGroup(ctx, 0),
		"key":      repo.DeleteClientKey(ctx, -5),
	} {
		if err == nil {
			t.Fatalf("%s delete accepted invalid id", name)
		}
	}
}
