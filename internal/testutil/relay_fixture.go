package testutil

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

type RelayFixture struct {
	Service *relay.Service
	Store   *store.Store
	Key     store.ClientKey
}

type fixtureFactory struct {
	client relay.UpstreamClient
}

func (f fixtureFactory) ForProvider(context.Context, int64) (relay.UpstreamClient, error) {
	return f.client, nil
}

func NewRelayFixture(t *testing.T, client relay.UpstreamClient) RelayFixture {
	t.Helper()
	box, err := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{
		Name:    "fixture",
		BaseURL: "https://fixture.example",
		APIKey:  "fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	if _, err := repo.CreateModel(ctx, store.NewModel{
		ProviderID:           provider.ID,
		PublicName:           "m",
		UpstreamName:         "up-m",
		Enabled:              true,
		InputPriceMicroyuan:  &price,
		OutputPriceMicroyuan: &price,
	}); err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, MaxOutputTokens: 64}
	service := relay.NewService(repo, fixtureFactory{client: client}, func() config.Settings { return settings })
	return RelayFixture{Service: service, Store: repo, Key: key}
}
