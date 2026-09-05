package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
