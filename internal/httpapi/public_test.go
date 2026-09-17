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
	if _, err := repo.CreateModel(t.Context(), store.NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	g, _ := repo.CreateModelGroup(t.Context(), store.NewModelGroup{Name: "g"})
	_ = repo.ReplaceGroupProviders(t.Context(), g.ID, []int64{p.ID})
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
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, TokenLimit5H: 100000, TokenLimitDaily: 20000, MaxOutputTokens: 16}
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
	_ = repo.ReplaceKeyGroups(t.Context(), key.ID, []int64{g.ID})
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
