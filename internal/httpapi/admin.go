package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/plugin"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/web"
)

type AdminHandler struct {
	repo     *store.Store
	service  *relay.Service
	mu       sync.RWMutex
	adminKey string
	settings config.Settings
	// configPath 非空时，管理端保存的设置会原子写回 relay.env（重启不丢）。
	configPath string
	// onSettingsChange 在设置验证通过并持久化后同步调用，用于把新值热应用到
	// 运行中的服务（并发闸门、配额、输出上限等），无需重启进程。
	onSettingsChange func(config.Settings)
	// groupMonitor 提供「立即检测」的按需触发；为 nil 时相关接口返回 503。
	// 定时轮询由 cmd 层的后台循环负责，这里只做手动触发与历史查询。
	groupMonitor  GroupMonitor
	pluginManager *plugin.Manager
}

// GroupMonitor 是管理台手动触发分组监测所需的最小依赖面，由 relay.Monitor 实现。
type GroupMonitor interface {
	RunOnce(ctx context.Context, trigger string) (int, error)
	RunGroup(ctx context.Context, groupID int64) (store.GroupMonitorResult, error)
}

// SetGroupMonitor 注册分组监测器，启用 /admin/api/groups/monitor 等接口。
func (h *AdminHandler) SetGroupMonitor(m GroupMonitor) { h.groupMonitor = m }

// SetPluginManager registers the external WASM package manager. Keeping the
// manager optional preserves the lightweight admin handler used by unit tests.
func (h *AdminHandler) SetPluginManager(m *plugin.Manager) { h.pluginManager = m }

func NewAdminHandler(repo *store.Store, service *relay.Service, settings config.Settings, adminKey string) *AdminHandler {
	return &AdminHandler{repo: repo, service: service, settings: settings, adminKey: adminKey}
}

// SetConfigPersistence 启用设置持久化：保存时原子写回该 env 文件。
func (h *AdminHandler) SetConfigPersistence(path string) { h.configPath = path }

// SetSettingsListener 注册设置热应用回调（例如 config.Source.Set）。
func (h *AdminHandler) SetSettingsListener(fn func(config.Settings)) { h.onSettingsChange = fn }

func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(web.AdminHTML)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	// 登录态查询本身不需要鉴权：只回答「这个密钥是不是管理员」，供前端决定
	// 展示公开只读视图还是完整视图。
	if path == "session" && isReadMethod(r.Method) {
		writeJSON(w, http.StatusOK, map[string]any{"admin": h.adminAuthorized(r)})
		return
	}
	// 概览、用量分析、供应商、模型组公开只读；客户端 Key、请求日志、WASM 插件、
	// 运行配置以及一切写操作仍然要求管理员密钥。
	if !(isReadMethod(r.Method) && isPublicRead(path)) {
		if !h.requireAdmin(w, r) {
			return
		}
	}
	switch {
	case path == "overview" && r.Method == http.MethodGet:
		h.overview(w, r)
	case path == "analytics" && r.Method == http.MethodGet:
		h.analytics(w, r)
	case path == "config" && r.Method == http.MethodGet:
		h.configGet(w, r)
	case path == "config" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		h.configSave(w, r, r.Method == http.MethodPost)
	case path == "providers" && r.Method == http.MethodGet:
		h.providersList(w, r)
	case path == "providers" && r.Method == http.MethodPost:
		h.providerCreate(w, r)
	case strings.HasPrefix(path, "providers/"):
		h.providerItem(w, r, strings.TrimPrefix(path, "providers/"))
	case path == "models" && r.Method == http.MethodGet:
		h.modelsList(w, r)
	case path == "models" && r.Method == http.MethodPost:
		h.modelCreate(w, r)
	case strings.HasPrefix(path, "models/"):
		h.modelItem(w, r, strings.TrimPrefix(path, "models/"))
	case path == "groups" && r.Method == http.MethodGet:
		h.groupsList(w, r)
	case path == "groups" && r.Method == http.MethodPost:
		h.groupCreate(w, r)
	// 这两条必须排在下面的 HasPrefix("groups/") 之前：它们的第一段不是数字 ID，
	// 交给 groupItem 会被 ParseInt 判成 invalid_id。
	case path == "groups/health" && r.Method == http.MethodGet:
		h.groupsHealth(w, r)
	case path == "groups/monitor" && r.Method == http.MethodPost:
		h.groupsMonitorAll(w, r)
	case strings.HasPrefix(path, "groups/"):
		h.groupItem(w, r, strings.TrimPrefix(path, "groups/"))
	case path == "keys" && r.Method == http.MethodGet:
		h.keysList(w, r)
	case path == "keys" && r.Method == http.MethodPost:
		h.keyCreate(w, r)
	case path == "keys/hints" && r.Method == http.MethodGet:
		h.keysHints(w, r)
	case strings.HasPrefix(path, "keys/"):
		h.keyItem(w, r, strings.TrimPrefix(path, "keys/"))
	case path == "requests" && r.Method == http.MethodGet:
		h.requestsList(w, r)
	case strings.HasPrefix(path, "requests/") && r.Method == http.MethodGet:
		h.requestItem(w, r, strings.TrimPrefix(path, "requests/"))
	case path == "plugins" && r.Method == http.MethodGet:
		h.pluginsList(w, r)
	case path == "plugins" && r.Method == http.MethodPost:
		h.pluginInstall(w, r)
	case strings.HasPrefix(path, "plugins/"):
		h.pluginItem(w, r, strings.TrimPrefix(path, "plugins/"))
	default:
		writeAdminError(w, http.StatusNotFound, "not_found")
	}
}

