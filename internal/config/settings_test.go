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
		"MODEL_AUTO_SYNC":          "true",
		"MODEL_SYNC_INTERVAL":      "30",
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
	if settings.UpstreamUserAgent != "civic-ai-relay/1.0" || settings.UpstreamSessionHeader != "" {
		t.Fatalf("unexpected upstream header defaults: %+v", settings)
	}
	if settings.ModelSyncInterval != 30*time.Minute || settings.MaxStreamDuration != 1800*time.Second {
		t.Fatalf("unexpected duration defaults: %+v", settings)
	}
	// 自动同步会访问所有启用渠道的上游并向数据库写入模型，属于改变现状的动作，
	// 所以默认关闭，必须由运维显式开启。
	if settings.ModelAutoSync {
		t.Fatal("model auto-sync must default to off")
	}
}

func TestUpstreamHeaderSettingsRoundTripAndRequireRestart(t *testing.T) {
	values := validValues()
	values["UPSTREAM_USER_AGENT"] = "custom-relay/2"
	values["UPSTREAM_SESSION_HEADER"] = "X-Session-Key"
	settings, err := Parse(values)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(settings.EnvMap())
	if err != nil || loaded != settings {
		t.Fatalf("round-trip mismatch: err=%v loaded=%+v", err, loaded)
	}
	changed := settings
	changed.UpstreamUserAgent = "next/3"
	changed.UpstreamSessionHeader = "-"
	if got := changed.RestartOnlyChanges(settings); len(got) != 2 || got[0] != "UPSTREAM_USER_AGENT" || got[1] != "UPSTREAM_SESSION_HEADER" {
		t.Fatalf("restart changes = %v", got)
	}
}

// 分组监测间隔要能「关掉」。用通用的 duration 解析会把 0 判成非法值，
// 于是「关掉监测」这个正常配置反而保存不了。
func TestGroupMonitorIntervalAcceptsZeroAsOff(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", 30 * time.Minute}, // 空 → 用默认
		{"0", 0},               // 关
		{"0s", 0},              // 回写后再读回来也得是关
		{"30m", 30 * time.Minute},
		{"5", 5 * time.Minute}, // 纯数字按分钟
		{"90s", 90 * time.Second},
	}
	for _, tc := range cases {
		values := validValues()
		values["GROUP_MONITOR_INTERVAL"] = tc.value
		settings, err := Parse(values)
		if err != nil {
			t.Fatalf("GROUP_MONITOR_INTERVAL=%q: %v", tc.value, err)
		}
		if settings.GroupMonitorInterval != tc.want {
			t.Fatalf("GROUP_MONITOR_INTERVAL=%q → %s, want %s", tc.value, settings.GroupMonitorInterval, tc.want)
		}
		// 回写必须能再解析回同一个值
		if _, err := Parse(settings.EnvMap()); err != nil {
			t.Fatalf("round-trip of %q failed: %v", tc.value, err)
		}
	}
	for _, bad := range []string{"-5m", "abc"} {
		values := validValues()
		values["GROUP_MONITOR_INTERVAL"] = bad
		if _, err := Parse(values); err == nil || !strings.Contains(err.Error(), "GROUP_MONITOR_INTERVAL") {
			t.Fatalf("GROUP_MONITOR_INTERVAL=%q: expected a validation error naming the setting, got %v", bad, err)
		}
	}
}

// LOG_LEVEL 打错时必须在保存/启动阶段就被拒绝。静默退回 INFO 会让人以为
// 「调成 DEBUG 了怎么还是没日志」——正是这个配置项以前「改了没反应」的老问题。
func TestLogLevelIsValidated(t *testing.T) {
	for _, good := range []string{"DEBUG", "info", "Warn", "error"} {
		values := validValues()
		values["LOG_LEVEL"] = good
		if _, err := Parse(values); err != nil {
			t.Errorf("LOG_LEVEL=%q should be accepted, got %v", good, err)
		}
	}
	// 空值被拒绝与 HOST/DB_PATH 等文本项一致（textDefault 只对「键缺失」用默认值）
	for _, bad := range []string{"", "verbose", "debugging", "1"} {
		values := validValues()
		values["LOG_LEVEL"] = bad
		if _, err := Parse(values); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
			t.Errorf("LOG_LEVEL=%q should be rejected naming the setting, got %v", bad, err)
		}
	}
}

