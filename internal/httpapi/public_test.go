package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/testutil"
)

func TestDrainingReturnsSettlementUnavailable(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	f.Service.BeginDraining()
	handler := NewPublicHandler(f.Service, 1<<20)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		if path == "/v1/responses" {
			body = `{"model":"m","input":"hi"}`
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.Key.Token)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "settlement_unavailable") {
			t.Fatalf("%s: code=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

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
	if _, err := repo.CreateModel(t.Context(), store.NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	g, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupProviders(t.Context(), g.ID, []int64{p.ID})
	key, _ := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "k", ConcurrencyLimit: 1})
	_ = repo.SetKeyGroup(t.Context(), key.ID, g.ID)
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, MaxOutputTokens: 16}
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

// TestExpiredTokenGetsDistinctErrorCode 确认短时令牌过期时，公共接口回的是
// 401 token_expired 而不是 invalid_api_key —— 客户端据此能提示「令牌过期，请换新的」，
// 而不是把「凭据不对」和「服务坏了」混在一起反复重试。两条公共路径都要一致。
func TestExpiredTokenGetsDistinctErrorCode(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	provider, _ := repo.CreateProvider(t.Context(), store.NewProvider{Name: "p", BaseURL: "https://p.example", APIKey: "s"})
	price := int64(1)
	if _, err := repo.CreateModel(t.Context(), store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	group, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupProviders(t.Context(), group.ID, []int64{provider.ID})

	past := time.Now().UTC().Add(-time.Minute)
	key, err := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "短时令牌", ConcurrencyLimit: 1, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.SetKeyGroup(t.Context(), key.ID, group.ID)

	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	handler := NewPublicHandler(service, 1<<20)

	cases := []struct {
		name, method, path, body string
	}{
		{"列举模型", http.MethodGet, "/v1/models", ""},
		{"对话", http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, item := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		request.Header.Set("Authorization", "Bearer "+key.Token)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s：过期令牌应 401，实际 %d %s", item.name, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "token_expired") {
			t.Fatalf("%s：过期令牌必须回 token_expired，实际 %s", item.name, body)
		}
		if strings.Contains(body, "invalid_api_key") {
			t.Fatalf("%s：过期不应被报成凭据无效：%s", item.name, body)
		}
	}

	// 续期后应当立刻恢复
	later := time.Now().UTC().Add(time.Hour)
	if _, err := repo.UpdateClientKey(t.Context(), key.ID, store.UpdateClientKey{ExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key.Token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("续期后应立即可用，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPathVariantsReachPublicRoutes 确认客户端 Base URL 多写/少写 /v1、尾斜杠
// 等路径变体都能命中真实路由（假 Key 应得到 401 而非 404），未知端点返回 JSON 提示。
func TestPathVariantsReachPublicRoutes(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	h := NewPublicHandler(service, 1<<20)

	payload := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	variants := []string{
		"/v1/chat/completions",
		"/v1/v1/chat/completions", // Base URL 末尾多带了 /v1
		"/chat/completions",       // Base URL 漏填 /v1
		"/v1/chat/completions/",   // 尾斜杠
	}
	for _, path := range variants {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer crk_dummy")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s hit 404, expected route match", path)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s code = %d, want 401 (route matched, dummy key)", path, rec.Code)
		}
	}
	// models 列表的缺前缀与尾斜杠变体同样可达
	for _, path := range []string{"/models", "/v1/models/"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer crk_dummy")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s code = %d, want 401 (route matched)", path, rec.Code)
		}
	}
	// 未知端点：JSON 404，带可操作的提示
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint code = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/v1/chat/completions") {
		t.Fatalf("404 body lacks hint: %s", rec.Body.String())
	}
	// /v1/responses：Responses API 已支持，假 Key 应 401（路由命中）而非 404
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer crk_dummy")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/responses code = %d, want 401 (route matched)", rec.Code)
	}
}

func (*testFactory) ForProvider(_ context.Context, _ int64) (relay.UpstreamClient, error) {
	return nil, nil
}

