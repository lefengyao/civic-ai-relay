# 阶段4：请求诊断 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 用请求编号和安全元数据解释每一次失败，管理端能筛选到完整记录。

**Architecture:** HTTP入口创建追踪上下文，relay/upstream标记阶段，lease一次结算写账本；请求查询和详情与业务内容隔离。阶段1的v13预留诊断列，本阶段填充并建立查询索引。

**Tech Stack:** Go context/net/http、SQLite、现有HTML/JS管理台。

---

执行顺序为1→2→4。依赖阶段1的正确状态与阶段2的超时/终态；不需要阶段3和5完成。

## Task 1：请求关联与阶段计时

**Files:** Create `internal/requestmeta/meta.go`、`internal/requestmeta/meta_test.go`、`internal/httpapi/requestlog.go`、`internal/httpapi/requestlog_test.go`；Modify `internal/httpapi/server.go`、`internal/relay/service.go`、`internal/upstream/client.go`、`internal/httpapi/accesslog.go`。

- [ ] **Step 1：写请求编号测试。** 同请求的HTTP响应X-Request-Id、错误对象request_id、账本request_id和异常日志相同；重复/超长/含换行的客户端编号不覆盖服务端编号。缺失header/first-event计时为NULL；心跳不填first_event。
- [ ] **Step 2：实现Trace契约。**

```go
type Trace struct {
    ID, ClientID, Protocol string
    Started time.Time
    mu sync.Mutex
    HeaderAt, FirstEventAt, LastUpstreamAt *time.Time
}
type traceKey struct{}
func WithTrace(ctx context.Context,t *Trace) context.Context { return context.WithValue(ctx,traceKey{},t) }
func FromContext(ctx context.Context) *Trace { t,_:=ctx.Value(traceKey{}).(*Trace); return t }
func millisSince(start time.Time, at *time.Time) *int64 {
    if at==nil { return nil }; n:=at.Sub(start).Milliseconds(); return &n
}
```

阶段1Begin的内部request-id生成改为读Trace已有ID，缺Trace时保留原生成函数；ID使用crypto/rand128位。Trace可见元数据上锁；HTTP入口先设置header，不将请求ID拿来替代凭据鉴权。
- [ ] **Step 3：标记实际边界。** request start、上游header、每次上游字节、首次JSON业务事件、终态各标记一次；trace保留至独立settlement context。HTTP状态和账本终态分别记录；HTTP200下游失败流的日志显示失败码，不能只靠访问日志200判断成功。
- [ ] **Step 4：跑测试。** `go test ./internal/requestmeta ./internal/httpapi ./internal/relay ./internal/upstream -count=1`；DEBUG包装和无DEBUG路径都有同一request_id，Unwrap透传ResponseController。
- [ ] **Step 5：提交。** `feat: correlate request lifecycle and safe timing metadata`。

## Task 2：安全异常日志与分页查询

**Files:** Create `internal/store/request_query.go`、`internal/store/request_query_test.go`；Modify `internal/store/schema.go`（v13内追加索引，仅迁移尚未发布前）、`internal/store/ledger.go`、`internal/httpapi/admin.go`、`internal/httpapi/admin_test.go`、`internal/logging/logging_test.go`。

- [ ] **Step 1：写查询回归。** 插入60条虚构记录，首次limit=20返回20及next_cursor；下一页不重复。以request_id/status/provider_id/model_id/protocol/from/to组合筛选；不合法cursor/limit/time返回400；最近50条旧接口data格式仍可读。
- [ ] **Step 2：定义查询输入和返回。**

```go
type RequestFilter struct {
    RequestID, Status, Protocol string
    ProviderID, ModelID, BeforeID int64
    FromUTC, ToUTC *time.Time
    Limit int
}
type RequestPage struct { Data []map[string]any `json:"data"`; NextCursor string `json:"next_cursor,omitempty"` }
func normalizedLimit(limit int) (int,error) {
    if limit==0 { return 50,nil }; if limit<1 || limit>100 { return 0,errors.New("limit must be 1..100") }; return limit,nil
}
```

查询固定id DESC，使用id<cursor并读limit+1；where条件拼接固定SQL片段，值参数化。游标编码十进制ID的base64URL，不在游标中塞SQL。时间用RFC3339解析后转UTC；管理页输入以北京时间解释并传带时区时间。
- [ ] **Step 3：查询详情与错误日志。** `/admin/api/requests/{request_id}`返回元数据，无内容/秘密；固定索引(created_at_utc,id)、(status,id)、(provider_id,id)优先，SQLiteEXPLAIN验证，避免给所有字段盲目加索引。INFO异常日志只输出枚举错误码、失败阶段、内部ID、数值计时/状态；Cause和供应商原错误body不落日志。添加虚构secret/prompt/tool参数在错误中出现的防泄漏测试。
- [ ] **Step 4：跑数据库与HTTP测试。** `go test ./internal/store ./internal/httpapi ./internal/logging -count=1`；确保访问查询串、Authorization、X-Admin-Key不出现于日志。
- [ ] **Step 5：提交。** `feat: query request failures with safe pagination and metadata`。

## Task 3：管理端请求诊断页

**Files:** Modify `web/admin.html`、`docs/admin-api.md`、`docs/client-integration.md`；Create `scripts/tests/test_request_diagnostics.py`。

- [ ] **Step 1：提供60条假请求的管理API夹具。** 包含failed503、header超时、已发200的stream_failed、aborted、estimated usage；模拟空页、查询失败、加载更多。操作只使用虚构管理员Key。
- [ ] **Step 2：实现筛选与详情。** 按状态/渠道/模型/协议/时间/请求编号查询，保留筛选后翻页；每行显示状态、真实上游HTTP、用量来源、时长。错误文字按error_code本地映射，不向UI输出堆栈。追加可复制request_id与详情弹窗，phase1/2失败可直接找到。预计字段示例：

```json
{"request_id":"relay-example","status":"failed","upstream_status":503,"protocol":"chat","error_code":"upstream_unavailable","failure_stage":"headers","duration_ms":214,"first_event_ms":null,"usage_source":"unknown"}
```

JS渲染沿用现有escapeHTML，禁止把request_id/供应商名等未转义插入innerHTML；异步筛选以查询序号丢弃旧响应，避免慢请求覆盖新结果。
- [ ] **Step 3：协议文档写明HTTP200与失败终态不同。** 提供按request_id排查的操作，说明estimated不等于真实上游usage、NULL时间含义和两种超时。API新字段属于追加兼容，不删旧data字段。
- [ ] **Step 4：真实浏览器操作验收。** 1440×900及390×844执行筛选/清空/更多/请求详情/复制；无横向溢出，查询失败可重试，旧响应不会覆盖当前筛选。脚本自动部分 `python -m unittest discover -s scripts/tests -p 'test_request_diagnostics.py' -v`；浏览器交互结果单独记录，不把API测试称为UI验收。
- [ ] **Step 5：阶段验收并提交。** `feat: expose actionable request diagnostics in admin`；运行总计划检查。阶段1/2/4可形成第一批稳定性发行包，然后进入阶段3。

## 本阶段出口

- [ ] 取消、无DONE、503三个已知缺陷的修复结果都能按请求编号查到。
- [ ] 原始内容、密钥和供应商错误body不进入账本/诊断日志。
- [ ] 旧记录不因为缺新计时列而打不开，分页无重复漏页。
- [ ] 记录真实首业务事件时间，而非把心跳当成模型开始回答。
