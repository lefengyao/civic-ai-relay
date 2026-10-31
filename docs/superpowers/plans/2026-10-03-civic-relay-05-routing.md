# 阶段5：备用渠道与固定报价实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. User may explicitly choose superpowers:subagent-driven-development instead. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 同一公开模型可配置多个授权上游，在安全范围内自动切换，客户端始终按公开模型固定报价结算。

**Architecture:** models 保留公开名称和单价，model_targets 保存供应商目标与顺序。一次准入冻结候选与报价、一次预留，有限尝试各记诊断，最终一次结算。任何已发送的业务事件都会关闭切换窗口。

**Tech Stack:** Go、SQLite、现有分组授权/报价函数、SSE 流会话、可注入时钟的内存熔断。

---

依赖阶段1/2/4/3，见[总计划](2026-10-03-civic-relay-hardening.md)。用户已选择固定公开模型报价；这里不设计供应商成本加价。现有默认供应商字段继续保留，避免改变旧模型删除和导入行为。

## Task 1：目标迁移、授权查询与事务内报价

**Files:**

- Create: `internal/store/targets.go`、`internal/store/targets_test.go`、`internal/store/routing_migration_test.go`。
- Modify: `internal/store/schema.go`、`internal/store/models.go`、`internal/store/keys.go`、`internal/store/ledger.go`、`internal/relay/service.go`、`internal/relay/pricing.go`（阶段1已建立）。

- [ ] **Step 1：写 v14 升级与授权矩阵。** `TestModelTargetsMigration` 断言每个旧模型生成一个默认目标，public_name/三种单价不变，max_attempts=1。`TestAuthorizedTargets` 设 A/B 两供应商、A/B 两组、一个公开模型与两目标，Key只授权B时只能访问B目标；停用目标/供应商/组/模型或模型未定价时不能访问。多个组授权同目标不重复列出模型。`TestPublicQuoteStableAcrossTargets` 设输入/输出价格不同的供应商场景，公开模型单价始终不变。
- [ ] **Step 2：确认缺失并运行。** `go test -count=1 ./internal/store ./internal/relay -run 'Test(ModelTargetsMigration|AuthorizedTargets|PublicQuoteStableAcrossTargets)' -v`。当前缺少 targets；实施后 PASS。
- [ ] **Step 3：追加 v15 与回填。** 先确认最新迁移编号再使用：

```sql
ALTER TABLE models ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 1
    CHECK (max_attempts BETWEEN 1 AND 2);
CREATE TABLE model_targets (
    id INTEGER PRIMARY KEY,
    model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_name TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    UNIQUE (model_id, provider_id, upstream_name)
);
CREATE INDEX idx_model_targets_route ON model_targets(model_id, enabled, priority, id);
INSERT INTO model_targets(model_id,provider_id,upstream_name,priority,enabled)
    SELECT id,provider_id,upstream_name,0,1 FROM models;
```

Create/ImportModel 在同一事务建立默认目标；UpdateModel 修改旧 provider_id/upstream_name 时同步默认目标，不能留下两份不同映射。允许删备用目标；旧模型主供应商删除仍按原 ON DELETE CASCADE 删除模型，不静默转属。默认目标标识通过对应 provider_id/upstream_name 判定，更新冲突返回明确错误，由操作者先处理重复目标。

- [ ] **Step 4：建立唯一授权口径。** `AuthorizedTargets(ctx,keyID,modelID) ([]ModelTarget,error)` 的类型与排序为：

```go
type ModelTarget struct {
    ID, ModelID, ProviderID int64
    UpstreamName string
    Priority int
    Enabled bool
    ResponsesMode string
}
```

查询核心 JOIN：models→model_targets→enabled providers→group_providers→enabled model_groups→key_groups，限定 keyID/modelID、模型启用且已定价、目标启用；DISTINCT 去重，ORDER BY priority,id。公共模型列表和管理接入信息改用同一 EXISTS 目标授权条件。管理端查询停用Key仍可查看配置授权，公共入口先认证启用/到期，保留现有区分。

报价倍率取 Key 启用组中授权本次协议可用候选的最大 rate_milli。剔除无能力目标和禁用目标，但不因暂时熔断或实际选择改变倍率；备用切换不能降低/升高当次价格。同一事务校验授权、读取公开单价和倍率、计算预留并保存阶段1 quote_*，不能让 ReserveRequest 再按默认供应商的基础价重算。cached 单价为 nil 时先规范为 input 单价。给 ReserveInput 增加候选 IDs/协议所需字段，均为服务内部值；Store 再查询验证，不能信任客户端价格。

- [ ] **Step 5：运行单目标兼容与倍率回归。** `go test -count=1 ./internal/store ./internal/relay`。加入2x倍率的预留=结算价格、配置请求途中改价后旧lease仍按旧quote、新请求按新quote、仅授权备用的Key可用等断言。建议检查点 `feat: add authorized model targets and immutable public quotes`。

## Task 2：一次预留、有限尝试与明确切换边界

**Files:**

