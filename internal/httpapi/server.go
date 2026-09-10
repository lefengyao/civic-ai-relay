package httpapi

import (
	"net/http"

	"civic-ai-relay/internal/relay"
)

func NewServer(service *relay.Service, maxBodyBytes int64, admin ...http.Handler) http.Handler {
	public := NewPublicHandler(service, maxBodyBytes)
	if len(admin) == 0 || admin[0] == nil {
		return public
	}
	mux := http.NewServeMux()
	mux.Handle("/admin", admin[0])
	mux.Handle("/admin/", admin[0])
	mux.Handle("/v1/", public)
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
	return mux
}
