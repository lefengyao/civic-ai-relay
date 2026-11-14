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

// 管理接口要能建/读/改并发上限（v26）。字段名用 snake_case，与其它额度字段一致。
func TestAdminGroupConcurrencyLimitRoundTrip(t *testing.T) {
	handler, adminKey := adminFixture(t)

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "夜间池", "kind": "normal", "rate": 1, "concurrency_limit": 4,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var payload struct {
		Data struct {
			ID               int64  `json:"ID"`
			ConcurrencyLimit *int64 `json:"ConcurrencyLimit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.ConcurrencyLimit == nil || *payload.Data.ConcurrencyLimit != 4 {
		t.Fatalf("创建后的并发上限 = %v, want 4", payload.Data.ConcurrencyLimit)
	}

	// 列表要带上限与实时占用（0），管理台靠这两个值渲染「3 / 4」。
	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/groups", adminKey, nil)
	var list struct {
		Data []struct {
			Name              string `json:"Name"`
			ConcurrencyLimit  *int64 `json:"ConcurrencyLimit"`
			ActiveConcurrency int64  `json:"ActiveConcurrency"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range list.Data {
		if item.Name != "夜间池" {
			continue
		}
		found = true
		if item.ConcurrencyLimit == nil || *item.ConcurrencyLimit != 4 {
			t.Fatalf("列表里的上限 = %v, want 4", item.ConcurrencyLimit)
		}
		if item.ActiveConcurrency != 0 {
			t.Fatalf("没有请求时在跑数应为 0，得到 %d", item.ActiveConcurrency)
		}
	}
	if !found {
		t.Fatal("列表里没有新建的组")
	}

	// 改名的同时不该顺手清掉上限；显式传 0 才是「改回不限」。
	renamed := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(payload.Data.ID), adminKey, map[string]any{"name": "夜间池-2"})
	if renamed.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", renamed.Code, renamed.Body.String())
	}
	var afterRename struct {
		Data struct {
			ConcurrencyLimit *int64 `json:"ConcurrencyLimit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(renamed.Body.Bytes(), &afterRename); err != nil {
		t.Fatal(err)
	}
	if afterRename.Data.ConcurrencyLimit == nil || *afterRename.Data.ConcurrencyLimit != 4 {
		t.Fatalf("改名后上限 = %v, want 4（不应被清掉）", afterRename.Data.ConcurrencyLimit)
	}

	cleared := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(payload.Data.ID), adminKey, map[string]any{"concurrency_limit": 0})
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", cleared.Code, cleared.Body.String())
	}
	var afterClear struct {
		Data struct {
			ConcurrencyLimit *int64 `json:"ConcurrencyLimit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cleared.Body.Bytes(), &afterClear); err != nil {
		t.Fatal(err)
	}
	if afterClear.Data.ConcurrencyLimit != nil {
		t.Fatalf("填 0 后应为不限（null），得到 %v", *afterClear.Data.ConcurrencyLimit)
	}
}

func TestAdminGroupConcurrencyLimitRejectsNegative(t *testing.T) {
	handler, adminKey := adminFixture(t)
	created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "坏值", "kind": "normal", "concurrency_limit": -1,
	})
	if created.Code != http.StatusBadRequest {
		t.Fatalf("负数 = %d, want 400: %s", created.Code, created.Body.String())
	}
}

// 客户端侧的结果必须是 429 + 可读中文：并发满不是服务端故障，客户端应当退避重试，
// 而不是当成 5xx 一直重打。
func TestGroupConcurrencyReturns429WithReadableMessage(t *testing.T) {
	raw := make([]byte, 32)
	box, err := secret.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	limit := int64(1)
	member, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "夜间池", Kind: store.GroupKindNormal, ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, member.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	aggregate, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "总池", Kind: store.GroupKindAggregate, GroupIDs: []int64{member.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, aggregate.ID); err != nil {
		t.Fatal(err)
	}
	// 先占掉唯一的并发名额（模拟一个正在跑的长请求）。
	if _, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "in-flight", KeyID: key.ID, ModelID: model.ID, ProviderID: provider.ID,
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
	if payload.Error.Code != "group_concurrency_exceeded" {
		t.Fatalf("code = %q, want group_concurrency_exceeded: %s", payload.Error.Code, rec.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "并发已满") || !strings.Contains(payload.Error.Message, "夜间池") {
		t.Fatalf("message = %q，应说明哪个组并发已满", payload.Error.Message)
	}
}
