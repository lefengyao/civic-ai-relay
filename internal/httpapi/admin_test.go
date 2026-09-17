package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func adminFixture(t *testing.T) (http.Handler, string) {
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
	settings := config.Settings{AdminAPIKey: "admin-key", EncryptionKey: base64.StdEncoding.EncodeToString(raw), Host: "127.0.0.1", Port: 8000, DBPath: "data/relay.db", RPMLimit: 30, TokenLimit5H: 1000, TokenLimitDaily: 1000, GlobalConcurrencyLimit: 1, MemoryLimitMB: 200, MaxOutputTokens: 64, RetentionDays: 7}
	return NewAdminHandler(repo, nil, settings, settings.AdminAPIKey), settings.AdminAPIKey
}

func adminRequest(t *testing.T, handler http.Handler, method, path string, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if key != "" {
		req.Header.Set("X-Admin-Key", key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAdminRequiresIndependentKeyAndRedactsConfig(t *testing.T) {
	handler, key := adminFixture(t)
	if response := adminRequest(t, handler, http.MethodGet, "/admin/api/config", "", nil); response.Code != http.StatusUnauthorized {
		t.Fatal(response.Code)
	}
	response := adminRequest(t, handler, http.MethodGet, "/admin/api/config", key, nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), key) {
		t.Fatal(response.Body.String())
	}
}

func TestAdminCanCreateProviderAndOneTimeKey(t *testing.T) {
	handler, key := adminFixture(t)
	provider := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", key, map[string]any{"name": "p", "base_url": "https://provider.example", "api_key": "upstream-secret"})
	if provider.Code != http.StatusCreated || strings.Contains(provider.Body.String(), "upstream-secret") {
		t.Fatal(provider.Code, provider.Body.String())
	}
	created := adminRequest(t, handler, http.MethodPost, "/admin/api/keys", key, map[string]any{"name": "alice", "max_concurrency": 1})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), "crk_") {
		t.Fatal(created.Body.String())
	}
	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/keys", key, nil)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "crk_") {
		t.Fatal("key leaked from list")
	}
}

func TestAdminServesChineseEmbeddedPage(t *testing.T) {
	handler, _ := adminFixture(t)
	response := adminRequest(t, handler, http.MethodGet, "/admin", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "运行配置") {
		t.Fatal("embedded page missing Chinese settings text")
	}
}

