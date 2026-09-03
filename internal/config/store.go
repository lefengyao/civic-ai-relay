package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	ensureMu      sync.Mutex
)

// DefaultPath resolves the only supported implicit configuration locations.
// An explicit override is always accepted, including on unsupported systems.
func DefaultPath(override string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		return filepath.Clean(value), nil
	}
	// Keep the environment override in this low-level helper as well as in
	// ManagedConfigPath so callers do not accidentally fall back to a project
	// local .env file.
	if value := strings.TrimSpace(os.Getenv("CIVIC_RELAY_CONFIG_FILE")); value != "" {
		return filepath.Clean(value), nil
	}
	switch runtime.GOOS {
	case "windows":
		root := strings.TrimSpace(os.Getenv("ProgramData"))
		if root == "" {
			root = `C:\ProgramData`
		}
		return filepath.Join(root, "CivicRelay", "relay.env"), nil
	case "linux":
		return "/etc/civic-relay/relay.env", nil
	default:
		return "", errors.New("CIVIC_RELAY_CONFIG_FILE is required on this operating system")
	}
}

// ManagedConfigPath resolves CIVIC_RELAY_CONFIG_FILE or the platform default.
func ManagedConfigPath() (string, error) {
	return DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE"))
}

// ParseEnvText parses the deliberately small, strict relay.env format.
// Empty lines and lines beginning with # are comments. Duplicate keys use
// last-write-wins, matching common env-file behavior, and are deterministic.
func ParseEnvText(text string) (map[string]string, error) {
	values := make(map[string]string)
	for lineNumber, line := range splitLines(text) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		}
		separator := strings.IndexByte(trimmed, '=')
		if separator <= 0 {
			return nil, fmt.Errorf("invalid configuration line %d", lineNumber)
		}
		name := strings.TrimSpace(trimmed[:separator])
		if !envKeyPattern.MatchString(name) {
			return nil, fmt.Errorf("invalid configuration key on line %d", lineNumber)
		}
		value, err := parseValue(trimmed[separator+1:])
		if err != nil {
			return nil, fmt.Errorf("invalid configuration value on line %d", lineNumber)
		}
		values[name] = value
	}
	return values, nil
}

func splitLines(text string) []string {
	// strings.Split handles a final newline without creating a meaningful
	// additional line; line numbers therefore remain intuitive for errors.
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

func parseValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '"':
		decoded, err := strconv.Unquote(value)
		if err != nil || !strings.HasPrefix(value, "\"") {
			return "", errors.New("invalid double-quoted value")
		}
		return decoded, nil
	case '\'':
		return parseSingleQuoted(value)
	default:
		return value, nil
	}
}

func parseSingleQuoted(value string) (string, error) {
	if len(value) < 2 || value[len(value)-1] != '\'' {
		return "", errors.New("unterminated single quote")
	}
	var out strings.Builder
	for i := 1; i < len(value)-1; i++ {
		if value[i] == '\'' {
			return "", errors.New("unescaped single quote")
		}
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		i++
		if i >= len(value)-1 {
			return "", errors.New("invalid escape")
		}
		switch value[i] {
		case '\\', '\'':
			out.WriteByte(value[i])
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		default:
			return "", errors.New("unsupported escape")
		}
	}
	return out.String(), nil
}

func quoteValue(value string) string {
	if value != strings.TrimSpace(value) || strings.ContainsAny(value, "#\\\"'\n\r\t") {
		return strconv.Quote(value)
	}
	return value
}

