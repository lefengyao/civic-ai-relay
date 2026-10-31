# 阶段2：长任务与流结束 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 长思考期间保持下行连接，按阶段识别超时，只有协议正常结束才记录成功。

**Architecture:** 从公共处理器中抽取单写入stream session，上游打开/读取通过有界channel交给发送循环。共享SSE读取器按完整事件分帧；不同协议各自决定终态。请求全程由阶段1的lease结算。

**Tech Stack:** Go net/http、ResponseController、context、time、SSE。

---

依赖阶段1。旧配置保持含义，新安装配置提供较长响应头预算；心跳不能延长上游HTTP或客户端绝对超时。下面文件相对总计划项目根目录。

## Task 1：0不限与分阶段配置

**Files:** Modify `internal/config/settings.go`、`internal/config/store.go`、`.env.example`、`internal/config/settings_test.go`、`internal/config/store_test.go`、`web/admin.html`。

- [ ] **Step 1：写配置往返测试。** 同一套有效基础配置依次设置MAX_STREAM_DURATION为0和0s，Parse成功且EnvMap→Parse仍0；-1/-1s/空值拒绝。新配置heartbeat=15s、header=1800s、idle=0；旧配置缺header时使用旧connect+read。保存Transport配置返回pending_restart_fields，保存心跳/总长标“新请求生效”。

```go
func TestMaxStreamZeroRoundTrip(t *testing.T) {
    values:=map[string]string{"ADMIN_API_KEY":"fake","RELAY_ENCRYPTION_KEY":base64.StdEncoding.EncodeToString(make([]byte,32)),"MAX_STREAM_DURATION":"0"}
    settings,err:=config.Parse(values)
    if err!=nil || settings.MaxStreamDuration!=0 { t.Fatalf("parse=%v duration=%v",err,settings.MaxStreamDuration) }
    again,err:=config.Parse(settings.EnvMap())
    if err!=nil || again.MaxStreamDuration!=0 { t.Fatalf("roundtrip=%v",err) }
}
```

- [ ] **Step 2：运行红色测试。** `go test ./internal/config -run 'TestMaxStreamZeroRoundTrip|TestTimeoutCompatibility' -count=1 -v`；当前0解析失败。
- [ ] **Step 3：实现零值解析和可选header。** duration原正数行为保留；新增仅用于允许零的设置：

```go
func durationOrZero(values map[string]string, name string, fallback, unit time.Duration) (time.Duration,error) {
    raw,ok:=values[name]
    if !ok { return fallback,nil }
    raw=strings.TrimSpace(raw)
    if raw=="0" || raw=="0s" { return 0,nil }
    return duration(values,name,fallback,unit)
}
```

Settings新增HeaderTimeout、UpstreamIdleTimeout、SSEHeartbeatInterval。HeaderTimeout=0仅内部表示“未配置，沿用旧值”，EnvMap在0时不输出该键；用户显式输入0则HeaderTimeout正数校验拒绝。bootstrap新增UPSTREAM_HEADER_TIMEOUT=1800s；旧文件不覆盖既有300s读取设置。心跳与idle用durationOrZero。SettingsSource仍按请求快照取值。
- [ ] **Step 4：修改管理文案与回归。** MaxStream为“请求总时长，0不限”；header为“等待上游响应头”；旧read为兼容字段；旧write明确兼容保留且不用于总超时，不误称新Transport写预算。`go test ./internal/config ./internal/httpapi -count=1`；用管理API保存、重新加载文件验证零值持久化。
- [ ] **Step 5：提交。** `fix: make stream limits and timeout settings consistent`。

## Task 2：上游超时与SSE读取器

**Files:** Modify `internal/upstream/client.go`、`internal/relay/providers.go`；Create `internal/upstream/options.go`、`internal/upstream/sse_reader.go`、`internal/upstream/sse_reader_test.go`、`internal/upstream/timeout_test.go`。

- [ ] **Step 1：给httptest上游分别设置“迟迟无头”和“先发头再静默”。** HeaderBudget=40ms，晚头80ms必须header_timeout；先头再body80ms，IdleBudget=0必须完整；IdleBudget=40ms必须idle_timeout；父总长20ms返回request_duration_exceeded。服务器等r.Context.Done结束，防止测试后台挂住。
- [ ] **Step 2：实现TransportOptions，保留现有New签名为包装。** 由Registry一次注入Options；新工厂NewWithOptions接入以下字段：

