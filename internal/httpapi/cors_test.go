package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"civic-ai-relay/internal/config"
)

func corsSettings(origins string) func() config.Settings {
	return func() config.Settings {
		return config.Settings{CORSAllowedOrigins: origins}
	}
}

func TestCORSWildcardAllowsAnyOrigin(t *testing.T) {
	handler := NewServer(nil, 1<<20) // settingsFn 为 nil，出厂默认 "*"
	for _, origin := range []string{"https://app.example.com", "http://localhost:5173"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Origin", origin)
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("origin %s: ACAO = %q, want *", origin, got)
		}
	}
}

func CORSPreflight(handler http.Handler, origin, method, headers string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCORSPreflightWildcard(t *testing.T) {
	handler := NewServer(nil, 1<<20)
	rec := CORSPreflight(handler, "https://app.example.com", "POST", "Authorization, Content-Type")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("ACAO = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Access-Control-Allow-Methods must be present on preflight")
	}
	// 回显浏览器声明的请求头：客户端新增自定义头时无需改服务端。
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Fatalf("Allow-Headers = %q, want the echoed request headers", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Fatal("Access-Control-Max-Age must be present on preflight")
	}
}

func TestCORSOriginWhitelist(t *testing.T) {
	handler := NewServerWithAdmission(nil, 1<<20, nil, nil, corsSettings("https://a.example.com, https://b.example.com:8443"))

	// 白名单内的来源：回显请求 Origin。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://a.example.com")
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://a.example.com" {
		t.Fatalf("ACAO = %q, want the exact allowed origin", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin (whitelist responses depend on the origin)", got)
	}

	// 白名单外的来源：不下发 ACAO，由浏览器拦截。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty for a non-whitelisted origin", got)
	}

	// 白名单外的预检：不短路、不下发 CORS 头。
	rec = CORSPreflight(handler, "https://evil.example.com", "POST", "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("preflight ACAO = %q, want empty for a non-whitelisted origin", got)
	}

	// 白名单内的预检：204 短路。
	rec = CORSPreflight(handler, "https://b.example.com:8443", "POST", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code = %d, want 204", rec.Code)
	}
}

func TestCORSOffSuppressesAllHeaders(t *testing.T) {
	handler := NewServerWithAdmission(nil, 1<<20, nil, nil, corsSettings("off"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://app.example.com")
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want no CORS headers when the policy is off", got)
	}

	rec = CORSPreflight(handler, "https://app.example.com", "POST", "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("preflight ACAO = %q, want none when the policy is off", got)
	}
}

func TestCORSNonBrowserRequestUnaffected(t *testing.T) {
	handler := NewServerWithAdmission(nil, 1<<20, nil, nil, corsSettings("https://a.example.com"))
	rec := httptest.NewRecorder()
	// SDK / curl 不带 Origin：不应下发任何 CORS 头，也不影响正常响应。
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty for a request without Origin", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
}

func TestCORSAllowedOriginsParsing(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "", want: "*"},
		{raw: "*", want: "*"},
		{raw: "*, https://a.example.com", want: "*"},
		{raw: "off", want: "off"},
		{raw: "OFF", want: "off"},
		{raw: " https://a.example.com , https://b.example.com ", want: "https://a.example.com,https://b.example.com"},
		{raw: "https://a.example.com/path", wantErr: true},
		{raw: "a.example.com", wantErr: true},
		{raw: "ftp://a.example.com", wantErr: true},
		{raw: " , , ", wantErr: true},
	}
	for _, tc := range cases {
		// 32 个零字节的 base64（标准填充），满足 RELAY_ENCRYPTION_KEY 的格式校验。
		values := map[string]string{"ADMIN_API_KEY": "x", "RELAY_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32))}
		if tc.raw != "" {
			values["CORS_ALLOWED_ORIGINS"] = tc.raw
		}
		settings, err := config.Parse(values)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("CORS_ALLOWED_ORIGINS=%q: want error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("CORS_ALLOWED_ORIGINS=%q: %v", tc.raw, err)
		}
		if settings.CORSAllowedOrigins != tc.want {
			t.Fatalf("CORS_ALLOWED_ORIGINS=%q: got %q, want %q", tc.raw, settings.CORSAllowedOrigins, tc.want)
		}
	}
}
