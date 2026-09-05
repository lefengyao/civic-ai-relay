package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func TestModelsRequiresBearerAndFiltersAuthorizedModels(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	p, _ := repo.CreateProvider(t.Context(), store.NewProvider{Name: "p", BaseURL: "https://p.example", APIKey: "s"})
	price := int64(1)
	m, _ := repo.CreateModel(t.Context(), store.NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	g, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupModels(t.Context(), g.ID, []int64{m.ID})
	key, _ := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "k", ConcurrencyLimit: 1})
	_ = repo.ReplaceKeyGroups(t.Context(), key.ID, []int64{g.ID})
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, TokenLimit5H: 100000, TokenLimitDaily: 20000, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	h := NewPublicHandler(service, 1<<20)
	unauthenticated := httptest.NewRecorder()
	h.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatal(unauthenticated.Code)
	}
	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key.Token)
	h.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatal(authorized.Code)
	}
}

type testFactory struct{}

func (*testFactory) ForProvider(_ context.Context, _ int64) (relay.UpstreamClient, error) {
	return nil, nil
}
