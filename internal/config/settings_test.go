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
	// 自动同步会访问所有启用渠道的上游并向数据库写入模型，属于改变现状的动作，
	// 所以默认关闭，必须由运维显式开启。
	if settings.ModelAutoSync {
		t.Fatal("model auto-sync must default to off")
	}
	// 出厂默认全部不限（0）。新部署不该因为一个出厂限额就在第一次请求时被拒，
	// 限额是运维的显式选择。
	if settings.TokenLimit5H != 0 || settings.TokenLimitDaily != 0 || settings.TokenLimitWeekly != 0 {
		t.Fatalf("unexpected token quota defaults: %+v", settings)
	}
	if settings.AmountLimit5H != 0 || settings.AmountLimitDaily != 0 || settings.AmountLimitWeekly != 0 {
		t.Fatalf("unexpected amount quota defaults: %+v", settings)
	}
}

func TestQuotaLimitsAcceptZeroAsUnlimitedAndYuanForAmounts(t *testing.T) {
	values := validValues()
	values["TOKEN_LIMIT_WEEKLY"] = "0"
	values["AMOUNT_LIMIT_5H"] = "0"
	values["AMOUNT_LIMIT_DAILY"] = "5"
	values["AMOUNT_LIMIT_WEEKLY"] = "12.5"
	settings, err := Parse(values)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TokenLimitWeekly != 0 || settings.AmountLimit5H != 0 {
		t.Fatalf("0 must mean unlimited: %+v", settings)
	}
	if settings.AmountLimitDaily != 5_000_000 || settings.AmountLimitWeekly != 12_500_000 {
		t.Fatalf("amount limits must be parsed as yuan into microyuan: %+v", settings)
	}
	// 回写时金额还原成「元」，便于管理台展示与再次编辑。
	env := settings.EnvMap()
	if env["AMOUNT_LIMIT_DAILY"] != "5" || env["AMOUNT_LIMIT_WEEKLY"] != "12.5" || env["AMOUNT_LIMIT_5H"] != "0" {
		t.Fatalf("unexpected amount env round-trip: %v", env)
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
	for _, gone := range []string{"UPSTREAM_BASE_URL", "UPSTREAM_API_KEY", "DOCS_ENABLED"} {
		if _, present := env[gone]; present {
			t.Errorf("%s should no longer be written back to relay.env", gone)
		}
	}
}

func TestQuotaLimitsRejectNegativeAndGarbage(t *testing.T) {
	for _, bad := range []struct{ name, value string }{
		{"TOKEN_LIMIT_WEEKLY", "-1"},
		{"TOKEN_LIMIT_5H", "abc"},
		{"AMOUNT_LIMIT_DAILY", "-0.5"},
		{"AMOUNT_LIMIT_WEEKLY", "十元"},
	} {
		values := validValues()
		values[bad.name] = bad.value
		if _, err := Parse(values); err == nil || !strings.Contains(err.Error(), bad.name) {
			t.Fatalf("%s=%q: expected a validation error naming the setting, got %v", bad.name, bad.value, err)
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