// 已删除的遗留配置项写进 relay.env 也不该影响解析：它们现在是「未知键」，
// 会被原样保留但不再被读取。
func TestRemovedLegacySettingsAreIgnored(t *testing.T) {
	values := validValues()
	values["UPSTREAM_BASE_URL"] = "https://legacy.example"
	values["UPSTREAM_API_KEY"] = "legacy-secret"
	values["DOCS_ENABLED"] = "true"
	settings, err := Parse(values)
	if err != nil {
		t.Fatalf("legacy keys must not break parsing: %v", err)
	}
	env := settings.EnvMap()
	for _, gone := range []string{"UPSTREAM_BASE_URL", "UPSTREAM_API_KEY", "DOCS_ENABLED",
		"TOKEN_LIMIT_5H", "TOKEN_LIMIT_DAILY", "AMOUNT_LIMIT_WEEKLY", "PERIOD_LIMIT_START"} {
		if _, present := env[gone]; present {
			t.Errorf("%s should no longer be written back to relay.env", gone)
		}
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
	settings := Settings{AdminAPIKey: "admin", EncryptionKey: "encryption"}
	got := fmt.Sprint(settings.Redacted())
	if got == "" || strings.Contains(got, "admin") || strings.Contains(got, "encryption") {
		t.Fatalf("secret leaked in redacted settings: %s", got)
	}
	redacted := settings.Redacted()
	for _, key := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY"} {
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
	next.AdminAPIKey = "new-secret"
	next.MemoryLimitMB++
	got := base.RestartOnlyChanges(next)
	want := []string{"HOST", "PORT", "DB_PATH"}
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

func TestMaxStreamZeroRoundTrip(t *testing.T) {
	for _, value := range []string{"0", "0s"} {
		values := validValues()
		values["MAX_STREAM_DURATION"] = value
		settings, err := Parse(values)
		if err != nil || settings.MaxStreamDuration != 0 {
			t.Fatalf("value=%q parse=%v duration=%v", value, err, settings.MaxStreamDuration)
		}
		again, err := Parse(settings.EnvMap())
		if err != nil || again.MaxStreamDuration != 0 {
			t.Fatalf("value=%q roundtrip=%v duration=%v", value, err, again.MaxStreamDuration)
		}
	}
	for _, value := range []string{"-1", "-1s", ""} {
		values := validValues()
		values["MAX_STREAM_DURATION"] = value
		if _, err := Parse(values); err == nil {
			t.Fatalf("MAX_STREAM_DURATION=%q accepted", value)
		}
	}
}

func TestTimeoutCompatibility(t *testing.T) {
	old, err := Parse(validValues())
	if err != nil {
		t.Fatal(err)
	}
	if old.HeaderTimeout != 0 || old.UpstreamIdleTimeout != 0 || old.SSEHeartbeatInterval != 15*time.Second {
		t.Fatalf("old config timeouts: header=%v idle=%v heartbeat=%v", old.HeaderTimeout, old.UpstreamIdleTimeout, old.SSEHeartbeatInterval)
	}
	if _, exists := old.EnvMap()["UPSTREAM_HEADER_TIMEOUT"]; exists {
		t.Fatal("old config gained an explicit header timeout")
	}
	for _, name := range []string{"UPSTREAM_HEADER_TIMEOUT", "UPSTREAM_IDLE_TIMEOUT", "SSE_HEARTBEAT_INTERVAL"} {
		values := validValues()
		values[name] = "-1s"
		if _, err := Parse(values); err == nil {
			t.Fatalf("%s accepted negative duration", name)
		}
	}
	values := validValues()
	values["UPSTREAM_HEADER_TIMEOUT"] = "0"
	if _, err := Parse(values); err == nil {
		t.Fatal("explicit zero header timeout accepted")
	}
	initial, err := GenerateInitialSettings()
	if err != nil {
		t.Fatal(err)
	}
	if initial.HeaderTimeout != 30*time.Minute || initial.UpstreamIdleTimeout != 0 || initial.SSEHeartbeatInterval != 15*time.Second {
		t.Fatalf("initial timeouts: header=%v idle=%v heartbeat=%v", initial.HeaderTimeout, initial.UpstreamIdleTimeout, initial.SSEHeartbeatInterval)
	}
}
