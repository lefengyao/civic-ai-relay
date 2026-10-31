package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConnectTimeoutHasStableStage(t *testing.T) {
	client := NewWithOptions("https://upstream.example", "secret", Options{ConnectTimeout: 40 * time.Millisecond})
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, os.ErrDeadlineExceeded
	})
	_, err := client.ChatJSON(context.Background(), nil)
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_connect_timeout" || upstreamErr.Stage != "connect" || upstreamErr.Status != 0 {
		t.Fatalf("err=%v, want upstream_connect_timeout with no HTTP status", err)
	}
}

func TestChatJSONIdleTimeoutKeepsReceivedStatus(t *testing.T) {
	server := silentBodyServer(t, 80*time.Millisecond)
	defer server.Close()
	client := NewWithOptions(server.URL, "", Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: 40 * time.Millisecond})
	_, err := client.ChatJSON(context.Background(), nil)
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_idle_timeout" || upstreamErr.Stage != "idle" || upstreamErr.Status != http.StatusOK {
		t.Fatalf("err=%v, want upstream_idle_timeout with HTTP 200", err)
	}
}

func TestStreamHeaderTimeoutHasStableStage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(80 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := NewWithOptions(server.URL, "secret", Options{ConnectTimeout: time.Second, HeaderTimeout: 40 * time.Millisecond})
	_, err := client.Stream(context.Background(), nil)
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_header_timeout" || upstreamErr.Stage != "header" || upstreamErr.Status != 0 {
		t.Fatalf("err=%v, want upstream_header_timeout with no HTTP status", err)
	}
	if errors.Unwrap(upstreamErr) == nil || containsSecretOrURL(upstreamErr.Error(), server.URL, "secret") {
		t.Fatalf("unsafe error=%v cause=%v", upstreamErr, errors.Unwrap(upstreamErr))
	}
}

func TestStreamIdleTimeoutZeroAllowsSilentBody(t *testing.T) {
	server := silentBodyServer(t, 80*time.Millisecond)
	defer server.Close()
	client := NewWithOptions(server.URL, "", Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: 0})
	response, err := client.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "data: [DONE]\n\n" {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

func TestStreamIdleTimeoutResetsOnReceivedBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := 0; i < 5; i++ {
			select {
			case <-time.After(30 * time.Millisecond):
				_, _ = io.WriteString(w, "x")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer server.Close()
	client := NewWithOptions(server.URL, "", Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: 120 * time.Millisecond})
	response, err := client.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "xxxxx" {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

type lateBody struct{}

func (lateBody) Read(p []byte) (int, error) {
	time.Sleep(60 * time.Millisecond)
	p[0] = 'x'
	return 1, nil
}
func (lateBody) Close() error { return nil }

func TestIdleDeadlineWinsOverLateRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := newTimedBody(ctx, cancel, lateBody{}, http.StatusOK, 20*time.Millisecond)
	defer body.Close()
	_, err := body.Read(make([]byte, 1))
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_idle_timeout" {
		t.Fatalf("late read err=%v, want upstream_idle_timeout", err)
	}
}

func TestStreamIdleTimeoutAfterHeaders(t *testing.T) {
	server := silentBodyServer(t, 80*time.Millisecond)
	defer server.Close()
	client := NewWithOptions(server.URL, "", Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: 40 * time.Millisecond})
	response, err := client.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	started := time.Now()
	_, err = io.ReadAll(response.Body)
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_idle_timeout" || upstreamErr.Stage != "idle" || upstreamErr.Status != http.StatusOK {
		t.Fatalf("err=%v, want upstream_idle_timeout with HTTP 200", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("idle timeout returned after %s, want prompt cancellation", elapsed)
	}
}

func TestStreamParentDeadlineKeepsReceivedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewWithOptions(server.URL, "", Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	response, err := client.Stream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, err = io.ReadAll(response.Body)
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "request_duration_exceeded" || upstreamErr.Stage != "duration" || upstreamErr.Status != http.StatusOK {
		t.Fatalf("err=%v, want request_duration_exceeded with HTTP 200", err)
	}
}

func silentBodyServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(delay):
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
}

func containsSecretOrURL(message, url, secret string) bool {
	return strings.Contains(message, url) || strings.Contains(message, secret)
}
