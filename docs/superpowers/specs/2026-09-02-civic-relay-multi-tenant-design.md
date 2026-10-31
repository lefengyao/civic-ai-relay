# Civic Relay 局域网多租户服务设计

## 目标

将当前“单一公共 API Key + 单供应商 + 全局限额”的 Relay 升级为适合局域网多人使用的多租户服务：管理员可以添加多个供应商，定义公开模型别名和模型组，为每位使用者发放独立客户端 Key，并分别限制并发、模型权限、Token 总量和人民币总额度。任一客户端 Key 达到其一次性总额度后自动停用。

## 已确认的产品规则

- 每个客户端 Key 是独立凭证，可随时启用、停用或轮换。
- 一个 Key 可以加入多个模型组；管理员可随时更换分组，权限对新请求立即生效，正在进行的流式请求不被强制中断。
- 模型组只负责授权，模型仍以唯一的公开别名对外提供。模型别名由管理员设置，例如 `deepseek/chat`。
- 每个模型绑定一个供应商和一个上游模型 ID。供应商之间的同名模型不会冲突，因为公开别名必须唯一。
- 每个 Key 的 Token 总量和人民币金额总量都是一次性额度，永不自动重置。任一额度达到上限，Key 自动停用。
- 金额使用人民币，按模型输入/输出单价计算，单价单位为“元 / 百万 Token”。单价允许为 0。
- 新同步模型默认“未定价、未启用”，在管理员填写价格并启用前不能加入模型组或被客户端调用。
- “空额度”表示不限额；并发上限始终要求为正整数。
- 供应商均按 OpenAI 兼容协议提供 `/v1/models` 和 `/v1/chat/completions`。

## 架构

```text
客户端 Key
  └─ 多个模型组
       └─ 多个公开模型别名
            └─ 一个供应商 + 一个上游模型 ID
                 └─ 独立的上游 HTTPS/API Key
```

请求流程：

1. 从 `Authorization: Bearer <client-key>` 解析 Key 前缀，使用摘要查找并校验客户端 Key 的启用状态。
2. 校验 Key 对目标公开模型的组权限，以及模型启用、定价和供应商启用状态。
3. 在 SQLite 事务中同时预留全局和 Key 级 Token/金额额度；预留失败时不发送上游请求。
4. 获取该模型绑定的供应商客户端，按供应商真实模型 ID 转发请求。
5. 非流式或流式完成后按实际用量结算；释放并发槽；若累计 Token 或金额达到总额度，将 Key 原子地标记为停用。

## 数据模型

新增或扩展 SQLite 表：

### `providers`

- `id`、`name`（唯一）、`base_url`、`encrypted_api_key`、`enabled`、`created_at`、`updated_at`。
- API Key 使用 `RELAY_ENCRYPTION_KEY` 保护的 Fernet 加密文本保存，不保存明文。
- 删除/停用供应商前，拒绝仍有启用模型绑定的操作，避免请求静默失败。

### `models`

- `id`、`public_name`（唯一）、`provider_id`、`upstream_name`、`input_price_microyuan_per_million`、`output_price_microyuan_per_million`、`priced`、`enabled`、`created_at`、`updated_at`。
- 单价使用整数微元存储，避免浮点误差；`1 元 = 1,000,000 微元`。
- 同步只新增/更新供应商返回的上游模型，不覆盖管理员设置的公开别名、价格、启用状态；首次导入时公开别名默认为 `<provider-name>/<upstream-name>`，发生冲突时要求管理员修改。

### `model_groups` 与 `model_group_members`

- 分组名称唯一，成员是模型 ID，多对多关系。
- 未加入任何组的模型不会对任何客户端 Key 可见。
- 删除模型组前，先移除成员关系；删除模型时保留请求账本中的历史模型文字记录。

### `client_keys` 与 `client_key_groups`

- `client_keys` 保存 `id`、`name`、`key_prefix`、`key_digest`、`enabled`、`max_concurrency`、`token_limit_total`、`money_limit_microyuan_total`、`token_used`、`money_used_microyuan`、`disabled_reason`、时间戳。
- 原始 Key 只在创建响应中返回一次；摘要使用 SHA-256/恒时比较，列表和日志不返回原文。
- 一个客户端 Key 可加入多个模型组。
- 迁移现有 `PUBLIC_API_KEY` 时创建“旧版兼容 Key”；其原始值只用于一次性导入摘要，管理员可以在控制台停用。

### `requests`

- 保留现有请求元数据和全局额度字段，新增 `client_key_id`、`provider_id`、`reserved_money_microyuan`、`charged_money_microyuan`。
- 只记录 Key ID/名称、模型、供应商、Token、金额、状态、HTTP 状态和耗时；不记录提示词、回复、Authorization 或任何上游 Key。

## 额度和并发算法

