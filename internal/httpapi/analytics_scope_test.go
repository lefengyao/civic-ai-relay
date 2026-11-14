package httpapi

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

// analyticsScopeFixture 造两把同组密钥与两条请求，用来验证「密钥主体的用量分析
// 只统计自己这把密钥」—— 同组其他密钥的消耗不能出现在他的图表里。
func analyticsScopeFixture(t *testing.T) (http.Handler, store.ClientKey, store.ClientKey) {
	t.Helper()
	raw := make([]byte, 32)
	box, err := secret.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	repo, err := store.Open(dbPath, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := t.Context()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "p", BaseURL: "https://up.example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	model, err := repo.CreateModel(ctx, store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", Enabled: true, InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price})
	if err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "总池", Kind: store.GroupKindAggregate})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	mine, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "mine", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, mine.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	other, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "other", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, other.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	// 第二个连接直接写账本：模拟「同组另一把密钥已经跑过请求」的历史数据。
	writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	seed := func(id string, keyID int64, tokens int64) {
		t.Helper()
		if _, err := writer.ExecContext(ctx, `INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc,group_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, keyID, model.ID, provider.ID, "completed", tokens, 0, tokens*10, now, group.ID); err != nil {
			t.Fatal(err)
		}
	}
	seed("mine-request", mine.ID, 10)
	seed("other-request", other.ID, 90)

	settings := config.Settings{AdminAPIKey: "admin-key", RPMLimit: 30, MaxOutputTokens: 64}
	return NewAdminHandler(repo, nil, settings, settings.AdminAPIKey), mine, other
}

// 断言三件事：只算自己那把密钥、带 ?group_id 也扩大不了、管理员视角仍然看得到整组。
func TestKeySubjectAnalyticsScopesToOwnKey(t *testing.T) {
	handler, mine, _ := analyticsScopeFixture(t)
	call := func(path, token string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return rec.Code, body
	}

	status, body := call("/admin/api/analytics?days=7&bucket=day&metric=tokens", mine.Token)
	if status != http.StatusOK {
		t.Fatalf("analytics status = %d body=%v", status, body)
	}
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %#v", models)
	}
	entry := models[0].(map[string]any)
	if entry["count"] != float64(1) || entry["tokens"] != float64(10) {
		t.Fatalf("key-scoped entry = %#v, want only my 1 request / 10 tokens", entry)
	}
	var total float64
	for _, raw := range body["series"].([]any) {
		total += raw.(map[string]any)["tokens"].(float64)
	}
	if total != 10 {
		t.Fatalf("key-scoped total tokens = %v, want 10", total)
	}

	// 传分组参数不能把范围放大到整组（密钥主体的范围不看 query）。
	status, widened := call("/admin/api/analytics?days=7&bucket=day&metric=tokens&group_id=1", mine.Token)
	if status != http.StatusOK {
		t.Fatalf("scoped analytics status = %d body=%v", status, widened)
	}
	if entry := widened["models"].([]any)[0].(map[string]any); entry["count"] != float64(1) || entry["tokens"] != float64(10) {
		t.Fatalf("group_id must not widen key scope: %#v", entry)
	}

	// 管理员不受影响：同一时间段两条都在。
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/api/analytics?days=7&bucket=day&metric=tokens", nil)
	adminReq.Header.Set("X-Admin-Key", "admin-key")
	adminRec := httptest.NewRecorder()
	handler.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin analytics status = %d body=%s", adminRec.Code, adminRec.Body.String())
	}
	var adminBody map[string]any
	if err := json.Unmarshal(adminRec.Body.Bytes(), &adminBody); err != nil {
		t.Fatal(err)
	}
	if entry := adminBody["models"].([]any)[0].(map[string]any); entry["count"] != float64(2) || entry["tokens"] != float64(100) {
		t.Fatalf("admin scope changed: %#v", entry)
	}
}