```go
type Options struct {
    ConnectTimeout, HeaderTimeout, WriteTimeout, PoolTimeout time.Duration
    IdleTimeout time.Duration
}
func effectiveHeaderTimeout(explicit, connect, legacyRead time.Duration) time.Duration {
    if explicit>0 { return explicit }
    return connect+legacyRead
}
```

DialContext使用net.Dialer.Timeout，TLSHandshakeTimeout采用connect，ResponseHeaderTimeout用effective值，http.Client.Timeout保持0。旧New包装仍能让现有client_test通过。Error增加Stage和Cause并实现Unwrap；对外Error.Error只显示稳定码/状态，不回显Cause中的URL/头。idle计时按收到上游字节更新，context终止时关闭Body使阻塞Read返回，不靠心跳续命。
- [ ] **Step 3：建立SSEReader完整接口。**

```go
type SSEFrame struct { Event string; Data []byte; Raw []byte; Comment bool }
type SSEReader struct { reader *bufio.Reader; maxBytes int }
func NewSSEReader(r io.Reader, maxBytes int) *SSEReader {
    return &SSEReader{reader:bufio.NewReader(r),maxBytes:maxBytes}
}
```

Next() (SSEFrame,error)逐行读至空行，去CRLF终止符，连续data以\n拼接，保留Raw；注释不拼入Data。EOF前非空未闭合事件返回io.ErrUnexpectedEOF；累计字节超过1MiB返回ErrSSEFrameTooLarge。分别测试CRLF、多行data、注释、随机chunk边界、空事件、超大事件，不能只按每行JSON假定上游永远单行。
- [ ] **Step 4：跑定向与旧慢流测试。** `go test ./internal/upstream ./internal/relay -count=1`；旧聚合超时预算不再截流，header/idle/父截止阶段可辨。
- [ ] **Step 5：提交。** `feat: parse SSE events and distinguish upstream timeout stages`。

## Task 3：单写循环与等待期间心跳

**Files:** Create `internal/httpapi/stream_session.go`、`internal/httpapi/stream_session_test.go`；Modify `internal/httpapi/public.go`、`internal/httpapi/responses.go`、`internal/httpapi/accesslog.go`、`internal/relay/service.go`。

- [ ] **Step 1：验证上游还没发头时能收到心跳。** 授权后上游等待100ms，heartbeat=10ms；真实httptest下游客户端前30ms能读到注释，但无JSON业务事件。本地鉴权/参数失败仍HTTP4xx且没有心跳、预留和上游请求。
- [ ] **Step 2：拆分PrepareStream。** Service增加PreparedStream：

```go
type PreparedStream struct {
    Lease *Lease
    Model store.Model
    Open func() (*http.Response,error)
    Cancel context.CancelFunc
}
```

PrepareStream先Begin一次，创建总时长context并封装Open；Open只打开上游，不再次Begin、不自行决定结算。保留Service.Stream作为兼容入口。处理器准入成功后持有PreparedStream，发送初始`: relay-open\n\n`，单循环负责心跳、业务帧和失败帧；Open和读取生产者通过容量1的channel传结果。producer退出条件为ctx取消/Body.Close/终态，session取消后等待producer退出再返回。所有lease出口由同一个defer结算。
- [ ] **Step 3：接入ResponseController。** 当前accesslog包装增加：

```go
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
func writeHeartbeat(w http.ResponseWriter) error {
    controller:=http.NewResponseController(w)
    if err:=controller.SetWriteDeadline(time.Now().Add(30*time.Second)); err!=nil && !errors.Is(err,http.ErrNotSupported) { return err }
    defer controller.SetWriteDeadline(time.Time{})
    if _,err:=io.WriteString(w,": relay-heartbeat\n\n"); err!=nil { return err }
    return controller.Flush()
}
```