// TestAppIdentifierIsAValidCredential 钉住「客户端不保存密钥」的接入方式：
// 请求只带公开的 X-App-Id 就能用；同时**绝不能**用它冒充密钥型 Key（那是权限提升）。
func TestAppIdentifierIsAValidCredential(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	provider, _ := repo.CreateProvider(t.Context(), store.NewProvider{Name: "p", BaseURL: "https://p.example", APIKey: "s"})
	price := int64(1)
	if _, err := repo.CreateModel(t.Context(), store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	group, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupProviders(t.Context(), group.ID, []int64{provider.ID})

	app, err := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "我的软件", ConcurrencyLimit: 1, Keyless: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.SetKeyGroup(t.Context(), app.ID, group.ID)
	normal, _ := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "普通", ConcurrencyLimit: 1})
	_ = repo.SetKeyGroup(t.Context(), normal.ID, group.ID)

	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	handler := NewPublicHandler(service, 1<<20)

	call := func(appID, token string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if appID != "" {
			request.Header.Set("X-App-Id", appID)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if recorder := call("", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，实际 %d", recorder.Code)
	}
	authorized := call(app.PublicID, "")
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"m"`) {
		t.Fatalf("只带应用标识应能拉到授权模型，实际 %d %s", authorized.Code, authorized.Body.String())
	}
	if recorder := call("app_不存在", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未知应用标识应 401，实际 %d", recorder.Code)
	}
	// 拿普通 Key 的密钥当应用标识用：必须失败，否则等于用公开值换到了密钥的权限
	if recorder := call(normal.Token, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("密钥不得被当作应用标识使用，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	// 反过来：应用型凭据没有密钥，用 Bearer 也进不来
	if recorder := call("", "crk_随便编一个"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("应用型凭据不应能用 Bearer 进来，实际 %d", recorder.Code)
	}
	if recorder := call(app.PublicID, app.PublicID); recorder.Code != http.StatusOK {
		t.Fatalf("同时给出时应仍可通过（密钥为空则回落到应用标识），实际 %d", recorder.Code)
	}
}

// 额度用满必须是 429 且带具体原因，而不是 500 relay_error 或 401 invalid_api_key。
// 旧实现把 QuotaError 落到 default 分支返回 500，客户端会当成服务端故障反复重试；
// Key 被自动停用时更是只回 401，运维看到概览里还有额度，完全无法定位。
func TestQuotaRejectionReturns429WithReason(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	p, _ := repo.CreateProvider(t.Context(), store.NewProvider{Name: "p", BaseURL: "https://p.example", APIKey: "s"})
	price := int64(1)
	model, err := repo.CreateModel(t.Context(), store.NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupProviders(t.Context(), g.ID, []int64{p.ID})
	limit := int64(1)
	key, _ := repo.CreateClientKey(t.Context(), store.NewClientKey{Name: "k", ConcurrencyLimit: 4, TokenLimit: &limit})
	_ = repo.SetKeyGroup(t.Context(), key.ID, g.ID)
	// 先直接占掉这唯一的 1 个 token 总额度，让后续请求处于「已欠费」状态。
	if _, err := repo.ReserveRequest(t.Context(), store.ReserveInput{
		RequestID: "seed", KeyID: key.ID, ModelID: model.ID, ProviderID: p.ID,
		InputText: "x", OutputTokenCeiling: 1, MaxOutputTokens: 1,
		Limits: store.QuotaLimits{RPMLimit: 30},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{GlobalConcurrencyLimit: 4, RPMLimit: 30, MaxOutputTokens: 16}
	service := relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
	h := NewPublicHandler(service, 1<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	h.ServeHTTP(rec, req)
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
	if payload.Error.Code != "key_token_quota_exceeded" {
		t.Fatalf("code = %q, want key_token_quota_exceeded: %s", payload.Error.Code, rec.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "欠费") {
		t.Fatalf("message = %q, want an overdraft explanation", payload.Error.Message)
	}
}
