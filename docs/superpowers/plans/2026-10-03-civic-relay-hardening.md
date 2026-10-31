# Civic Relay 六项补齐实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. User may explicitly choose superpowers:subagent-driven-development instead. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐可靠结算、长任务、智能体协议、诊断、多渠道备用和备份发布六项，使现有中转服务能稳定承载智能体请求。

**Architecture:** 保留 Go 单服务与 SQLite，在现有准入、预留和结算上建立统一请求生命周期。协议桥接与原生转发分开，路由只改变上游目标；公开模型单价和分组倍率在准入时冻结。各阶段分别测试和交付。

**Tech Stack:** Go 1.27、net/http、modernc.org/sqlite、嵌入式中文管理端、Docker/systemd、Python ZIP/发布工具；异地备份使用主机端 age 公钥加密。

---

## 执行状态与约束

日期：2026-10-03。用户要求本轮写计划，尚未开始实现。用户已选定固定公开模型报价。

项目根目录：`E:/生产项目/项目/TrailblazerStation/civic-ai-relay`。下面命令均在此目录执行；文件清单相对此根目录。

设计：[2026-10-03-civic-relay-reliability-design.md](../specs/2026-10-03-civic-relay-reliability-design.md)。现有 `.gitignore` 忽略 docs；这些文档目前保存在本地，不因生成计划而强制提交。

执行前读取适用 AGENTS.md，再记录 git status 和最新迁移编号。保留 `.gitignore`、脚本、deploy 目录的现有未提交工作，不整仓 add/reset/checkout。任务提交只包含本任务文件；若用户仅要求本地实现，提交步骤改为本地检查点。

## 六个分计划与依赖

| 阶段 | 分计划 | 依赖 | 独立交付条件 |
|---|---|---|---|
| 1 | [可靠结算](2026-10-03-civic-relay-01-settlement.md) | 无 | 取消/超时可结算、并发释放、真实HTTP状态、幂等和恢复 |
| 2 | [长任务与流结束](2026-10-03-civic-relay-02-streaming.md) | 1 | 心跳、0不限、坏流不假成功、停机顺序正确 |
| 4 | [请求诊断](2026-10-03-civic-relay-04-diagnostics.md) | 1、2 | 请求编号、失败阶段、时间与用量来源可查 |
| 3 | [智能体协议](2026-10-03-civic-relay-03-responses.md) | 1、2、4 | bridge/native工具两轮闭环、推理字段保留/明确拒绝 |
| 5 | [备用渠道与固定报价](2026-10-03-civic-relay-05-routing.md) | 1、2、3、4 | 授权候选、输出前有限切换、固定价格、熔断 |
| 6 | [备份与发布验证](2026-10-03-civic-relay-06-operations.md) | 本地备份需1、2、4；最终发布需全部 | 在线快照、隔离恢复、定时轮换、版本发布闸门 |

顺序为 **1 → 2 → 4 → 3 → 5 → 6**。诊断先于协议扩展，避免新增复杂路径却无法解释失败。各分计划均用具体任务、回归例子、文件和命令交付，不把六项压成一个不可验收的大改动。

备份不必等协议和路由全部完成：阶段6的Task1可在首次迁移发布前先完成，Task2/3在1/2/4后可提前。Task4的全功能发布门禁再等3/5完成；不以编号靠后为理由推迟数据恢复能力。

## 工作单元与文件边界

| 单元 | 新文件/主要既有文件 | 职责 |
|---|---|---|
| 结算 | internal/store/settlement.go、internal/relay/settlement.go | 幂等终态、独立context、有界重试 |
| 活跃请求/恢复 | internal/relay/active.go、internal/relay/reconcile.go、internal/store/maintenance.go | 活跃ID、孤儿回收、失败结算重试 |
| SSE | internal/upstream/sse_reader.go、internal/httpapi/stream_session.go | 事件分帧、单写循环、心跳和终态 |
| 超时 | internal/config/settings.go、internal/upstream/client.go | 分阶段预算与配置兼容 |
| Responses | internal/httpapi/responses_bridge.go、internal/upstream/responses.go | 协议映射和原生传输 |
| 追踪 | internal/requestmeta/meta.go、internal/httpapi/requestlog.go、internal/store/request_query.go | 关联ID、安全元数据和查询 |
| 路由 | internal/store/targets.go、internal/relay/router.go、internal/relay/circuit.go | 授权候选与有限切换 |
| 备份 | internal/backup/service.go、internal/store/snapshot.go、internal/httpapi/admin_backup.go | 一致快照和管理元数据 |
| 发布 | scripts/verify_release.py、.github/workflows/ci.yml | 交付校验和持续回归 |

大文件仅抽取对应职责，不进行无关重构。新增文件各自同名 `_test.go`；涉及已知故障的集成夹具进仓库 `internal/testutil`/`scripts/tests`，不能只留在被忽略的 _e2e 或 .codex 下。

## 公共契约

以下类型与诊断字段由阶段1先建立，阶段4填充计时与查询；阶段5只使用已定义类型：

```go
// internal/store/settlement.go
type Settlement struct {
    Status string // completed, failed, aborted
    InputTokens, OutputTokens, CachedInputTokens, AmountMicroyuan int64
    UpstreamStatus int // 0 means no upstream response received
    ErrorCode, FailureStage, UsageSource, Protocol, IncompleteReason string
    DurationMS, HeaderMS, FirstEventMS, LastUpstreamMS *int64
}

// internal/relay/pricing.go; public model quote, never target-specific
type PriceSnapshot struct {
    ModelID int64
    InputPriceMicroyuan, OutputPriceMicroyuan, CachedInputPriceMicroyuan int64
    RateMilli int64
}
```

