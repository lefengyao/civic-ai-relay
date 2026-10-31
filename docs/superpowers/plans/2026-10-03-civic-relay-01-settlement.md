# 阶段1：可靠结算 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. This document authorizes no execution by itself.

**Goal:** 取消、超时和瞬时数据库错误后可靠释放预留，准确记录真实HTTP状态，不重复计费。

**Architecture:** Store事务是终态与计费的唯一原子入口；Lease用独立有界context处理清理，内存闸门释放和数据库成功状态分开。活跃注册与有界恢复队列配合启动/周期恢复。

**Tech Stack:** Go 1.27、context.WithoutCancel、sync、modernc SQLite。

---

项目根目录与兼容要求见[总计划](2026-10-03-civic-relay-hardening.md)。本阶段无依赖。原缺陷样例在 `.codex/maturity-audit/audit_test.go`；正式回归断言必须改为期望正确行为，不复制“观察到缺陷即通过”的判断。

## Task 1：可复用隔离夹具与三条失败回归

**Files:** Create `internal/testutil/relay_fixture.go`、`internal/relay/settlement_lifecycle_test.go`（外部包 relay_test）、`internal/store/settlement_test.go`（store_test）；Modify `internal/relay/service_test.go`。

- [ ] **Step 1：建立夹具。** 新夹具仅用于测试；执行前确认已有同包测试没有因为导入testutil而形成循环，新生命周期测试统一用外部包。基础内容：

```go
package testutil

import (
    "context"
    "encoding/base64"
    "path/filepath"
    "testing"
    "civic-ai-relay/internal/config"
    "civic-ai-relay/internal/relay"
    "civic-ai-relay/internal/secret"
    "civic-ai-relay/internal/store"
)

type RelayFixture struct { Service *relay.Service; Store *store.Store; Key store.ClientKey }
type fixtureFactory struct { client relay.UpstreamClient }
func (f fixtureFactory) ForProvider(context.Context, int64) (relay.UpstreamClient, error) { return f.client, nil }

func NewRelayFixture(t *testing.T, client relay.UpstreamClient) RelayFixture {
    t.Helper()
    box, err := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
    if err != nil { t.Fatal(err) }
    repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = repo.Close() })
    ctx := context.Background()
    p, err := repo.CreateProvider(ctx, store.NewProvider{Name:"fixture", BaseURL:"https://fixture.example", APIKey:"fake"})
    if err != nil { t.Fatal(err) }
    price := int64(1_000_000)
    _, err = repo.CreateModel(ctx, store.NewModel{ProviderID:p.ID, PublicName:"m", UpstreamName:"up-m", Enabled:true, InputPriceMicroyuan:&price, OutputPriceMicroyuan:&price})
    if err != nil { t.Fatal(err) }
    g, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name:"g"})
    if err != nil { t.Fatal(err) }
    if err = repo.ReplaceGroupProviders(ctx, g.ID, []int64{p.ID}); err != nil { t.Fatal(err) }
    k, err := repo.CreateClientKey(ctx, store.NewClientKey{Name:"k", ConcurrencyLimit:1})
    if err != nil { t.Fatal(err) }
    if err = repo.ReplaceKeyGroups(ctx, k.ID, []int64{g.ID}); err != nil { t.Fatal(err) }
    settings := config.Settings{GlobalConcurrencyLimit:2, RPMLimit:30, MaxOutputTokens:64}
    svc := relay.NewService(repo, fixtureFactory{client}, func() config.Settings{return settings})
    return RelayFixture{svc, repo, k}
}
```

- [ ] **Step 2：添加取消回归。** 包relay_test导入context/testing/relay/testutil；这里使用已有接口，不需要fake client的方法被调用。

```go
func TestCanceledSettlementReleasesKey(t *testing.T) {
    f := testutil.NewRelayFixture(t, nil)
    req := relay.Request{Token:f.Key.Token, Model:"m", InputText:"hi", Payload:map[string]any{}}
    lease, _, err := f.Service.Begin(context.Background(), req)
    if err != nil { t.Fatal(err) }
    ctx, cancel := context.WithCancel(context.Background()); cancel()
    if err = lease.Close(ctx, relay.Outcome{Status:"aborted"}); err != nil { t.Fatal(err) }
    rows, err := f.Store.RecentRequests(context.Background(), 1)
    if err != nil { t.Fatal(err) }
    if len(rows)!=1 || rows[0]["status"]!="aborted" { t.Fatalf("rows=%v", rows) }
    next, _, err := f.Service.Begin(context.Background(), req)
    if err != nil { t.Fatalf("key still blocked: %v", err) }
    if err = next.Close(context.Background(), relay.Outcome{Status:"aborted"}); err != nil { t.Fatal(err) }
}
```

