package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/testutil"
)

type waitingStreamClient struct {
	started chan struct{}
	release chan struct{}
}

func (c *waitingStreamClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return nil, errors.New("unused")
}

func (c *waitingStreamClient) Stream(ctx context.Context, _ map[string]any) (*http.Response, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")),
	}, nil
}

type streamSessionFactory struct{ client relay.UpstreamClient }

func (f streamSessionFactory) ForProvider(context.Context, int64) (relay.UpstreamClient, error) {
	return f.client, nil
}

func TestStreamHeartbeatArrivesBeforeUpstreamHeaders(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstreamClient := &waitingStreamClient{started: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(upstreamClient.release) }) }
			t.Cleanup(release)
			fixture := testutil.NewRelayFixture(t, upstreamClient)
			settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, MaxOutputTokens: 64, SSEHeartbeatInterval: 10 * time.Millisecond}
			service := relay.NewService(fixture.Store, streamSessionFactory{client: upstreamClient}, func() config.Settings { return settings })
			server := httptest.NewServer(NewPublicHandler(service, 1<<20))
			defer server.Close()

			request, err := http.NewRequest(http.MethodPost, server.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+fixture.Key.Token)
			client := &http.Client{Timeout: 2 * time.Second}
			result := make(chan struct {
				response *http.Response
				err      error
			}, 1)
			go func() {
				response, err := client.Do(request)
				result <- struct {
					response *http.Response
					err      error
				}{response, err}
			}()

			select {
			case <-upstreamClient.started:
			case <-time.After(time.Second):
				t.Fatal("upstream request did not start")
			}
			var response *http.Response
			select {
			case value := <-result:
				if value.err != nil {
					t.Fatal(value.err)
				}
				response = value.response
			case <-time.After(100 * time.Millisecond):
				release()
				t.Fatal("downstream received no headers while upstream was waiting")
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
			}
			reader := bufio.NewReader(response.Body)
			lines := make(chan string, 1)
			go func() {
				var collected strings.Builder
				for i := 0; i < 4; i++ {
					line, err := reader.ReadString('\n')
					if err != nil {
						lines <- "read error: " + err.Error()
						return
					}
					collected.WriteString(line)
				}
				lines <- collected.String()
			}()
			select {
			case got := <-lines:
				if got != ": relay-open\n\n: relay-heartbeat\n\n" {
					t.Fatalf("before upstream headers: %q", got)
				}
			case <-time.After(100 * time.Millisecond):
				release()
				t.Fatal("heartbeat did not arrive before upstream headers")
			}
			release()
			if _, err := io.ReadAll(reader); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type closeUnblocksBody struct {
	reading chan struct{}
	closed  chan struct{}
	once    sync.Once
	read    sync.Once
}

func (b *closeUnblocksBody) Read([]byte) (int, error) {
	b.read.Do(func() { close(b.reading) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *closeUnblocksBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type blockingBodyClient struct{ body *closeUnblocksBody }

func (blockingBodyClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return nil, errors.New("unused")
}

func (c blockingBodyClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: c.body}, nil
}

func TestClientDisconnectClosesBlockedUpstreamBody(t *testing.T) {
	body := &closeUnblocksBody{reading: make(chan struct{}), closed: make(chan struct{})}
	upstreamClient := blockingBodyClient{body: body}
	fixture := testutil.NewRelayFixture(t, upstreamClient)
	settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, MaxOutputTokens: 64, SSEHeartbeatInterval: 10 * time.Millisecond}
	service := relay.NewService(fixture.Store, streamSessionFactory{client: upstreamClient}, func() config.Settings { return settings })
	server := httptest.NewServer(NewPublicHandler(service, 1<<20))
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.Key.Token)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-body.reading:
	case <-time.After(time.Second):
		response.Body.Close()
		t.Fatal("producer never read upstream body")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body stayed blocked after client disconnect")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rows, err := fixture.Store.RecentRequests(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0]["status"] == "aborted" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canceled stream did not settle")
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineWriter) FlushError() error { return nil }

func TestStreamWriteClearsDeadline(t *testing.T) {
	writer := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	session := &streamSession{w: writer, controller: http.NewResponseController(writer)}
	if err := session.write([]byte(": relay-heartbeat\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(writer.deadlines) != 2 || writer.deadlines[0].IsZero() || !writer.deadlines[1].IsZero() {
		t.Fatalf("write deadlines were not set and cleared: %v", writer.deadlines)
	}
}

type heartbeatFailureWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *heartbeatFailureWriter) FlushError() error {
	w.flushes++
	if w.flushes > 1 {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestHeartbeatWriteFailureClosesBlockedUpstreamBody(t *testing.T) {
	body := &closeUnblocksBody{reading: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = body.Close() })
	upstreamClient := blockingBodyClient{body: body}
	fixture := testutil.NewRelayFixture(t, upstreamClient)
	settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, MaxOutputTokens: 64, SSEHeartbeatInterval: 100 * time.Millisecond}
	service := relay.NewService(fixture.Store, streamSessionFactory{client: upstreamClient}, func() config.Settings { return settings })
	handler := NewPublicHandler(service, 1<<20)
	writer := &heartbeatFailureWriter{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+fixture.Key.Token)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, request)
	}()
	select {
	case <-body.reading:
	case <-time.After(time.Second):
		t.Fatal("producer never read upstream body")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler stayed blocked after heartbeat write failed")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body was not closed")
	}
	rows, err := fixture.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" || rows[0]["error_code"] != "downstream_write_failed" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestStreamAdmissionChecksWhileUpstreamIsSilent(t *testing.T) {
	body := &closeUnblocksBody{reading: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = body.Close() })
	upstreamClient := blockingBodyClient{body: body}
	fixture := testutil.NewRelayFixture(t, upstreamClient)
	settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, MaxOutputTokens: 64}
	service := relay.NewService(fixture.Store, streamSessionFactory{client: upstreamClient}, func() config.Settings { return settings })
	handler := NewPublicHandler(service, 1<<20).(*PublicHandler)
	handler.SetStreamAdmission(func() error { return errors.New("memory limit") })
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+fixture.Key.Token)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-body.reading:
	case <-time.After(time.Second):
		t.Fatal("producer never read upstream body")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("silent upstream prevented periodic memory check")
	}
	rows, err := fixture.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" || rows[0]["error_code"] != "memory_limit_exceeded" {
		t.Fatalf("rows=%v", rows)
	}
}
