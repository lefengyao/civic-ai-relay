package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/testutil"
)

type responsesJSONClient struct{}

func (responsesJSONClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return []byte(`{"id":"chatcmpl_fixture","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`), nil
}

func (responsesJSONClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	return nil, errors.New("unused")
}

func TestResponsesNonStreamSettlesResponsesProtocol(t *testing.T) {
	f := testutil.NewRelayFixture(t, responsesJSONClient{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)

	NewPublicHandler(f.Service, 1<<20).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "completed" || rows[0]["protocol"] != "responses" {
		t.Fatalf("rows = %v, want completed responses request", rows)
	}
}

var _ relay.UpstreamClient = responsesJSONClient{}
