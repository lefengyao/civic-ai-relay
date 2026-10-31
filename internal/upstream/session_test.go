package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeriveSessionIDStableAndPayloadSpecific(t *testing.T) {
	payload := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "first"},
	}}
	first := deriveSessionID("key", payload)
	if got := deriveSessionID("key", payload); got != first {
		t.Fatalf("same payload produced different ids: %q != %q", first, got)
	}
	payload["messages"] = []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "second"},
	}}}
	if got := deriveSessionID("key", payload); got == first {
		t.Fatal("different first user message produced same session id")
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("id is not UUID-shaped: %q", first)
	}
}

func TestOpenCodeEndpointHostMatching(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://opencode.ai/inference/go", true}, {"https://x.opencode.ai/v1", true},
		{"https://opencode.ai.evil.com", false}, {"https://notopencode.ai", false},
		{"http://127.0.0.1:8000", false}, {"https://example.com", false},
	} {
		if got := isOpenCodeEndpoint(tc.url); got != tc.want {
			t.Errorf("isOpenCodeEndpoint(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestRequestAddsConfiguredHeadersAndPrefersContextSession(t *testing.T) {
	var userAgent, session string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent, session = r.UserAgent(), r.Header.Get("X-Relay-Session")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client := NewWithOptions(server.URL, "key", Options{ConnectTimeout: time.Second, ReadTimeout: time.Second,
		WriteTimeout: time.Second, PoolTimeout: time.Second, UserAgent: "relay-test/2", UpstreamSessionHeader: "X-Relay-Session"})
	payload := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if _, err := client.ChatJSON(WithSessionID(context.Background(), "  client-session  "), payload); err != nil {
		t.Fatal(err)
	}
	if userAgent != "relay-test/2" || session != "client-session" {
		t.Fatalf("upstream headers: User-Agent=%q session=%q", userAgent, session)
	}
}

func TestRequestAutoAddsOpenCodeSessionAndDefaultUserAgent(t *testing.T) {
	var userAgent, session string
	client := NewWithOptions("https://opencode.ai/inference/go", "key", Options{})
	client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		userAgent, session = r.UserAgent(), r.Header.Get(SessionIDHeader)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	payload := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if _, err := client.ChatJSON(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if userAgent != DefaultUserAgent || session == "" || len(session) != 36 {
		t.Fatalf("upstream headers: User-Agent=%q session=%q", userAgent, session)
	}
}

func TestSessionHeaderCanBeDisabled(t *testing.T) {
	client := NewWithOptions("https://opencode.ai/inference/go", "key", Options{UpstreamSessionHeader: "-"})
	if client.sessionHeader != "" {
		t.Fatalf("session header = %q, want disabled", client.sessionHeader)
	}
}

func TestSessionIDContextIgnoresBlankValues(t *testing.T) {
	if got := SessionIDFromContext(WithSessionID(context.Background(), " \t")); got != "" {
		t.Fatalf("blank session = %q", got)
	}
}
