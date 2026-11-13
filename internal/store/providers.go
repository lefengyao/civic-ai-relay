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
	Protocol         string
	Enabled          bool
	APIKeyConfigured bool
	// 生效时段（v21）：Mode 为 always 时下面几列没有意义。
	ScheduleMode        string
	ScheduleStartUTC    string
	ScheduleEndUTC      string
	ScheduleStartMinute int
	ScheduleEndMinute   int
	ScheduleWeekdays    int
	// ScheduleActive 是按当前时刻算出的「是否在时段内」，只用于展示，不落库。
	ScheduleActive bool
	// GroupID / GroupName 是这条渠道归哪个组（v24 起一个渠道最多属于一个组）。
	// 由 ListProviders / GetProvider 填充：管理台列表要显示归属，组编辑的渠道
	// 勾选列表也要靠它把已经在别组的渠道灰掉。
	GroupID   int64
	GroupName string
}

type NewProvider struct {
	Name     string
	BaseURL  string
	APIKey   string
	Protocol string
	Enabled  bool
	Schedule ProviderSchedule
}

type UpdateProvider struct {
	Name     string
	BaseURL  string
	APIKey   string
	Protocol string
	Enabled  *bool
	// 时段是整段替换：只要给了 ScheduleMode，就按这几项重建时段；
	// 没给（nil）表示保持原时段不变 —— 只改名字的请求不会顺手清掉时段。
	ScheduleMode       *string
	ScheduleStart      *string
	ScheduleEnd        *string
	ScheduleDailyStart *string
	ScheduleDailyEnd   *string
	ScheduleWeekdays   *[]int
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func normalizeProviderProtocol(raw string) (string, error) {
	protocol := strings.ToLower(strings.TrimSpace(raw))
	if protocol == "" {
		return "openai", nil
	}
	switch protocol {
	case "openai", "anthropic", "gemini":
		return protocol, nil
	default:
		return "", fmt.Errorf("不支持的供应商协议 %q：可选 openai、anthropic、gemini", raw)
	}
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
	protocol, err := normalizeProviderProtocol(in.Protocol)
	if err != nil {
		return Provider{}, err
	}
	ciphertext, err := s.box.Seal(apiKey)
	if err != nil {
		return Provider{}, fmt.Errorf("上游 API Key 加密失败（请检查 RELAY_ENCRYPTION_KEY 是否有效）：%v", err)
	}
	enabled := 1
	// New providers are enabled by default; disabled state can be set using UpdateProvider.
	schedule := in.Schedule
	if strings.TrimSpace(schedule.Mode) == "" {
		schedule = ProviderSchedule{Mode: ProviderScheduleAlways}
	}
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO providers(name,base_url,protocol,api_key_ciphertext,enabled,schedule_mode,schedule_start_utc,schedule_end_utc,schedule_start_minute,schedule_end_minute,schedule_weekdays,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, baseURL, protocol, []byte(ciphertext), enabled,
		schedule.Mode, schedule.StartUTC, schedule.EndUTC, schedule.StartMinute, schedule.EndMinute, schedule.Weekdays,
		now, now)
	if err != nil {
		return Provider{}, friendlyProviderWriteError(err, name)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Provider{}, err
	}
	return Provider{ID: id, Name: name, BaseURL: baseURL, Protocol: protocol, Enabled: true, APIKeyConfigured: true,
		ScheduleMode: schedule.Mode, ScheduleStartUTC: schedule.StartUTC, ScheduleEndUTC: schedule.EndUTC,
		ScheduleStartMinute: schedule.StartMinute, ScheduleEndMinute: schedule.EndMinute, ScheduleWeekdays: schedule.Weekdays,
		ScheduleActive: schedule.ActiveAt(time.Now())}, nil
}

func (s *Store) UpdateProvider(ctx context.Context, id int64, in UpdateProvider) (Provider, error) {
	if id <= 0 {
		return Provider{}, errors.New("供应商 ID 不合法")
	}
	var currentName, currentURL, currentProtocol, currentCipher string
	var currentEnabled int
	var currentScheduleMode, currentStart, currentEnd string
	var currentStartMinute, currentEndMinute, currentWeekdays int
	if err := s.db.QueryRowContext(ctx, `SELECT name,base_url,protocol,api_key_ciphertext,enabled,schedule_mode,schedule_start_utc,schedule_end_utc,schedule_start_minute,schedule_end_minute,schedule_weekdays FROM providers WHERE id=?`, id).Scan(
		&currentName, &currentURL, &currentProtocol, &currentCipher, &currentEnabled,
		&currentScheduleMode, &currentStart, &currentEnd, &currentStartMinute, &currentEndMinute, &currentWeekdays); err != nil {
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
	protocol := currentProtocol
	if strings.TrimSpace(in.Protocol) != "" {
		normalized, protocolErr := normalizeProviderProtocol(in.Protocol)
		if protocolErr != nil {
			return Provider{}, protocolErr
		}
		protocol = normalized
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
	scheduleMode, scheduleStart, scheduleEnd := currentScheduleMode, currentStart, currentEnd
	scheduleStartMinute, scheduleEndMinute, scheduleWeekdays := currentStartMinute, currentEndMinute, currentWeekdays
	if in.ScheduleMode != nil {
		normalized, scheduleErr := NormalizeProviderSchedule(ProviderScheduleInput{
			Mode:       *in.ScheduleMode,
			Start:      derefString(in.ScheduleStart),
			End:        derefString(in.ScheduleEnd),
			DailyStart: derefString(in.ScheduleDailyStart),
			DailyEnd:   derefString(in.ScheduleDailyEnd),
			Weekdays:   derefWeekdays(in.ScheduleWeekdays),
		})
		if scheduleErr != nil {
			return Provider{}, scheduleErr
		}
		scheduleMode, scheduleStart, scheduleEnd = normalized.Mode, normalized.StartUTC, normalized.EndUTC
		scheduleStartMinute, scheduleEndMinute, scheduleWeekdays = normalized.StartMinute, normalized.EndMinute, normalized.Weekdays
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE providers SET name=?,base_url=?,protocol=?,api_key_ciphertext=?,enabled=?,schedule_mode=?,schedule_start_utc=?,schedule_end_utc=?,schedule_start_minute=?,schedule_end_minute=?,schedule_weekdays=?,updated_at_utc=? WHERE id=?`,
		name, baseURL, protocol, []byte(ciphertext), enabled,
		scheduleMode, scheduleStart, scheduleEnd, scheduleStartMinute, scheduleEndMinute, scheduleWeekdays,
		nowUTC(), id); err != nil {
		return Provider{}, friendlyProviderWriteError(err, name)
	}
	// 轮换密钥或修改地址后，缓存的 upstream client 已过期，必须驱逐重建；
	// 否则旧凭据/旧地址会一直用到进程重启。
	if s.providerCacheEvict != nil && (rotated || baseURL != currentURL || protocol != currentProtocol) {
		s.providerCacheEvict(id)
	}
	updated := Provider{ID: id, Name: name, BaseURL: baseURL, Protocol: protocol, Enabled: enabled == 1, APIKeyConfigured: s.providerConfigured(ciphertext),
		ScheduleMode: scheduleMode, ScheduleStartUTC: scheduleStart, ScheduleEndUTC: scheduleEnd,
		ScheduleStartMinute: scheduleStartMinute, ScheduleEndMinute: scheduleEndMinute, ScheduleWeekdays: scheduleWeekdays}
	updated.ScheduleActive = updated.ActiveAt(time.Now())
	return updated, nil
}

func derefWeekdays(value *[]int) []int {
	if value == nil {
		return nil
	}
	return *value
}

// providerSelect 是读渠道的唯一列清单：时段列必须一起读，否则管理台会看到
// 「时段已保存但状态永远是不限」这种自相矛盾的界面。
const providerSelect = `SELECT id,name,base_url,protocol,enabled,api_key_ciphertext,schedule_mode,schedule_start_utc,schedule_end_utc,schedule_start_minute,schedule_end_minute,schedule_weekdays FROM providers`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanProvider(row rowScanner) (Provider, error) {
	var p Provider
	var enabled int
	var ciphertext string
	if err := row.Scan(&p.ID, &p.Name, &p.BaseURL, &p.Protocol, &enabled, &ciphertext,
		&p.ScheduleMode, &p.ScheduleStartUTC, &p.ScheduleEndUTC,
		&p.ScheduleStartMinute, &p.ScheduleEndMinute, &p.ScheduleWeekdays); err != nil {
		return Provider{}, err
	}
	p.Enabled, p.APIKeyConfigured = enabled == 1, s.providerConfigured(ciphertext)
	if strings.TrimSpace(p.ScheduleMode) == "" {
		p.ScheduleMode = ProviderScheduleAlways
	}
	p.ScheduleActive = p.ActiveAt(time.Now())
	return p, nil
}

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, providerSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]Provider, 0)
	for rows.Next() {
		p, err := s.scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 归属分组：一次查全再回填，避免每条渠道各查一次（N+1）。
	owner, err := s.providerGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if ref, ok := owner[out[i].ID]; ok {
			out[i].GroupID, out[i].GroupName = ref.GroupID, ref.GroupName
		}
	}
	return out, nil
}

func (s *Store) GetProvider(ctx context.Context, id int64) (Provider, error) {
	p, err := s.scanProvider(s.db.QueryRowContext(ctx, providerSelect+` WHERE id=?`, id))
	if err != nil {
		return Provider{}, err
	}
	var groupID int64
	var groupName string
	if err := s.db.QueryRowContext(ctx, `SELECT g.id,g.name FROM group_providers gp JOIN model_groups g ON g.id=gp.group_id WHERE gp.provider_id=?`, id).Scan(&groupID, &groupName); err == nil {
		p.GroupID, p.GroupName = groupID, groupName
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Provider{}, err
	}
	return p, nil
}

// ProviderGroupRef 是「渠道归属」的最小描述。
type ProviderGroupRef struct {
	GroupID   int64
	GroupName string
}

// providerGroups 返回 provider_id → 所属分组。v24 起一条渠道最多属于一个组，
// 所以这里天然是 1:1；用 map 只是为了让调用方一次拿全。
func (s *Store) providerGroups(ctx context.Context) (map[int64]ProviderGroupRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT gp.provider_id,g.id,g.name FROM group_providers gp JOIN model_groups g ON g.id=gp.group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]ProviderGroupRef)
	for rows.Next() {
		var providerID int64
		var ref ProviderGroupRef
		if err := rows.Scan(&providerID, &ref.GroupID, &ref.GroupName); err != nil {
			return nil, err
		}
		out[providerID] = ref
	}
	return out, rows.Err()
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
// CASCADE foreign key; settled ledger rows are kept so group quota survives the
// deletion (historical requests also lose only their provider reference).
// Deleting a channel that still has an in-flight request is refused.
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("provider ID is required")
	}
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if err := s.ensureNoActiveReservation(ctx, `SELECT count(*) FROM key_reservations WHERE status='reserved' AND model_id IN (SELECT id FROM models WHERE provider_id=?)`, id); err != nil {
		return err
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
