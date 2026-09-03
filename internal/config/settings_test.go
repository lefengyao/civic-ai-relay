package config

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validValues() map[string]string {
	return map[string]string{
		"ADMIN_API_KEY":            "admin-secret",
		"RELAY_ENCRYPTION_KEY":     base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"UPSTREAM_BASE_URL":        "https://provider.example/v1/",
		"UPSTREAM_API_KEY":         "upstream-secret",
		"MODEL_AUTO_SYNC":          "true",
		"MODEL_SYNC_INTERVAL":      "30",
		"TOKEN_LIMIT_5H":           "10000",
		"TOKEN_LIMIT_DAILY":        "5000",
		"RPM_LIMIT":                "30",
		"GLOBAL_CONCURRENCY_LIMIT": "2",
		"MEMORY_LIMIT_MB":          "200",
		"MAX_OUTPUT_TOKENS":        "256",
		"MAX_BODY_MB":              "8",
		"MAX_STREAM_DURATION":      "1800",
		"RETENTION_DAYS":           "7",
		"DB_PATH":                  "data/relay.db",
		"HOST":                     "127.0.0.1",
		"PORT":                     "9000",
		"LOG_LEVEL":                "debug",
		"DOCS_ENABLED":             "true",
		"UPSTREAM_CONNECT_TIMEOUT": "1.5s",
		"UPSTREAM_READ_TIMEOUT":    "2.5s",
		"UPSTREAM_WRITE_TIMEOUT":   "3.5s",
		"UPSTREAM_POOL_TIMEOUT":    "4.5s",
	}
}

func TestParseAppliesDefaultsAndTypes(t *testing.T) {
	settings, err := Parse(map[string]string{
		"ADMIN_API_KEY":        "admin",
		"RELAY_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Port != 8000 || settings.MemoryLimitMB != 200 || settings.MaxBodyBytes != 8*1024*1024 {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if settings.ModelSyncInterval != 30*time.Minute || settings.MaxStreamDuration != 1800*time.Second {
		t.Fatalf("unexpected duration defaults: %+v", settings)
	}
	if !settings.ModelAutoSync {
		t.Fatal("model auto-sync should preserve the existing default")
	}
	if settings.TokenLimit5H != 100000 || settings.TokenLimitDaily != 20000 {
		t.Fatalf("unexpected quota defaults: %+v", settings)
	}
}

func TestLoadRejectsInvalidMemoryLimit(t *testing.T) {
	values := validValues()
	values["MEMORY_LIMIT_MB"] = "0"
	_, err := Parse(values)
	if err == nil || !strings.Contains(err.Error(), "MEMORY_LIMIT_MB") {
		t.Fatalf("expected memory validation error, got %v", err)
	}
}

func TestParseSupportsUnconfiguredProvider(t *testing.T) {
	values := validValues()
	values["UPSTREAM_BASE_URL"] = ""
	values["UPSTREAM_API_KEY"] = ""
	settings, err := Parse(values)
	if err != nil {
		t.Fatal(err)
	}
	if settings.UpstreamBaseURL != "" || settings.UpstreamAPIKey != "" {
		t.Fatalf("provider should be empty: %+v", settings)
	}
}

func TestParseNormalizesV1URLAndRejectsInvalidURL(t *testing.T) {
	settings, err := Parse(validValues())
	if err != nil || settings.UpstreamBaseURL != "https://provider.example" {
		t.Fatalf("normalized URL = %q, err = %v", settings.UpstreamBaseURL, err)
	}
	for _, value := range []string{"not-a-url", "ftp://provider.example", "https://provider.example:bad", "https://user:pass@provider.example", "https://provider.example?token=secret"} {
		values := validValues()
		values["UPSTREAM_BASE_URL"] = value
		if _, err := Parse(values); err == nil || !strings.Contains(err.Error(), "UPSTREAM_BASE_URL") {
			t.Errorf("URL %q was accepted: %v", value, err)
		}
	}
}

func TestParseRetainsDecimalSecondTimeoutCompatibility(t *testing.T) {
	values := validValues()
	values["UPSTREAM_CONNECT_TIMEOUT"] = "1.5"
	settings, err := Parse(values)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ConnectTimeout != 1500*time.Millisecond {
		t.Fatalf("connect timeout = %s", settings.ConnectTimeout)
	}
}

func TestParseRejectsInvalidEncryptionKey(t *testing.T) {
	values := validValues()
	values["RELAY_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 31))
	_, err := Parse(values)
	if err == nil || !strings.Contains(err.Error(), "RELAY_ENCRYPTION_KEY") {
		t.Fatalf("expected encryption key validation error, got %v", err)
	}
}

func TestRedactedNeverReturnsCredential(t *testing.T) {
	settings := Settings{AdminAPIKey: "admin", EncryptionKey: "encryption", UpstreamAPIKey: "upstream"}
	got := fmt.Sprint(settings.Redacted())
	if got == "" || strings.Contains(got, "admin") || strings.Contains(got, "encryption") || strings.Contains(got, "upstream") {
		t.Fatalf("secret leaked in redacted settings: %s", got)
	}
	redacted := settings.Redacted()
	for _, key := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY", "UPSTREAM_API_KEY"} {
		if !reflect.DeepEqual(redacted[key], map[string]bool{"is_configured": true}) {
			t.Errorf("%s = %#v", key, redacted[key])
		}
	}
}

func TestRestartOnlyChangesUsesStableOrder(t *testing.T) {
	base, err := Parse(validValues())
	if err != nil {
		t.Fatal(err)
	}
	next := base
	next.Host = "0.0.0.0"
	next.Port++
	next.DBPath = "other.db"
	next.DocsEnabled = !next.DocsEnabled
	next.AdminAPIKey = "new-secret"
	next.MemoryLimitMB++
	got := base.RestartOnlyChanges(next)
	want := []string{"HOST", "PORT", "DB_PATH", "DOCS_ENABLED"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("restart changes = %v, want %v", got, want)
	}
}

func TestEnvMapRoundTripsTypedValues(t *testing.T) {
	settings, err := Parse(validValues())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(settings.EnvMap())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != settings {
		t.Fatalf("round trip changed settings: before %+v after %+v", settings, parsed)
	}
}
