package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Provider is the redacted provider representation returned by the store.
type Provider struct {
	ID               int64
	Name             string
	BaseURL          string
	Enabled          bool
	APIKeyConfigured bool
}

type NewProvider struct {
	Name    string
	BaseURL string
	APIKey  string
	Enabled bool
}

type UpdateProvider struct {
	Name    string
	BaseURL string
	APIKey  string
	Enabled *bool
}

// SetProviderCacheEvictor registers a callback invoked after API-key rotation.
// The store remains independent from any upstream HTTP client cache.
func (s *Store) SetProviderCacheEvictor(fn func(providerID int64)) {
	s.providerCacheEvict = fn
}

func validateProviderURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalProviderHost(u.Hostname())) {
		return "", errors.New("base URL must use HTTPS (HTTP is allowed only for localhost/test hosts)")
	}
	if port := u.Port(); port != "" {
		var n int
		if _, err := fmt.Sscan(port, &n); err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid base URL port")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1") {
		u.Path = strings.TrimSuffix(u.Path, "/v1")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLocalProviderHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return strings.HasSuffix(host, ".test")
}

func (s *Store) CreateProvider(ctx context.Context, in NewProvider) (Provider, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Provider{}, errors.New("provider name is required")
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" {
		return Provider{}, errors.New("provider API key is required")
	}
	baseURL, err := validateProviderURL(in.BaseURL)
	if err != nil {
		return Provider{}, err
	}
	ciphertext, err := s.box.Seal(apiKey)
	if err != nil {
		return Provider{}, fmt.Errorf("encrypt provider API key: %w", err)
	}
	enabled := 1
	// New providers are enabled by default; disabled state can be set using UpdateProvider.
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO providers(name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?)`, name, baseURL, []byte(ciphertext), enabled, now, now)
	if err != nil {
		return Provider{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Provider{}, err
	}
	return Provider{ID: id, Name: name, BaseURL: baseURL, Enabled: true, APIKeyConfigured: true}, nil
}

func (s *Store) UpdateProvider(ctx context.Context, id int64, in UpdateProvider) (Provider, error) {
	if id <= 0 {
		return Provider{}, errors.New("provider ID is required")
	}
	var currentName, currentURL, currentCipher string
	var currentEnabled int
	if err := s.db.QueryRowContext(ctx, `SELECT name,base_url,api_key_ciphertext,enabled FROM providers WHERE id=?`, id).Scan(&currentName, &currentURL, &currentCipher, &currentEnabled); err != nil {
		return Provider{}, err
	}
	name := currentName
	if strings.TrimSpace(in.Name) != "" {
		name = strings.TrimSpace(in.Name)
	}
	baseURL := currentURL
	if strings.TrimSpace(in.BaseURL) != "" {
		var err error
		baseURL, err = validateProviderURL(in.BaseURL)
		if err != nil {
			return Provider{}, err
		}
	}
	ciphertext := currentCipher
	apiKey := strings.TrimSpace(in.APIKey)
	rotated := apiKey != ""
	if rotated {
		var err error
		ciphertext, err = s.box.Seal(apiKey)
		if err != nil {
			return Provider{}, fmt.Errorf("encrypt provider API key: %w", err)
		}
	}
	enabled := currentEnabled
	if in.Enabled != nil {
		if *in.Enabled {
			enabled = 1
		} else {
			enabled = 0
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE providers SET name=?,base_url=?,api_key_ciphertext=?,enabled=?,updated_at_utc=? WHERE id=?`, name, baseURL, []byte(ciphertext), enabled, nowUTC(), id); err != nil {
		return Provider{}, err
	}
	if rotated && s.providerCacheEvict != nil {
		s.providerCacheEvict(id)
	}
	return Provider{ID: id, Name: name, BaseURL: baseURL, Enabled: enabled == 1, APIKeyConfigured: s.providerConfigured(ciphertext)}, nil
}

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,base_url,enabled,api_key_ciphertext FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var enabled int
		var ciphertext string
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &enabled, &ciphertext); err != nil {
			return nil, err
		}
		p.Enabled, p.APIKeyConfigured = enabled == 1, s.providerConfigured(ciphertext)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProvider(ctx context.Context, id int64) (Provider, error) {
	var p Provider
	var enabled int
	var ciphertext string
	if err := s.db.QueryRowContext(ctx, `SELECT id,name,base_url,enabled,api_key_ciphertext FROM providers WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.BaseURL, &enabled, &ciphertext); err != nil {
		return Provider{}, err
	}
	p.Enabled, p.APIKeyConfigured = enabled == 1, s.providerConfigured(ciphertext)
	return p, nil
}

// ProviderAPIKey decrypts a provider credential for the upstream registry.
// Callers must keep the returned value in memory only and must never expose it
// in logs or API responses; catalog methods above intentionally never return it.
func (s *Store) ProviderAPIKey(ctx context.Context, id int64) (string, error) {
	var ciphertext string
	if err := s.db.QueryRowContext(ctx, `SELECT api_key_ciphertext FROM providers WHERE id=?`, id).Scan(&ciphertext); err != nil {
		return "", err
	}
	return s.box.Open(ciphertext)
}

// providerConfigured decrypts only to determine whether an API key is empty;
// the plaintext is never returned or logged.
func (s *Store) providerConfigured(ciphertext string) bool {
	if s == nil || s.box == nil || ciphertext == "" {
		return false
	}
	plain, err := s.box.Open(ciphertext)
	return err == nil && strings.TrimSpace(plain) != ""
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