func (h *AdminHandler) analytics(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.AnalyticsFilter{FromDate: query.Get("from_date"), ToDate: query.Get("to_date"), Bucket: query.Get("bucket"), Metric: query.Get("metric"), Status: query.Get("status")}
	if raw := query.Get("days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			writeAdminErrorDetail(w, 400, "invalid_query", "days must be an integer")
			return
		}
		filter.Days = days
	}
	for _, raw := range query["model_id"] {
		if raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeAdminErrorDetail(w, 400, "invalid_query", "model_id must be a positive integer")
			return
		}
		filter.ModelIDs = append(filter.ModelIDs, id)
		if len(filter.ModelIDs) > 20 {
			writeAdminErrorDetail(w, 400, "invalid_query", "at most 20 models may be selected")
			return
		}
	}
	data, err := h.repo.Analytics(r.Context(), time.Now().UTC(), filter)
	if err != nil {
		writeAdminErrorDetail(w, 400, "invalid_query", err.Error())
		return
	}
	writeJSON(w, 200, data)
}

func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// isPublicRead 列出未登录也能读的管理端路径。写操作永远不在此列。
func isPublicRead(path string) bool {
	switch {
	case path == "overview", path == "analytics", path == "providers", path == "models", path == "groups", path == "groups/health":
		return true
	case strings.HasPrefix(path, "providers/"):
		// remote-models 会带着已保存的上游密钥去请求上游，仍然只对管理员开放。
		return !strings.HasSuffix(path, "/remote-models")
	case strings.HasPrefix(path, "models/"), strings.HasPrefix(path, "groups/"):
		return true
	}
	return false
}

func (h *AdminHandler) adminAuthorized(r *http.Request) bool {
	h.mu.RLock()
	expected := h.adminKey
	h.mu.RUnlock()
	supplied, wanted := []byte(r.Header.Get("X-Admin-Key")), []byte(expected)
	return len(supplied) == len(wanted) && subtle.ConstantTimeCompare(supplied, wanted) == 1
}
func (h *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.adminAuthorized(r) {
		return true
	}
	writeAdminError(w, 401, "authentication_required")
	return false
}
func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

// writeAdminErrorDetail 在管理端错误结构上附加具体原因，形状与 json.go 的
// writeErrorDetail 一致（message 带原因、code 保持稳定），但不带 relay_error
// 的 type —— 那是面向上游客户端的标记，不属于管理台。
func writeAdminErrorDetail(w http.ResponseWriter, status int, code, detail string) {
	message := code
	if detail != "" {
		message = code + ": " + detail
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "code": code}})
}

// writeProviderError 透出渠道建/改失败的真实原因。只回一个 "provider_invalid"
// 会让操作员看到一串没有任何指向性的 toast，无法判断是地址格式、重名还是
// 加密失败（2026-09-16 实机踩到的正是这个：一个缺协议头的地址反复弹了 9 次）。
func writeProviderError(w http.ResponseWriter, err error) {
	writeAdminErrorDetail(w, 400, "provider_invalid", err.Error())
}

// writeDeleteResult reports the outcome of a delete, mapping "row missing" to
// 404 so operators can tell an unknown ID apart from a failed delete.
func writeDeleteResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]any{"ok": true})
	case errors.Is(err, sql.ErrNoRows):
		writeAdminError(w, 404, "not_found")
	default:
		writeAdminError(w, 400, "delete_failed")
	}
}

func (h *AdminHandler) overview(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	settings := h.settings
	h.mu.RUnlock()
	data, err := h.repo.Overview(r.Context(), time.Now().UTC(), store.QuotaLimits{
		RPMLimit:     int64(settings.RPMLimit),
		Token5H:      settings.TokenLimit5H,
		TokenDaily:   settings.TokenLimitDaily,
		TokenWeekly:  settings.TokenLimitWeekly,
		Amount5H:     settings.AmountLimit5H,
		AmountDaily:  settings.AmountLimitDaily,
		AmountWeekly: settings.AmountLimitWeekly,
		TokenPeriod:  settings.TokenLimitPeriod, AmountPeriod: settings.AmountLimitPeriod,
		PeriodDays: settings.PeriodLimitDays, PeriodStart: parsePeriodStart(settings.PeriodLimitStart),
	})
	if err != nil {
		writeAdminError(w, 500, "ledger_unavailable")
		return
	}
	data["ledger_status"] = "ok"
	if h.service != nil {
		data["concurrency"] = map[string]any{"active": h.service.ActiveGlobal(), "limit": settings.GlobalConcurrencyLimit}
	}
	writeJSON(w, 200, data)
}

func parsePeriodStart(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, raw)
	return t
}

func safeSettings(s config.Settings) map[string]string {
	values := s.EnvMap()
	if _, configured := values["UPSTREAM_HEADER_TIMEOUT"]; !configured {
		values["UPSTREAM_HEADER_TIMEOUT"] = ""
	}
	for _, key := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY"} {
		values[key] = ""
	}
	return values
}
func (h *AdminHandler) configGet(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	writeJSON(w, 200, map[string]any{"settings": safeSettings(h.settings)})
}

