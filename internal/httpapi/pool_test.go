package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/testutil"
)

func poolAdminFixture(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	raw := make([]byte, 32)
	box, err := secret.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const adminKey = "admin-key"
	h := NewAdminHandler(repo, nil, config.Settings{AdminAPIKey: adminKey}, adminKey)
	return h, repo, adminKey
}

func poolTestModel(t *testing.T, repo *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "p", BaseURL: "https://p.example", APIKey: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	model, err := repo.CreateModel(ctx, store.NewModel{
		ProviderID: provider.ID, PublicName: "model-x", UpstreamName: "up-x",
		InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.ID
}

func poolIDFromResponse(t *testing.T, body string) int64 {
	t.Helper()
	var payload struct {
		Data struct {
			ID float64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.ID <= 0 {
		t.Fatalf("no pool ID in response: %s", body)
	}
	return int64(payload.Data.ID)
}

func TestAdminPoolsCRUD(t *testing.T) {
	handler, repo, adminKey := poolAdminFixture(t)
	modelID := poolTestModel(t, repo)
	created := adminRequest(t, handler, http.MethodPost, "/admin/api/pools", adminKey, map[string]any{
		"name": "pool-x", "members": []map[string]any{{"model_id": modelID, "enabled": true}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	id := poolIDFromResponse(t, created.Body.String())
	list := adminRequest(t, handler, http.MethodGet, "/admin/api/pools", adminKey, nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "pool-x") || !strings.Contains(list.Body.String(), "model-x") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	updated := adminRequest(t, handler, http.MethodPut, "/admin/api/pools/"+strconv.FormatInt(id, 10), adminKey, map[string]any{
		"name": "pool-y", "enabled": false,
	})
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), "pool-y") {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	// PUT 不带 members 时成员不能被清空。
	if !strings.Contains(updated.Body.String(), "model-x") {
		t.Fatalf("members were dropped by a metadata-only update: %s", updated.Body.String())
	}
	one := adminRequest(t, handler, http.MethodGet, "/admin/api/pools/"+strconv.FormatInt(id, 10), adminKey, nil)
	if one.Code != http.StatusOK || !strings.Contains(one.Body.String(), "pool-y") {
		t.Fatalf("get status=%d body=%s", one.Code, one.Body.String())
	}
	removed := adminRequest(t, handler, http.MethodDelete, "/admin/api/pools/"+strconv.FormatInt(id, 10), adminKey, nil)
	if removed.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", removed.Code, removed.Body.String())
	}
	again := adminRequest(t, handler, http.MethodDelete, "/admin/api/pools/"+strconv.FormatInt(id, 10), adminKey, nil)
	if again.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d, want 404", again.Code)
	}
}

func TestAdminPoolsRejectNameCollision(t *testing.T) {
	handler, repo, adminKey := poolAdminFixture(t)
	poolTestModel(t, repo)
	response := adminRequest(t, handler, http.MethodPost, "/admin/api/pools", adminKey, map[string]any{"name": "model-x"})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "pool_invalid") {
		t.Fatalf("collision status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminPoolsRequireAdmin(t *testing.T) {
	handler, repo, adminKey := poolAdminFixture(t)
	ctx := context.Background()
	modelID := poolTestModel(t, repo)
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := repo.ListProviders(ctx)
	if err != nil || len(provider) != 1 {
		t.Fatalf("providers=%+v err=%v", provider, err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider[0].ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModelPool(ctx, store.NewModelPool{Name: "pool-x", Members: []store.PoolMemberInput{{ModelID: modelID, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	anonymous := adminRequest(t, handler, http.MethodGet, "/admin/api/pools", "", nil)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", anonymous.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/pools", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("key subject must not see pool config, status=%d body=%s", rec.Code, rec.Body.String())
	}
	asAdmin := adminRequest(t, handler, http.MethodGet, "/admin/api/pools", adminKey, nil)
	if asAdmin.Code != http.StatusOK {
		t.Fatalf("admin status=%d", asAdmin.Code)
	}
}

type noopPoolClient struct{}

func (noopPoolClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return nil, nil
}
func (noopPoolClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	return nil, nil
}

func TestPublicModelsIncludePoolName(t *testing.T) {
	fixture := testutil.NewRelayFixture(t, noopPoolClient{})
	models, err := fixture.Store.ListModels(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if _, err := fixture.Store.CreateModelPool(context.Background(), store.NewModelPool{
		Name: "pool-x", Members: []store.PoolMemberInput{{ModelID: models[0].ID, Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewPublicHandler(fixture.Service, 1<<20)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.Key.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pool-x") || !strings.Contains(rec.Body.String(), `"id":"m"`) {
		t.Fatalf("/v1/models must list both the real model and the pool: %s", rec.Body.String())
	}
}
