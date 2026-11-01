// Package config owns the external relay.env configuration and its bootstrap
// material. Secrets are intentionally kept out of repository configuration.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"civic-ai-relay/internal/logging"
)

// SettingsError identifies a setting without echoing the supplied value.
type SettingsError struct {
	Name  string
	Cause error
}

func (e *SettingsError) Error() string {
	if e == nil {
		return "invalid setting"
	}
	return "invalid setting: " + e.Name
}

func (e *SettingsError) Unwrap() error { return e.Cause }

func invalid(name string, cause error) error {
	return &SettingsError{Name: name, Cause: cause}
}

// Settings is the typed runtime configuration for Civic Relay.
//
// AdminAPIKey and EncryptionKey are bootstrap secrets. They are never exposed
// by Redacted and should not be logged. EncryptionKey is the base64 encoding
// of exactly 32 random bytes.
type Settings struct {
	Host          string
	Port          int
	DBPath        string
	PluginDir     string
	AdminAPIKey   string
	EncryptionKey string
	// 注意：这里刻意不再有 UPSTREAM_BASE_URL / UPSTREAM_API_KEY —— 那是单上游
	// 时代的遗留配置。现在渠道（含各自的地址与密钥）全部存在数据库里、由管理台
	// 维护，密钥是加密存储的；留着这两个键会让人以为要在这里填上游，而它们既不
	// 生效、还会把 Key 明文写进 relay.env。同理 DOCS_ENABLED 也已移除：服务端
	// 从来没有 docs 路由，那个开关什么也不控制。
	ModelAutoSync     bool
	ModelSyncInterval time.Duration
	// GroupMonitorInterval 是分组可用性监测的轮询间隔，0 = 关闭监测。
	// 开启后每轮会对每个分组内启用中的渠道打一次上游 /v1/models（不产生
	// token 费用），结果落库并展示在管理台，不改变任何服务行为。
	GroupMonitorInterval   time.Duration
	MemoryLimitMB          int
	MaxBodyBytes           int64
	MaxOutputTokens        int
	MaxStreamDuration      time.Duration
	GlobalConcurrencyLimit int
	// RPMLimit 是唯一还留在全局配置里的限流值。窗口额度（5h/当日/本周/自定义
	// 周期 × Token/金额）在 v18 迁到了 model_groups：判定在预留事务内从密钥
	// 所属分组读取，管理台「模型组」维护——每组独立分配，全局配置不再参与。
	RPMLimit              int
	ConnectTimeout        time.Duration
	HeaderTimeout         time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	PoolTimeout           time.Duration
	UpstreamIdleTimeout   time.Duration
	UpstreamUserAgent     string
	UpstreamSessionHeader string
	SSEHeartbeatInterval  time.Duration
	RetentionDays         int
	LogLevel              string
	// CORSAllowedOrigins 控制浏览器跨域访问的放行策略：
	// "*"（默认）＝全部放行；"off"＝不下发任何 CORS 头；
	// 逗号分隔的来源白名单＝仅精确匹配的来源放行。运行时逐请求读取，
	// 管理台修改后立即生效（无需重启）。
	CORSAllowedOrigins string
}

var settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse converts an environment-style mapping into validated settings.
// Provider settings are optional during first start; providers are managed in
// the database after bootstrap. All quota and resource limits remain positive.
func Parse(values map[string]string) (Settings, error) {
	var s Settings
	var err error

	s.AdminAPIKey, err = required(values, "ADMIN_API_KEY")
	if err != nil {
		return Settings{}, err
	}
	s.EncryptionKey, err = required(values, "RELAY_ENCRYPTION_KEY")
	if err != nil {
		return Settings{}, err
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(s.EncryptionKey)
	if decodeErr != nil || len(decoded) != 32 {
		return Settings{}, invalid("RELAY_ENCRYPTION_KEY", decodeErr)
	}

	s.Host, err = textDefault(values, "HOST", "0.0.0.0")
	if err != nil {
		return Settings{}, err
	}
	s.DBPath, err = textDefault(values, "DB_PATH", "data/relay.db")
	if err != nil {
		return Settings{}, err
	}
	s.PluginDir, err = textDefault(values, "PLUGIN_DIR", "data/plugins")
	if err != nil {
		return Settings{}, err
	}
	s.LogLevel, err = textDefault(values, "LOG_LEVEL", "INFO")
	if err != nil {
		return Settings{}, err
	}
	// 级别名打错时直接拒绝保存/启动：静默退回 INFO 会让人以为「调成 DEBUG 了
	// 怎么还是没日志」，而这恰恰是 LOG_LEVEL 之前那个「改了没反应」的老问题。
	if _, levelErr := logging.ParseLevel(s.LogLevel); levelErr != nil {
		return Settings{}, invalid("LOG_LEVEL", levelErr)
	}
	s.Port, err = positiveInt(values, "PORT", 8000)
	if err != nil {
		return Settings{}, err
	}
	if s.Port > 65535 {
		return Settings{}, invalid("PORT", errors.New("out of range"))
	}

	// 自动同步默认**关闭**：它会定期访问所有启用渠道的上游、并向数据库写入新
	// 模型，属于会改变现状的动作，应当由运维显式开启。引导模板同样写 false。
	s.ModelAutoSync, err = boolean(values, "MODEL_AUTO_SYNC", false)
	if err != nil {
		return Settings{}, err
	}

	s.ModelSyncInterval, err = duration(values, "MODEL_SYNC_INTERVAL", 30*time.Minute, time.Minute)
	if err != nil {
		return Settings{}, err
	}
	s.GroupMonitorInterval, err = intervalOrZero(values, "GROUP_MONITOR_INTERVAL", 30*time.Minute)
	if err != nil {
		return Settings{}, err
	}
	s.MemoryLimitMB, err = positiveInt(values, "MEMORY_LIMIT_MB", 200)
	if err != nil {
		return Settings{}, err
	}
	s.MaxBodyBytes, err = bodyBytes(values)
	if err != nil {
		return Settings{}, err
	}
	s.MaxOutputTokens, err = positiveInt(values, "MAX_OUTPUT_TOKENS", 4096)
	if err != nil {
		return Settings{}, err
	}
	s.MaxStreamDuration, err = durationOrZero(values, "MAX_STREAM_DURATION", 1800*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.GlobalConcurrencyLimit, err = positiveInt(values, "GLOBAL_CONCURRENCY_LIMIT", 8)
	if err != nil {
		return Settings{}, err
	}
	s.RPMLimit, err = positiveInt(values, "RPM_LIMIT", 30)
	if err != nil {
		return Settings{}, err
	}
	s.ConnectTimeout, err = duration(values, "UPSTREAM_CONNECT_TIMEOUT", 10*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.HeaderTimeout, err = duration(values, "UPSTREAM_HEADER_TIMEOUT", 0, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.ReadTimeout, err = duration(values, "UPSTREAM_READ_TIMEOUT", 300*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.WriteTimeout, err = duration(values, "UPSTREAM_WRITE_TIMEOUT", 30*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.PoolTimeout, err = duration(values, "UPSTREAM_POOL_TIMEOUT", 10*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.UpstreamIdleTimeout, err = durationOrZero(values, "UPSTREAM_IDLE_TIMEOUT", 0, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.UpstreamUserAgent = "civic-ai-relay/1.0"
	if raw, ok := values["UPSTREAM_USER_AGENT"]; ok && strings.TrimSpace(raw) != "" {
		s.UpstreamUserAgent = strings.TrimSpace(raw)
	}
	s.UpstreamSessionHeader, err = textOptional(values, "UPSTREAM_SESSION_HEADER")
	if err != nil {
		return Settings{}, err
	}
	s.SSEHeartbeatInterval, err = durationOrZero(values, "SSE_HEARTBEAT_INTERVAL", 15*time.Second, time.Second)
	if err != nil {
		return Settings{}, err
	}
	s.RetentionDays, err = positiveInt(values, "RETENTION_DAYS", 7)
	if err != nil {
		return Settings{}, err
	}
	s.CORSAllowedOrigins, err = corsOrigins(values, "CORS_ALLOWED_ORIGINS")
	if err != nil {
		return Settings{}, err
	}
	return s, nil
}

// Load is a descriptive alias for Parse used by callers that load a mapping
// from disk before validation.
func Load(values map[string]string) (Settings, error) { return Parse(values) }

func required(values map[string]string, name string) (string, error) {
	value := strings.TrimSpace(values[name])
	if value == "" {
		return "", invalid(name, errors.New("required"))
	}
	return value, nil
}

func textDefault(values map[string]string, name, fallback string) (string, error) {
	if raw, ok := values[name]; ok {
		value := strings.TrimSpace(raw)
		if value == "" {
			return "", invalid(name, errors.New("empty"))
		}
		return value, nil
	}
	return fallback, nil
}

func boolean(values map[string]string, name string, fallback bool) (bool, error) {
	raw, ok := values[name]
	if !ok {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, invalid(name, errors.New("invalid boolean"))
	}
}

func positiveInt(values map[string]string, name string, fallback int) (int, error) {
	raw, ok := values[name]
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, invalid(name, err)
	}
	return value, nil
}

func textOptional(values map[string]string, name string) (string, error) {
	if raw, ok := values[name]; ok {
		return strings.TrimSpace(raw), nil
	}
	return "", nil
}

// corsOrigins 解析 CORS_ALLOWED_ORIGINS。缺省或空值视为 "*"（出厂默认不限，
// 与额度配置同一哲学：限制是运维的显式选择）；"off" 表示不下发任何 CORS 头；
// 其余必须是逗号分隔的来源白名单，每个来源形如 scheme://host[:port]，不允许
// 携带路径。白名单里混入 "*" 时整条配置按全放行处理。返回值是规范化后的
// 配置（去空格、小写 off），供 EnvMap 序列化与运行时逐请求判断。
func corsOrigins(values map[string]string, name string) (string, error) {
	raw, ok := values[name]
	value := strings.TrimSpace(raw)
	if !ok || value == "" {
		return "*", nil
	}
	if value == "*" {
		return "*", nil
	}
	if strings.EqualFold(value, "off") {
		return "off", nil
	}
	items := strings.Split(value, ",")
	cleaned := make([]string, 0, len(items))
	for _, item := range items {
		origin := strings.TrimSpace(item)
		if origin == "" {
			continue
		}
		if origin == "*" {
			return "*", nil
		}
		if err := validateOrigin(origin); err != nil {
			return "", invalid(name, err)
		}
		cleaned = append(cleaned, origin)
	}
	if len(cleaned) == 0 {
		return "", invalid(name, errors.New("no valid origin"))
	}
	return strings.Join(cleaned, ","), nil
}

// validateOrigin 校验单个来源：必须是 http/https 的 scheme://host[:port]，
// 不允许路径、查询串或片段——浏览器 Origin 头从来不含这些。
func validateOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("origin scheme must be http or https")
	}
	if parsed.Host == "" {
		return errors.New("origin requires a host")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("origin must not contain a path")
	}
	return nil
}

// intervalOrZero 解析「可以关闭」的轮询间隔：0 或 "0s" 表示关闭该功能。
// 与 duration 的区别只在允许零值——duration 要求严格为正，用它解析
// GROUP_MONITOR_INTERVAL 会导致「关掉监测」这个正常配置被判为非法。
func intervalOrZero(values map[string]string, name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := values[name]
	if !ok {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback, nil
	}
	if value == "0" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		// 纯数字按分钟解释，与其它时长配置保持一致
		numeric, numErr := strconv.ParseFloat(value, 64)
		if numErr != nil || math.IsNaN(numeric) || math.IsInf(numeric, 0) {
			return 0, invalid(name, err)
		}
		parsed = time.Duration(numeric * float64(time.Minute))
	}
	if parsed < 0 {
		return 0, invalid(name, errors.New("must not be negative"))
	}
	return parsed, nil
}

func duration(values map[string]string, name string, fallback, numericUnit time.Duration) (time.Duration, error) {
	raw, ok := values[name]
	if !ok {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, invalid(name, errors.New("empty"))
	}
	if numeric, err := strconv.ParseInt(value, 10, 64); err == nil {
		if numeric <= 0 || numeric > int64(math.MaxInt64)/int64(numericUnit) {
			return 0, invalid(name, errors.New("out of range"))
		}
		return time.Duration(numeric) * numericUnit, nil
	}
	// The Python configuration accepted decimal seconds for timeout values;
	// retain that compatibility while still storing a typed duration.
	if numeric, err := strconv.ParseFloat(value, 64); err == nil {
		if !math.IsNaN(numeric) && !math.IsInf(numeric, 0) && numeric > 0 && numeric <= float64(math.MaxInt64)/float64(numericUnit) {
			return time.Duration(numeric * float64(numericUnit)), nil
		}
		return 0, invalid(name, errors.New("out of range"))
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, invalid(name, err)
	}
	return parsed, nil
}

func durationOrZero(values map[string]string, name string, fallback, numericUnit time.Duration) (time.Duration, error) {
	if raw, ok := values[name]; ok {
		switch strings.TrimSpace(raw) {
		case "0", "0s":
			return 0, nil
		}
	}
	return duration(values, name, fallback, numericUnit)
}

func bodyBytes(values map[string]string) (int64, error) {
	if raw, ok := values["MAX_BODY_BYTES"]; ok {
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || value <= 0 {
			return 0, invalid("MAX_BODY_BYTES", err)
		}
		return value, nil
	}
	megabytes, err := positiveInt(values, "MAX_BODY_MB", 8)
	if err != nil {
		return 0, err
	}
	if megabytes > math.MaxInt64/(1024*1024) {
		return 0, invalid("MAX_BODY_MB", errors.New("out of range"))
	}
	return int64(megabytes) * 1024 * 1024, nil
}

// EnvMap returns a canonical environment representation. It contains secret
// values for persistence only; use Redacted for diagnostics and API responses.
func (s Settings) EnvMap() map[string]string {
	values := map[string]string{
		"ADMIN_API_KEY":            s.AdminAPIKey,
		"DB_PATH":                  s.DBPath,
		"PLUGIN_DIR":               s.PluginDir,
		"GLOBAL_CONCURRENCY_LIMIT": strconv.Itoa(s.GlobalConcurrencyLimit),
		"HOST":                     s.Host,
		"LOG_LEVEL":                s.LogLevel,
		"MAX_OUTPUT_TOKENS":        strconv.Itoa(s.MaxOutputTokens),
		"MAX_STREAM_DURATION":      s.MaxStreamDuration.String(),
		"MEMORY_LIMIT_MB":          strconv.Itoa(s.MemoryLimitMB),
		"MODEL_AUTO_SYNC":          strconv.FormatBool(s.ModelAutoSync),
		"MODEL_SYNC_INTERVAL":      s.ModelSyncInterval.String(),
		"GROUP_MONITOR_INTERVAL":   s.GroupMonitorInterval.String(),
		"PORT":                     strconv.Itoa(s.Port),
		"RELAY_ENCRYPTION_KEY":     s.EncryptionKey,
		"RETENTION_DAYS":           strconv.Itoa(s.RetentionDays),
		"RPM_LIMIT":                strconv.Itoa(s.RPMLimit),
		"UPSTREAM_CONNECT_TIMEOUT": s.ConnectTimeout.String(),
		"UPSTREAM_POOL_TIMEOUT":    s.PoolTimeout.String(),
		"UPSTREAM_READ_TIMEOUT":    s.ReadTimeout.String(),
		"UPSTREAM_USER_AGENT":      s.UpstreamUserAgent,
		"UPSTREAM_SESSION_HEADER":  s.UpstreamSessionHeader,
		"UPSTREAM_WRITE_TIMEOUT":   s.WriteTimeout.String(),
		"UPSTREAM_IDLE_TIMEOUT":    s.UpstreamIdleTimeout.String(),
		"SSE_HEARTBEAT_INTERVAL":   s.SSEHeartbeatInterval.String(),
		"CORS_ALLOWED_ORIGINS":     s.CORSAllowedOrigins,
	}
	if strings.TrimSpace(values["PLUGIN_DIR"]) == "" {
		values["PLUGIN_DIR"] = "data/plugins"
	}
	if s.HeaderTimeout > 0 {
		values["UPSTREAM_HEADER_TIMEOUT"] = s.HeaderTimeout.String()
	}
	const megabyte int64 = 1024 * 1024
	if s.MaxBodyBytes%megabyte == 0 {
		values["MAX_BODY_MB"] = strconv.FormatInt(s.MaxBodyBytes/megabyte, 10)
	} else {
		values["MAX_BODY_BYTES"] = strconv.FormatInt(s.MaxBodyBytes, 10)
	}
	return values
}

// Redacted returns settings suitable for logs and administrator diagnostics.
// Secret fields are replaced by an explicit configured flag, never a value.
func (s Settings) Redacted() map[string]any {
	values := make(map[string]any, len(s.EnvMap()))
	for name, value := range s.EnvMap() {
		values[name] = value
	}
	for _, name := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY"} {
		values[name] = map[string]bool{"is_configured": strings.TrimSpace(s.EnvMap()[name]) != ""}
	}
	return values
}

// RestartOnlyChanges lists only settings that require a process restart.
// Ordering is stable so callers can produce deterministic API responses.
func (s Settings) RestartOnlyChanges(next Settings) []string {
	changes := make([]string, 0, 8)
	if s.Host != next.Host {
		changes = append(changes, "HOST")
	}
	if s.Port != next.Port {
		changes = append(changes, "PORT")
	}
	if s.DBPath != next.DBPath {
		changes = append(changes, "DB_PATH")
	}
	// 上游超时在构建应用时就按**值**交给 Registry 与 HTTP 客户端了，运行中改只是
	// 写回 relay.env，不重启不生效。从前这里漏了这几项，管理台会显示「已即时生效」，
	// 运维据此以为调大了读超时 —— 实际还在用旧值，排查时极难想到。
	if s.ConnectTimeout != next.ConnectTimeout {
		changes = append(changes, "UPSTREAM_CONNECT_TIMEOUT")
	}
	if s.ReadTimeout != next.ReadTimeout {
		changes = append(changes, "UPSTREAM_READ_TIMEOUT")
	}
	if s.HeaderTimeout != next.HeaderTimeout {
		changes = append(changes, "UPSTREAM_HEADER_TIMEOUT")
	}
	if s.WriteTimeout != next.WriteTimeout {
		changes = append(changes, "UPSTREAM_WRITE_TIMEOUT")
	}
	if s.PoolTimeout != next.PoolTimeout {
		changes = append(changes, "UPSTREAM_POOL_TIMEOUT")
	}
	if s.UpstreamIdleTimeout != next.UpstreamIdleTimeout {
		changes = append(changes, "UPSTREAM_IDLE_TIMEOUT")
	}
	if s.UpstreamUserAgent != next.UpstreamUserAgent {
		changes = append(changes, "UPSTREAM_USER_AGENT")
	}
	if s.UpstreamSessionHeader != next.UpstreamSessionHeader {
		changes = append(changes, "UPSTREAM_SESSION_HEADER")
	}
	return changes
}

// EncryptionKeyBytes decodes and validates the configured AES-256 key.
func (s Settings) EncryptionKeyBytes() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, invalid("RELAY_ENCRYPTION_KEY", err)
	}
	return key, nil
}

func (s Settings) String() string {
	return fmt.Sprintf("Settings{Host:%s Port:%d DBPath:%s}", s.Host, s.Port, s.DBPath)
}

// ValidEnvKey is shared by the env parser and serializer.
func ValidEnvKey(name string) bool { return settingName.MatchString(name) }
