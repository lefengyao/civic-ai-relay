package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func TestSyncImportsUnknownModelsAsDisabledUnpriced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "chat"}, {"id": "reasoner"}}})
	}))
	defer server.Close()
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	p, err := repo.CreateProvider(context.Background(), store.NewProvider{Name: "p", BaseURL: server.URL, APIKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := NewRegistry(repo, 0, 0, 0, 0).SyncProvider(context.Background(), p.ID)
	if err != nil || len(imported) != 2 {
		t.Fatalf("imported=%v err=%v", imported, err)
	}
	models, err := repo.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Enabled || models[0].InputPriceMicroyuan != nil {
		t.Fatalf("models=%#v", models)
	}
}
