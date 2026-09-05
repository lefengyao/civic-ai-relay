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
	return mux
}
