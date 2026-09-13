package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func newHealthFixture(t *testing.T) (*store.Store, *relay.Service) {
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
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, TokenLimit5H: 100000, TokenLimitDaily: 20000, MaxOutputTokens: 16}
	return repo, relay.NewService(repo, &testFactory{}, func() config.Settings { return settings })
}

func decodeHealth(t *testing.T, body []byte) HealthPayload {
	t.Helper()
	var payload HealthPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("health body is not JSON: %v (%s)", err, body)
	}
	return payload
}

func TestHealthzReportsLivenessWithoutAuth(t *testing.T) {
	_, service := newHealthFixture(t)
	handler := NewServerWithAdmission(service, 1<<20, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d, want 200", rec.Code)
	}
	payload := decodeHealth(t, rec.Body.Bytes())
	if payload.Status != "ok" {
		t.Fatalf("status = %q, want ok", payload.Status)
	}
	if payload.Version == "" {
		t.Fatal("version must always be reported")
	}
	// 探针不接受写方法，避免被当成免认证的写入入口。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz code = %d, want 405", rec.Code)
	}
}

func TestReadyzTracksDatabaseAvailability(t *testing.T) {
	repo, service := newHealthFixture(t)
	handler := NewServerWithAdmission(service, 1<<20, nil, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz code = %d, want 200 while the database is open", rec.Code)
	}

	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz code = %d, want 503 after the database is closed", rec.Code)
	}
	payload := decodeHealth(t, rec.Body.Bytes())
	if payload.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", payload.Status)
	}
	// 存活探针必须与数据库解耦，否则数据库故障会触发无意义的重启循环。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d, want 200 even when the database is down", rec.Code)
	}
}

// TestHealthRoutesCoexistWithAdminAndPublic 确认挂载管理端后，探针、管理台与
// 公共接口三者互不遮挡。
func TestHealthRoutesCoexistWithAdminAndPublic(t *testing.T) {
	_, service := newHealthFixture(t)
	adminHit := false
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { adminHit = true })
	handler := NewServerWithAdmission(service, 1<<20, nil, nil, admin)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code = %d, want 200", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if !adminHit {
		t.Fatal("/admin/ did not reach the admin handler")
	}
	// 公共接口仍可路由：无 Key 的 models 请求必须是 401 而不是 404。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models code = %d, want 401", rec.Code)
	}
	// 根路径仍重定向到管理台。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/ code = %d, want 302", rec.Code)
	}
}

func TestSetVersionIsReflectedInHealthz(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	SetVersion("v9.9.9-test")
	_, service := newHealthFixture(t)
	handler := NewServerWithAdmission(service, 1<<20, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if payload := decodeHealth(t, rec.Body.Bytes()); payload.Version != "v9.9.9-test" {
		t.Fatalf("version = %q, want v9.9.9-test", payload.Version)
	}
	// 空值不得抹掉已注入的版本，否则构建信息会在运行期被清空。
	SetVersion("")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if payload := decodeHealth(t, rec.Body.Bytes()); payload.Version != "v9.9.9-test" {
		t.Fatalf("version = %q, want the previous version to be preserved", payload.Version)
	}
}
