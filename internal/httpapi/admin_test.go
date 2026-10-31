package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/plugin"
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

func TestAdminPluginRoutes(t *testing.T) {
	raw := make([]byte, 32)
	box, _ := secret.New(base64.StdEncoding.EncodeToString(raw))
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	h := NewAdminHandler(repo, nil, config.Settings{AdminAPIKey: "admin-key"}, "admin-key")
	mgr, err := plugin.NewManager(repo, filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	h.SetPluginManager(mgr)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("package", "audit.zip")
	zw := zip.NewWriter(part)
	manifest := `{"id":"audit-plugin","name":"Audit","version":"1.0.0","api_version":1}`
	w, _ := zw.Create("manifest.json")
	_, _ = io.WriteString(w, manifest)
	w, _ = zw.Create("module.wasm")
	_, _ = w.Write([]byte{0, 'a', 's', 'm', 1, 0, 0, 0})
	_ = zw.Close()
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/plugins", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Admin-Key", "admin-key")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", response.Code, response.Body.String())
	}
	list := adminRequest(t, h, http.MethodGet, "/admin/api/plugins", "admin-key", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "audit-plugin") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
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

func TestAdminRequestQueryValidatesFiltersAndUsesCursorEnvelope(t *testing.T) {
	handler, key := adminFixture(t)
	valid := adminRequest(t, handler, http.MethodGet, "/admin/api/requests?limit=1", key, nil)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"data":[]`) || strings.Contains(valid.Body.String(), `"data":null`) {
		t.Fatalf("empty request page=%d %s", valid.Code, valid.Body.String())
	}
	for _, path := range []string{
		"/admin/api/requests?limit=101",
		"/admin/api/requests?cursor=bad",
		"/admin/api/requests?from=2026-10-03T01:00:00Z&to=2026-10-03T00:00:00Z",
		"/admin/api/requests?provider_id=-1",
	} {
		response := adminRequest(t, handler, http.MethodGet, path, key, nil)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_query") {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/admin/api/requests/", "/admin/api/requests/a%2Fb"} {
		response := adminRequest(t, handler, http.MethodGet, path, key, nil)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request_id") {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestAdminAnalyticsSupportsRangeAndStatusFilters(t *testing.T) {
	handler, key := adminFixture(t)
	response := adminRequest(t, handler, http.MethodGet, "/admin/api/analytics?days=30&bucket=day&metric=requests&status=failed", key, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"days":30`) || strings.Contains(response.Body.String(), `"series":[]`) {
		t.Fatalf("analytics response=%d %s", response.Code, response.Body.String())
	}
	bad := adminRequest(t, handler, http.MethodGet, "/admin/api/analytics?days=31", key, nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "invalid_query") {
		t.Fatalf("invalid analytics range=%d %s", bad.Code, bad.Body.String())
	}
}