- Create: `internal/relay/router.go`、`internal/relay/router_test.go`、`internal/store/attempts.go`、`internal/store/attempts_test.go`。
- Modify: `internal/store/schema.go`、`internal/relay/service.go`、`internal/httpapi/stream_session.go`、`internal/httpapi/responses.go`、`internal/upstream/client.go`。

- [ ] **Step 1：用假客户端验证尝试序列。** `TestRouterFailoverBeforeOutput`：A503→B正常，两个attempt、一笔request、一笔预留、一次RPM计数，金额按公开quote。`TestRouterStopsAfterBusinessEvent`：已发送文本、tool delta、native response.created 或 bridge response.created 后故障都只尝试A。`TestRouterDoesNotRetryAmbiguousPost`：POST已写入后的响应头超时不尝试B。心跳已发但A返回503时允许B。max_attempts=1旧模型始终只请求一个目标。
- [ ] **Step 2：建立切换判定函数。** 先运行 `go test -count=1 ./internal/relay -run 'TestRouter' -v` 观察缺失。以下类型/函数放 router.go：

```go
type AttemptFailure struct {
    Status int
    Code string
    RequestWritten bool
    ConnectFailure bool
    BusinessSent bool
}

func retryEligible(f AttemptFailure) bool {
    if f.BusinessSent { return false }
    switch f.Code {
    case "client_canceled", "service_shutdown", "insufficient_quota",
        "upstream_quota_exceeded", "invalid_api_key", "unsupported_capability":
        return false
    }
    if f.ConnectFailure && !f.RequestWritten { return true }
    return f.Status == 429 || f.Status == 502 || f.Status == 503 || f.Status == 504
}
```

RequestWritten 来自 net/http/httptrace 的 WroteRequest 和连接阶段记录；只有 DNS/dial/TLS 失败且可证明没有请求写入才置 ConnectFailure。不能把任意 net.Error 置为连接失败。HTTP429只在没有识别出永久额度错误时用上述默认策略；错误分类只读取有界正文的 code/type，不存原正文，不发供应商原文。

- [ ] **Step 3：实现顺序尝试。** Router 在 Begin 之后使用已冻结的目标IDs/quote；每次新建payload深拷贝并替换 upstream model，不把上次目标名称复用给下次。max_attempts上限2，整个请求共享原截止时间，不为每次尝试刷新总预算；第二次前检查 context、Key/组/目标仍启用且授权、剩余预算和熔断。撤销授权时终止，不用已冻结授权绕过管理员停用。若两次都失败，最后eligible错误作为终态，attempt保留两个原因。

桥接的 response.created 移到成功选定目标、收到首个可转发业务事件时发送；之前只发送注释心跳。单写循环在实际发送每个JSON事件前置 BusinessSent=true，写失败也关闭切换许可。非流200后解析坏JSON、流200后缺终态、已执行但等头超时均不推断可重试。切换关闭前一个 Body 并等读goroutine退出，不能让旧事件进入新流。

- [ ] **Step 4：建立尝试账本。** v15追加：

```sql
CREATE TABLE request_attempts (
    id INTEGER PRIMARY KEY,
    request_row_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    target_id INTEGER REFERENCES model_targets(id) ON DELETE SET NULL,
    provider_id INTEGER REFERENCES providers(id) ON DELETE SET NULL,
    status TEXT NOT NULL,
    protocol TEXT NOT NULL DEFAULT 'chat',
    upstream_status INTEGER,
    error_code TEXT NOT NULL DEFAULT '',
    failure_stage TEXT NOT NULL DEFAULT '',
    started_at_utc TEXT NOT NULL,
    finished_at_utc TEXT,
    duration_ms INTEGER,
    upstream_header_ms INTEGER,
    first_event_ms INTEGER,
    business_sent INTEGER NOT NULL DEFAULT 0 CHECK (business_sent IN (0,1)),
    UNIQUE(request_row_id,ordinal)
);
ALTER TABLE requests ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;
```

定义 `StartAttempt(ctx context.Context, requestRowID, ordinal int64, target ModelTarget, protocol string) (int64,error)` 与 `FinishAttempt(ctx context.Context, attemptID int64, result AttemptResult) error`；AttemptResult字段为 Status/ErrorCode/FailureStage string、UpstreamStatus int、DurationMS/HeaderMS/FirstEventMS *int64、BusinessSent bool。protocol记录实际chat/responses_bridge/responses_native，requests.protocol取最后实际路径。结束写入独立清理context，started→终态幂等，不能二次创建同ordinal。requests.provider_id记录最后实际尝试供应商；渠道筛选也用 EXISTS attempts，能查到首个失败渠道。GroupOverview 继续按实际最后渠道归属并在界面注明口径，不把两个attempt算成两次客户端请求。首次没有可尝试目标时为 upstream_unavailable，provider_id为NULL、attempt_count=0。

attempt元数据写入失败不重复上游调用，进入可观察的诊断错误；客户端账本仍按阶段1终态处理。requests清理级联删除attempts，恢复时未结束attempt标 aborted。只有最终选定响应的usage计入客户端账单，不将失败切换的未知供应商消耗捏造为额外收费。

