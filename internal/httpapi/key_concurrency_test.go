package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

// keyConcurrencyFixture 建一套能真正跑起来的最小环境：一把并发上限为 1 的 Key，
// 以及一个自带渠道的聚合组（成员组并发上限是另一道闸，这里不掺进来）。
func keyConcurrencyFixture(t *testing.T) (*store.Store, store.ClientKey, *store.Model, *store.Provider) {
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
	ctx := t.Context()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "p", BaseURL: "https://up.example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	model, err := repo.CreateModel(ctx, store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", Enabled: true, InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price})
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "总池", Kind: store.GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, aggregate.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "外包A", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, aggregate.ID); err != nil {
		t.Fatal(err)
	}
	return repo, key, &model, &provider
}

// Key 自己也有并发上限，而且它与分组并发是两道独立的闸：这里把分组留成「不限」，
// 只剩 Key 这一道，验证拒绝原因说得清「是哪一把 Key、在跑几个、上限多少」。
// 只回一个 rate_limit_exceeded 的话，客户端和运维分不清是网关满了还是这把 Key 满了。
func TestKeyConcurrencyReturns429WithReadableMessage(t *testing.T) {
	repo, key, model, provider := keyConcurrencyFixture(t)
	ctx := t.Context()
	// 先占掉唯一的名额（模拟一个正在跑的长请求）。直接写账本 = 只有持久化那道闸
	// 会看到它，正好验证「重启/多副本时内存名额为零也拦得住」。
	if _, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "in-flight-key", KeyID: key.ID, ModelID: model.ID, ProviderID: provider.ID,
		InputText: "x", OutputTokenCeiling: 8, MaxOutputTokens: 8,
		Limits: store.QuotaLimits{RPMLimit: 30},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{GlobalConcurrencyLimit: 4, RPMLimit: 30, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	handler := NewPublicHandler(service, 1<<20)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	// 公共错误码保持不变（客户端可能按它做退避），细节放在 message 里。
	if payload.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("code = %q, want rate_limit_exceeded: %s", payload.Error.Code, rec.Body.String())
	}
	for _, want := range []string{"并发已满", "外包A", "在跑 1", "上限 1"} {
		if !strings.Contains(payload.Error.Message, want) {
			t.Fatalf("message = %q，应包含 %q", payload.Error.Message, want)
		}
	}
}

// 管理台的密钥列表要给出「在跑 / 上限」的两个数字，且口径与闸门一致（只数 reserved）。
func TestAdminKeyListReportsConcurrencyUsage(t *testing.T) {
	repo, key, model, provider := keyConcurrencyFixture(t)
	ctx := t.Context()
	if _, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "in-flight-list", KeyID: key.ID, ModelID: model.ID, ProviderID: provider.ID,
		InputText: "x", OutputTokenCeiling: 8, MaxOutputTokens: 8,
		Limits: store.QuotaLimits{RPMLimit: 30},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{AdminAPIKey: "admin-key", RPMLimit: 30, GlobalConcurrencyLimit: 4}
	handler := NewAdminHandler(repo, nil, settings, settings.AdminAPIKey)

	rec := adminRequest(t, handler, http.MethodGet, "/admin/api/keys", settings.AdminAPIKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			ID                int64 `json:"ID"`
			ConcurrencyLimit  int   `json:"ConcurrencyLimit"`
			ActiveConcurrency int64 `json:"ActiveConcurrency"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, item := range payload.Data {
		if item.ID != key.ID {
			continue
		}
		if item.ConcurrencyLimit != 1 {
			t.Fatalf("上限 = %d, want 1", item.ConcurrencyLimit)
		}
		if item.ActiveConcurrency != 1 {
			t.Fatalf("在跑 = %d, want 1", item.ActiveConcurrency)
		}
		return
	}
	t.Fatal("列表里没有找到这把 Key")
}