// SerializeEnvMapping produces sorted, canonical, parseable relay.env text.
func SerializeEnvMapping(values map[string]string) (string, error) {
	keys := make([]string, 0, len(values))
	for name, value := range values {
		if !envKeyPattern.MatchString(name) {
			return "", fmt.Errorf("invalid configuration key: %s", name)
		}
		if value == "" {
			// Empty values are meaningful for optional provider credentials.
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, name := range keys {
		out.WriteString(name)
		out.WriteByte('=')
		out.WriteString(quoteValue(values[name]))
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// ConfigStore reads and atomically persists one external configuration file.
type ConfigStore struct {
	Path string
}

func NewStore(path string) *ConfigStore {
	if strings.TrimSpace(path) == "" {
		return &ConfigStore{}
	}
	return &ConfigStore{Path: filepath.Clean(path)}
}

func (s *ConfigStore) ReadMapping() (map[string]string, error) {
	if s == nil || s.Path == "" {
		return nil, errors.New("configuration path is empty")
	}
	content, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	return ParseEnvText(string(content))
}

// Read is a short alias retained for callers that prefer store-like naming.
func (s *ConfigStore) Read() (map[string]string, error) { return s.ReadMapping() }

// Write validates and atomically replaces the managed file. Existing unknown
// keys are retained so operators can keep deployment-specific metadata.
func (s *ConfigStore) Write(settings Settings) error {
	if s == nil || s.Path == "" {
		return errors.New("configuration path is empty")
	}
	values := make(map[string]string)
	if _, err := os.Stat(s.Path); err == nil {
		var readErr error
		values, readErr = s.ReadMapping()
		if readErr != nil {
			return readErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for name, value := range settings.EnvMap() {
		values[name] = value
	}
	content, err := SerializeEnvMapping(values)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.Path), ".relay.env-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, s.Path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

// WriteSettings is an explicit alias for code that distinguishes settings
// persistence from other store writes.
func (s *ConfigStore) WriteSettings(settings Settings) error { return s.Write(settings) }

// BuildCandidate overlays a validated patch onto the current settings. Empty
// secret patches deliberately preserve the existing value.
func (s *ConfigStore) BuildCandidate(patch map[string]string, baseline *Settings) (Settings, error) {
	values := make(map[string]string)
	if baseline != nil {
		for name, value := range baseline.EnvMap() {
			values[name] = value
		}
	}
	if s != nil && s.Path != "" {
		if _, err := os.Stat(s.Path); err == nil {
			current, readErr := s.ReadMapping()
			if readErr != nil {
				return Settings{}, readErr
			}
			for name, value := range current {
				values[name] = value
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Settings{}, err
		}
	}
	for name, value := range patch {
		if (name == "ADMIN_API_KEY" || name == "RELAY_ENCRYPTION_KEY" || name == "UPSTREAM_API_KEY") && strings.TrimSpace(value) == "" {
			continue
		}
		values[name] = value
	}
	return Parse(values)
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func GenerateInitialSettings() (Settings, error) {
	adminBytes, err := randomBytes(32)
	if err != nil {
		return Settings{}, err
	}
	encryptionBytes, err := randomBytes(32)
	if err != nil {
		return Settings{}, err
	}
	values := map[string]string{
		"ADMIN_API_KEY":            "adm_" + base64.RawURLEncoding.EncodeToString(adminBytes),
		"RELAY_ENCRYPTION_KEY":     base64.StdEncoding.EncodeToString(encryptionBytes),
		"UPSTREAM_BASE_URL":        "",
		"UPSTREAM_API_KEY":         "",
		"MODEL_AUTO_SYNC":          "false",
		"MODEL_SYNC_INTERVAL":      "30m",
		"MEMORY_LIMIT_MB":          "200",
		"MAX_BODY_BYTES":           strconv.FormatInt(8*1024*1024, 10),
		"MAX_OUTPUT_TOKENS":        "4096",
		"MAX_STREAM_DURATION":      "1800s",
		"GLOBAL_CONCURRENCY_LIMIT": "8",
		"RPM_LIMIT":                "30",
		"TOKEN_LIMIT_5H":           "100000",
		"TOKEN_LIMIT_DAILY":        "20000",
		"RETENTION_DAYS":           "7",
		"DB_PATH":                  "data/relay.db",
		"HOST":                     "0.0.0.0",
		"PORT":                     "8000",
		"LOG_LEVEL":                "INFO",
		"DOCS_ENABLED":             "false",
		"UPSTREAM_CONNECT_TIMEOUT": "10s",
		"UPSTREAM_READ_TIMEOUT":    "300s",
		"UPSTREAM_WRITE_TIMEOUT":   "30s",
		"UPSTREAM_POOL_TIMEOUT":    "10s",
	}
	return Parse(values)
}

// Ensure loads an existing external configuration or creates a fresh one.
// The returned bootstrap key is non-empty only on a successful first write;
// the key is also persisted beside relay.env for operator retrieval.
func Ensure(path string) (settings Settings, bootstrapKey string, created bool, err error) {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if strings.TrimSpace(path) == "" {
		path, err = ManagedConfigPath()
		if err != nil {
			return Settings{}, "", false, err
		}
	}
	store := NewStore(path)
	if _, statErr := os.Stat(store.Path); statErr == nil {
		values, readErr := store.ReadMapping()
		if readErr != nil {
			return Settings{}, "", false, readErr
		}
		settings, parseErr := Parse(values)
		if parseErr != nil {
			return Settings{}, "", false, parseErr
		}
		return settings, "", false, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Settings{}, "", false, statErr
	}
	settings, err = GenerateInitialSettings()
	if err != nil {
		return Settings{}, "", false, err
	}
	if err = store.Write(settings); err != nil {
		return Settings{}, "", false, err
	}
	bootstrapPath := filepath.Join(filepath.Dir(store.Path), "bootstrap-admin-key.txt")
	if err = writeBootstrap(bootstrapPath, settings.AdminAPIKey); err != nil {
		return Settings{}, "", false, err
	}
	return settings, settings.AdminAPIKey, true, nil
}

func writeBootstrap(path, credential string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bootstrap-admin-key-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(credential + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}
