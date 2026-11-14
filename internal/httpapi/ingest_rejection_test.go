package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/testutil"
)

// oversizedChatBody 返回一个语法完全合法、但体积超过 limit 的对话请求。
// 用合法 JSON 是刻意的：这条用例要证明「体积超限」不会被误报成「JSON 有语法
// 错误」—— 线上那个 400 invalid_request: ... request body too large 就是这么来的。
func oversizedChatBody(padding int) string {
	return `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", padding) + `"}]}`
}

// 请求体超过上限必须是 413 request_too_large，并且在账本里留一行 rejected：
// 客户端能看懂该缩包还是该调上限，运维也能在管理台查到是谁在发。
func TestChatCompletionsOversizedBodyIs413AndRecorded(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	handler := NewPublicHandler(f.Service, 1024) // 1 KB 上限，便于构造

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oversizedChatBody(4096)))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d，应为 413: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, recorder.Body.String())
	}
	if payload.Error.Code != "request_too_large" {
		t.Fatalf("code = %q", payload.Error.Code)
	}
	// 文案要带上限本身，客户端才知道差多少；也要点名 MAX_BODY_MB，运维才知道改哪。
	if !strings.Contains(payload.Error.Message, "1 KB") || !strings.Contains(payload.Error.Message, "MAX_BODY_MB") {
		t.Fatalf("message = %q", payload.Error.Message)
	}
	if payload.Error.RequestID == "" || payload.Error.RequestID != recorder.Header().Get("X-Request-Id") {
		t.Fatalf("request_id = %q, header = %q", payload.Error.RequestID, recorder.Header().Get("X-Request-Id"))
	}

	// 账本留痕：状态/错误码/阶段齐全，且挂在识别出来的 Key 上。
	page, err := f.Store.QueryRequests(t.Context(), store.RequestFilter{Status: "rejected", KeyID: f.Key.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("应有 1 条 rejected 记录（按 key_id 过滤），得到 %d", len(page.Data))
	}
	row := page.Data[0]
	if row["request_id"] != payload.Error.RequestID {
		t.Fatalf("账本 request_id = %v，响应 = %q", row["request_id"], payload.Error.RequestID)
	}
	if row["error_code"] != "request_too_large" || row["failure_stage"] != "ingest" {
		t.Fatalf("账本行 = %+v", row)
	}
	// 没读到 body，就不该有任何用量：否则用量分析会凭空多一次输入。
	if row["input_tokens"] != int64(0) || row["amount_microyuan"] != int64(0) {
		t.Fatalf("拒绝行不应有 token/金额：%+v", row)
	}
	if row["protocol"] != "chat" {
		t.Fatalf("protocol = %v", row["protocol"])
	}
}

// /v1/responses 走同一套入口，不能只修一半：同样的超大包必须同样 413。
func TestResponsesOversizedBodyIs413AndRecorded(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	handler := NewPublicHandler(f.Service, 512)

	body := `{"model":"m","input":"` + strings.Repeat("y", 4096) + `"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "request_too_large") {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	page, err := f.Store.QueryRequests(t.Context(), store.RequestFilter{Status: "rejected", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0]["protocol"] != "responses" {
		t.Fatalf("账本行 = %+v", page.Data)
	}
}

// 真正的语法错误仍然是 400 invalid_request —— 不能被体积分支吞掉，
// 但同样要留痕（否则这类客户端 bug 也查不到）。
func TestMalformedJSONStays400AndIsRecorded(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	handler := NewPublicHandler(f.Service, 1<<20)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m",`))
	request.Header.Set("Authorization", "Bearer "+f.Key.Token)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	// 体积没超，不能报成 request_too_large。
	if strings.Contains(recorder.Body.String(), "request_too_large") {
		t.Fatalf("语法错误被误报成超限: %s", recorder.Body.String())
	}
	page, err := f.Store.QueryRequests(t.Context(), store.RequestFilter{Status: "rejected", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0]["error_code"] != "invalid_request" {
		t.Fatalf("账本行 = %+v", page.Data)
	}
}

// 没带凭据时在鉴权那一步就 401 了，根本不会读 body，因此不该产生 rejected 行。
// 记录点只在「读 body 失败」处，不能把每个未认证请求都写进账本。
func TestOversizedBodyWithoutCredentialIsNotRecorded(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	handler := NewPublicHandler(f.Service, 512)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oversizedChatBody(4096)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，得到 %d: %s", recorder.Code, recorder.Body.String())
	}
	// 401 在解析 body 之前就返回了，所以这次不该产生 rejected 行 —— 记录点只在
	// 「读 body 失败」处，不能把每个未认证请求都写进账本。
	page, err := f.Store.QueryRequests(t.Context(), store.RequestFilter{Status: "rejected", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 0 {
		t.Fatalf("401 不应写 rejected 行: %+v", page.Data)
	}
}

// MAX_BODY_MB 支持热更新：同一个 handler 上抬高上限后，不用重建进程就生效。
// 这条用例防止将来又把上限固化成启动参数（管理台改完却毫无反应）。
func TestBodyLimitSourceTakesEffectImmediately(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	base := NewPublicHandler(f.Service, 64*1024)
	handler, ok := base.(*PublicHandler)
	if !ok {
		t.Fatalf("unexpected handler type %T", base)
	}
	limit := int64(64 * 1024)
	handler.SetBodyLimitSource(func() int64 { return limit })

	send := func(model string) *httptest.ResponseRecorder {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"` + strings.Repeat("x", 128*1024) + `"}]}`
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.Key.Token)
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if got := send("m"); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("64 KB 上限下应 413，得到 %d: %s", got.Code, got.Body.String())
	}
	limit = 4 << 20 // 管理台把 MAX_BODY_MB 调大
	// 用不存在的模型：请求体已能正常解析，会在选模型那步被拒，不会真的打上游。
	if got := send("no-such-model"); got.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("上限抬高后不应再 413: %s", got.Body.String())
	}

	// 来源返回 0 时必须回落到启动值，不能把上限变成「拒绝一切」。
	limit = 0
	if got := send("m"); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("上限来源为 0 时应回落 64 KB 并 413，得到 %d: %s", got.Code, got.Body.String())
	}
}
