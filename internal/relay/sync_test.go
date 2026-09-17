package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"civic-ai-relay/internal/store"
)

type fakeCatalog struct {
	imported map[int64][]string
	errs     map[int64]error
	calls    []int64
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{imported: map[int64][]string{}, errs: map[int64]error{}}
}

func (f *fakeCatalog) SyncProvider(_ context.Context, providerID int64) ([]string, error) {
	f.calls = append(f.calls, providerID)
	if err := f.errs[providerID]; err != nil {
		return nil, err
	}
	return f.imported[providerID], nil
}

func TestModelSyncerSkipsDisabledProviders(t *testing.T) {
	repo, _ := monitorFixture(t)
	ctx := context.Background()
	on := addProvider(t, repo, "on", false)
	off := addProvider(t, repo, "off", false)
	setProviderEnabled(t, repo, off.ID, false)

	catalog := newFakeCatalog()
	catalog.imported[on.ID] = []string{"on/new-model"}
	summary, err := NewModelSyncer(repo, catalog).RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Providers != 1 || summary.Imported != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v, want 1 provider / 1 imported / 0 failed", summary)
	}
	if len(catalog.calls) != 1 || catalog.calls[0] != on.ID {
		t.Fatalf("catalog calls = %v, want only the enabled provider %d", catalog.calls, on.ID)
	}
}

// 一个渠道拉不动不应该连累其他渠道：否则一个坏渠道会让后面所有渠道都同步不上。
func TestModelSyncerContinuesAfterAProviderFails(t *testing.T) {
	repo, _ := monitorFixture(t)
	ctx := context.Background()
	broken := addProvider(t, repo, "broken", false)
	healthy := addProvider(t, repo, "healthy", false)

	catalog := newFakeCatalog()
	catalog.errs[broken.ID] = errors.New("upstream down")
	catalog.imported[healthy.ID] = []string{"healthy/a", "healthy/b"}
	summary, err := NewModelSyncer(repo, catalog).RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Providers != 2 || summary.Failed != 1 || summary.Imported != 2 {
		t.Fatalf("summary = %#v, want 2 providers / 1 failed / 2 imported", summary)
	}
	if len(catalog.calls) != 2 {
		t.Fatalf("catalog calls = %v, want both providers attempted", catalog.calls)
	}
}

func TestModelSyncerWithNoProvidersIsANoop(t *testing.T) {
	repo, _ := monitorFixture(t)
	catalog := newFakeCatalog()
	summary, err := NewModelSyncer(repo, catalog).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != (SyncSummary{}) || len(catalog.calls) != 0 {
		t.Fatalf("summary = %#v, calls = %v; want a no-op", summary, catalog.calls)
	}
}

// 没有上游目录能力时（例如测试装配不完整）不应 panic。
func TestModelSyncerWithoutCatalogIsSafe(t *testing.T) {
	repo, _ := monitorFixture(t)
	if _, err := NewModelSyncer(repo, nil).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nilSyncer *ModelSyncer
	if _, err := nilSyncer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 导入进来的模型必须是停用 + 未定价，否则自动同步会绕过定价环节、悄悄改变
// 对外可用的模型列表。这里用真 Registry + 假上游走一遍真实导入路径。
func TestSyncProviderImportsNewModelsDisabledAndUnpriced(t *testing.T) {
	repo, _ := monitorFixture(t)
	ctx := context.Background()
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"alpha"},{"id":"beta"}]}`))
	}))
	defer upstreamServer.Close()

	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "up", BaseURL: upstreamServer.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(repo, time.Second, time.Second, time.Second, time.Second)
	imported, err := registry.SyncProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 {
		t.Fatalf("imported = %v, want both upstream models", imported)
	}
	for _, name := range imported {
		model, err := repo.GetModelByPublicName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if model.Enabled {
			t.Fatalf("auto-synced model %q must stay disabled until priced by an operator", name)
		}
		if model.InputPriceMicroyuan != nil || model.OutputPriceMicroyuan != nil {
			t.Fatalf("auto-synced model %q must not carry a price: %#v", name, model)
		}
	}
	// 幂等：再同步一次不应重复导入
	again, err := registry.SyncProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second sync imported %v, want nothing", again)
	}
}
