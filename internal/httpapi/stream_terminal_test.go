package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/testutil"
)

type terminalStreamClient struct{ body string }

func (terminalStreamClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return nil, errors.New("unused")
}

func (c terminalStreamClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

func TestStreamWithoutDoneFails(t *testing.T) {
	for _, tc := range []struct{ name, path, request string }{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewRelayFixture(t, terminalStreamClient{body: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request))
			r.Header.Set("Authorization", "Bearer "+f.Key.Token)
			NewPublicHandler(f.Service, 1<<20).ServeHTTP(w, r)
			rows, err := f.Store.RecentRequests(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0]["status"] != "failed" || rows[0]["error_code"] != "upstream_stream_incomplete" {
				t.Fatalf("rows=%v", rows)
			}
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" || !strings.Contains(w.Body.String(), requestID) {
				t.Fatalf("stream error must carry request id header=%q body=%s", requestID, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatalf("incomplete stream was completed: %s", w.Body.String())
			}
		})
	}
}

func TestMalformedStreamEventFails(t *testing.T) {
	for _, tc := range []struct{ name, path, request string }{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewRelayFixture(t, terminalStreamClient{body: "data: {broken\n\ndata: [DONE]\n\n"})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request))
			r.Header.Set("Authorization", "Bearer "+f.Key.Token)
			NewPublicHandler(f.Service, 1<<20).ServeHTTP(w, r)
			rows, err := f.Store.RecentRequests(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0]["status"] != "failed" || rows[0]["error_code"] != "upstream_event_invalid" {
				t.Fatalf("rows=%v", rows)
			}
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" || !strings.Contains(w.Body.String(), requestID) {
				t.Fatalf("stream error must carry request id header=%q body=%s", requestID, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatalf("invalid stream was completed: %s", w.Body.String())
			}
		})
	}
}

func TestLengthFinishReasonUsesProtocolSpecificTerminal(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	for _, tc := range []struct {
		name, path, request, wantStatus, wantCode, wantReason, wantEvent string
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, "completed", "", "max_output_tokens", "data: [DONE]"},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`, "failed", "upstream_incomplete", "max_output_tokens", "event: response.incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewRelayFixture(t, terminalStreamClient{body: stream})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request))
			r.Header.Set("Authorization", "Bearer "+f.Key.Token)
			NewPublicHandler(f.Service, 1<<20).ServeHTTP(w, r)
			rows, err := f.Store.RecentRequests(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0]["status"] != tc.wantStatus || rows[0]["error_code"] != tc.wantCode || rows[0]["incomplete_reason"] != tc.wantReason {
				t.Fatalf("rows=%v", rows)
			}
			if !strings.Contains(w.Body.String(), tc.wantEvent) {
				t.Fatalf("missing %q: %s", tc.wantEvent, w.Body.String())
			}
			if tc.name == "responses" && strings.Contains(w.Body.String(), "event: response.completed") {
				t.Fatalf("length stream completed: %s", w.Body.String())
			}
		})
	}
}