另添加以下两个测试，补充io/net/http/strings/upstream导入；完整伪client保存在此测试文件，不依赖被忽略的观察脚本：

```go
type settlementFakeClient struct { body string; err error }
func (f *settlementFakeClient) ChatJSON(context.Context, map[string]any) ([]byte,error) {
    return []byte(f.body),f.err
}
func (f *settlementFakeClient) Stream(context.Context, map[string]any) (*http.Response,error) {
    if f.err!=nil { return nil,f.err }
    return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(f.body))},nil
}
func TestRepeatedSettlementChargesOnce(t *testing.T) {
    f:=testutil.NewRelayFixture(t,nil)
    lease,_,err:=f.Service.Begin(context.Background(),relay.Request{Token:f.Key.Token,Model:"m",InputText:"hi"})
    if err!=nil { t.Fatal(err) }
    outcome:=relay.Outcome{Status:"completed",InputTokens:7,AmountMicroyuan:7,HTTPStatus:200}
    if err=lease.Close(context.Background(),outcome); err!=nil { t.Fatal(err) }
    if err=lease.Close(context.Background(),outcome); err!=nil { t.Fatal(err) }
    rows,err:=f.Store.RecentRequests(context.Background(),1)
    if err!=nil { t.Fatal(err) }
    if len(rows)!=1 || rows[0]["amount_microyuan"]!=int64(7) { t.Fatalf("rows=%v",rows) }
}
func TestFailedUpstreamKeeps503(t *testing.T) {
    f:=testutil.NewRelayFixture(t,&settlementFakeClient{err:&upstream.Error{Code:"upstream_unavailable",Status:503}})
    _,err:=f.Service.Chat(context.Background(),relay.Request{Token:f.Key.Token,Model:"m",InputText:"hi",Payload:map[string]any{}})
    if err==nil { t.Fatal("expected upstream failure") }
    rows,err:=f.Store.RecentRequests(context.Background(),1)
    if err!=nil { t.Fatal(err) }
    if len(rows)!=1 || rows[0]["status"]!="failed" || rows[0]["upstream_status"]!=int64(503) { t.Fatalf("rows=%v",rows) }
}
```

- [ ] **Step 3：跑失败测试。** `go test ./internal/relay -run 'TestCanceledSettlementReleasesKey|TestRepeatedSettlementChargesOnce|TestFailedUpstreamKeeps503' -count=1 -v`。取消与503在当前代码失败，重复结算守住当前已有不重复收费行为。
- [ ] **Step 4：确认所有新测试只使用TempDir、虚构凭据和假客户端。** 不连接真实供应商，不依赖被忽略的文件。
- [ ] **Step 5：保留为任务检查点。** 本阶段全部通过后再提交夹具与测试，提交信息 `test: cover cancellation and upstream settlement outcomes`；不要只提交红色测试作为可发布状态。

## Task 2：Store终态入口与真实上游状态

**Files:** Create `internal/store/settlement.go`、`internal/relay/pricing.go`；Modify `internal/store/ledger.go`、`internal/store/schema.go`、`internal/relay/service.go`；Test `internal/store/settlement_test.go`、`internal/relay/settlement_lifecycle_test.go`、`internal/relay/pricing_test.go`。

- [ ] **Step 1：追加v13迁移。** 使用总计划的Settlement字段；先建错误/usage/protocol/计时/报价字段，原始单价和rate可为NULL，旧记录保留。核心SQL：

```sql
ALTER TABLE requests ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN failure_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN usage_source TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE requests ADD COLUMN protocol TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE requests ADD COLUMN client_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN incomplete_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN duration_ms INTEGER;
ALTER TABLE requests ADD COLUMN upstream_header_ms INTEGER;
ALTER TABLE requests ADD COLUMN first_event_ms INTEGER;
ALTER TABLE requests ADD COLUMN last_upstream_ms INTEGER;
ALTER TABLE requests ADD COLUMN quote_input_microyuan INTEGER;
ALTER TABLE requests ADD COLUMN quote_output_microyuan INTEGER;
ALTER TABLE requests ADD COLUMN quote_cached_microyuan INTEGER;
ALTER TABLE requests ADD COLUMN quote_rate_milli INTEGER;
```

