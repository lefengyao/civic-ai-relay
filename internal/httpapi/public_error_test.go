package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/upstream"
)

func TestWriteServiceErrorKeepsJoinedCausePrivate(t *testing.T) {
	const secret = "sqlite /var/lib/relay/private.db https://provider.example/v1?api_key=secret"
	response := httptest.NewRecorder()
	h := &PublicHandler{}
	h.writeServiceError(response, errors.Join(relay.ErrSettlementUnavailable, errors.New(secret)))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "settlement_unavailable" {
		t.Fatalf("code = %q, want settlement_unavailable", payload.Error.Code)
	}
	if payload.Error.Message != "settlement_unavailable: request settlement is temporarily unavailable" {
		t.Fatalf("message = %q, want stable settlement detail", payload.Error.Message)
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("joined cause leaked to public response: %s", response.Body.String())
	}
}

func TestWriteServiceErrorExposesOnlyStructuredUpstreamError(t *testing.T) {
	const secret = "provider response body with api_key=secret"
	response := httptest.NewRecorder()
	h := &PublicHandler{}
	h.writeServiceError(response, &upstream.Error{
		Code: "upstream_unavailable", Status: http.StatusBadGateway,
		Cause: errors.New(secret),
	})

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("upstream cause leaked to public response: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "upstream_unavailable (status 502)") {
		t.Fatalf("response lacks stable upstream detail: %s", response.Body.String())
	}
}

func TestWriteServiceErrorUsesStableFallbackForUnknownError(t *testing.T) {
	const secret = "provider=/srv/private token=secret"
	response := httptest.NewRecorder()
	h := &PublicHandler{}
	h.writeServiceError(response, errors.New(secret))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("unknown cause leaked to public response: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "request could not be completed") {
		t.Fatalf("response lacks stable fallback detail: %s", response.Body.String())
	}
}
