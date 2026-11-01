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
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

// visibilityFixture 造一个最小场景：两个分组、一把绑在第一组的客户端密钥、
// 一条挂在该组下的渠道。可见性矩阵要能被这张表逐个断言。
type visibilityFixture struct {
	repo       *store.Store
	handler    http.Handler
	adminKey   string
	keyToken   string
	keyID      int64
	ownGroup   int64
	otherGroup int64
	providerID int64
}

func newVisibilityFixture(t *testing.T) visibilityFixture {
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
	ctx := context.Background()
	own, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "福利"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "别组"})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "上游", BaseURL: "https://api.example.com", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, own.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "客户端", Enabled: true, ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, own.ID); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		AdminAPIKey: "admin-key", EncryptionKey: base64.StdEncoding.EncodeToString(raw),
		Host: "127.0.0.1", Port: 8000, DBPath: "data/relay.db",
		RPMLimit: 30, GlobalConcurrencyLimit: 1, MemoryLimitMB: 200, MaxOutputTokens: 64, RetentionDays: 7,
	}
	return visibilityFixture{
		repo:     repo,
		handler:  NewAdminHandler(repo, nil, settings, settings.AdminAPIKey),
		adminKey: settings.AdminAPIKey,
		keyToken: key.Token, keyID: key.ID, ownGroup: own.ID, otherGroup: other.ID, providerID: provider.ID,
	}
}

