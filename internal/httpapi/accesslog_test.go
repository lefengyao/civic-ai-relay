package httpapi

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/logging"
)

// 访问日志包装必须把 http.Flusher 透传下去。SSE 流式响应靠 `w.(http.Flusher)`
// 逐块推送，一旦被藏起来，所有流式请求都会退化成「攒完再发」甚至直接失败——
// 而这只在 DEBUG 级别下才会发生，最容易漏测。
func TestAccessLogWrapperKeepsFlusherForSSE(t *testing.T) {
	logging.SetLevel(logging.LevelDebug)
	t.Cleanup(func() { logging.SetLevel(logging.LevelInfo) })

	flushed := false
	handler := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("access-log wrapper hid http.Flusher; SSE streaming would break")
			return
		}
		_, _ = w.Write([]byte("data: chunk\n\n"))
		flusher.Flush()
		flushed = true
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if !flushed {
		t.Fatal("handler never reached the flush path")
	}
	if recorder.Body.String() != "data: chunk\n\n" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

// 访问日志不得记录查询串与请求头：客户端可能把密钥放进 ?api_key=，
// Authorization / X-Admin-Key 也都在头里。这里把「不该出现的东西」逐条钉住。
func TestAccessLogNeverRecordsQueryOrHeaders(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	logging.SetLevel(logging.LevelDebug)
	t.Cleanup(func() { logging.SetLevel(logging.LevelInfo) })

	handler := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models?api_key=super-secret-value&token=another", nil)
	request.Header.Set("Authorization", "Bearer crk_leaked_client_token")
	request.Header.Set("X-Admin-Key", "adm_leaked_admin_key")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	out := buffer.String()
	if !strings.Contains(out, "/v1/models") {
		t.Fatalf("access log lacks the path: %q", out)
	}
	if !strings.Contains(out, "418") {
		t.Fatalf("access log lacks the status code: %q", out)
	}
	for _, secret := range []string{
		"super-secret-value", "api_key", "token=another",
		"crk_leaked_client_token", "adm_leaked_admin_key", "Bearer",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("access log leaked %q: %q", secret, out)
		}
	}
}

// 非 DEBUG 级别不做任何包装：这条路径应当零开销，也不该改变 ResponseWriter 的
// 具体类型（有些处理器会做类型断言）。
func TestAccessLogDoesNotWrapWhenDebugDisabled(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	logging.SetLevel(logging.LevelInfo)
	t.Cleanup(func() { logging.SetLevel(logging.LevelInfo) })

	var seen http.ResponseWriter
	handler := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { seen = w }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if _, wrapped := seen.(*statusRecorder); wrapped {
		t.Fatal("ResponseWriter was wrapped although DEBUG is off")
	}
	if output.Len() != 0 {
		t.Fatalf("access log wrote output at INFO level: %q", output.String())
	}
}

func TestRemoteHostStripsPort(t *testing.T) {
	cases := map[string]string{
		"192.0.2.10:54321":  "192.0.2.10",
		"[2001:db8::1]:443": "2001:db8::1",
		"192.0.2.10":        "192.0.2.10",
		"":                  "",
	}
	for input, want := range cases {
		if got := remoteHost(input); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", input, got, want)
		}
	}
}
