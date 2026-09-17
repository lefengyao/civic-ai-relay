package store

import (
	"context"
	"database/sql"
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

// SetProviderCacheEvictor registers a callback invoked after a provider's
// API key rotates or its base URL changes, so cached upstream clients are
// rebuilt with the new credentials instead of serving stale ones.
func (s *Store) SetProviderCacheEvictor(fn func(providerID int64)) {
	s.providerCacheEvict = fn
}

// validateProviderURL 的错误信息会直接透传到管理台 toast（见 httpapi.writeProviderError），
// 因此这里的文案以"照着改就能过"为目标，不用 "base URL is invalid" 这类无指向性的说法。
func validateProviderURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("上游地址不能为空")
	}
	// 漏写协议头是最常见的错误（如 127.0.0.1:6000）。此时 url.Parse 只会回
	// "first path segment in URL cannot contain colon"，对使用者毫无指向性，
	// 所以在这里单独识别并给出可直接照抄的写法。
	if !strings.Contains(raw, "://") {
		return "", fmt.Errorf("上游地址缺少协议头 %q：要写成 http://主机:端口 或 https://域名，例如 http://172.17.0.1:6000", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("上游地址无法解析：%v", err)
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("上游地址须是不带账号、查询串、锚点的完整地址")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalProviderHost(u.Hostname())) {
		return "", fmt.Errorf("%s://%s 不在放行名单内：非 HTTPS 只允许 localhost、*.localhost、*.test，以及回环/私有 IP（127.0.0.1、10.x、172.16-31.x、192.168.x）。公网 IP、公网域名、容器名/服务名都必须用 https://", u.Scheme, u.Host)
	}
	if port := u.Port(); port != "" {
		var n int
		if _, err := fmt.Sscan(port, &n); err != nil || n < 1 || n > 65535 {
			return "", errors.New("上游地址的端口不合法（应为 1-65535）")
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

// friendlyProviderWriteError 把 SQLite 的约束错误翻译成操作台看得懂的说明。
// providers.name 是 UNIQUE，重名是本表唯一会撞的约束。
func friendlyProviderWriteError(err error, name string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed: providers.name") {
		return fmt.Errorf("供应商名称 %q 已存在，请换一个名称", name)
	}
	return err
}

func (s *Store) CreateProvider(ctx context.Context, in NewProvider) (Provider, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Provider{}, errors.New("供应商名称不能为空")
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" {
		return Provider{}, errors.New("上游 API Key 不能为空")
	}
	baseURL, err := validateProviderURL(in.BaseURL)
	if err != nil {
		return Provider{}, err
	}
	ciphertext, err := s.box.Seal(apiKey)
	if err != nil {
		return Provider{}, fmt.Errorf("上游 API Key 加密失败（请检查 RELAY_ENCRYPTION_KEY 是否有效）：%v", err)
	}
	enabled := 1
	// New providers are enabled by default; disabled state can be set using UpdateProvider.
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO providers(name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?)`, name, baseURL, []byte(ciphertext), enabled, now, now)
	if err != nil {
		return Provider{}, friendlyProviderWriteError(err, name)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Provider{}, err
	}
	return Provider{ID: id, Name: name, BaseURL: baseURL, Enabled: true, APIKeyConfigured: true}, nil
}

func (s *Store) UpdateProvider(ctx context.Context, id int64, in UpdateProvider) (Provider, error) {
	if id <= 0 {
		return Provider{}, errors.New("供应商 ID 不合法")
	}
	var currentName, currentURL, currentCipher string
	var currentEnabled int
	if err := s.db.QueryRowContext(ctx, `SELECT name,base_url,api_key_ciphertext,enabled FROM providers WHERE id=?`, id).Scan(&currentName, &currentURL, &currentCipher, &currentEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Provider{}, fmt.Errorf("供应商 #%d 不存在（可能已被删除，请刷新页面）", id)
		}
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
			return Provider{}, fmt.Errorf("上游 API Key 加密失败（请检查 RELAY_ENCRYPTION_KEY 是否有效）：%v", err)
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
		return Provider{}, friendlyProviderWriteError(err, name)
	}
	// 轮换密钥或修改地址后，缓存的 upstream client 已过期，必须驱逐重建；
	// 否则旧凭据/旧地址会一直用到进程重启。
	if s.providerCacheEvict != nil && (rotated || baseURL != currentURL) {
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
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]Provider, 0)
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
// DeleteProvider removes a channel. Its models are removed by the ON DELETE
// CASCADE foreign key; historical requests keep their amount but lose the
// provider reference (ON DELETE SET NULL).
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("provider ID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	if s.providerCacheEvict != nil {
		s.providerCacheEvict(id)
	}
	return nil
}

func (s *Store) providerConfigured(ciphertext string) bool {
	if s == nil || s.box == nil || ciphertext == "" {
		return false
	}
	plain, err := s.box.Open(ciphertext)
	return err == nil && strings.TrimSpace(plain) != ""
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