// perform 按主体发一次请求。mode: "anon" | "admin" | "key"。
func (f visibilityFixture) perform(t *testing.T, method, path, mode string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	switch mode {
	case "admin":
		req.Header.Set("X-Admin-Key", f.adminKey)
	case "key":
		req.Header.Set("Authorization", "Bearer "+f.keyToken)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// TestAdminVisibilityMatrix 是这次改造最重要的一张测试：可见性漏洞不报错，
// 只会静默泄露，所以每条路径都钉死状态码。
func TestAdminVisibilityMatrix(t *testing.T) {
	f := newVisibilityFixture(t)
	id := func(v int64) string { return strconv.FormatInt(v, 10) }
	cases := []struct {
		name, method, path, mode string
		want                     int
	}{
		// 匿名：没有任何可读路径
		{"匿名概览", "GET", "/admin/api/overview", "anon", http.StatusUnauthorized},
		{"匿名分析", "GET", "/admin/api/analytics", "anon", http.StatusUnauthorized},
		{"匿名分组", "GET", "/admin/api/groups", "anon", http.StatusUnauthorized},
		{"匿名渠道", "GET", "/admin/api/providers", "anon", http.StatusUnauthorized},
		{"匿名会话", "GET", "/admin/api/session", "anon", http.StatusUnauthorized},

		// 管理员：全通
		{"管理员概览", "GET", "/admin/api/overview", "admin", http.StatusOK},
		{"管理员按组概览", "GET", "/admin/api/overview?group_id=" + id(f.ownGroup), "admin", http.StatusOK},
		{"管理员分析", "GET", "/admin/api/analytics", "admin", http.StatusOK},
		{"管理员按组分析", "GET", "/admin/api/analytics?group_id=" + id(f.otherGroup), "admin", http.StatusOK},
		{"管理员密钥列表", "GET", "/admin/api/keys", "admin", http.StatusOK},
		{"管理员配置", "GET", "/admin/api/config", "admin", http.StatusOK},
		{"管理员别组概览", "GET", "/admin/api/groups/" + id(f.otherGroup) + "/overview", "admin", http.StatusOK},

		// 密钥主体：只读，且只看得到自己那一组
		{"密钥会话", "GET", "/admin/api/session", "key", http.StatusOK},
		{"密钥概览", "GET", "/admin/api/overview", "key", http.StatusOK},
		{"密钥明细", "GET", "/admin/api/me", "key", http.StatusOK},
		{"密钥分析", "GET", "/admin/api/analytics", "key", http.StatusOK},
		{"密钥渠道", "GET", "/admin/api/providers", "key", http.StatusOK},
		{"密钥模型", "GET", "/admin/api/models", "key", http.StatusOK},
		{"密钥分组列表", "GET", "/admin/api/groups", "key", http.StatusOK},
		{"密钥本组详情", "GET", "/admin/api/groups/" + id(f.ownGroup), "key", http.StatusOK},
		{"密钥本组概览", "GET", "/admin/api/groups/" + id(f.ownGroup) + "/overview", "key", http.StatusOK},
		{"密钥本组渠道", "GET", "/admin/api/groups/" + id(f.ownGroup) + "/providers", "key", http.StatusOK},
		{"密钥请求日志", "GET", "/admin/api/requests", "key", http.StatusOK},
		{"密钥跨组详情", "GET", "/admin/api/groups/" + id(f.otherGroup), "key", http.StatusNotFound},
		{"密钥跨组概览", "GET", "/admin/api/groups/" + id(f.otherGroup) + "/overview", "key", http.StatusNotFound},
		{"密钥跨组渠道", "GET", "/admin/api/groups/" + id(f.otherGroup) + "/providers", "key", http.StatusNotFound},
		{"密钥看密钥列表", "GET", "/admin/api/keys", "key", http.StatusNotFound},
		{"密钥看运行配置", "GET", "/admin/api/config", "key", http.StatusNotFound},
		{"密钥看插件", "GET", "/admin/api/plugins", "key", http.StatusNotFound},
		{"密钥拉上游模型", "GET", "/admin/api/providers/" + id(f.providerID) + "/remote-models", "key", http.StatusNotFound},
		{"密钥建分组", "POST", "/admin/api/groups", "key", http.StatusNotFound},
		{"密钥触发监测", "POST", "/admin/api/groups/monitor", "key", http.StatusNotFound},
		{"密钥改配置", "PUT", "/admin/api/config", "key", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := f.perform(t, c.method, c.path, c.mode)
			if rec.Code != c.want {
				t.Fatalf("%s %s (%s) = %d, want %d, body=%s", c.method, c.path, c.mode, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestKeySubjectSessionTellsWhoItIs 锁住 session 的语义：它不再对匿名返回 200，
// 而且要告诉前端这把密钥属于哪个分组。
func TestKeySubjectSessionTellsWhoItIs(t *testing.T) {
	f := newVisibilityFixture(t)
	rec := f.perform(t, "GET", "/admin/api/session", "key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Admin bool `json:"admin"`
		Scope struct {
			KeyID     int64  `json:"key_id"`
			GroupID   *int64 `json:"group_id"`
			GroupName string `json:"group_name"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Admin {
		t.Fatal("client key must not be reported as administrator")
	}
	if body.Scope.KeyID != f.keyID || body.Scope.GroupID == nil || *body.Scope.GroupID != f.ownGroup {
		t.Fatalf("scope = %+v, want key %d / group %d", body.Scope, f.keyID, f.ownGroup)
	}
	if body.Scope.GroupName != "福利" {
		t.Fatalf("group name = %q", body.Scope.GroupName)
	}
}

// TestLoginRejectionCodesDiffer 四种拒绝必须分开：客户端据此判断该换令牌还是
// 凭据本身不对；应用型 app_ 标识不能登录控制台（它的密钥是公开的）。
func TestLoginRejectionCodesDiffer(t *testing.T) {
	f := newVisibilityFixture(t)
	cases := []struct {
		name, header, value, wantCode string
	}{
		{"没有凭据", "X-Admin-Key", "", authAuthenticationRequired},
		{"管理员密钥不对", "X-Admin-Key", "wrong-admin", authAuthenticationRequired},
		{"客户端密钥无效", "Authorization", "Bearer crk_not-a-real-token", authInvalidAPIKey},
		{"应用型标识", "Authorization", "Bearer app_public-id", authConsoleNotSupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/admin/api/session", nil)
			if c.value != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, c.wantCode)
			}
		})
	}
}

// TestKeySubjectSeesOnlyItsGroup 覆盖行级裁剪：列表只剩自己那组，渠道不给上游
// 地址，概览的最近请求只能是自己那把密钥的。
func TestKeySubjectSeesOnlyItsGroup(t *testing.T) {
	f := newVisibilityFixture(t)

	var groups struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, f.perform(t, "GET", "/admin/api/groups", "key"), &groups)
	if len(groups.Data) != 1 || groups.Data[0]["Name"] != "福利" {
		t.Fatalf("groups = %+v, want only 福利", groups.Data)
	}

	var providers struct {
		Data []struct {
			ID      int64
			BaseURL string
		} `json:"data"`
	}
	decodeBody(t, f.perform(t, "GET", "/admin/api/providers", "key"), &providers)
	if len(providers.Data) != 1 || providers.Data[0].ID != f.providerID {
		t.Fatalf("providers = %+v, want only the group channel", providers.Data)
	}
	if providers.Data[0].BaseURL != "" {
		t.Fatalf("base_url leaked to key subject: %q", providers.Data[0].BaseURL)
	}

	var overview struct {
		Group  map[string]any   `json:"group"`
		Me     map[string]any   `json:"me"`
		Scope  map[string]any   `json:"scope"`
		Recent []map[string]any `json:"recent"`
	}
	decodeBody(t, f.perform(t, "GET", "/admin/api/overview", "key"), &overview)
	if overview.Group == nil || overview.Group["group_name"] != "福利" {
		t.Fatalf("overview group = %+v", overview.Group)
	}
	if overview.Me == nil || overview.Me["key_id"] == nil {
		t.Fatalf("overview me = %+v", overview.Me)
	}
	if overview.Scope == nil || overview.Scope["group_name"] != "福利" {
		t.Fatalf("overview scope = %+v", overview.Scope)
	}
	if len(overview.Recent) != 0 {
		t.Fatalf("recent = %+v, want empty for a fresh key", overview.Recent)
	}
}

// TestAdminOverviewGroupSwitch 锁住「全站保留 + 可切分组」这两个字段同时存在：
// 全局 rpm 与分组额度是两份不同语义的数据，不能互相顶替。
func TestAdminOverviewGroupSwitch(t *testing.T) {
	f := newVisibilityFixture(t)
	var global map[string]any
	decodeBody(t, f.perform(t, "GET", "/admin/api/overview", "admin"), &global)
	if _, ok := global["rpm"]; !ok {
		t.Fatalf("global overview missing rpm: %+v", global)
	}
	if _, ok := global["group"]; ok {
		t.Fatal("global overview must not embed a group block")
	}
	var grouped map[string]any
	decodeBody(t, f.perform(t, "GET", "/admin/api/overview?group_id="+strconv.FormatInt(f.ownGroup, 10), "admin"), &grouped)
	g, ok := grouped["group"].(map[string]any)
	if !ok {
		t.Fatalf("grouped overview missing group block: %+v", grouped)
	}
	if g["group_name"] != "福利" {
		t.Fatalf("group = %+v", g)
	}
	if _, ok := grouped["rpm"]; !ok {
		t.Fatal("grouped overview must keep the global rpm card")
	}
	// 不存在的组必须是 404：GroupOverview 为容忍「组刚被删、台子还在轮询」会返回
	// 空概览，若直接透传，`?group_id=999` 会拿到一个全 0 的分组块，看起来像
	// 「这组额度已用尽」——比报错更容易误导人。
	missing := f.perform(t, "GET", "/admin/api/overview?group_id=999999", "admin")
	if missing.Code != 404 {
		t.Fatalf("unknown group should be 404, got %d: %s", missing.Code, missing.Body.String())
	}
	if body := missing.Body.String(); !strings.Contains(body, "not_found") {
		t.Fatalf("unknown group error body = %s", body)
	}
}

// TestExpiredKeyGetsTokenExpired 过期与无效必须分开：前者该换令牌，后者是凭据
// 本身不对，混在一起会让调用方一直重试一个永远不会好的请求。
func TestExpiredKeyGetsTokenExpired(t *testing.T) {
	f := newVisibilityFixture(t)
	// 直接改库里那把密钥的有效期，避免再造一套夹具。
	repo := f.repo
	past := time.Now().Add(-time.Hour).UTC()
	if _, err := repo.UpdateClientKey(context.Background(), f.keyID, store.UpdateClientKey{ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	rec := f.perform(t, "GET", "/admin/api/session", "key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), authTokenExpired) {
		t.Fatalf("body = %s, want token_expired", rec.Body.String())
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode: %v, body=%s", err, rec.Body.String())
	}
}