`Store.FinishReservation(ctx, reservationID, Settlement) error` 是新入口，旧 SettleRequest 作为兼容包装保留；Lease 使用新入口。价格快照后续迁移到请求字段，存储值取倍率折算之前的单价+rate_milli，结算统一调用已有 PriceUsage 折算一次，禁止倍率重复相乘。

PriceSnapshot在阶段1建立；cached单价为nil时规范为普通input单价。Store.ReserveRequest在事务内取公开单价与授权倍率，保存quote_*并按同一报价计算预留；当前代码会重新读取基础价，实施时必须一起修正。PriceUsage本身不施加倍率，调用它前先用现有applyRate构造有效Model，不能把已经折算过的Model再乘一次倍率。

失败原因至少覆盖：client_canceled、upstream_connect_timeout、upstream_header_timeout、upstream_idle_timeout、request_duration_exceeded、upstream_http_error、upstream_connection_failed、upstream_stream_read_failed、upstream_stream_incomplete、upstream_event_invalid、upstream_incomplete、upstream_unavailable、downstream_write_failed、memory_limit_exceeded、service_shutdown、settlement_failed、settlement_unavailable、crash_recovered。

请求状态只增加语义，不随意改账本CHECK约束：合法截断的Responses response.incomplete对应数据库failed、error_code=upstream_incomplete；客户端取消对应aborted。备份恢复的孤儿请求对应aborted、usage_source=unknown。

## 数据库迁移

当前最高 v12；执行时再次读取 `internal/store/schema.go`，如果其他工作已经新增编号，向后顺延。

| 计划编号 | 内容 | 后向兼容策略 |
|---|---|---|
| v13 | requests的错误/协议/计时/usage来源与价格快照列 | NULL/空值默认，旧记录可读；既有模型/Key不变化 |
| v14 | providers.responses_mode | 默认bridge |
| v15 | model_targets、request_attempts、models.max_attempts | 每个旧模型映射一个target，max_attempts默认1 |

禁止删除原模型provider_id、upstream_name和既有价格列；旧模型删除语义保持一致，不能让被删除的默认供应商下模型自动改属备用供应商。新增target的删除与主模型删除分别处理。

## 回归矩阵

| 场景 | 必须满足 |
|---|---|
| Key/App-ID、已有模型/组 | 旧配置和v12数据升级后仍能认证、列模型、请求、计费 |
| 取消/超时/数据库busy | 终态可见；无悬挂预留、双收费、误释放其他请求的并发 |
| 等头/静默/慢流/坏流 | 心跳与超时作用正确；终态按各协议认定 |
| Responses工具/推理 | 输入字段不会默默丢；两轮工具调用保持call_id |
| 原生未知事件 | 完整转发，不拆错帧、不输出供应商密钥 |
| 流式错误 | 已发200仍有失败事件和真实诊断；没有伪completed |
| 备用路由 | 只用授权、启用、协议匹配渠道；一次客户端预留和一次固定报价 |
| 快照与恢复 | 并发写入快照一致；错误哈希/密钥/逃逸归档被拒；恢复后模型密钥可解密 |
| 停机 | 请求取消、结算、后台循环、数据库按顺序结束 |
| 发布包 | 构建版本和源码/镜像/ZIP实际内容匹配；无配置、DB、密钥夹带 |

常规验收命令：

```powershell
go test -count=1 ./...
go vet ./...
python -m unittest discover -s scripts -p 'test_*.py'
git diff --check
```

Linux具备GCC时运行 `go test -race -count=1 ./...`；Windows无CGO环境不把race未执行记成通过。浏览器检查使用项目现有测试工具或可用浏览器控制，关注请求筛选、响应模式、目标排序、配置保存、备份状态，不凭截图代替行为检查。

## 发布与回退检查点

- [ ] 阶段1/2/4及阶段6-Task1的备份恢复安全修正通过后，生成建议版本v0.3.8的离线镜像包和部署源码包；用隔离v12数据库完成升级/取消/慢流回归。
- [ ] 阶段3通过后，生成建议版本v0.3.9；所有旧渠道默认bridge，native必须显式启用。
- [ ] 阶段5/6通过后，生成建议版本v0.4.0；模型默认单target/max_attempts=1，自动备份默认关闭。
- [ ] 每次迁移前生成并验证旧版本对应备份；归档当前配置、数据库、镜像标签和hash。
- [ ] 灰度检查真实构建版本、成功率、错误码、首事件时间、预留数、内存、备份成功；异常按上一版镜像+对应数据库备份恢复。

版本号是计划建议，不在本轮修改；真实发布前核对最新版本，不能覆盖已有同版本产物。规划仅准备交付物；访问服务器、启用自动备份目的地和生产部署需要后续实际实施范围。

## 计划自查

- [x] 六项需求各有独立分计划和验收，不遗漏取消、错误HTTP状态和无DONE三个已复现问题。
- [x] 固定公开模型报价写入路由、授权和账本契约。
- [x] 处理无限流与reaper、已发心跳后的HTTP错误、备用授权、Responses会话ID隔离四处容易互相冲突的边界。
- [x] 主机备份路径与distroless/只读根文件系统/UID65532/Deploy卷同步写入操作计划。
- [x] 所有任务进度框仍未勾选；生成计划不冒充完成实现或测试通过。
