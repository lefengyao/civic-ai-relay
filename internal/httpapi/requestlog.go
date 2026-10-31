package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"civic-ai-relay/internal/requestmeta"
)

func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = ensureRequestTrace(w, r)
		next.ServeHTTP(w, r)
	})
}

func ensureRequestTrace(w http.ResponseWriter, r *http.Request) *http.Request {
	if existing := requestmeta.FromContext(r.Context()); existing != nil {
		w.Header().Set("X-Request-Id", existing.ID)
		return r
	}
	// Always mint the server request ID. A client supplied X-Request-Id is
	// optional correlation metadata and must never become the ledger key.
	clientID := r.Header.Get("X-Client-Request-Id")
	if clientID == "" {
		clientID = r.Header.Get("X-Request-Id")
	}
	trace := requestmeta.NewTrace("", clientID, protocolForPath(r.URL.Path), time.Now())
	w.Header().Set("X-Request-Id", trace.ID)
	return r.WithContext(requestmeta.WithTrace(r.Context(), trace))
}

func protocolForPath(path string) string {
	if strings.HasSuffix(strings.TrimSuffix(path, "/"), "/responses") {
		return "responses"
	}
	return "chat"
}

func requestIDFromContext(ctx context.Context) string {
	if trace := requestmeta.FromContext(ctx); trace != nil {
		return trace.ID
	}
	return ""
}
