package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"civic-ai-relay/internal/config"
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
}

func NewAdminHandler(repo *store.Store, service *relay.Service, settings config.Settings, adminKey string) http.Handler {
	return &AdminHandler{repo: repo, service: service, settings: settings, adminKey: adminKey}
}

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
	if !h.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	switch {
	case path == "overview" && r.Method == http.MethodGet:
		h.overview(w, r)
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
	case strings.HasPrefix(path, "groups/"):
		h.groupItem(w, r, strings.TrimPrefix(path, "groups/"))
	case path == "keys" && r.Method == http.MethodGet:
		h.keysList(w, r)
	case path == "keys" && r.Method == http.MethodPost:
		h.keyCreate(w, r)
	case strings.HasPrefix(path, "keys/"):
		h.keyItem(w, r, strings.TrimPrefix(path, "keys/"))
	case path == "requests" && r.Method == http.MethodGet:
		h.requestsList(w, r)
	default:
		writeAdminError(w, http.StatusNotFound, "not_found")
	}
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

func (h *AdminHandler) overview(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	settings := h.settings
	h.mu.RUnlock()
	data, err := h.repo.Overview(r.Context(), time.Now().UTC(), int64(settings.RPMLimit), settings.TokenLimit5H, settings.TokenLimitDaily)
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

func safeSettings(s config.Settings) map[string]string {
	values := s.EnvMap()
	for _, key := range []string{"ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY", "UPSTREAM_API_KEY"} {
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
		if value == "" && (key == "ADMIN_API_KEY" || key == "RELAY_ENCRYPTION_KEY" || key == "UPSTREAM_API_KEY") {
			continue
		}
		values[key] = value
	}
	next, err := config.Parse(values)
	if err != nil {
		writeAdminError(w, 400, "invalid_config")
		return
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
		h.settings = next
		h.adminKey = next.AdminAPIKey
	}
	writeJSON(w, 200, map[string]any{"valid": true, "changed_fields": changed, "pending_restart_fields": pending, "settings": safeSettings(next)})
}

func (h *AdminHandler) providersList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListProviders(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (h *AdminHandler) providerCreate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if json.NewDecoder(r.Body).Decode(&payload) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	in := store.NewProvider{Name: payload.Name, BaseURL: payload.BaseURL, APIKey: payload.APIKey}
	value, err := h.repo.CreateProvider(r.Context(), in)
	if err != nil {
		writeAdminError(w, 400, "provider_invalid")
		return
	}
	writeJSON(w, 201, map[string]any{"data": value})
}
func (h *AdminHandler) providerItem(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if r.Method != http.MethodPut {
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
		writeAdminError(w, 400, "provider_invalid")
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
	InputPriceMicro  *int64   `json:"input_price_microyuan"`
	OutputPriceMicro *int64   `json:"output_price_microyuan"`
	Enabled          bool     `json:"enabled"`
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
func (h *AdminHandler) modelCreate(w http.ResponseWriter, r *http.Request) {
	var p modelPayload
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	value, err := h.repo.CreateModel(r.Context(), store.NewModel{ProviderID: p.ProviderID, PublicName: p.PublicName, UpstreamName: p.UpstreamName, InputPriceMicroyuan: microPrice(p.InputPrice, p.InputPriceMicro), OutputPriceMicroyuan: microPrice(p.OutputPrice, p.OutputPriceMicro), Enabled: p.Enabled})
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
	value, err := h.repo.UpdateModel(r.Context(), id, store.UpdateModel{ProviderID: provider, PublicName: p.PublicName, UpstreamName: p.UpstreamName, InputPriceMicroyuan: microPrice(p.InputPrice, p.InputPriceMicro), OutputPriceMicroyuan: microPrice(p.OutputPrice, p.OutputPriceMicro), Enabled: &p.Enabled})
	if err != nil {
		writeAdminError(w, 400, "model_invalid")
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}

func (h *AdminHandler) groupCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name     string  `json:"name"`
		ModelIDs []int64 `json:"model_ids"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	g, err := h.repo.CreateModelGroup(r.Context(), store.NewModelGroup{Name: p.Name})
	if err == nil {
		err = h.repo.ReplaceGroupModels(r.Context(), g.ID, p.ModelIDs)
	}
	if err != nil {
		writeAdminError(w, 400, "group_invalid")
		return
	}
	writeJSON(w, 201, map[string]any{"data": g})
}
func (h *AdminHandler) groupsList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.ListModelGroups(r.Context())
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (h *AdminHandler) groupItem(w http.ResponseWriter, r *http.Request, raw string) {
	parts := strings.Split(raw, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if len(parts) == 2 && parts[1] == "models" && r.Method == http.MethodPut {
		var p struct {
			ModelIDs []int64 `json:"model_ids"`
		}
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		if err := h.repo.ReplaceGroupModels(r.Context(), id, p.ModelIDs); err != nil {
			writeAdminError(w, 400, "group_invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeAdminError(w, 405, "method_not_allowed")
}

func (h *AdminHandler) keyCreate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name           string   `json:"name"`
		MaxConcurrency int      `json:"max_concurrency"`
		TokenLimit     *int64   `json:"token_limit_total"`
		MoneyLimitYuan *float64 `json:"money_limit_yuan_total"`
		GroupIDs       []int64  `json:"group_ids"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil {
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
	k, err := h.repo.CreateClientKey(r.Context(), store.NewClientKey{Name: p.Name, ConcurrencyLimit: p.MaxConcurrency, TokenLimit: p.TokenLimit, AmountLimitMicroyuan: money})
	if err == nil && len(p.GroupIDs) > 0 {
		err = h.repo.ReplaceKeyGroups(r.Context(), k.ID, p.GroupIDs)
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
	writeJSON(w, 200, map[string]any{"data": data})
}
func (h *AdminHandler) keyItem(w http.ResponseWriter, r *http.Request, raw string) {
	parts := strings.Split(raw, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeAdminError(w, 400, "invalid_id")
		return
	}
	if len(parts) == 2 && parts[1] == "groups" && r.Method == http.MethodPut {
		var p struct {
			GroupIDs []int64 `json:"group_ids"`
		}
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		if err := h.repo.ReplaceKeyGroups(r.Context(), id, p.GroupIDs); err != nil {
			writeAdminError(w, 400, "key_invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method != http.MethodPut {
		writeAdminError(w, 405, "method_not_allowed")
		return
	}
	var p store.UpdateClientKey
	if json.NewDecoder(r.Body).Decode(&p) != nil {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	value, err := h.repo.UpdateClientKey(r.Context(), id, p)
	if err != nil {
		writeAdminError(w, 400, "key_invalid")
		return
	}
	writeJSON(w, 200, map[string]any{"data": value})
}
func (h *AdminHandler) requestsList(w http.ResponseWriter, r *http.Request) {
	data, err := h.repo.RecentRequests(r.Context(), 50)
	if err != nil {
		writeAdminError(w, 500, "store_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
