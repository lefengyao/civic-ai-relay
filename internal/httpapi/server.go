package httpapi

import (
	"net/http"

	"civic-ai-relay/internal/relay"
)

func NewServer(service *relay.Service, maxBodyBytes int64) http.Handler {
	return NewPublicHandler(service, maxBodyBytes)
}