func (h *AdminHandler) configSave(w http.ResponseWriter, r *http.Request, validateOnly bool) {
	var body struct {
		Settings map[string]string `json:"settings"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAdminError(w, 400, "invalid_config")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	values := h.settings.EnvMap()
	for key, value := range body.Settings {
		if value == "" && (key == "ADMIN_API_KEY" || key == "RELAY_ENCRYPTION_KEY" || key == "UPSTREAM_HEADER_TIMEOUT") {
			continue
		}
		values[key] = value
	}
	next, err := config.Parse(values)
	if err != nil {
		// SettingsError 只包含字段名和安全的解析原因，不回显密钥或完整值。
		// 管理台需要知道是哪一项导致保存失败，否则周期配置等新增字段会被
		// 误认为「后端不支持」。
		writeAdminErrorDetail(w, 400, "invalid_config", err.Error())
		return
	}
	// An omitted/blank start field means "keep the active period's original
	// boundary". Only the explicit +day controls or an explicit timestamp may
	// change it; changing another quota must not shift/reset period usage.
	if current := h.settings; current.PeriodLimitDays > 0 && next.PeriodLimitDays > 0 {
		if value, supplied := body.Settings["PERIOD_LIMIT_START"]; !supplied || strings.TrimSpace(value) == "" {
			next.PeriodLimitStart = current.PeriodLimitStart
		}
	}
	pending := h.settings.RestartOnlyChanges(next)
	changed := make([]string, 0)
	current := h.settings.EnvMap()
	for key, value := range next.EnvMap() {
		if value != current[key] {
			changed = append(changed, key)
		}
	}
	if !validateOnly {
		// 先持久化再热应用：写盘失败时保持旧设置生效，避免"内存已改、重启即失"。
		if h.configPath != "" {
			if err := config.NewStore(h.configPath).Write(next); err != nil {
				writeAdminError(w, 500, "config_persist_failed")
				return
			}
		}
		h.settings = next
		h.adminKey = next.AdminAPIKey
		if h.onSettingsChange != nil {
			h.onSettingsChange(next)
		}
	}
	writeJSON(w, 200, map[string]any{"valid": true, "changed_fields": changed, "pending_restart_fields": pending, "settings": safeSettings(next)})
}

func (h *AdminHandler) providersList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListProviders(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	// 上游地址属于内部拓扑：公开只读接口只给名称与状态，不返回地址。
	if !h.adminAuthorized(r) {
		for i := range data {
			data[i].BaseURL = ""
		}
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (h *AdminHandler) providerCreate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name     string `json:"name"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Protocol string `json:"protocol"`
	}
	if json.NewDecoder(r.Body).Decode(&payload) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	in := store.NewProvider{Name: payload.Name, BaseURL: payload.BaseURL, APIKey: payload.APIKey, Protocol: payload.Protocol}
	value, err := h.repo.CreateProvider(r.Context(), in)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"data": value})
}
func (h *AdminHandler) providerItem(w http.ResponseWriter, r *http.Request, raw string) {
	parts := strings.Split(raw, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		writeDeleteResult(w, h.repo.DeleteProvider(r.Context(), id))
		return
	}
	if len(parts) == 2 && parts[1] == "remote-models" {
		if r.Method != http.MethodGet {
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		h.providerRemoteModels(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "models" && parts[2] == "import" {
		if r.Method != http.MethodPost {
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		h.providerImportModels(w, r, id)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodPut {
		writeAdminError(w, 405, "method_not_allowed")
		return
	}
	var in store.UpdateProvider
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	value, err := h.repo.UpdateProvider(r.Context(), id, in)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}

type modelPayload struct {
	ProviderID       int64    `json:"provider_id"`
	PublicName       string   `json:"public_name"`
	UpstreamName     string   `json:"upstream_name"`
	InputPrice       *float64 `json:"input_price"`
	OutputPrice      *float64 `json:"output_price"`
	CachedInputPrice *float64 `json:"cached_input_price"`
	InputPriceMicro  *int64   `json:"input_price_microyuan"`
	OutputPriceMicro *int64   `json:"output_price_microyuan"`
	CachedInputMicro *int64   `json:"cached_input_price_microyuan"`
	// Enabled 为指针：nil 表示保持原值，部分更新（如只改价格）不会误停用
	Enabled *bool `json:"enabled"`
}

func microPrice(value *float64, direct *int64) *int64 {
	if direct != nil {
		return direct
	}
	if value == nil {
		return nil
	}
	n := int64(*value*1000000 + 0.5)
	return &n
}

// providerRemoteModels previews the upstream model catalog for a provider
// without importing anything. Existing local public names are flagged.
func (h *AdminHandler) providerRemoteModels(w http.ResponseWriter, r *http.Request, id int64) {
	if h.service == nil {
		writeAdminError(w, 503, "relay_unavailable")
		return
	}
	names, err := h.service.PreviewProviderModels(r.Context(), id)
	if err != nil {
		writeAdminError(w, 502, "upstream_unavailable")
		return
	}
	provider, err := h.repo.GetProvider(r.Context(), id)
	if err != nil {
		writeAdminError(w, 404, "provider_not_found")
		return
	}
	existing := make(map[string]bool)
	for _, m := range h.mustListModels(r) {
		existing[m.PublicName] = true
	}
	type remoteModel struct {
		UpstreamName string `json:"upstream_name"`
		PublicName   string `json:"public_name"`
		Exists       bool   `json:"exists"`
	}
	data := make([]remoteModel, 0, len(names))
	for _, name := range names {
		public := provider.Name + "/" + name
		data = append(data, remoteModel{UpstreamName: name, PublicName: public, Exists: existing[public]})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

// providerImportModels bulk-imports selected upstream models as disabled and
// unpriced. Already-known public names are skipped so the call is idempotent.
func (h *AdminHandler) providerImportModels(w http.ResponseWriter, r *http.Request, id int64) {
	var p struct {
		Models []struct {
			UpstreamName string `json:"upstream_name"`
			PublicName   string `json:"public_name"`
		} `json:"models"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	provider, err := h.repo.GetProvider(r.Context(), id)
	if err != nil {
		writeAdminError(w, 404, "provider_not_found")
		return
	}
	existing := make(map[string]bool)
	for _, m := range h.mustListModels(r) {
		existing[m.PublicName] = true
	}
	created := make([]string, 0)
	for _, item := range p.Models {
		upstream := strings.TrimSpace(item.UpstreamName)
		if upstream == "" {
			continue
		}
		public := strings.TrimSpace(item.PublicName)
		if public == "" {
			public = provider.Name + "/" + upstream
		}
		if existing[public] {
			continue
		}
		if err := h.repo.CreateImportedModel(r.Context(), store.NewModel{ProviderID: id, PublicName: public, UpstreamName: upstream, Enabled: false}); err != nil {
			writeAdminError(w, 400, "model_invalid")
			return
		}
		existing[public] = true
		created = append(created, public)
	}
	writeJSON(w, 200, map[string]any{"created": created, "count": len(created)})
}

func (h *AdminHandler) mustListModels(r *http.Request) []store.Model {
	models, err := h.repo.ListModels(r.Context())
	if err != nil {
		return nil
	}
	return models
}

func (h *AdminHandler) modelCreate(w http.ResponseWriter, r *http.Request) {
	var p modelPayload
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	// 创建时 enabled 缺省视为启用
	value, err := h.repo.CreateModel(r.Context(), store.NewModel{ProviderID: p.ProviderID, PublicName: p.PublicName, UpstreamName: p.UpstreamName, InputPriceMicroyuan: microPrice(p.InputPrice, p.InputPriceMicro), OutputPriceMicroyuan: microPrice(p.OutputPrice, p.OutputPriceMicro), CachedInputPriceMicroyuan: microPrice(p.CachedInputPrice, p.CachedInputMicro), Enabled: p.Enabled == nil || *p.Enabled})
	if err != nil {
		writeAdminError(w, 400, "model_invalid")
		return
	}
	writeJSON(w, 201, map[string]any{"data": value})
}
func (h *AdminHandler) modelsList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListModels(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (h *AdminHandler) modelItem(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if r.Method == http.MethodDelete {
		writeDeleteResult(w, h.repo.DeleteModel(r.Context(), id))
		return
	}
	if r.Method != http.MethodPut {
		writeAdminError(w, 405, "method_not_allowed")
		return
	}
	var p modelPayload
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	var provider *int64
	if p.ProviderID > 0 {
		provider = &p.ProviderID
	}
	value, err := h.repo.UpdateModel(r.Context(), id, store.UpdateModel{ProviderID: provider, PublicName: p.PublicName, UpstreamName: p.UpstreamName, InputPriceMicroyuan: microPrice(p.InputPrice, p.InputPriceMicro), OutputPriceMicroyuan: microPrice(p.OutputPrice, p.OutputPriceMicro), CachedInputPriceMicroyuan: microPrice(p.CachedInputPrice, p.CachedInputMicro), Enabled: p.Enabled})
	if err != nil {
		writeAdminError(w, 400, "model_invalid")
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}

func (h *AdminHandler) groupCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name        string   `json:"name"`
		Rate        *float64 `json:"rate"`
		RateMilli   *int64   `json:"rate_milli"`
		ProviderIDs []int64  `json:"provider_ids"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	g, err := h.repo.CreateModelGroup(r.Context(), store.NewModelGroup{Name: p.Name, RateMilli: rateMilli(p.Rate, p.RateMilli)})
	if err == nil {
		err = h.repo.ReplaceGroupProviders(r.Context(), g.ID, p.ProviderIDs)
	}
	if err != nil {
		writeAdminError(w, 400, "group_invalid")
		return
	}
	writeJSON(w, 201, map[string]any{"data": g})
}

// rateMilli converts an optional float multiplier (1.5 = 1.5x) or direct
// thousandths value into a single rate_milli input; 0 means "use default".
func rateMilli(rate *float64, direct *int64) int64 {
	if direct != nil {
		return *direct
	}
	if rate == nil {
		return 0
	}
	return int64(*rate*1000 + 0.5)
}
func (h *AdminHandler) groupsList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListModelGroups(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

// groupsHealth 返回每个分组最近一次监测结果（键为分组 ID），以及当前是否开启了
// 定时监测。管理台分组列表一次拿全，不必逐组发请求。
func (h *AdminHandler) groupsHealth(w http.ResponseWriter, r *http.Request) {
	latest, err := h.repo.LatestGroupMonitorResults(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	h.mu.RLock()
	interval := h.settings.GroupMonitorInterval
	h.mu.RUnlock()
	data := make(map[string]any, len(latest))
	for id, result := range latest {
		data[strconv.FormatInt(id, 10)] = result
	}
	writeJSON(w, 200, map[string]any{
		"data":                data,
		"monitor_enabled":     interval > 0,
		"monitor_interval":    interval.String(),
		"monitor_interval_ms": interval.Milliseconds(),
	})
}

// groupsMonitorAll 立即监测全部分组（不等定时轮询）。
func (h *AdminHandler) groupsMonitorAll(w http.ResponseWriter, r *http.Request) {
	if h.groupMonitor == nil {
		writeAdminError(w, 503, "monitor_unavailable")
		return
	}
	count, err := h.groupMonitor.RunOnce(r.Context(), "manual")
	if err != nil {
		writeAdminErrorDetail(w, 500, "monitor_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"monitored": count})
}

// groupMonitorNow 立即监测单个分组并返回本轮结果。
func (h *AdminHandler) groupMonitorNow(w http.ResponseWriter, r *http.Request, id int64) {
	if h.groupMonitor == nil {
		writeAdminError(w, 503, "monitor_unavailable")
		return
	}
	result, err := h.groupMonitor.RunGroup(r.Context(), id)
	if err != nil {
		writeAdminErrorDetail(w, 400, "monitor_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"data": result})
}

// groupMonitorHistory 返回单个分组最近 50 轮结果，最新的在前。
func (h *AdminHandler) groupMonitorHistory(w http.ResponseWriter, r *http.Request, id int64) {
	history, err := h.repo.GroupMonitorHistory(r.Context(), id, 50)
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": history})
}
func (h *AdminHandler) groupItem(w http.ResponseWriter, r *http.Request, raw string) {
	parts := strings.Split(raw, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		writeDeleteResult(w, h.repo.DeleteModelGroup(r.Context(), id))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPut {
		var p struct {
			Name      string   `json:"name"`
			Enabled   *bool    `json:"enabled"`
			Rate      *float64 `json:"rate"`
			RateMilli *int64   `json:"rate_milli"`
		}
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		var rate *int64
		if p.Rate != nil || p.RateMilli != nil {
			v := rateMilli(p.Rate, p.RateMilli)
			rate = &v
		}
		g, err := h.repo.UpdateModelGroup(r.Context(), id, store.UpdateModelGroup{Name: p.Name, Enabled: p.Enabled, RateMilli: rate})
		if err != nil {
			writeAdminError(w, 400, "group_invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"data": g})
		return
	}
	if len(parts) == 2 && parts[1] == "monitor" {
		switch r.Method {
		case http.MethodPost:
			h.groupMonitorNow(w, r, id)
		case http.MethodGet:
			h.groupMonitorHistory(w, r, id)
		default:
			writeAdminError(w, 405, "method_not_allowed")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "overview" && r.Method == http.MethodGet {
		data, err := h.repo.GroupOverview(r.Context(), id, time.Now().UTC())
		if err != nil {
			writeAdminError(w, 500, "ledger_unavailable")
			return
		}
		writeJSON(w, 200, data)
		return
	}
	if len(parts) == 2 && parts[1] == "providers" {
		if r.Method == http.MethodGet {
			ids, err := h.repo.GroupProviderIDs(r.Context(), id)
			if err != nil {
				writeAdminError(w, 400, "group_invalid")
				return
			}
			writeJSON(w, 200, map[string]any{"data": ids})
			return
		}
		if r.Method == http.MethodPut {
			var p struct {
				ProviderIDs []int64 `json:"provider_ids"`
			}
			if json.NewDecoder(r.Body).Decode(&p) != nil {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if err := h.repo.ReplaceGroupProviders(r.Context(), id, p.ProviderIDs); err != nil {
				writeAdminError(w, 400, "group_invalid")
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeAdminError(w, 405, "method_not_allowed")
}

// expiryFromTTL 把「还有多少秒到期」换算成绝对时间点。>0 才设有效期，其余
// （nil / 0 / 负数）一律表示永久有效 —— 只有运维显式要求短时令牌时才该有到期时间，
// 默认值不该让任何一个 Key 悄悄失效。
func expiryFromTTL(seconds *int64) *time.Time {
	if seconds == nil || *seconds <= 0 {
		return nil
	}
	at := time.Now().UTC().Add(time.Duration(*seconds) * time.Second)
	return &at
}

func (h *AdminHandler) keyCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name           string   `json:"name"`
		MaxConcurrency int      `json:"max_concurrency"`
		TokenLimit     *int64   `json:"token_limit_total"`
		MoneyLimitYuan *float64 `json:"money_limit_yuan_total"`
		GroupID        *int64   `json:"group_id"`
		// TTLSeconds > 0 时创建为「短时令牌」：从现在起 N 秒后自动失效。
		// 缺省 / 0 / 负数 = 永久有效（与升级前的行为一致）。
		TTLSeconds *int64 `json:"ttl_seconds"`
		// Keyless = true 时创建「应用型凭据」：**不存在可用密钥**，客户端用公开的
		// 应用标识（请求头 X-App-Id）接入。给「软件里不能保存密钥」的客户端用。
		Keyless bool `json:"keyless"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	if p.GroupID != nil && *p.GroupID < 0 {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	if p.MaxConcurrency == 0 {
		p.MaxConcurrency = 1
	}
	var money *int64
	if p.MoneyLimitYuan != nil {
		v := int64(*p.MoneyLimitYuan*1000000 + 0.5)
		money = &v
	}
	k, err := h.repo.CreateClientKey(r.Context(), store.NewClientKey{Name: p.Name, ConcurrencyLimit: p.MaxConcurrency, TokenLimit: p.TokenLimit, AmountLimitMicroyuan: money, ExpiresAt: expiryFromTTL(p.TTLSeconds), Keyless: p.Keyless})
	if err == nil && p.GroupID != nil && *p.GroupID > 0 {
		if err = h.repo.SetKeyGroup(r.Context(), k.ID, *p.GroupID); err == nil {
			// 绑定成功后回填，创建响应里能看到归属分组
			k.GroupID = p.GroupID
		}
	}
	if err != nil {
		writeAdminError(w, 400, "key_invalid")
		return
	}
	writeJSON(w, 201, map[string]any{"data": k, "token": k.Token})
}
func (h *AdminHandler) keysList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListClientKeys(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	usage, err := h.repo.KeyUsage(r.Context())
	if err != nil {
		writeAdminError(w, 500, "usage_error")
		return
	}
	// 消耗口径与限额检查一致：所有预留（含未结算）都计入
	now := time.Now().UTC()
	items := make([]map[string]any, 0, len(data))
	for _, k := range data {
		stat := usage[k.ID]
		// AuthMode 让前端一眼分清「拿密钥接的」还是「拿应用标识接的」——
		// 两者的接入信息完全不同，混在一起会让人去找一个并不存在的密钥。
		authMode := "token"
		if k.Keyless() {
			authMode = "app"
		}
		item := map[string]any{
			"ID": k.ID, "Name": k.Name, "TokenLabel": k.TokenLabel,
			"AuthMode": authMode, "PublicID": k.PublicID,
			"GroupID": k.GroupID,
			"Enabled": k.Enabled, "ConcurrencyLimit": k.ConcurrencyLimit,
			"TokenLimit": k.TokenLimit, "AmountLimitMicroyuan": k.AmountLimitMicroyuan,
			"DisabledReason":      k.DisabledReason,
			"UsedTokens":          stat.Tokens,
			"UsedAmountMicroyuan": stat.AmountMicroyuan,
			"RequestCount":        stat.Requests,
			// 短时令牌：前端据此显示「剩余 X 天」/「已过期」，无需自己算时间差
			"Expired":       k.Expired(now),
			"ExpiresAtUTC":  nil,
			"ExpiresInSecs": nil,
		}
		if k.ExpiresAt != nil {
			item["ExpiresAtUTC"] = k.ExpiresAt.UTC().Format(time.RFC3339)
			item["ExpiresInSecs"] = int64(k.ExpiresAt.Sub(now).Seconds())
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"data": items})
}

// keysHints serves masked token prefixes through a dedicated endpoint: the key
// listing must never contain any character of a client secret.
func (h *AdminHandler) keysHints(w http.ResponseWriter, r *http.Request) {
	hints, err := h.repo.KeyTokenHints(r.Context())
	if err != nil {
		writeAdminError(w, 500, "hint_lookup_failed")
		return
	}
	out := make([]map[string]any, 0, len(hints))
	for id, hint := range hints {
		out = append(out, map[string]any{"id": id, "hint": hint})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (h *AdminHandler) keyItem(w http.ResponseWriter, r *http.Request, raw string) {
	parts := strings.Split(raw, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	// 删除客户端 Key：立即吊销访问，历史账目金额保留
	if len(parts) == 1 && r.Method == http.MethodDelete {
		writeDeleteResult(w, h.repo.DeleteClientKey(r.Context(), id))
		return
	}
	// 重置客户端密钥：旧 token 立即失效，新 token 只返回这一次
	if len(parts) == 2 && parts[1] == "rotate" {
		if r.Method != http.MethodPost {
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		value, err := h.repo.RotateClientKey(r.Context(), id)
		if err != nil {
			writeAdminError(w, 400, "key_invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"data": value, "token": value.Token})
		return
	}
	// 接入信息：把「其他软件该填什么」一次算清楚，供运维直接照抄。
	if len(parts) == 2 && parts[1] == "connection" {
		if r.Method != http.MethodGet {
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		h.keyConnection(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "group" {
		if r.Method == http.MethodGet {
			key, err := h.repo.GetClientKey(r.Context(), id)
			if err != nil {
				writeAdminError(w, 404, "not_found")
				return
			}
			var groupID any
			if key.GroupID != nil {
				groupID = *key.GroupID
			}
			writeJSON(w, 200, map[string]any{"data": groupID})
			return
		}
		if r.Method == http.MethodPut {
			var p struct {
				GroupID int64 `json:"group_id"`
			}
			if json.NewDecoder(r.Body).Decode(&p) != nil {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if p.GroupID < 0 {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if err := h.repo.SetKeyGroup(r.Context(), id, p.GroupID); err != nil {
				writeAdminError(w, 400, "key_invalid")
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	if r.Method != http.MethodPut {
		writeAdminError(w, 405, "method_not_allowed")
		return
	}
	// 与 keyCreate 相同的 snake_case 命名；nil 字段表示保持原值
	var p struct {
		Name           string   `json:"name"`
		Enabled        *bool    `json:"enabled"`
		Concurrency    *int     `json:"concurrency_limit"`
		TokenLimit     *int64   `json:"token_limit_total"`
		MoneyLimitYuan *float64 `json:"money_limit_yuan_total"`
		// 续期 / 改有效期：>0 = 从现在起 N 秒后过期（续期就是再传一次）；
		// 0 或负数 = 改为永久有效；缺省（nil）= 不动。三者必须分得开，
		// 否则「只改个名字」会把有效期顺手清掉。
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	in := store.UpdateClientKey{Name: p.Name, Enabled: p.Enabled}
	if p.TTLSeconds != nil {
		if *p.TTLSeconds > 0 {
			in.ExpiresAt = expiryFromTTL(p.TTLSeconds)
		} else {
			in.ClearExpiresAt = true
		}
	}
	if p.Concurrency != nil {
		in.ConcurrencyLimit = p.Concurrency
	}
	if p.TokenLimit != nil {
		in.TokenLimit = p.TokenLimit
	}
	if p.MoneyLimitYuan != nil {
		v := int64(*p.MoneyLimitYuan*1000000 + 0.5)
		in.AmountLimitMicroyuan = &v
	}
	value, err := h.repo.UpdateClientKey(r.Context(), id, in)
	if err != nil {
		writeAdminError(w, 400, "key_invalid")
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}

// keyConnection 汇总「其他软件接入这个 Key 需要填什么」。
//
// 为什么要这个接口：客户端 Key 的明文只在创建/重置时返回一次，之后运维手里只剩
// 脱敏缩略；Base URL 与模型名又分散在运行配置页和模型页。让运维自己拼极易拼错
// （Base URL 少带 /v1、模型名填成上游名），而拼错的后果是客户端报 400/404 ——
// 从错误信息里根本看不出是「填错了」还是「服务坏了」。
//
// 模型清单与公共 /v1/models 走同一份 SQL（store 侧 authorizedModelsSelect），
// 所以不会出现「管理台说已授权、客户端却拉不到」这种最难查的漂移。
func (h *AdminHandler) keyConnection(w http.ResponseWriter, r *http.Request, keyID int64) {
	key, err := h.repo.GetClientKey(r.Context(), keyID)
	if err != nil {
		writeAdminError(w, 404, "not_found")
		return
	}
	models, err := h.repo.AuthorizedModelsForKey(r.Context(), keyID)
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	// 一对一：接入信息里的分组是单个对象。组详情拿不到时降级为「只有 ID」，
	// 不影响接入信息整体可用。
	var group any
	enabledGroup := false
	if key.GroupID != nil {
		entry := map[string]any{"id": *key.GroupID, "name": "", "enabled": false, "rate_milli": store.DefaultGroupRateMilli}
		if catalog, err := h.repo.ListModelGroups(r.Context()); err == nil {
			for _, g := range catalog {
				if g.ID == *key.GroupID {
					entry["name"] = g.Name
					entry["enabled"] = g.Enabled
					entry["rate_milli"] = g.RateMilli
					enabledGroup = g.Enabled
					break
				}
			}
		} else {
			entry["enabled"] = true
			enabledGroup = true
		}
		group = entry
	}

	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.PublicName)
	}

	// token 明文无法从库里还原（只存 HMAC 摘要），所以这里只给脱敏缩略；
	// 运维需要明文时唯一途径是「重置密钥」。
	tokenHint := ""
	if hints, err := h.repo.KeyTokenHints(r.Context()); err == nil {
		tokenHint = hints[keyID]
	}

	// issues = 会直接导致客户端连不上的问题；warnings = 不影响连通性但值得知道。
	now := time.Now().UTC()
	issues := make([]string, 0, 4)
	warnings := make([]string, 0, 3)
	if !key.Enabled {
		issues = append(issues, "该 Key 已停用：客户端会收到 401 invalid_api_key")
	}
	// 短时令牌到期与「Key 被停用」是两回事：前者客户端换令牌即可恢复，
	// 后者要找管理员启用。错误码也刻意不同（token_expired vs invalid_api_key）。
	if key.Expired(now) {
		issues = append(issues, "该令牌已过期：客户端会收到 401 token_expired，请在管理台续期（编辑里重设有效期）")
	} else if key.ExpiresAt != nil && key.ExpiresAt.Sub(now) < 24*time.Hour {
		warnings = append(warnings, "该令牌将在 24 小时内过期，记得提前续期")
	}
	if key.GroupID == nil {
		issues = append(issues, "该 Key 未绑定任何分组：客户端拉不到模型，对话返回 400 model_not_allowed")
	} else if !enabledGroup {
		issues = append(issues, "所属分组已停用：授权模型为空")
	}
	if len(names) == 0 {
		issues = append(issues, "没有可用模型：分组内渠道下要有「已启用且已定价」的模型，客户端 /v1/models 才会返回内容")
	}
	authMode := "token"
	if key.Keyless() {
		authMode = "app"
		// 应用型凭据的边界必须写在运维看得见的地方：客户端里没有密钥可提取，
		// 但应用标识是公开的 —— 真正的门是「谁能访问到这个网关」。
		warnings = append(warnings, "应用型凭据：客户端不需要密钥（用请求头 X-App-Id 接入）。"+
			"应用标识是公开的，能访问到本网关的人都能调用，请把网关限制在可信网络内（内网 / 防火墙 / 反向代理鉴权）。")
	}

	base := clientBaseURL(r)
	// 短时令牌的有效期：null 表示永久有效（前端据此显示「永久」而不是「已过期」）
	var expiresAtUTC, expiresInSecs any
	if key.ExpiresAt != nil {
		expiresAtUTC = key.ExpiresAt.UTC().Format(time.RFC3339)
		expiresInSecs = int64(key.ExpiresAt.Sub(now).Seconds())
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{
		"key_id":          key.ID,
		"name":            key.Name,
		"enabled":         key.Enabled,
		"token_hint":      tokenHint,
		"token_label":     key.TokenLabel,
		"token_visible":   false,
		"auth_mode":       authMode,
		"app_id":          key.PublicID,
		"expired":         key.Expired(now),
		"expires_at_utc":  expiresAtUTC,
		"expires_in_secs": expiresInSecs,
		"group":           group,
		"models":          names,
		"base_url":        base,
		"models_url":      base + "/models",
		"chat_url":        base + "/chat/completions",
		"issues":          issues,
		"warnings":        warnings,
		"ready":           len(issues) == 0,
	}})
}

// clientBaseURL 按当前请求推断客户端该填的 Base URL。反向代理后面的对外地址
// 没法从 Host 得知（运维可能正从 127.0.0.1 打开管理台），所以这里给的是「按访问
// 管理台的方式」立刻能用的默认值，前端会提示可以改成对外地址。
func clientBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host + "/v1"
}

func (h *AdminHandler) requestsList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter, err := requestFilterFromQuery(query)
	if err != nil {
		writeAdminErrorDetail(w, 400, "invalid_query", err.Error())
		return
	}
	page, err := h.repo.QueryRequests(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidRequestQuery) {
			writeAdminErrorDetail(w, 400, "invalid_query", err.Error())
			return
		}
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, page)
}

func requestFilterFromQuery(query map[string][]string) (store.RequestFilter, error) {
	get := func(name string) string { return strings.TrimSpace(firstQueryValue(query, name)) }
	filter := store.RequestFilter{RequestID: get("request_id"), Status: get("status"), Protocol: get("protocol")}
	for _, field := range []struct {
		name string
		dst  *int64
	}{{"provider_id", &filter.ProviderID}, {"model_id", &filter.ModelID}, {"before_id", &filter.BeforeID}} {
		value := get(field.name)
		if value == "" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			return store.RequestFilter{}, fmt.Errorf("%s must be a positive integer", field.name)
		}
		*field.dst = n
	}
	if cursor := get("cursor"); cursor != "" {
		if get("before_id") != "" {
			return store.RequestFilter{}, errors.New("cursor and before_id cannot be combined")
		}
		id, err := store.DecodeRequestCursor(cursor)
		if err != nil {
			return store.RequestFilter{}, err
		}
		filter.BeforeID = id
	}
	if value := get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return store.RequestFilter{}, errors.New("limit must be an integer")
		}
		filter.Limit = n
	}
	for _, field := range []struct {
		name string
		dst  **time.Time
	}{{"from", &filter.FromUTC}, {"to", &filter.ToUTC}} {
		value := get(field.name)
		if value == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return store.RequestFilter{}, fmt.Errorf("%s must be RFC3339", field.name)
		}
		utc := at.UTC()
		*field.dst = &utc
	}
	if filter.FromUTC != nil && filter.ToUTC != nil && filter.FromUTC.After(*filter.ToUTC) {
		return store.RequestFilter{}, errors.New("from must not be after to")
	}
	return filter, nil
}

func firstQueryValue(query map[string][]string, name string) string {
	values := query[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (h *AdminHandler) requestItem(w http.ResponseWriter, r *http.Request, raw string) {
	if raw == "" {
		writeAdminError(w, 400, "invalid_request_id")
		return
	}
	requestID, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(requestID) == "" || strings.Contains(requestID, "/") || strings.Contains(requestID, "\\") {
		writeAdminError(w, 400, "invalid_request_id")
		return
	}
	data, err := h.repo.GetRequest(r.Context(), requestID)
	if errors.Is(err, sql.ErrNoRows) {
		writeAdminError(w, 404, "not_found")
		return
	}
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (h *AdminHandler) pluginsList(w http.ResponseWriter, r *http.Request) {
	if h.pluginManager == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "plugin_manager_unavailable")
		return
	}
	plugins, err := h.pluginManager.List(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": plugins})
}

func (h *AdminHandler) pluginInstall(w http.ResponseWriter, r *http.Request) {
	if h.pluginManager == nil {
		writeAdminError(w, 503, "plugin_manager_unavailable")
		return
	}
	limited := http.MaxBytesReader(w, r.Body, plugin.MaxPackageBytes+1)
	defer limited.Close()
	var src io.Reader = limited
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		if err := r.ParseMultipartForm(plugin.MaxPackageBytes); err != nil {
			writeAdminErrorDetail(w, 400, "invalid_plugin", err.Error())
			return
		}
		file, _, err := r.FormFile("package")
		if err != nil {
			writeAdminError(w, 400, "plugin_package_required")
			return
		}
		defer file.Close()
		src = file
	}
	value, err := h.pluginManager.Install(r.Context(), src)
	if err != nil {
		writeAdminErrorDetail(w, 400, "invalid_plugin", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": value})
}

func (h *AdminHandler) pluginItem(w http.ResponseWriter, r *http.Request, raw string) {
	if h.pluginManager == nil {
		writeAdminError(w, 503, "plugin_manager_unavailable")
		return
	}
	parts := strings.Split(raw, "/")
	if len(parts) == 0 || parts[0] == "" || strings.ContainsAny(parts[0], `\\`) {
		writeAdminError(w, 400, "invalid_plugin_id")
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || strings.Contains(id, "/") {
		writeAdminError(w, 400, "invalid_plugin_id")
		return
	}
	var value store.Plugin
	switch {
	case len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "enable":
		value, err = h.pluginManager.Enable(r.Context(), id)
	case len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "disable":
		value, err = h.pluginManager.Disable(r.Context(), id)
	case len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "start":
		value, err = h.pluginManager.Start(r.Context(), id)
	case len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "stop":
		value, err = h.pluginManager.Stop(r.Context(), id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		err = h.pluginManager.Delete(r.Context(), id)
		if err == nil {
			writeJSON(w, 200, map[string]any{"deleted": true})
			return
		}
	default:
		writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeAdminError(w, 404, "not_found")
		return
	}
	if err != nil {
		writeAdminErrorDetail(w, 400, "plugin_operation_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}
