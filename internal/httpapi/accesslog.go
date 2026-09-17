package httpapi

import (
	"net"
	"net/http"
	"strings"
	"time"

	"civic-ai-relay/internal/logging"
)

// statusRecorder 记录响应状态码，并把 Flush 透传下去。
//
// ⚠️ 必须实现 http.Flusher：SSE 流式响应靠 `w.(http.Flusher)` 逐块推送，
// 一旦这个包装把 Flusher 藏起来，所有流式请求都会退化成「攒完再发」甚至失败。
// 由 accesslog_test.go 钉住。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// withAccessLog 在 DEBUG 级别下记录每个 HTTP 请求，用于回答「客户端说报错了，
// 但容器日志里什么都看不到」这类问题。非 DEBUG 时直接透传，不包装 ResponseWriter。
//
// 只记方法、路径、状态码、耗时与来源地址：
//   - **不记查询串** —— 有客户端会把密钥放在 `?api_key=` 里；
//   - **不记请求头** —— Authorization / X-Admin-Key 都在里面；
//   - **不记请求体** —— 提示词是用户隐私，与本项目「账本只存元数据」的口径一致。
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !logging.Enabled(logging.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logging.Debugf("http %s %s %d %s remote=%s", r.Method, r.URL.Path, recorder.status,
			time.Since(started).Round(time.Millisecond), remoteHost(r.RemoteAddr))
	})
}

func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.TrimSpace(addr)
}
