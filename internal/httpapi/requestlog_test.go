package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"civic-ai-relay/internal/requestmeta"
)

func TestPublicErrorCarriesServerRequestID(t *testing.T) {
	handler := NewPublicHandler(nil, 1<<20)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-Id", "client-id")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	serverID := recorder.Header().Get("X-Request-Id")
	if serverID == "" || serverID == "client-id" {
		t.Fatalf("server request id=%q", serverID)
	}
	var payload struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.RequestID != serverID {
		t.Fatalf("body request id=%q header=%q", payload.Error.RequestID, serverID)
	}
}

func TestRequestLogInjectsStableRequestID(t *testing.T) {
	var seen string
	handler := withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFromContext(r.Context())
		w.WriteHeader(http.StatusBadRequest)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "client supplied\ninvalid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if seen == "" || recorder.Header().Get("X-Request-Id") != seen || seen == "client supplied invalid" {
		t.Fatalf("seen=%q response=%q", seen, recorder.Header().Get("X-Request-Id"))
	}
}

func TestRequestLogKeepsClientIDSeparate(t *testing.T) {
	var clientID string
	handler := withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID = requestmeta.FromContext(r.Context()).ClientID
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "client-123")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Header().Get("X-Request-Id") == "client-123" || clientID != "client-123" {
		t.Fatalf("server id=%q client id=%q", recorder.Header().Get("X-Request-Id"), clientID)
	}
}
