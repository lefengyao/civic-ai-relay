package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/testutil"
)

type interruptedStream struct {
	sent bool
}

func (r *interruptedStream) Read(p []byte) (int, error) {
	if r.sent {
		return 0, context.Canceled
	}
	r.sent = true
	return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"), nil
}

func (r *interruptedStream) Close() error { return nil }

type interruptedClient struct{}

func (interruptedClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return nil, errors.New("unused")
}

func (interruptedClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &interruptedStream{}}, nil
}

func TestResponsesCanceledStreamDoesNotComplete(t *testing.T) {
	f := testutil.NewRelayFixture(t, interruptedClient{})
	handler := NewPublicHandler(f.Service, 1<<20)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)
	handler.ServeHTTP(recorder, request)
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" {
		t.Fatalf("rows=%v", rows)
	}
	if strings.Contains(recorder.Body.String(), "event: response.completed") {
		t.Fatalf("canceled stream emitted completion: %s", recorder.Body.String())
	}
}

type failingStreamWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *failingStreamWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 2 {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func (w *failingStreamWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func TestResponsesDownstreamWriteFailureAbortsSettlement(t *testing.T) {
	f := testutil.NewRelayFixture(t, interruptedClient{})
	handler := NewPublicHandler(f.Service, 1<<20)
	writer := &failingStreamWriter{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)
	handler.ServeHTTP(writer, request)
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" || rows[0]["error_code"] != "downstream_write_failed" {
		t.Fatalf("rows=%v", rows)
	}
}

type failingFlushWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *failingFlushWriter) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

func TestStreamFlushFailureAbortsSettlement(t *testing.T) {
	cases := []struct {
		name, path, body string
		debug            bool
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, false},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`, false},
		{"chat with access log", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, true},
		{"responses with access log", "/v1/responses", `{"model":"m","input":"hi","stream":true}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewRelayFixture(t, interruptedClient{})
			var handler http.Handler = NewPublicHandler(f.Service, 1<<20)
			if tc.debug {
				logging.SetLevel(logging.LevelDebug)
				t.Cleanup(func() { logging.SetLevel(logging.LevelInfo) })
				handler = withAccessLog(handler)
			}
			writer := &failingFlushWriter{ResponseRecorder: httptest.NewRecorder()}
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+f.Key.Token)
			handler.ServeHTTP(writer, request)
			if writer.flushes == 0 || writer.Body.Len() == 0 {
				t.Fatalf("write and flush were not attempted: writes=%d flushes=%d", writer.Body.Len(), writer.flushes)
			}
			rows, err := f.Store.RecentRequests(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0]["status"] != "aborted" || rows[0]["error_code"] != "downstream_write_failed" {
				t.Fatalf("rows=%v", rows)
			}
		})
	}
}