func TestAdminDeleteRoutes(t *testing.T) {
	handler, key := adminFixture(t)

	provider := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", key, map[string]any{"name": "p", "base_url": "https://provider.example", "api_key": "k"})
	if provider.Code != http.StatusCreated {
		t.Fatal(provider.Body.String())
	}
	var created struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(provider.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	providerID := created.Data.ID

	model := adminRequest(t, handler, http.MethodPost, "/admin/api/models", key, map[string]any{"provider_id": providerID, "public_name": "m", "upstream_name": "m", "input_price": 1, "output_price": 1, "enabled": true})
	if model.Code != http.StatusCreated {
		t.Fatal(model.Body.String())
	}
	if err := json.Unmarshal(model.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	modelID := created.Data.ID

	group := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", key, map[string]any{"name": "g", "model_ids": []int64{modelID}})
	if group.Code != http.StatusCreated {
		t.Fatal(group.Body.String())
	}
	if err := json.Unmarshal(group.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	groupID := created.Data.ID

	clientKey := adminRequest(t, handler, http.MethodPost, "/admin/api/keys", key, map[string]any{"name": "k", "max_concurrency": 1, "group_ids": []int64{groupID}})
	if clientKey.Code != http.StatusCreated {
		t.Fatal(clientKey.Body.String())
	}
	if err := json.Unmarshal(clientKey.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	keyID := created.Data.ID

	for _, path := range []string{
		"/admin/api/keys/" + itoa(keyID),
		"/admin/api/groups/" + itoa(groupID),
		"/admin/api/models/" + itoa(modelID),
		"/admin/api/providers/" + itoa(providerID),
	} {
		if response := adminRequest(t, handler, http.MethodDelete, path, key, nil); response.Code != http.StatusOK {
			t.Fatalf("DELETE %s = %d: %s", path, response.Code, response.Body.String())
		}
		// A second delete must report 404 rather than silently succeeding.
		if response := adminRequest(t, handler, http.MethodDelete, path, key, nil); response.Code != http.StatusNotFound {
			t.Fatalf("repeat DELETE %s = %d, want 404", path, response.Code)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// groups/health 与 groups/monitor 的第一段不是数字 ID：它们必须排在
// HasPrefix("groups/") 之前，否则会被 groupItem 的 ParseInt 判成 invalid_id。
// 这条顺序是隐式契约，用测试钉住。
func TestAdminGroupMonitorRoutesAreNotParsedAsIDs(t *testing.T) {
	handler, key := adminFixture(t)
	health := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/health", key, nil)
	if health.Code != http.StatusOK {
		t.Fatalf("GET groups/health = %d: %s", health.Code, health.Body.String())
	}
	if !strings.Contains(health.Body.String(), "monitor_enabled") {
		t.Fatalf("health payload lacks the monitor flags: %s", health.Body.String())
	}
	// 没注册监测器时手动触发应当是 503（功能不可用），不是 404/400/500
	if got := adminRequest(t, handler, http.MethodPost, "/admin/api/groups/monitor", key, nil); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST groups/monitor = %d, want 503: %s", got.Code, got.Body.String())
	}
	if got := adminRequest(t, handler, http.MethodPost, "/admin/api/groups/1/monitor", key, nil); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST groups/1/monitor = %d, want 503: %s", got.Code, got.Body.String())
	}
	// 历史查询只读数据库，不依赖监测器
	history := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/1/monitor", key, nil)
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), `"data"`) {
		t.Fatalf("GET groups/1/monitor = %d: %s", history.Code, history.Body.String())
	}
}

type fakeGroupMonitor struct {
	runs      int
	lastGroup int64
}

func (f *fakeGroupMonitor) RunOnce(context.Context, string) (int, error) { f.runs++; return 3, nil }
func (f *fakeGroupMonitor) RunGroup(_ context.Context, id int64) (store.GroupMonitorResult, error) {
	f.lastGroup = id
	return store.GroupMonitorResult{GroupID: id, Status: store.GroupStatusOK, Detail: "全部渠道可达"}, nil
}

func TestAdminGroupMonitorTriggersReturnResult(t *testing.T) {
	handler, key := adminFixture(t)
	admin, ok := handler.(*AdminHandler)
	if !ok {
		t.Fatalf("fixture returned %T, want *AdminHandler", handler)
	}
	monitor := &fakeGroupMonitor{}
	admin.SetGroupMonitor(monitor)

	all := adminRequest(t, handler, http.MethodPost, "/admin/api/groups/monitor", key, nil)
	if all.Code != http.StatusOK || !strings.Contains(all.Body.String(), `"monitored":3`) {
		t.Fatalf("POST groups/monitor = %d: %s", all.Code, all.Body.String())
	}
	one := adminRequest(t, handler, http.MethodPost, "/admin/api/groups/7/monitor", key, nil)
	if one.Code != http.StatusOK || !strings.Contains(one.Body.String(), "全部渠道可达") {
		t.Fatalf("POST groups/7/monitor = %d: %s", one.Code, one.Body.String())
	}
	if monitor.runs != 1 || monitor.lastGroup != 7 {
		t.Fatalf("monitor calls = runs %d / last group %d, want 1 / 7", monitor.runs, monitor.lastGroup)
	}
}

// 管理台编辑渠道走的是 PascalCase 载荷：store.UpdateProvider 没有 json tag，
// 靠 encoding/json 的大小写不敏感匹配才能对上（POST 用的却是 snake_case）。
// 这条不一致的契约没写进 docs，一旦被"顺手统一成 snake_case"，PUT 会静默变成
// 空更新——不报错、也不生效，很难查。所以在这里钉住。
func TestAdminUpdateProviderAcceptsPascalCasePayload(t *testing.T) {
	handler, key := adminFixture(t)
	created := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", key, map[string]any{
		"name": "p", "base_url": "https://provider.example", "api_key": "k",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var payload struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	updated := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), key, map[string]any{
		"Name": "p2", "BaseURL": "http://127.0.0.1:6000", "Enabled": true,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", updated.Code, updated.Body.String())
	}
	if !strings.Contains(updated.Body.String(), `"Name":"p2"`) || !strings.Contains(updated.Body.String(), `"BaseURL":"http://127.0.0.1:6000"`) {
		t.Fatalf("PascalCase 载荷没有生效（静默空更新）: %s", updated.Body.String())
	}
}

// 管理台的渠道错误必须带上具体原因。现场（2026-09-16）只回 "provider_invalid"，
// 操作员连续收到 9 条同样的 toast，完全无从判断是地址格式、重名还是加密失败。
func TestAdminProviderErrorCarriesReason(t *testing.T) {
	handler, key := adminFixture(t)
	cases := []struct{ name, baseURL, want string }{
		{"no-scheme", "127.0.0.1:6000", "协议头"},
		{"public-http", "http://8.8.8.8:6000", "放行名单"},
	}
	for i, tc := range cases {
		response := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", key, map[string]any{
			"name": "p" + itoa(int64(i)), "base_url": tc.baseURL, "api_key": "k",
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", tc.name, response.Code, response.Body.String())
		}
		var payload struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Error.Code != "provider_invalid" {
			t.Errorf("%s: code = %q, want provider_invalid", tc.name, payload.Error.Code)
		}
		if !strings.Contains(payload.Error.Message, tc.want) {
			t.Errorf("%s: message = %q, want it to mention %q", tc.name, payload.Error.Message, tc.want)
		}
	}
}

func TestConfigSavePersistsAndAppliesWithoutRestart(t *testing.T) {
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
	envPath := filepath.Join(t.TempDir(), "relay.env")
	settings := config.Settings{AdminAPIKey: "admin-key", EncryptionKey: base64.StdEncoding.EncodeToString(raw), Host: "127.0.0.1", Port: 8000, DBPath: "data/relay.db", LogLevel: "INFO", RPMLimit: 30, TokenLimit5H: 1000, TokenLimitDaily: 1000, GlobalConcurrencyLimit: 1, MemoryLimitMB: 200, MaxOutputTokens: 64, RetentionDays: 7,
		MaxBodyBytes: 8 << 20, MaxStreamDuration: time.Minute, ConnectTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second, ModelSyncInterval: time.Minute}
	if err := config.NewStore(envPath).Write(settings); err != nil {
		t.Fatal(err)
	}
	handler := NewAdminHandler(repo, nil, settings, settings.AdminAPIKey)
	handler.SetConfigPersistence(envPath)
	applied := make(chan config.Settings, 1)
	handler.SetSettingsListener(func(s config.Settings) { applied <- s })

	// 修改一个运行时参数（非重启项）：应持久化并热应用
	save := adminRequest(t, handler, http.MethodPut, "/admin/api/config", settings.AdminAPIKey,
		map[string]any{"settings": map[string]string{"TOKEN_LIMIT_DAILY": "999999"}})
	if save.Code != http.StatusOK {
		t.Fatal(save.Body.String())
	}
	select {
	case got := <-applied:
		if got.TokenLimitDaily != 999999 {
			t.Fatalf("applied TokenLimitDaily = %d", got.TokenLimitDaily)
		}
	default:
		t.Fatal("settings listener was not notified")
	}
	// 持久化校验：重读 env 文件应包含新值
	stored, err := config.NewStore(envPath).ReadMapping()
	if err != nil {
		t.Fatal(err)
	}
	if stored["TOKEN_LIMIT_DAILY"] != "999999" {
		t.Fatalf("persisted TOKEN_LIMIT_DAILY = %q", stored["TOKEN_LIMIT_DAILY"])
	}
	// GET 返回的也是新值
	get := adminRequest(t, handler, http.MethodGet, "/admin/api/config", settings.AdminAPIKey, nil)
	if !strings.Contains(get.Body.String(), "999999") {
		t.Fatal(get.Body.String())
	}
}