- [ ] **Step 5：验证与检查点。** 上述 TestRouter、`go test -count=1 ./internal/store ./internal/httpapi` PASS。建议 `feat: route bounded failover before business output`。

## Task 3：熔断与并发边界

**Files:** Create `internal/relay/circuit.go`、`internal/relay/circuit_test.go`；Modify `internal/relay/router.go`、`internal/relay/monitor.go`。

- [ ] **Step 1：定义可测试的状态。** 以 target ID 为键，不按 public model 名或共享供应商粗暴熔断；时钟参数是 `now func() time.Time`。核心类型如下：

```go
type circuitEntry struct {
    Failures int
    OpenUntil time.Time
    ProbeInFlight bool
}

type Circuits struct {
    mu sync.Mutex
    now func() time.Time
    entries map[int64]*circuitEntry
}
```

对外定义 `Allow(targetID int64) bool`、`Complete(targetID int64, success, eligibleFailure bool)`、`Forget(targetID int64)`。所有状态在短临界区内修改；不能持锁发上游请求。open窗口到期仅一个请求获得ProbeInFlight，其他跳过；该probe取消时释放标记并保留熔断，不能永久卡在half-open。
- [ ] **Step 2：写边界测试。** `TestCircuitThreeFailuresOpen`：三次 eligibleFailure 后60s跳过，59.9s仍拒、60s只准一探测；`TestCircuitSingleHalfOpenProbe` 并发100次Allow只1次true；成功清计数，探测失败再open60s；取消、不eligible失败释放probe但不计瞬时故障。`TestMonitorDoesNotAlterRouting` 模拟 /models 探活成功/失败均不改变熔断。运行 `go test -count=1 ./internal/relay -run 'Test(Circuit|MonitorDoesNotAlterRouting)' -v`，先缺失失败，实施后 PASS。
- [ ] **Step 3：接入Router与只读状态。** 连续成功重置计数，只有retryEligible定义的瞬时失败触发计数；输出后故障不触发自动切换，也不按首版规则计熔断。所有候选open时直接 upstream_unavailable，不绕过open大量探测。目标删除、供应商关键配置/模式修改时清对应状态并淘汰客户端；重启恢复空状态。/models探活保留展示用途。API展示remaining_ms及closed/open/half_open，不将内存状态写成永久enabled=false。
- [ ] **Step 4：验证竞争检查。** `go test -count=1 ./internal/relay`；Linux `go test -race -count=1 ./internal/relay`，预期 PASS且无竞争。建议 `feat: add bounded target circuit breakers`。

## Task 4：管理端配置、授权说明与全链路验收

**Files:** Modify `internal/httpapi/admin.go`、`web/admin.html`、`internal/httpapi/requestlog.go`、`README.md`；Create `internal/httpapi/admin_targets_test.go`、`internal/httpapi/routing_integration_test.go`。

- [ ] **Step 1：实现目标CRUD与顺序。** 管理API为 `/admin/api/models/{id}/targets` 的 GET/POST 及 `/admin/api/model-targets/{id}` 的 PUT/DELETE，沿用现有管理员认证与前缀；子路由必须在现有models/{id}解析前分流。批量排序在一个事务校验目标属于同一模型。输入只接受存在provider、非空合法upstream_name、priority整数、enabled布尔；重复唯一键返回409。max_attempts只接受1/2，并在UI显示“关闭切换 / 最多尝试两个渠道”。
- [ ] **Step 2：UI保持公开价格唯一。** 模型详情展示公开三种单价、默认目标和备用目标列表，不为每个target新增客户端收费字段。展示权限取决于Key分组、已发业务事件后不能切换；目标删除确认列明是否默认目标。请求详情显示各attempt状态、耗时、最后渠道和当时quote，保留阶段4筛选/分页。
- [ ] **Step 3：执行真实HTTP矩阵。** `go test -count=1 ./internal/httpapi -run 'Test(AdminTargets|RoutingIntegration)' -v` 覆盖 Chat/bridge/native，A503→B成功、A已发工具事件后断开、仅授权B、停用组、改价中的流、max_attempts1、两目标失败、全open。每例断言上游调用数、一次预留/结算、报价、request/attempt诊断和下游终态。再运行 `go test -count=1 ./...`、`go vet ./...`。
- [ ] **Step 4：浏览器验收与检查点。** 配置顺序后刷新仍保持；两组不同倍率的Key看到同一公开模型但各自倍率稳定；接入信息与实际模型目录一致。建议 `feat: expose model fallback configuration and attempt diagnostics`。

## 阶段验收

- [ ] 升级后默认单目标、一次尝试，旧模型请求/删除行为保持。
- [ ] 备用供应商不绕过Key分组授权；报价在事务内冻结且只应用一次倍率。
- [ ] 只在未发业务事件且明确允许的故障切换，POST执行不明时不重试。
- [ ] 熔断并发安全，探活仅展示；请求与尝试数量可核对。
- [ ] 公开客户端收费独立于实际备用供应商成本。
