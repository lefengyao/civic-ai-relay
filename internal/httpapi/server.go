package httpapi

import (
	"context"
	"net/http"
	"time"

	"civic-ai-relay/internal/relay"
)

// Version is reported by /healthz so operators can confirm which build a
// container is actually running. The container build links it with
// -ldflags "-X main.version=..." via SetVersion.
var Version = "dev"

// SetVersion records the linked build version for the health endpoint.
func SetVersion(value string) {
	if value != "" {
		Version = value
	}
}

// HealthPayload is the JSON body returned by /healthz and /readyz.
type HealthPayload struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
}

// readinessProbe is the dependency check behind /readyz. A nil probe means the
// process has no state to verify and is therefore always ready.
type readinessProbe func(context.Context) error

func NewServer(service *relay.Service, maxBodyBytes int64, admin ...http.Handler) http.Handler {
	return NewServerWithAdmission(service, maxBodyBytes, nil, nil, admin...)
}

// NewServerWithAdmission 同 NewServer，并为公开接口接入准入检查：
// admit 在接受对话请求前调用（如 RSS 软保护，超限返回 503），
// streamAdmit 在流式转发中周期调用，超限时中止流。两者均可为 nil。
func NewServerWithAdmission(service *relay.Service, maxBodyBytes int64, admit, streamAdmit func() error, admin ...http.Handler) http.Handler {
	public := NewPublicHandler(service, maxBodyBytes)
	if concrete, ok := public.(*PublicHandler); ok {
		concrete.SetAdmission(admit)
		concrete.SetStreamAdmission(streamAdmit)
	}
	mux := http.NewServeMux()
	registerHealth(mux, service)
	if len(admin) == 0 || admin[0] == nil {
		// 无管理端（测试或纯转发实例）：其余路径仍交给公开接口分发。
		mux.Handle("/", public)
		return withAccessLog(mux)
	}
	mux.Handle("/admin", admin[0])
	mux.Handle("/admin/", admin[0])
	mux.Handle("/v1/", public)
	// 兼容 Base URL 漏填 /v1 的客户端：裸路径同样交给公开接口分发
	mux.Handle("/chat/completions", public)
	mux.Handle("/models", public)
	// 根路径与常见浏览器请求：直接访问时进入管理台，避免误以为服务没起来
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	return withAccessLog(mux)
}

// registerHealth 挂载两个免认证探针：
//
//   - /healthz 只证明进程活着，刻意不碰数据库——容器重启判定不应因一次
//     瞬时锁竞争而误判；
//   - /readyz 额外确认加密数据库仍能应答，供编排层决定是否放流量进来。
//
// 两者都不返回任何配置、密钥或上游信息。
func registerHealth(mux *http.ServeMux, service *relay.Service) {
	var probe readinessProbe
	if service != nil {
		probe = service.Ready
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !readOnlyRequest(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, HealthPayload{Status: "ok", Version: Version})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !readOnlyRequest(w, r) {
			return
		}
		if probe != nil {
			// 有界探针：数据库卡住时也要在编排层的超时之前给出结论。
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := probe(ctx); err != nil {
				// 只回错误码，不回错误正文：端点免认证，避免泄露内部细节。
				writeJSON(w, http.StatusServiceUnavailable, HealthPayload{Status: "unavailable", Version: Version, Error: "database_unavailable"})
				return
			}
		}
		writeJSON(w, http.StatusOK, HealthPayload{Status: "ok", Version: Version})
	})
}

func readOnlyRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, http.StatusMethodNotAllowed, HealthPayload{Status: "error", Version: Version, Error: "method_not_allowed"})
	return false
}