func TestAdminAnalyticsAcceptsMultipleModelsAndBoundsSelection(t *testing.T) {
	handler, key := adminFixture(t)
	response := adminRequest(t, handler, http.MethodGet, "/admin/api/analytics?days=7&model_id=1&model_id=2", key, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"days":7`) {
		t.Fatalf("multi-model analytics response=%d %s", response.Code, response.Body.String())
	}
	tooMany := "/admin/api/analytics?days=7" + strings.Repeat("&model_id=1", 21)
	bad := adminRequest(t, handler, http.MethodGet, tooMany, key, nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "at most 20 models") {
		t.Fatalf("unbounded model selection=%d %s", bad.Code, bad.Body.String())
	}
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
	provider := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", key, map[string]any{"name": "p", "base_url": "https://provider.example", "api_key": "upstream-secret", "protocol": "anthropic"})
	if provider.Code != http.StatusCreated || strings.Contains(provider.Body.String(), "upstream-secret") {
		t.Fatal(provider.Code, provider.Body.String())
	}
	if !strings.Contains(provider.Body.String(), `"Protocol":"anthropic"`) {
		t.Fatalf("protocol missing from provider response: %s", provider.Body.String())
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
		MaxBodyBytes: 8 << 20, MaxStreamDuration: time.Minute, ConnectTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second, ModelSyncInterval: time.Minute,
		PeriodLimitDays: 7, TokenLimitPeriod: 1000, PeriodLimitStart: "2026-10-20T00:00:00Z"}
	if err := config.NewStore(envPath).Write(settings); err != nil {
		t.Fatal(err)
	}
	handler := NewAdminHandler(repo, nil, settings, settings.AdminAPIKey)
	handler.SetConfigPersistence(envPath)
	applied := make(chan config.Settings, 1)
	handler.SetSettingsListener(func(s config.Settings) { applied <- s })

	// 修改一个运行时参数（非重启项）：应持久化并热应用
	save := adminRequest(t, handler, http.MethodPut, "/admin/api/config", settings.AdminAPIKey,
		map[string]any{"settings": map[string]string{
			"TOKEN_LIMIT_DAILY": "999999", "UPSTREAM_HEADER_TIMEOUT": "",
			"PERIOD_LIMIT_DAYS": "10", "TOKEN_LIMIT_PERIOD": "2000",
		}})
	if save.Code != http.StatusOK {
		t.Fatal(save.Body.String())
	}
	select {
	case got := <-applied:
		if got.TokenLimitDaily != 999999 {
			t.Fatalf("applied TokenLimitDaily = %d", got.TokenLimitDaily)
		}
		if got.PeriodLimitDays != 10 || got.TokenLimitPeriod != 2000 || got.AmountLimitPeriod != 0 || got.PeriodLimitStart != settings.PeriodLimitStart {
			t.Fatalf("period settings were not applied: %+v", got)
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
	if !strings.Contains(get.Body.String(), `"UPSTREAM_HEADER_TIMEOUT":""`) {
		t.Fatalf("legacy header timeout is not editable: %s", get.Body.String())
	}

	updated := adminRequest(t, handler, http.MethodPut, "/admin/api/config", settings.AdminAPIKey,
		map[string]any{"settings": map[string]string{
			"MAX_STREAM_DURATION": "0", "UPSTREAM_HEADER_TIMEOUT": "60s", "SSE_HEARTBEAT_INTERVAL": "10s",
			"UPSTREAM_USER_AGENT": "relay-test/2", "UPSTREAM_SESSION_HEADER": "x-session-id",
		}})
	if updated.Code != http.StatusOK {
		t.Fatalf("timeout save=%d body=%s", updated.Code, updated.Body.String())
	}
	var updateResult struct {
		PendingRestartFields []string `json:"pending_restart_fields"`
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &updateResult); err != nil {
		t.Fatal(err)
	}
	if len(updateResult.PendingRestartFields) != 3 || updateResult.PendingRestartFields[0] != "UPSTREAM_HEADER_TIMEOUT" || updateResult.PendingRestartFields[1] != "UPSTREAM_USER_AGENT" || updateResult.PendingRestartFields[2] != "UPSTREAM_SESSION_HEADER" {
		t.Fatalf("restart fields=%v", updateResult.PendingRestartFields)
	}
	stored, err = config.NewStore(envPath).ReadMapping()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Parse(stored)
	if err != nil || reloaded.MaxStreamDuration != 0 || reloaded.HeaderTimeout != time.Minute || reloaded.SSEHeartbeatInterval != 10*time.Second {
		t.Fatalf("reloaded timeouts: %+v err=%v", reloaded, err)
	}
}

// TestAdminKeyConnectionTellsClientsWhatToFill 钉住「接入信息」接口。
//
// 它是运维把其他软件接进来的唯一入口：必须一次给全「填什么」（Base URL、密钥标识、
// 模型名），模型清单与公共 /v1/models 同源，并在客户端一定连不上时说明原因 ——
// 否则运维只能靠客户端返回的 400/404 反推，而这两类错误从信息上看不出成因。
// 同时钉住它绝不回显明文密钥与上游密钥。
func TestAdminKeyConnectionTellsClientsWhatToFill(t *testing.T) {
	handler, adminKey := adminFixture(t)

	provider := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey,
		map[string]any{"name": "p", "base_url": "https://provider.example", "api_key": "upstream-secret"})
	var created struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(provider.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	providerID := created.Data.ID

	model := adminRequest(t, handler, http.MethodPost, "/admin/api/models", adminKey,
		map[string]any{"provider_id": providerID, "public_name": "p/chat", "upstream_name": "chat", "input_price": 1, "output_price": 2, "enabled": true})
	if model.Code != http.StatusCreated {
		t.Fatal(model.Body.String())
	}

	group := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey,
		map[string]any{"name": "客户端组", "rate": 1, "provider_ids": []int64{providerID}})
	if group.Code != http.StatusCreated {
		t.Fatal(group.Body.String())
	}
	if err := json.Unmarshal(group.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	groupID := created.Data.ID

	createdKey := adminRequest(t, handler, http.MethodPost, "/admin/api/keys", adminKey,
		map[string]any{"name": "其他软件", "max_concurrency": 2, "group_ids": []int64{groupID}})
	if createdKey.Code != http.StatusCreated {
		t.Fatal(createdKey.Body.String())
	}
	var keyCreated struct {
		Token string `json:"token"`
		Data  struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createdKey.Body.Bytes(), &keyCreated); err != nil {
		t.Fatal(err)
	}
	keyPath := "/admin/api/keys/" + strconv.FormatInt(keyCreated.Data.ID, 10)
	path := keyPath + "/connection"

	var payload struct {
		Data struct {
			BaseURL   string   `json:"base_url"`
			ModelsURL string   `json:"models_url"`
			ChatURL   string   `json:"chat_url"`
			Models    []string `json:"models"`
			Issues    []string `json:"issues"`
			Ready     bool     `json:"ready"`
			TokenHint string   `json:"token_hint"`
			Groups    []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"groups"`
		} `json:"data"`
	}
	decode := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
	}

	response := adminRequest(t, handler, http.MethodGet, path, adminKey, nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	decode(response)
	if !payload.Data.Ready || len(payload.Data.Issues) != 0 {
		t.Fatalf("配置齐全时应当可直接接入：%s", response.Body.String())
	}
	// 管理台是从 example.com 访问的，Base URL 必须按这个来源推断，派生地址要自洽
	if payload.Data.BaseURL != "http://example.com/v1" ||
		payload.Data.ModelsURL != payload.Data.BaseURL+"/models" ||
		payload.Data.ChatURL != payload.Data.BaseURL+"/chat/completions" {
		t.Fatalf("Base URL 或派生地址不对：%s", response.Body.String())
	}
	if len(payload.Data.Models) != 1 || payload.Data.Models[0] != "p/chat" {
		t.Fatalf("模型清单应与公共 /v1/models 一致：%v", payload.Data.Models)
	}
	if len(payload.Data.Groups) != 1 || payload.Data.Groups[0].Name != "客户端组" {
		t.Fatalf("分组信息缺失：%s", response.Body.String())
	}
	// 库里只有摘要，接口只能给脱敏缩略；明文与上游密钥都不得出现
	if payload.Data.TokenHint == "" || strings.Contains(response.Body.String(), keyCreated.Token) {
		t.Fatalf("密钥标识缺失或泄露明文：%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "upstream-secret") {
		t.Fatal("接入信息泄露了上游密钥")
	}

	// 停用 Key：要点明客户端会收到 401，而不是让运维去猜
	if off := adminRequest(t, handler, http.MethodPut, keyPath, adminKey, map[string]any{"enabled": false}); off.Code != http.StatusOK {
		t.Fatal(off.Body.String())
	}
	response = adminRequest(t, handler, http.MethodGet, path, adminKey, nil)
	decode(response)
	if payload.Data.Ready || !containsText(payload.Data.Issues, "已停用") {
		t.Fatalf("Key 停用后应提示 401：%v", payload.Data.Issues)
	}

	// 停用分组：授权模型归零，要提示去配渠道/定价
	if off := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+strconv.FormatInt(groupID, 10), adminKey, map[string]any{"enabled": false}); off.Code != http.StatusOK {
		t.Fatal(off.Body.String())
	}
	response = adminRequest(t, handler, http.MethodGet, path, adminKey, nil)
	decode(response)
	if len(payload.Data.Models) != 0 || !containsText(payload.Data.Issues, "分组已全部停用") {
		t.Fatalf("分组停用后应提示无可授权模型：%v %v", payload.Data.Models, payload.Data.Issues)
	}

	// 路由必须落在 keyItem 里：未知 Key 是 404，写方法是 405，
	// 而不是被 ParseInt 当成无效 ID 判成 400。
	if missing := adminRequest(t, handler, http.MethodGet, "/admin/api/keys/999999/connection", adminKey, nil); missing.Code != http.StatusNotFound {
		t.Fatal(missing.Code, missing.Body.String())
	}
	if badMethod := adminRequest(t, handler, http.MethodPost, path, adminKey, map[string]any{}); badMethod.Code != http.StatusMethodNotAllowed {
		t.Fatal(badMethod.Code, badMethod.Body.String())
	}
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

// TestAdminKeyTTLThreeStates 钉住管理端「短时令牌」有效期的三种语义必须分得开：
// 缺省（不动）/ 0（改为永久）/ >0（从现在起重新计时，即续期）。
// 混在一起最典型的后果是「只改个名字」把短时令牌变成永久令牌 —— 这类静默的安全
// 降级不会有任何报错，只能靠测试挡住。
func TestAdminKeyTTLThreeStates(t *testing.T) {
	handler, adminKey := adminFixture(t)

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/keys", adminKey,
		map[string]any{"name": "短时令牌", "max_concurrency": 1, "ttl_seconds": 3600})
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	var createdBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}
	keyPath := "/admin/api/keys/" + strconv.FormatInt(createdBody.Data.ID, 10)

	type keyRow struct {
		ID            int64 `json:"ID"`
		Expired       bool  `json:"Expired"`
		ExpiresAtUTC  any   `json:"ExpiresAtUTC"`
		ExpiresInSecs any   `json:"ExpiresInSecs"`
	}
	readRow := func() keyRow {
		t.Helper()
		listed := adminRequest(t, handler, http.MethodGet, "/admin/api/keys", adminKey, nil)
		var body struct {
			Data []keyRow `json:"data"`
		}
		if err := json.Unmarshal(listed.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, row := range body.Data {
			if row.ID == createdBody.Data.ID {
				return row
			}
		}
		t.Fatalf("列表里找不到 Key #%d", createdBody.Data.ID)
		return keyRow{}
	}

	row := readRow()
	if row.ExpiresAtUTC == nil || row.Expired {
		t.Fatalf("新建的短时令牌应有到期时间且未过期：%+v", row)
	}
	if secs, ok := row.ExpiresInSecs.(float64); !ok || secs < 3500 || secs > 3700 {
		t.Fatalf("剩余秒数应约为 3600，实际 %v", row.ExpiresInSecs)
	}

	// 缺省：只改名字，有效期必须原样保留
	if resp := adminRequest(t, handler, http.MethodPut, keyPath, adminKey, map[string]any{"name": "改个名"}); resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	if row = readRow(); row.ExpiresAtUTC == nil {
		t.Fatal("只改名字不应清掉有效期")
	}

	// 0 → 改为永久
	if resp := adminRequest(t, handler, http.MethodPut, keyPath, adminKey, map[string]any{"ttl_seconds": 0}); resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	if row = readRow(); row.ExpiresAtUTC != nil || row.Expired {
		t.Fatalf("ttl_seconds=0 应改为永久有效：%+v", row)
	}

	// 设 1 秒后到期：列表要标「已过期」，接入信息要提前讲清客户端会收到什么
	if resp := adminRequest(t, handler, http.MethodPut, keyPath, adminKey, map[string]any{"ttl_seconds": 1}); resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	time.Sleep(1200 * time.Millisecond)
	if row = readRow(); !row.Expired {
		t.Fatalf("到期后应标记为已过期：%+v", row)
	}
	conn := adminRequest(t, handler, http.MethodGet, keyPath+"/connection", adminKey, nil)
	if !strings.Contains(conn.Body.String(), "token_expired") || !strings.Contains(conn.Body.String(), `"ready":false`) {
		t.Fatalf("接入信息应说明已过期并置 ready=false：%s", conn.Body.String())
	}

	// 续期：再传一次正向 ttl（TTLSeconds>0 即「从现在起重新计时」）
	if resp := adminRequest(t, handler, http.MethodPut, keyPath, adminKey, map[string]any{"ttl_seconds": 7200}); resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	row = readRow()
	if row.Expired || row.ExpiresAtUTC == nil {
		t.Fatalf("续期后应恢复有效：%+v", row)
	}
}
