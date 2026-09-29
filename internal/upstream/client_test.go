package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClientHasNoAggregateTimeout 钉住一条线上事故级的约束：
// `http.Client.Timeout` 是「整个请求」的总超时（**包含读取响应体的全部时间**），
// 对流式回答等于一道固定的硬闸。默认 connect+read+write = 10+300+30 = 340 秒，
// 任何超过它的对话都会在那刻被无声掐断，记录里只留下 upstream_status=200 + 零 token
// 的 failed（2026-09-25 线上真实故障，运维完全看不出是超时）。
// 职责必须拆开：「上游不响应」由 ResponseHeaderTimeout 拦，「流跑太久」由
// MAX_STREAM_DURATION 在 relay 层用 context 拦。
func TestClientHasNoAggregateTimeout(t *testing.T) {
	client := New("https://upstream.example", "k", 10*time.Second, 300*time.Second, 30*time.Second, 10*time.Second)
	if client.http.Timeout != 0 {
		t.Fatalf("http.Client.Timeout 必须为 0（当前 %v）：它会把长流式回答在固定秒数处掐断", client.http.Timeout)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型异常：%T", client.http.Transport)
	}
	if transport.ResponseHeaderTimeout != 310*time.Second {
		t.Fatalf("ResponseHeaderTimeout 应保持 connect+read = 310s，当前 %v", transport.ResponseHeaderTimeout)
	}
}

// TestStreamSurvivesPastAggregateTimeoutBudget 用慢速 SSE 复现旧缺陷的形状：
// 假上游每 120ms 发一个 chunk、共 5 个（约 600ms），而 connect+read+write 只有 400ms。
// 旧实现会在第 3 个 chunk 之后掐断读取；修好后必须完整读到 [DONE]。
func TestStreamSurvivesPastAggregateTimeoutBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("httptest server 应当支持 Flush")
		}
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
			time.Sleep(120 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	// 三个超时相加只有 400ms，故意小于这条流的总时长
	client := New(server.URL, "k", 200*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, time.Second)
	response, err := client.Stream(context.Background(), map[string]any{"stream": true})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("流在读完前被掐断（正是线上长对话中断的成因）：%v", err)
	}
	if !strings.Contains(string(body), "[DONE]") || strings.Count(string(body), "chunk-") != 5 {
		t.Fatalf("流未完整读到：%q", body)
	}
}

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
