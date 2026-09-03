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
	Host                   string
	Port                   int
	DBPath                 string
	DocsEnabled            bool
	AdminAPIKey            string
	EncryptionKey          string
	UpstreamBaseURL        string
	UpstreamAPIKey         string
	ModelAutoSync          bool
	ModelSyncInterval      time.Duration
	MemoryLimitMB          int
	MaxBodyBytes           int64
	MaxOutputTokens        int
	MaxStreamDuration      time.Duration
	GlobalConcurrencyLimit int
	RPMLimit               int
	TokenLimit5H           int64
	TokenLimitDaily        int64
	ConnectTimeout         time.Duration
	ReadTimeout            time.Duration
	WriteTimeout           time.Duration
	PoolTimeout            time.Duration
	RetentionDays          int
	LogLevel               string
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
	s.LogLevel, err = textDefault(values, "LOG_LEVEL", "INFO")
	if err != nil {
		return Settings{}, err
	}
	s.Port, err = positiveInt(values, "PORT", 8000)
	if err != nil {
		return Settings{}, err
	}
	if s.Port > 65535 {
		return Settings{}, invalid("PORT", errors.New("out of range"))
	}

	s.DocsEnabled, err = boolean(values, "DOCS_ENABLED", false)
	if err != nil {
		return Settings{}, err
	}
	// Existing deployments defaulted background model synchronization on;
	// first-start bootstrap explicitly writes it off until a provider is added.
	s.ModelAutoSync, err = boolean(values, "MODEL_AUTO_SYNC", true)
	if err != nil {
		return Settings{}, err
	}

	s.UpstreamBaseURL, err = optionalURL(values, "UPSTREAM_BASE_URL")
	if err != nil {
		return Settings{}, err
	}
	s.UpstreamAPIKey = strings.TrimSpace(values["UPSTREAM_API_KEY"])

	s.ModelSyncInterval, err = duration(values, "MODEL_SYNC_INTERVAL", 30*time.Minute, time.Minute)
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
	s.MaxStreamDuration, err = duration(values, "MAX_STREAM_DURATION", 1800*time.Second, time.Second)
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
	s.TokenLimit5H, err = positiveInt64(values, "TOKEN_LIMIT_5H", 100000)
	if err != nil {
		return Settings{}, err
	}
	s.TokenLimitDaily, err = positiveInt64(values, "TOKEN_LIMIT_DAILY", 20000)
	if err != nil {
		return Settings{}, err
	}
	s.ConnectTimeout, err = duration(values, "UPSTREAM_CONNECT_TIMEOUT", 10*time.Second, time.Second)
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
	s.RetentionDays, err = positiveInt(values, "RETENTION_DAYS", 7)
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

func optionalURL(values map[string]string, name string) (string, error) {
	value := strings.TrimSpace(values[name])
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", invalid(name, err)
	}
	if parsed.Port() != "" {
		port, portErr := strconv.Atoi(parsed.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return "", invalid(name, portErr)
		}
	}
	value = strings.TrimRight(value, "/")
	if strings.HasSuffix(value, "/v1") {
		value = strings.TrimSuffix(value, "/v1")
	}
	return value, nil
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

func positiveInt64(values map[string]string, name string, fallback int64) (int64, error) {
	raw, ok := values[name]
	if !ok {
		return fallback, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0, invalid(name, err)
	}
	return value, nil
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
		"DOCS_ENABLED":             strconv.FormatBool(s.DocsEnabled),
		"GLOBAL_CONCURRENCY_LIMIT": strconv.Itoa(s.GlobalConcurrencyLimit),
		"HOST":                     s.Host,
		"LOG_LEVEL":                s.LogLevel,
		"MAX_OUTPUT_TOKENS":        strconv.Itoa(s.MaxOutputTokens),
		"MAX_STREAM_DURATION":      s.MaxStreamDuration.String(),
		"MEMORY_LIMIT_MB":          strconv.Itoa(s.MemoryLimitMB),
		"MODEL_AUTO_SYNC":          strconv.FormatBool(s.ModelAutoSync),
		"MODEL_SYNC_INTERVAL":      s.ModelSyncInterval.String(),
		"PORT":                     strconv.Itoa(s.Port),
		"RELAY_ENCRYPTION_KEY":     s.EncryptionKey,
		"RETENTION_DAYS":           strconv.Itoa(s.RetentionDays),
		"RPM_LIMIT":                strconv.Itoa(s.RPMLimit),
		"TOKEN_LIMIT_5H":           strconv.FormatInt(s.TokenLimit5H, 10),
		"TOKEN_LIMIT_DAILY":        strconv.FormatInt(s.TokenLimitDaily, 10),
		"UPSTREAM_API_KEY":         s.UpstreamAPIKey,
		"UPSTREAM_BASE_URL":        s.UpstreamBaseURL,
		"UPSTREAM_CONNECT_TIMEOUT": s.ConnectTimeout.String(),
		"UPSTREAM_POOL_TIMEOUT":    s.PoolTimeout.String(),
		"UPSTREAM_READ_TIMEOUT":    s.ReadTimeout.String(),
		"UPSTREAM_WRITE_TIMEOUT":   s.WriteTimeout.String(),
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
	for _, name := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY", "UPSTREAM_API_KEY"} {
		values[name] = map[string]bool{"is_configured": strings.TrimSpace(s.EnvMap()[name]) != ""}
	}
	return values
}

// RestartOnlyChanges lists only settings that require a process restart.
// Ordering is stable so callers can produce deterministic API responses.
func (s Settings) RestartOnlyChanges(next Settings) []string {
	changes := make([]string, 0, 4)
	if s.Host != next.Host {
		changes = append(changes, "HOST")
	}
	if s.Port != next.Port {
		changes = append(changes, "PORT")
	}
	if s.DBPath != next.DBPath {
		changes = append(changes, "DB_PATH")
	}
	if s.DocsEnabled != next.DocsEnabled {
		changes = append(changes, "DOCS_ENABLED")
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