- [ ] **Step 2：实现 `FinishReservation(ctx,id,Settlement) error`。** 复制现有finishRequest事务边界和validation，在BEGIN IMMEDIATE后按id查status。reserved才更新请求与key_reservation并清预留；已终态且终态/金额/用量一致返回nil；冲突返回`ErrSettlementConflict`，不写库。不得通过“查询不到reserved即忽略”吞掉不存在的ID。新增错误：

```go
var ErrSettlementConflict = errors.New("settlement outcome conflicts with finalized request")
var ErrReservationNotFound = errors.New("reservation not found")
```

真实状态转换以已知值为准；无响应头时写NULL：

```go
func upstreamStatusValue(status int) any {
    if status == 0 { return nil }
    return status
}
```

SQL更新的upstream_status参数改用该函数，同时写入Settlement的稳定元数据。旧SettleRequest包装为Status+200的历史接口；Service的所有终态改调用FinishReservation，不能继续使用硬编码200入口。成功结算与异常结算共用一次事务，不在失败后另建第二笔账。

建立总计划PriceSnapshot；ReserveRequest当前会重读模型基础价，需在同一事务查询授权组最大倍率并按公开单价折算预留，返回该快照写入Lease。保存quote_*（原始单价+倍率）到requests，预留和最终结算均按它计价。增加 `TestReservationUsesGroupQuote`：2x组倍率下预留金额为基础价的2倍；`TestLeaseQuoteSurvivesPriceUpdate`：流中改价不会改变旧lease，下一请求采用新价。阶段5再把授权查询扩展为多目标，不能等到阶段5才让单目标报价一致。

- [ ] **Step 3：在Chat/Stream失败分支提取HTTP状态。** 不读取供应商原始响应正文；errors.As识别upstream.Error：

```go
func upstreamFailure(err error) (int, string) {
    var u *upstream.Error
    if errors.As(err, &u) { return u.Status, u.Code }
    if errors.Is(err, context.Canceled) { return 0, "client_canceled" }
    if errors.Is(err, context.DeadlineExceeded) { return 0, "request_duration_exceeded" }
    return 0, "upstream_connection_failed"
}
```

阶段2补细分超时原因；此处Outcome增加ErrorCode/FailureStage/UsageSource/Protocol和可空计时，按总计划类型转换到Settlement。

- [ ] **Step 4：验证原子与迁移。** `go test ./internal/store ./internal/relay -count=1`；增加v12→v13重复启动、unknown ID、重复/冲突结算、中途数据库错误回滚案例。并发Close守住只收一次金额。
- [ ] **Step 5：检查并提交。** `git diff --check`；只提交本任务涉及文件，信息 `fix: persist idempotent settlement and actual upstream status`。

## Task 3：独立清理context与重试

**Files:** Create `internal/relay/settlement.go`、`internal/relay/settlement_retry_test.go`；Modify `internal/relay/service.go`、`internal/httpapi/public.go`、`internal/httpapi/responses.go`。

- [ ] **Step 1：写截止时间已过/手动取消/上游body超时回归。** 全部期待aborted或failed落库，当前代码取消测试失败；用短context而不是等分钟。数据库busy用可控写事务持锁再解除验证成功重试；永久错误测试要求有界返回。
- [ ] **Step 2：实现清理context和retry判定。** 父请求值保留，取消和deadline解除；只对SQLite错误码busy/locked重试：

```go
func settlementContext(parent context.Context) (context.Context, context.CancelFunc) {
    return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
func retryableSettlement(err error) bool {
    var coded interface { Code() int }
    if !errors.As(err, &coded) { return false }
    code := coded.Code() & 0xff
    return code == 5 || code == 6
}
func settleWithRetry(ctx context.Context, fn func(context.Context) error) error {
    waits := []time.Duration{0,100*time.Millisecond,300*time.Millisecond}
    var err error
    for _, wait := range waits {
        if wait>0 {
            timer:=time.NewTimer(wait)
            select { case <-timer.C: case <-ctx.Done(): timer.Stop(); return ctx.Err() }
        }
        err=fn(ctx)
        if err==nil || !retryableSettlement(err) { return err }
    }
    return err
}
```

