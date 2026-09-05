package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamAddsUsageOptionAndForwardsBearerKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		options, ok := body["stream_options"].(map[string]any)
		if !ok || options["include_usage"] != true {
			t.Fatalf("stream_options = %#v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client := New(server.URL, "upstream-secret", time.Second, time.Second, time.Second, time.Second)
	response, err := client.Stream(context.Background(), map[string]any{"stream": true})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil {
		t.Fatal("nil response")
	}
	_ = response.Body.Close()
}

func TestUpstreamAuthenticationFailureIsRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `body contains fake-upstream-secret`)
	}))
	defer server.Close()
	_, err := New(server.URL, "upstream-secret", time.Second, time.Second, time.Second, time.Second).ChatJSON(context.Background(), map[string]any{"model": "m"})
	if err == nil || !strings.Contains(err.Error(), "upstream_authentication_failed") || strings.Contains(err.Error(), "fake-upstream-secret") {
		t.Fatalf("err = %v", err)
	}
}