业务写入使用同样的有界deadline与Flush错误处理，每次Write+Flush之后清空deadline，不能遗留“上次写后30s”成为隐式空闲超时。加入heartbeat=0、上游静默超过写预算后恢复输出的真实HTTP测试，正常写不能因旧deadline失败。ErrNotSupported只在没有底层连接的测试Writer下允许，真实服务器验证写截止生效。ResponseWriter仅由发送循环调用，禁止ticker goroutine直接写。心跳不更新上游idle时钟、不算usage/first_event/路由commit。
- [ ] **Step 4：测试取消、写失败、内存与停机。** 下游关闭触发Body.Close和aborted，lease清理完成；内存保护按时间检查，不再必须等256行；write失败无需再向断开的客户端发错误；无goroutine积累。`go test ./internal/httpapi ./cmd/civic-relay -count=1`，Linuxrace执行。
- [ ] **Step 5：提交。** `feat: keep long-thinking streams alive with bounded single-writer sessions`。

## Task 4：协议终态与代理验收

**Files:** Create `internal/upstream/terminal.go`、`internal/upstream/terminal_test.go`；Modify `internal/httpapi/public.go`、`internal/httpapi/responses.go`、`internal/upstream/sse.go`、`docs/client-integration.md`、`docs/deploy-with-trailblazer.md`；Create `scripts/tests/test_stream_gateway.py`。

- [ ] **Step 1：无DONE回归期望failed。** 包httpapi_test用阶段1夹具和返回partial后EOF的client；Chat不能追加伪DONE/成功，Responses不能response.completed。补正常DONE+usage、finish_reason=length+DONE、异常中断、坏JSON事件和客户端取消案例。原生failed/incomplete在此只固定纯终态判定测试，原生HTTP集成由阶段3负责，避免反向依赖。
- [ ] **Step 2：实现终态判定。** 共享读取器解析不等于某协议已经完成；基本策略函数完整内容：

```go
func ChatTerminal(done bool, readErr error, canceled bool) (string,string) {
    if canceled { return "aborted","client_canceled" }
    if readErr!=nil { return "failed","upstream_stream_read_failed" }
    if !done { return "failed","upstream_stream_incomplete" }
    return "completed",""
}
```

循环见DONE立即正常停止并Close body，不继续等供应商保持连接；DONE前收到finish_reason仍等usage尾帧。原生Responses用明确终态判定；桥接失败输出response.failed，受限length输出response.incomplete，输出Item状态也与整体一致。Chat流内错误用OpenAI兼容error对象，传稳定code+request_id；已发心跳后无法改HTTP状态，必须同时记实际上游状态和failure_stage。
- [ ] **Step 3：修正缺usage估算。** RequestReservation 增加 InputTokens int64，ReserveRequest返回已使用的输入估算供结算；统一按rune数量估算，工具参数/可见推理计入估算但不落内容。usage_source标reported/estimated/unknown；缓存token不超输入，reasoning不重复加到上游completion总量。没有任何生成证据且无usage时unknown，清预留而不虚构金额。合法Chat length+DONE记录 incomplete_reason=max_output_tokens，Chat仍按正常终态结算；Responses bridge length记failed/upstream_incomplete并发response.incomplete，两种客户端协议不能混淆。
- [ ] **Step 4：代理集成。** 测试脚本自己启动假上游+测试网关+临时Nginx配置（绑定随机回环端口），proxy_read_timeout=50ms、心跳10ms、思考200ms；逐步收到心跳并正常DONE。另关闭心跳复现proxy空闲断开，确认网关aborted并可再次请求。配置例子关闭buffering/cache，服务设置X-Accel-Buffering:no。`python -m unittest discover -s scripts/tests -p 'test_stream_gateway.py' -v`；没有Docker时单元部分可跑、代理集成明确未运行，发布闸门必须在Linux执行该部分。
- [ ] **Step 5：提交并阶段出口检查。** `fix: report incomplete streams and validate proxy heartbeats`；跑总计划检查。人工验证管理端0值保存/恢复、慢思考、取消后再请求；不调用付费模型作为默认自动验收。

## 本阶段出口

- [ ] 思考期间有心跳，header/idle/总时长各自生效；0不限可保存重启恢复。
- [ ] 无终态不假成功；usage估算和reported可辨；诊断没有内容泄露。
- [ ] 取消/超限/写失败/停机都退出producer并结算；活跃无限流不被reaper误杀。
- [ ] 进入阶段4，之后再做阶段3。