- [ ] **Step 3：更新Lease Close状态。** 增加outcomeFrozen、settled、frozenOutcome、releaseOnce；互斥锁保护第一次终态选择和settled检查。第一次结果冻结，后续Close不改结果；调用settlementContext/settleWithRetry；数据库成功才settled=true，闸门通过releaseOnce释放。不能把旧released=true同时当作“数据库已经结算成功”。HTTP处理器检查错误、输出安全request_id+settlement_failed日志，停止`_ = lease.Close(...)`静默处理。
- [ ] **Step 4：运行定向和全相关测试。** `go test ./internal/relay ./internal/httpapi ./internal/store -count=1`；Task1取消/503转绿，重复结算金额不增加；测试永久错误不超过清理budget+测试容差。
- [ ] **Step 5：提交本任务。** 信息 `fix: detach settlement cleanup from canceled requests`；HTTP200流式异常的客户端事件暂由阶段2处理。

## Task 4：活跃注册、有界恢复和停机顺序

**Files:** Create `internal/relay/active.go`、`internal/relay/reconcile.go`及其测试；Modify `internal/store/maintenance.go`、`cmd/civic-relay/main.go`、`cmd/civic-relay/main_test.go`。

- [ ] **Step 1：写恢复验收测试。** 无限时长活跃lease不被reap；热改小总时长不误reap旧活跃lease；崩溃残留启动前恢复；数据库恢复后pending终态写成功；队列满暂停公共新请求；SIGTERM在数据库关闭之前完成取消与结算。
- [ ] **Step 2：实现ActiveReservations。** API契约：

```go
type ActiveReservations struct { mu sync.RWMutex; ids map[int64]struct{} }
func (a *ActiveReservations) Register(id int64) {
    a.mu.Lock(); defer a.mu.Unlock()
    if a.ids==nil { a.ids=make(map[int64]struct{}) }; a.ids[id]=struct{}{}
}
func (a *ActiveReservations) Remove(id int64) { a.mu.Lock(); defer a.mu.Unlock(); delete(a.ids,id) }
func (a *ActiveReservations) IDs() []int64 {
    a.mu.RLock(); defer a.mu.RUnlock()
    ids:=make([]int64,0,len(a.ids)); for id:=range a.ids { ids=append(ids,id) }; return ids
}
```

Service.Begin在预留成功后注册；结算成功后移除，失败则先原子移交pending再移除active，不能留下reaper可见的空窗。Store增加 `ReapOrphanReservations(ctx, cutoff time.Time, activeIDs []int64, pendingIDs []int64)`，事务中排除两个ID集合，参数化SQL；空集合不能构造非法NOT IN()。Prune同样排除reserved请求及预留，不能因为长流超过retention而删活跃账目。启动时单实例所有reserved可恢复为aborted；记录明确的crash_recovered，unknown usage不收费。

- [ ] **Step 3：实现Reconciler和结算槽。** 每5s对pending Settlement调用FinishReservation；按reservationID去重，失败保留、成功删除。公共准入先申请一个结算槽，active与pending共用以global concurrency为目标的容量，active→pending只转移既有槽，结算成功才归还。无空槽时PublicAdmission返回503 settlement_unavailable，避免“pending尚未满又放入一批active”使有界队列溢出；失败不得丢弃已有结果。热调低容量只阻止新请求，保留当前active/pending直到自然回落，不能截断队列；热调高允许扩容。正常Close失败先移交pending，不能对active流用旧配置阈值猜测终态。给槽申请/归还/移交增加并发故障测试。
- [ ] **Step 4：修正Close顺序。** 公共准入进入draining→Server.Shutdown最多15s→截止时取消活跃upstream contexts并关闭body→短窗口排空结算→停止维护/reconciler→关闭Store。所有goroutine生命周期由Application持有，测试不能靠进程退出兜底。运行 `go test ./cmd/civic-relay ./internal/relay ./internal/store -count=1`；Linux另跑race。
- [ ] **Step 5：提交并阶段验收。** 信息 `fix: reconcile failed settlements and exclude active reservations`。执行总计划常规命令，记录故障重试可用性和崩溃usage无法准确复原的边界，不将恢复为aborted宣称为精确计费。

## 本阶段出口

- [ ] 并发1取消后下一请求可立即进入；重复Close、reconciliation重试不重复收费。
- [ ] 真实503/429/无头超时保留真实状态或NULL，不能伪200。
- [ ] 无限流、热改配置、失败结算队列、停机与崩溃恢复均有可重复隔离测试。
- [ ] 报告包含执行的命令和结果，进入阶段2。