- Key 级并发使用单进程内按 `client_key_id` 分桶的并发守卫；全局并发守卫继续保留。
- 预留 Token = 输入估算 + 有效最大输出 Token。
- 预留金额 = 输入预估 Token × 输入单价 + 最大输出 Token × 输出单价，按微元整数向上取整。
- SQLite 事务检查 `已结算 + 已预留 + 本次预留`，同时满足全局限额和 Key 限额才插入 `reserved` 记录。
- 请求失败或中断时释放预留并结算实际消耗；实际响应缺少 usage 时沿用现有字符估算回退。
- 只有在结算后的累计值达到 Token 或金额总额时才自动停用，避免一次大 `max_tokens` 预留失败导致尚有余额的 Key 被误停用。
- 已停用 Key 的请求返回标准 `401 invalid_api_key`；达到额度的管理列表显示停用原因和最终用量。

## 管理 API 与页面

保留现有 `X-Admin-Key` 管理认证，新增以下管理资源：

- 供应商：列表、创建、编辑、启停、删除、单供应商模型同步。
- 模型：列表、公开别名编辑、价格编辑、启停、供应商归属查看。
- 模型组：创建、重命名、删除、成员模型编辑。
- 客户端 Key：创建（一次性返回原文）、列表（只显示前缀和用量）、编辑额度/并发/启停、分组编辑、吊销并重新生成。
- 现有全局配置和监控页面继续保留。

页面分区使用中文标签并保留英文技术字段；危险操作（删除供应商、停用 Key、吊销 Key）要求二次确认。创建 Key 的明文只在当前页面内存中显示，不写入 localStorage、数据库或日志。

## 供应商同步和运行时

- 启动和定时任务逐个同步启用供应商的 `/v1/models`；单个供应商失败不会影响其他供应商和已有模型。
- 供应商配置变更后，新请求使用新配置；正在执行的请求持有旧客户端直到结束。
- 供应商 API Key 只在内存中解密并注入出站 Authorization，绝不返回客户端。
- 上游地址只允许管理员配置，并校验为 HTTPS（开发环境可显式允许 HTTP）；不接受来自客户端请求的 URL。

## 兼容迁移

- 首次启动创建 `providers`/`models`/`client_keys` 等表，并从现有 `.env` 导入一个默认供应商、现有模型白名单和“旧版兼容 Key”。迁移操作幂等。
- 生产配置文件不写入项目目录：Windows 默认使用 `C:\\ProgramData\\CivicRelay\\relay.env`，Linux 使用 `/etc/civic-relay/relay.env`；启动参数和 `CIVIC_RELAY_CONFIG_FILE` 显式指向该文件。项目目录只保留 `.env.example`，并提供首次初始化命令创建受保护配置文件。
- Windows 配置目录仅授予运行账户、Administrators 和 `SYSTEM` 访问权限；Linux 配置文件使用服务账户可读和 `0600` 权限。Docker 部署通过宿主机的 C 盘配置路径只读挂载到容器内，不把配置文件复制进镜像。
- 原有全局 RPM、Token、并发和最大输出限制继续作为服务级硬上限；客户端 Key 限额是额外的一层限制。
- 现有 `/v1/models` 仍返回 OpenAI 列表格式，但仅返回当前客户端 Key 有权访问的已启用且已定价模型。
- 现有 `/v1/chat/completions` 保持请求和响应格式；`/v1/responses` 不在本阶段实现。

## 错误处理

- 无效/停用/额度耗尽 Key：`401 authentication_error / invalid_api_key`。
- 无模型组权限、模型未启用或未定价：`400 invalid_request_error`，不泄露供应商内部配置。
- Key 或服务级并发已满：`429 rate_limit_error`。
- Key Token/金额余额不足：`429 rate_limit_error`，返回稳定错误码，管理页显示剩余量。
- 单供应商不可用：该供应商模型返回 `502 upstream_error`，其他供应商请求继续服务。

## 测试策略

使用 TDD，先为每项行为添加失败测试，再实现最小代码：

- Key 摘要认证、一次性明文返回、停用/吊销、分组权限和 `/v1/models` 过滤。
- Key 级并发在并行请求下只允许配置数量，释放后可再次请求。
- Token 与金额预留、实际结算、并发预留不超额、达到任一额度自动停用。
- 模型组变更实时生效；未定价模型不能加入组或调用。
- 多供应商模型别名路由、独立同步失败隔离、供应商 API Key 不出现在响应/日志。
- 旧配置幂等迁移和现有单 Key/全局限额回归。
- 管理 API 权限、危险操作确认和敏感字段脱敏。

## 非目标

- 本阶段不实现 OAuth/SSO、多组织账单、按月自动重置、实际付费扣款、`/v1/responses` 或多进程共享并发协调。
- 运行在多进程/多副本前，需要将 Key 并发从内存守卫升级为 Redis 等共享限流器。
