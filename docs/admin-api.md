# Civic Relay API 参考

> 与 `internal/httpapi/admin.go`、`public.go`、`server.go` 保持同步（2026-11-09 核对）。

所有管理接口挂在 `/admin/api/` 下，**没有任何匿名路径**：要么用管理员密钥（请求头 `X-Admin-Key: <管理员密钥>`，能读写一切），要么用分发出去的客户端密钥（`Authorization: Bearer crk_...`，只读且只能看到自己所属分组）。公共转发接口挂在 `/v1/` 下，同样用 `Authorization: Bearer crk_...`。

控制台（`/admin/`）打开即是登录页，输入框自动判别两种密钥：`crk_` 开头按客户端密钥校验，其余按管理员密钥校验。登录探针是 `GET /admin/api/session`：成功返回 `{"admin":true}` 或 `{"admin":false,"scope":{"key_id":1,"group_id":2,"group_name":"福利"}}`；凭据缺失或无效返回 401。

**密钥主体的可见范围**（写操作一律 404，跨组一律 404 而非 403 —— 组 ID 是自增整数，403 等于确认它存在）：

| 路径 | 管理员 | 客户端密钥 |
|---|---|---|
| `overview` / `analytics` / `requests` | 全站，可按 `?group_id=N` 收窄 | 只有自己那一组；`requests` 只有自己那把密钥的行 |
| `me` | 400 `not_applicable` | 自己这把密钥的用量、限额、有效期、可用模型数 |
| `providers` / `models` | 全部 | 仅组内渠道（**不含 `base_url`**）与该组授权模型 |
| `groups` / `groups/{id}` / `groups/{id}/overview` / `groups/health` | 全部 | 只有自己那一个组 |
| `pools`（故障转移供应商）、`keys`、`keys/hints`、`config`、`plugins`、`providers/{id}/remote-models`、一切写操作 | 允许 | 404 |

认证失败一律 401，码分四种：`authentication_required`（没带凭据或管理员密钥不对）、`invalid_api_key`（客户端密钥不存在或已停用）、`token_expired`（短时令牌已过期 —— 凭据没错，只是该换新令牌了，客户端不应把它当服务故障重试）、`console_login_not_supported`（用应用型 `app_...` 标识登录控制台；它的密钥创建时即丢弃，无可验证物）。

错误统一返回 `{"error": {"message": "...", "type": "relay_error", "code": "...", "request_id": "..."}}`；`request_id` 同时出现在响应头 `X-Request-Id`，用于把客户端错误、流内错误和账本记录关联起来。服务端始终生成新的 request ID，客户端提供的 `X-Client-Request-Id`（兼容读取 `X-Request-Id`）只作为关联字段。公共转发接口的认证失败同样是 401，码为 `invalid_api_key`（令牌不存在或已停用）与 `token_expired`（短时令牌已过期 —— 凭据没错，只是该换新令牌了，客户端不应把它当服务故障重试）。所有列表字段在无数据时返回 `[]` 而非 `null`（含 `data`、`trend`、各组 `provider_ids`/`model_ids` 等），客户端无需额外判空。公共接口的请求体解析错误和上游错误会携带具体原因（`message = code: detail`）；服务层 `invalid_request` 和其他内部错误使用稳定提示，不回传底层错误正文。管理端的渠道建/改失败（`provider_invalid`）同样携带具体原因，并额外附 `code` 字段（`message` 带原因便于管理台直接展示，`code` 保持稳定便于程序匹配）。当进程 RSS 超过 `MEMORY_LIMIT_MB` 时，公共对话请求返回 503 `memory_limit_exceeded`，进行中的流式响应会被中止（按 aborted 结算）；管理接口不受内存保护影响。

## 服务路由总览

| 路径 | 行为 |
|---|---|
| `/healthz` | 免认证存活探针：进程可响应即 200 `{"status":"ok","version":"..."}`；**不触达数据库**，因此数据库瞬时故障不会引发容器重启循环。仅接受 GET/HEAD，其余方法 405 |
| `/readyz` | 免认证就绪探针：SQLite 仍能应答返回 200；数据库不可用返回 503 `{"status":"unavailable","error":"database_unavailable"}`（只回错误码不回错误正文，端点免认证故不泄露内部细节）。探针本身有 2 秒超时 |
| `/` | 302 跳转到 `/admin/` |
| `/favicon.ico` | 204 No Content |
| `/admin`、`/admin/` | 中文管理控制台（内嵌 `web/admin.html`） |
| `/admin/api/...` | 管理端 REST API |
| `/v1/models` | 公共：列出当前 Key 授权的模型 |
| `/v1/chat/completions` | 公共：对话补全（JSON / SSE 流式） |
| `/v1/responses` | 公共：OpenAI Responses API 兼容层（转成 chat/completions 转发，计费同口径，暂不支持工具调用） |

---

## 管理端 API

### 概览与配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/overview` | 账本概览 + 全局并发（active/limit）。返回 `{rpm{used,limit}, concurrency{active,limit}, last_hour{...}, trend[], recent[], model_calls[]}`。**v0.5.0 起不再含五个额度窗口**——额度按分组分配，全局视图没有可展示的限额。带 `?group_id=N` 时额外返回 `group`（与 `/admin/api/groups/{id}/overview` 同形状：四个窗口的已用/限额、本组 RPM、进行中、近一小时成功率）并把 `recent` 换成该组的最近请求；全局字段原样保留，两份数据语义不同，不能互相顶替。密钥主体调用时 `group` 恒为它自己那组，另带 `scope` 与 `me`，`recent` 只有自己那把密钥的行 |
| GET | `/admin/api/me` | 仅客户端密钥主体：`{key_id, name, group_id, group_name, concurrency_limit, token_limit, amount_limit_microyuan, used_tokens, used_microyuan, requests, expires_at_utc, expires_in_secs, authorized_models}`。管理员调用返回 400 `not_applicable`（管理员没有「自己那把密钥」） |
| GET | `/admin/api/config` | 读取运行配置（敏感项返回空字符串） |
| PUT/POST | `/admin/api/config` | 保存配置；body `{"settings": {...}}`。`ADMIN_API_KEY`、`RELAY_ENCRYPTION_KEY` 传空串表示保持不变；保存后**原子写回 relay.env 并热应用到运行中的服务**（配额、输出上限、并发闸门、内存保护、`LOG_LEVEL`、监测与同步间隔都即时生效）；仅 `HOST`/`PORT`/`DB_PATH` 与四个 `UPSTREAM_*_TIMEOUT` 需要重启（超时在启动时按值交给上游 HTTP 客户端），见 `pending_restart_fields`。`LOG_LEVEL` 只接受 `DEBUG`/`INFO`/`WARN`/`ERROR`（大小写不敏感，另接受 `TRACE`/`WARNING`/`FATAL` 别名），写错返回 400 `invalid_config` |

### 外部 WASM 插件

插件目录由 `PLUGIN_DIR` 配置（默认 `data/plugins`），包是 multipart 字段 `package` 上传的 ZIP，必须包含 `manifest.json` 和 `module.wasm`。清单要求 `api_version: 1`、小写插件 `id`、名称和版本；可声明 `permissions`、`limits.memory_bytes`（默认 16 MiB，最高 128 MiB）、`limits.timeout_ms`（默认 1 秒，最高 30 秒）及 `entrypoint`（默认 `civic_plugin_start`）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/plugins` | 列出已安装插件、清单、SHA-256、权限、资源限制、启停状态和运行/失败计数 |
| POST | `/admin/api/plugins` | 上传并校验插件 ZIP；仅写入清单和 WASM 文件，不自动启用 |
| POST | `/admin/api/plugins/{id}/enable` | 启用插件；校验 WASM 头和包路径 |
| POST | `/admin/api/plugins/{id}/disable` | 停用插件；停用后不能启动或删除前需先停用 |
| POST | `/admin/api/plugins/{id}/start` | 在独立 wazero 实例中执行入口；无宿主导入，受内存和超时限制 |
| POST | `/admin/api/plugins/{id}/stop` | 记录停止状态 |
| DELETE | `/admin/api/plugins/{id}` | 删除已停用插件及其文件 |

启动时模块只允许无导入的 WebAssembly 实例，不能访问宿主文件、网络、环境变量或 Civic 进程内存。入口缺失、执行异常或超时会记录为 `failed` 并保留错误原因，便于管理台排查。

### 上游超时与流式时长（v0.3.7 起职责拆分）

| 配置 | 默认 | 管什么 |
|---|---|---|
| `UPSTREAM_CONNECT_TIMEOUT` | `10s` | 与上游建立 TCP/TLS 连接 |
| `UPSTREAM_HEADER_TIMEOUT` | 留空 | 等待上游响应头；留空时使用 `connect + read` 的兼容计算值 |
| `UPSTREAM_READ_TIMEOUT` | `300s` | **等待上游响应头**（含上游排队/思考的时间）——`ResponseHeaderTimeout = connect + read` |
| `UPSTREAM_WRITE_TIMEOUT` | `30s` | 发送请求体 |
| `UPSTREAM_POOL_TIMEOUT` | `10s` | 空闲连接保活 |
| `UPSTREAM_IDLE_TIMEOUT` | `0` | 流式响应两次上游字节之间的空闲超时，`0` = 不限 |
| `SSE_HEARTBEAT_INTERVAL` | `15s` | 下行 SSE 保活注释间隔，`0` = 关闭 |
| `MAX_STREAM_DURATION` | `1800s` | **单次请求（流式与非流式）的总时长上限**，`0` = 不限；**热生效** |

> ⚠️ **v0.3.7 之前这里有个会造成线上故障的缺陷**：上游 HTTP 客户端设了
> `http.Client.Timeout = connect + read + write`（默认 **340 秒**）。它是「整个请求」的总超时、
> **包含读取响应体的全部时间**，于是任何超过 340 秒的流式回答都会被无声掐断 ——
> 记录里只留下 `upstream_status=200` + **零 token** 的 `failed`，运维完全看不出是超时；
> 而管理台里配的 `MAX_STREAM_DURATION` 当时根本没作用到请求上（只被后台回收循环使用）。
> 现在职责分开：等上游响应由 `UPSTREAM_READ_TIMEOUT` 管，流能跑多久由 `MAX_STREAM_DURATION` 管，
> 客户端断开由请求自身的 context 管。
> 回归测试：`internal/upstream/client_test.go` 与 `_e2e/run_stream_timeout_check.py`（后者用一条
> 6 秒慢流 + 3 秒聚合超时，旧版只收到 3 帧且无 `[DONE]`，修好后完整读完并记为 `completed`）。

### 额度模型与错误码

**额度按分组分配**（v0.5.0 起，取代原来的全局配置项）。每个模型组有 4 个窗口 × `Token` / `金额` 共 9 个可填项（加上周期长度与周期起点共 10 列）：

| 窗口 | 字段 | 说明 |
|---|---|---|
| 5 小时 | `token_limit_5h` / `amount_limit_5h` | **滚动** 5 小时 |
| 当日 | `token_limit_daily` / `amount_limit_daily` | **北京时间自然日** |
| 本周 | `token_limit_weekly` / `amount_limit_weekly` | **北京时间自然周（周一起）** |
| 自定义周期 | `period_limit_days` + `token_limit_period` / `amount_limit_period` | 天数 + 起点；金额单位是**元**，内部换算成微元（`*_microyuan` 列） |

判定时读的是**该请求密钥所属分组**的那一行，在预留事务内读取，所以改完下一次请求即生效。

自定义周期需要**同时**设置 `period_limit_days` 与至少一项周期限额才生效（只填天数不起作用）。`period_limit_start` 为空时，设置天数的那一刻自动填 `now`。周期有效期间不统计/拦截周限额；**周期 Token 或金额任一用满即关闭服务**：后续请求返回 429 `token_quota_exceeded` / `amount_quota_exceeded`（`scope=period`，带具体已用/限额），管理台周期卡片仍显示真实累计用量，不会从 0 重新计。调大限额或清空周期设置后下一次请求立即恢复（欠费即拒，不是永久锁死）。到达周期截止日后按既有语义交给周限额。概览里的 `period.transition_utc` 保留字段但恒为空串（v0.7.1 起不再交接）。

另有两项不随窗口轮换的 Key 总额度：`token_limit`、`amount_limit_microyuan`。

额度账目（`key_reservations`）是独立账本：**不随 `RETENTION_DAYS` 裁剪**（保留期只清 `requests` 请求明细），**也不会因为删除 Key / 模型 / 渠道而消失**。所以周期（最长 60 天）与 Key 总额度统计的是真实累计值，删一把 Key 再建一把不会把用量重置为 0。删除主体时若还有 `reserved` 状态的在途请求会被拒绝，结算后即可删除，历史账目继续保留。

**任何一项填 `0` 或留空都表示不限**，出厂默认所有分组都不限。

**判定口径 = 先欠费后停服**：只检查「该窗口是否已用满」，不再要求「已用 + 本次预留 ≤ 限额」。
只要窗口还有余量就放行（本次请求可能让它略微超出），用到限额之后才拒绝后续请求。
调大限额或窗口轮换后**自动恢复**，不需要人工干预。

额度类拒绝一律返回 **429**（不是 500，也不是 401 —— 否则客户端会误判成服务端故障而反复重试）：

| `error.code` | 含义 |
|---|---|
| `token_quota_exceeded` | 所属分组的某个 Token 窗口已用满 |
| `amount_quota_exceeded` | 所属分组的某个金额窗口已用满 |
| `key_token_quota_exceeded` | Key 的 Token 总额度已用满 |
| `key_amount_quota_exceeded` | Key 的金额总额度已用满 |
| `rpm_exceeded` | 超过 `RPM_LIMIT`（滚动一分钟） |
| `rate_limit_exceeded` | 全局并发或 Key 并发达到上限（`message` 会区分是哪一个；Key 并发带 Key 名、在跑数与上限） |
| `group_concurrency_exceeded` | 该组自己的并发上限已满（v26） |

`error.message` 形如（`error.detail` 还会带上**分组名与 ID**，一眼看出是哪一组的额度咬住了）：

```
token_quota_exceeded: 5 小时窗口Token已用满（已用 5,000,000 tokens，限额 5,000,000，分组「团队A」#3）。当前处于欠费状态，请等待窗口轮换或在管理台调大限额
```

#### 并发上限：全局 / Key / 分组（三道闸）

同一时刻能跑几笔请求由三道**互相独立**的闸决定，任何一道满都返回 429，不要把它们的数字
换算成同一尺度：

| 闸 | 配置位置 | 计数口径 | 满时的错误 |
|---|---|---|---|
| 全局 | `GLOBAL_CONCURRENCY_LIMIT` | relay 进程内的在途请求（含等待结算的） | 429 `rate_limit_exceeded`，`message` =「网关并发已满，请稍后重试」 |
| Key | `client_keys.concurrency_limit`（最小 1） | 账本里该 Key 的 `status='reserved'` 行数；relay 内存计数器先挡一道，事务内再数一次兜底 | 429 `rate_limit_exceeded`，`message` =「Key「名字」并发已满（在跑 N，上限 M），请等前面的请求结束后重试」 |
| 分组 | `model_groups.concurrency_limit`（`NULL`/0 = 不限） | 账本里 `source_group_id` 为该组且 `status='reserved'` 的行数 | 429 `group_concurrency_exceeded`，`message` 带组名与两个数字 |

- 三道闸都在**准入阶段**判定：被拒的请求不会调用上游，也不产生费用（账目行都没写入）。
- Key 与分组两道的计数与插入在同一个 `BEGIN IMMEDIATE` 事务里完成（先数后插），不会超发；
  relay 的内存计数器只负责快速失败，**不是**唯一依据（重启后内存清零，靠事务内那次兜底）。
- 管理台改了 Key 或分组的并发上限会**立刻生效**：分组下一次预留就按新值判定；Key 的内存
  计数器每次取名额都会同步当前上限（调大立即放行，调小只拦新请求，不中断在跑的）。
- 分组并发在 `source_group_id` 上计数，所以成员组的渠道被聚合组调用时占的是**成员组**的名额；
  聚合组自有渠道的请求占聚合组自己的名额。这与额度扣减同口径。

#### 分组并发上限（v26）

额度管「一段时间里用掉多少」，并发管「同一时刻有几笔在跑」。上游账号常见的限制是并发数
（超了排队或直接 429），窗口额度咬不住瞬时打满，所以每个组另有 `concurrency_limit`：

- **0 或 `null` = 不限**（默认，既有组升级后行为不变）。
- 计数口径与额度一致：按 `key_reservations.source_group_id` 数 `status='reserved'`，也就是
  「这条渠道真正由哪个组提供」。成员组被聚合组调用时才会计入它自己的名额；聚合组自有
  渠道的请求计入聚合组自己。
- **与全局并发上限（`GLOBAL_CONCURRENCY_LIMIT`）各判各的**：两者都要满足，全局先超同样是
  超限，不需要把两个数字换算成同一尺度（管理台也不做「必须小于全局」这类联动校验）。
- 名额满时返回 429 `group_concurrency_exceeded`，`message` 指出是哪个组、在跑几个、上限多少；
  前面的请求结算后**立即恢复**，无需等窗口轮换，也不需要人工干预。
- 计数与预留同在一个事务里（`BEGIN IMMEDIATE` + 进程内串行），不会超发。

> 额度用满**不会**停用 Key。Key 停用只能手动操作；停用后公共接口返回 401 `invalid_api_key`。
> 旧版本会在 Key 总额度用满时自动置 `enabled=0`，该行为已移除，升级时的迁移会把历史遗留的
> `disabled_reason='quota_exhausted'` 恢复为启用。

### 供应商（渠道）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/providers` | 列表（不返回明文 Key，仅返回是否已配置）。含生效时段字段与只读的 `ScheduleActive`（按**当前时刻**算出的「是否在时段内」）。每项另含 `GroupID`/`GroupName`（**归属分组**，0/空串 = 未加入任何组）。**组作用域主体**（非管理员）只看到自己分组实际能用的渠道：聚合组 = 自有 ∪ 启用成员组的渠道（与授权口径一致），且 `BaseURL` 与归属字段都为空 |
| POST | `/admin/api/providers` | 创建；body `{name, base_url, api_key}`，可选带时段：`schedule_mode`（`always`/`range`/`daily`）、`schedule_start`、`schedule_end`（北京时间或 RFC3339）、`schedule_daily_start`、`schedule_daily_end`（`HH:MM`，单段老写法）、`schedule_weekdays`（`[1..7]`，1=周一）、`ScheduleWindows`（v25 多段，见下） |
| PUT | `/admin/api/providers/{id}` | 更新（nil 字段保持原值）。⚠️ **body 用 PascalCase**：`{Name, BaseURL, APIKey, Enabled, ScheduleMode, ScheduleStart, ScheduleEnd, ScheduleDailyStart, ScheduleDailyEnd, ScheduleWeekdays, ScheduleWindows}` —— `store.UpdateProvider` 没有 json tag，靠 `encoding/json` 的大小写不敏感匹配对上（与 POST 的 snake_case 不一致，管理台就是这么发的）。`ScheduleMode` 缺省 = 保持原时段；给了就按同一请求里的 `Schedule*` 整段替换。**发 snake_case 会静默变成空更新**（不报错也不生效），契约由 `admin_test.go::TestAdminUpdateProviderAcceptsPascalCasePayload` 与 `provider_schedule_test.go` 钉住 |
| DELETE | `/admin/api/providers/{id}` | 删除，**级联删除该渠道下的所有模型**；重复删除返回 404 |
| GET | `/admin/api/providers/{id}/remote-models` | 拉取上游 `/v1/models` 预览，返回 `{upstream_name, public_name, exists}`，不导入 |
| POST | `/admin/api/providers/{id}/models/import` | 批量导入；body `{"models": [{upstream_name, public_name?}]}`。public_name 缺省为 `渠道名/上游名`。**幂等**：已存在的 public_name 跳过；导入结果为停用 + 未定价，需到模型页定价并启用后才能加入分组 |

供应商 `base_url` 存库时自动去掉 `/v1` 后缀，请求上游时自动补回。

#### 生效时段（v0.8.0）

渠道可以只在选定时间段启用。三种方式**互斥**，判定口径只有一处（`store.ProviderSchedule.ActiveAt`），时区固定**北京时间**：

| `schedule_mode` | 生效条件 | 用到的字段 |
|---|---|---|
| `always`（默认） | 供应商 `enabled=1` 即全天可用，与升级前行为一致 | 无 |
| `range` | 区间 `[start, end)`；任一侧留空表示该侧不限 | `schedule_start_utc` / `schedule_end_utc`（UTC RFC3339） |
| `daily` | 每天若干段 `HH:MM` 区间，**每段可以有自己的星期**，支持跨零点（v25） | `schedule_windows`（JSON 多段；空 = 回落到下面的单段列）、`schedule_start_minute` / `schedule_end_minute`（0..1439，北京时间）、`schedule_weekdays` |

约定与坑：

- `schedule_weekdays` 是位掩码：bit0=周一 … bit6=周日。`0` 与 `127` 都表示每天。
- `daily` 起止相同 = 全天（只受星期约束）。

#### 多段时段（v25）

`daily` 支持一天多个窗口，每段带自己的星期 —— 「工作日 22:00-08:00 + 周末全天」是
同一条渠道的一组配置，不需要拆成两条（v24 起一个渠道只能属于一个组，拆开就等于要
两个上游账号）。管理台的时段弹窗就是按这个结构渲染的。

```json
{
  "ScheduleMode": "daily",
  "ScheduleWindows": [
    {"start": "22:00", "end": "08:00", "weekdays": [1,2,3,4,5]},
    {"start": "00:00", "end": "00:00", "weekdays": [6,7]}
  ]
}
```

- `weekdays` 是这一段的星期（`[1..7]`，1=周一）；**省略或空数组 = 这一段每天生效**。
  管理台不允许把一段的所有星期都取消（那样这段永远不生效，几乎总是误操作），会提示
  改选「不限」。
- 段数上限 24；相同的段（起止 + 星期都相同）保存时去重，按开始时间排序。
- 任一段命中即可用；段之间重叠不报错（结果相同，只是冗余）。
- 跨零点的星期归属仍然按**段开始那天**算：`周一 22:00-08:00` 覆盖周二凌晨，
  但周二 22:00 起的就不算周一了。这条对每一段独立生效。
- `ScheduleWindows` 缺省（字段不出现）= 走 `ScheduleDailyStart/End` + `ScheduleWeekdays`
  的单段老写法；显式传 `[]` 会被拒绝（避免「清空所有段」静默变成全天）。
- 存库时 `schedule_windows` 存 JSON；同时把**第一段**（含它的星期）写回
  `schedule_start_minute` / `schedule_end_minute` / `schedule_weekdays`，这样回滚到
  旧版本后至少还留着一段，不会整条渠道变成「全天可用」。
- 读取时 `schedule_windows` 为空或 JSON 损坏 → 回落到单段列；越界分钟/掩码被丢弃。
- **跨零点归属开始那天**：`周一 22:00-08:00` 包含周二凌晨；周二 22:00 起的就不算周一了。
- `range` 只填一侧时另一侧不限；两侧都空等价于 `always`（管理台保存时会自动回退成 `always`，不留一个永远生效的 `range`）。
- 时段外的后果：该渠道下的模型**不会出现在 `/v1/models`**，请求也不会被路由过去（故障转移池会自动跳到时段内的成员）。分组探活与定时模型同步同样跳过这些渠道。
- 时段只在**请求开始时**判定：已经建立的在途（流式）请求不会被到点掐断，下次请求才受新状态影响。
- 管理台「接入信息」里的模型清单**不按时段过滤** —— 那是「配置上授权了哪些模型」；想知道此刻能不能用，看供应商列表的「未到时段」标签。
- `range` 的 `start_utc` / `end_utc` 若被手工改成非法时间，该侧按「不限」处理（fail-open）：宁可让渠道多开一会儿，也不让脏数据把渠道永久停掉。正常写入路径经 `NormalizeProviderSchedule` 校验，不会产生这种数据。
- 脏数据（`range` 的时间解析不出）按「该侧不限」处理：宁可放行，也不让一条坏记录把渠道永久停掉。

`PUT` 更新时段时 `ScheduleMode` 缺省表示**保持原时段**（只改名字不会顺手清掉时段）；要清空请显式传 `{"ScheduleMode":"always"}`。

`base_url` 的校验规则（不符则 400 `provider_invalid: <原因>`，原因会说明该改成什么）：

- **必须带协议头**：`http://主机:端口` 或 `https://域名`。`127.0.0.1:6000` 这类裸写法会被拒（Go 的 `url.Parse` 无法解析）。
- `https://` 任意主机均可；`http://` 只放行 `localhost`、`*.localhost`、`*.test`，以及回环/私有 IP（`127.0.0.1`、`10.x`、`172.16-31.x`、`192.168.x`）。**公网 IP 或公网域名必须用 https**。
- `host.docker.internal` 不在放行名单内。容器访问宿主机上的服务要用网桥网关 IP（默认 bridge 为 `172.17.0.1`，compose 自建网络通常是 `172.18.0.1`，以 `docker network inspect` 为准），或用宿主机内网 IP；上游服务必须绑 `0.0.0.0` 而非 `127.0.0.1`。同 compose 网络的**服务名/容器名同样不在放行名单内**。
- 容器内的 `127.0.0.1` 指向容器自身，填它等于把请求发给 relay 自己。
- 不接受 URL 内的账号、查询串、锚点；端口须在 1-65535。

定位失败原因时优先用这个接口直接测（响应里的 `message` 会给出具体原因，不必翻容器日志）：

```bash
curl -sS -X POST http://<服务器>:8000/admin/api/providers \
  -H "X-Admin-Key: <管理员密钥>" -H "Content-Type: application/json" \
  -d '{"name":"local","base_url":"http://172.17.0.1:6000","api_key":"dummy"}'
```

### 模型

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/models` | 列表（含定价） |
| POST | `/admin/api/models` | 创建；body `{provider_id, public_name, upstream_name, enabled, input_price, output_price, cached_input_price}` 或直接传微元 `{input_price_microyuan, output_price_microyuan, cached_input_price_microyuan}` |
| PUT | `/admin/api/models/{id}` | 更新；body 均为可选（snake_case）：`{provider_id?, public_name?, upstream_name?, input_price_microyuan?, output_price_microyuan?, cached_input_price_microyuan?, enabled?}`，缺省字段保持原值 |
| DELETE | `/admin/api/models/{id}` | 删除；重复删除返回 404 |

计价单位：元/百万 token；`*_price` 为浮点元，服务端折算为微元（1 元 = 1,000,000 microyuan）。

**缓存输入计价**：`cached_input_price_microyuan` 为可空字段——
- `null`（未配置）：缓存命中部分按普通输入价计费（无优惠，与历史行为一致）；
- `0`：缓存命中免费；
- 其他值：缓存命中部分按该单价计费。
计费时解析上游 `usage.prompt_tokens_details.cached_tokens`（`prompt_tokens` 的子集），金额 = 未命中输入 × 输入价 + 命中 × 缓存价 + 输出 × 输出价（分组倍率对三项同样生效）。请求日志的 `cached_input_tokens` 列记录每次请求的命中量。

### 故障转移供应商（模型池，v0.7.0 起）

与「供应商」并列的第二类供应商形式：**一条记录只对应一个对外模型名**（单一模型类别，例如 `claude-sonnet-4-5`），下面挂的是**已有供应商下的模型**（可跨供应商），数组顺序就是优先级。客户端在 `/v1/chat/completions` 等接口照常把 `model` 填成池名；成员故障时网关按优先级自动切到下一个可用成员，客户端无需改配置。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/pools` | 列出全部池（含成员与顺序），返回 `{data:[{ID,Name,Enabled,Members:[...]}]}`；字段为 PascalCase（与 `providers`/`models` 列表一致） |
| POST | `/admin/api/pools` | 创建；body `{name, members:[{model_id, enabled?}]}`。`members` 数组顺序 = 优先级，`enabled` 缺省为 true；允许空池（之后编辑补成员），创建即启用 |
| GET | `/admin/api/pools/{id}` | 单个池；不存在返回 404 |
| PUT | `/admin/api/pools/{id}` | 更新；body `{name?, enabled?, members?}`。`members` **不传 = 不动成员**，传空数组 = 清空；其余字段缺省保持原值 |
| DELETE | `/admin/api/pools/{id}` | 删除；重复删除返回 404。只删池与成员关系，**不动成员模型本身，也不动历史账目** |

成员对象字段：`ModelID / PublicName / UpstreamName / ProviderID / ProviderName / SortOrder / Enabled`。

**名称不能撞车**：池名与任何模型的 `public_name`、其它池名重复都会被拒（400 `pool_invalid`），反向（新建/改名模型时占用池名）同样被拒——两者都会出现在 `/v1/models`，同名会让请求无法判定目标。

**路由与故障判定**：

- 可切换故障：连接失败 / 连接或响应头超时、5xx、429、401/403（上游凭据失效）、404（模型下架）、上游响应体无效。
- 不切换：400/422（请求体本身有问题）、客户端取消、请求整体时长超限（换成员也救不回来）。
- 连续失败的成员在进程内冷却 **60 秒**（排到候选末尾，不是禁用；全部都在冷却时仍按配置顺序试，重启清零）。容器日志会打 `pool_failover`，带 request_id、池 ID、切换前后成员 ID 与错误码。
- **流式只在响应头 / 首帧之前切换**：一旦已经开始向下游输出内容，切换会造成重复文本，此后按既有流结算路径处理。
- 一次请求只做一次预留与一次结算；候选共用 `MAX_STREAM_DURATION` 这道总闸门，切换不会把请求无限拉长。

**授权与计费口径**：

- 池名出现在 `/v1/models`（与管理端「接入信息」）的前提是：池启用，且**至少一个成员在该客户端的授权模型集合内**。授权集合就是原有口径（组内渠道 → 渠道启用 → 模型启用且已定价）。
- 实际候选 = 池成员 ∩ 该 Key 的授权模型：池里挂了不在客户端分组内的渠道模型也不会被调用，池不会绕过分组授权。
- 预留按**首选成员**的单价与倍率估算；实扣按**实际成功成员**的单价与倍率计算（`relay.PriceUsage`）。请求日志的 `model_id` / `provider_id` 记录实际成功（或最后尝试）的成员，不是首选成员。
- 池删除、成员停用都不影响历史账目；删除成员模型会级联清掉它在各池里的成员资格。

### 模型组

分组有两种 `Kind`：`normal`（自己挂渠道）与 `aggregate`（聚合组，除自有渠道外还包含若干普通组）。**聚合组只允许一层**，不支持包含聚合组。

**计费口径（与额度判定、概览统计同口径）**

- 一笔请求同时占两个组的额度：`group_id` = Key 绑定的组（聚合组），`source_group_id` = 真正提供该模型的组（v24 起渠道归属唯一，所以它恒为「提供该模型的渠道所属的那个成员组」；聚合组自有渠道命中时两者相同）。
- 任一组任一窗口用满都会 429，`error.message` 里写明是哪个组咬住的（`分组「xxx」#N`）。
- 计价倍率只取 **Key 绑定的聚合组**的 `rate_milli`，两边金额是同一个数；**成员组自己的 `rate_milli` 不参与计费**（v0.10.0 起成员组无法被 Key 绑定，这个字段只在库里保留、不提供修改入口）。
- 组概览的「已用」= `group_id=? OR source_group_id=?` 的行，所以聚合组与成员组看到的是同一笔流量。**账本只有一行**（不会翻倍），全局概览与 Key 总额度因此不受影响。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/groups` | 列表。每项含 `ID/Name/Kind/Enabled/RateMilli`、`BoundKeys`（绑定到该组的密钥数；**成员组恒为 0**，v23 起 Key 只能绑聚合组）、`ParentCount`（被几个聚合组包含；聚合组恒为 0）、`Providers`（**自有**渠道数）、`Members`/`MemberIDs`（聚合组的成员组摘要 `{ID,Name,Enabled,RateMilli,Providers}`，普通组为空数组）、`PoolIDs`（v27：聚合组直接包含的故障转移供应商 ID；普通组为空数组）、10 个额度字段（`TokenLimit5H` … `PeriodLimitStart`）、`ConcurrencyLimit` / `ActiveConcurrency`（v26：并发上限与此刻在跑数，`null` = 不限）以及 `TensionPct` / `Overdrawn`（三个主窗口中最高用量占比按 Token 计；`Overdrawn=true` 表示至少一个窗口已用满）。密钥主体登录时 `Members`/`MemberIDs`/`PoolIDs` 恒为空 —— 成员组名属于管理员视角的拓扑 |
| POST | `/admin/api/groups` | 创建；body `{name, kind?, rate?, rate_milli?, provider_ids?, group_ids?, pool_ids?, concurrency_limit?}`（`concurrency_limit` v26，见下方「并发上限」；`pool_ids` v27）。`kind` 缺省 `aggregate`（v23：可对外是默认，成员组要显式声明）；`aggregate` 时 `group_ids` 是成员组（必须存在且为 `normal`，不接受聚合组与自身，违反返回 400 且**不留下半成品**）。成员组（`normal`）带 `group_ids` 一律 400。`provider_ids` 里的渠道必须**尚未属于其他组**（v24），否则 400 `provider_in_use` 并带「已属于哪个组」；`pool_ids` 里的池必须存在且启用，且组必须是聚合组。任一失败都**不会留下这个组**。**创建不带额度字段**，额度创建后在 PUT 里设 |
| PUT | `/admin/api/groups/{id}` | 更新 `{name?, enabled?, rate?, rate_milli?}` + 9 个额度字段（`token_limit_5h`/`amount_limit_5h`/`token_limit_daily`/`amount_limit_daily`/`token_limit_weekly`/`amount_limit_weekly`/`period_limit_days`/`token_limit_period`/`amount_limit_period`）、`period_limit_start` 与 `concurrency_limit`（v26）。**缺省 = 保持原值**，`0` = 不限，负数 400（`group_invalid`）。`period_limit_days > 0` 且起点为空时自动填 `now`；天数归零会清掉起点。**`kind` 不可改**（建组时定） |
| DELETE | `/admin/api/groups/{id}` | 删除；返回 `{"ok": true, "unbound_keys": N, "aggregates": [...]}`（N = 刚才被解绑的密钥数，解绑后它们立即无法调用任何模型；`aggregates` = 把该组当成员的聚合组，删成员组会改变它们的可用模型）；重复删除返回 404 |
| GET | `/admin/api/groups/{id}/providers` | 组内**自有**渠道 ID 列表（聚合组不含成员组带来的渠道）；组不存在返回 404（v0.11.1 之前会回一个 `200 {"data":[]}`，看起来像「这个组没有渠道」） |
| PUT | `/admin/api/groups/{id}/providers` | **整体替换**自有渠道；body `{"provider_ids": [...]}`；拒绝停用渠道（400，文案带渠道名）；分组已停用 400；渠道已被**别的组**占用时 400 `provider_in_use`（同一组重复保存是幂等的）。聚合组同样可用。**组不存在返回 404**（v0.11.1 之前是 400 `group_invalid: sql: no rows in result set`） |
| GET | `/admin/api/groups/{id}/pools` | **包含的故障转移供应商 ID 列表**（v27），组不存在 404。池是**模型级**成员：它只授权池里的模型，不写入 `group_providers`、不改渠道归属 |
| PUT | `/admin/api/groups/{id}/pools` | **整体替换**包含的池；body `{"pool_ids": [...]}`。只有**聚合组**可调用（成员组 400，文案「只有聚合模型组可以包含故障转移供应商」）；池必须存在且启用（停用 400，文案带池名）；分组已停用 400。失败类型 `pool_invalid`，组不存在 404 |
| GET | `/admin/api/groups/{id}/members` | 聚合组的成员组摘要，返回 `{"data": [...], "kind": "aggregate|normal"}`；普通组 `data` 为空 |
| PUT | `/admin/api/groups/{id}/members` | **整体替换**成员组；body `{"group_ids": [...]}`；只有聚合组可调用（普通组 400），成员必须是 `normal` 组，写入失败不改变已有关系 |
| GET | `/admin/api/groups/{id}/overview` | **分组概览**：仅统计**归属本组**的请求（按账目行上的 `group_id`，与额度扣减同口径；切换分组不搬历史）。返回 `{group_id, group_name, rpm{used,limit}, five_hour/daily/weekly/period{used_tokens,used_microyuan,limit,amount_limit}, weekly{active,from_utc}, period{active,start_utc,end_utc,transition_utc}, last_hour{...}, active, generated_at_utc}`；令牌/金额含未结算预留，`limit`/`amount_limit` 取自本组额度行（0 = 不限） |
| GET | `/admin/api/groups/health` | **分组可用性**：每个分组最近一轮监测结果（键为分组 ID 字符串）+ `{monitor_enabled, monitor_interval, monitor_interval_ms}`。⚠️ 该路径必须排在 `groups/{id}` 之前匹配，其第一段不是数字 ID |
| POST | `/admin/api/groups/monitor` | **立即检测全部分组**，不等定时轮询。返回 `{"monitored": N}` |
| GET | `/admin/api/groups/{id}/monitor` | 该分组最近 50 轮监测结果，最新在前 |
| POST | `/admin/api/groups/{id}/monitor` | **立即检测该分组**，返回本轮结果（`trigger` 为 `manual`） |

### 分组可用性监测

配置项 `GROUP_MONITOR_INTERVAL`（时长字符串，如 `30m`；默认 `30m`）控制定时轮询，**`0` 表示关闭**。
关闭只影响自动轮询，两个手动触发接口始终可用。

判定顺序与状态语义：

| 状态 | 含义 | 该做什么 |
|---|---|---|
| `ok` | 启用渠道全部探活成功 | 无需处理 |
| `degraded` | 部分渠道探活失败；组仍可服务，但走这些渠道的模型会失败 | 看 `detail` 里点名的渠道 |
| `upstream_unreachable` | 全部启用渠道探活失败，整组无法服务 | 查网络/上游/密钥 |
| `config_unavailable` | 配置层面不成立（无启用渠道，或渠道下没有已启用且已定价的模型；v0.8.1 起若渠道有时段，按**时段内**的渠道判断定价） | 去配渠道或定价；**此时不做探活** |
| `schedule_idle` | 全部启用渠道此刻都在生效时段外，没有探活（到点自动恢复） | 不是故障，灰色显示；若不该休眠就检查时段配置 |
| `disabled` | 分组被主动停用，跳过探活 | 不算故障，灰色显示 |

`detail` 是给运维直接照着处理的中文说明，例如
`1/2 个渠道探活失败，走这些渠道的模型会请求失败 —— broken：上游拒绝认证，检查该渠道的上游 API Key 是否失效`。
上游错误只映射成 `upstream_*` 这类自带码的说明，不会带出任何凭据；多行错误只保留第一行，超长会截断。

该功能**只写库与日志，不改变服务行为**（不自动停用渠道、不影响请求判定）。
监测历史每个分组最多保留 200 条，并按 `RETENTION_DAYS` 一并清理。

> 组**管理渠道**而非模型：授权模型 = Key 启用分组内渠道下所有已定价且启用的模型（去重并集）；
> 倍率取 Key 启用分组中包含该模型所属渠道的最大倍率，无匹配分组回落默认 1.0×。

**倍率语义**：`rate_milli` 为千分比整数（1000 = 1.0×，1500 = 1.5×），必须 > 0；也可传浮点 `rate` 由服务端折算。Key 请求某模型时，取其所属**启用**分组中包含该模型的**最大**倍率；无匹配回落 1.0×。模型原始单价存库不改写，只在预留与结算时折算。

### 客户端 Key

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/keys` | 列表，含用量统计（见下） |
| POST | `/admin/api/keys` | 创建；body `{name, max_concurrency, token_limit_total?, money_limit_yuan_total?, group_id?, ttl_seconds?, keyless?}`。`group_id > 0` 时创建即绑定到该分组（一对一，**必须是聚合组**；组不可绑时返回 400 且**不会留下 Key**——明文 token 只在 201 里返回一次，失败必须等于什么都没发生）；缺省/0 = 不绑；负数 = 400 `invalid_request`；目标组是成员组 = 400 `group_not_callable`（带原因）；组不存在或已停用 = 400 `key_invalid`——此时 Key 本体已创建（历史保留，需事后在编辑里改绑或删除）。**明文 token 只在本次响应返回一次**。`ttl_seconds > 0` 时创建为**短时令牌**（从现在起 N 秒后自动失效），缺省/0/负数 = 永久有效。`keyless: true` 时创建**应用型凭据**：不产生任何可用密钥（响应里 `token` 为空串），改用公开的应用标识接入，见下节 |
| GET | `/admin/api/keys/hints` | 脱敏缩略 `crk_xxxx…` + 非敏感标识 `key-xxxxxx`（独立接口，保证列表永不泄露密钥字符） |
| PUT | `/admin/api/keys/{id}` | 更新 `{name?, enabled?, concurrency_limit?, token_limit_total?, money_limit_yuan_total?, ttl_seconds?}`；**nil 字段保持原值**。`ttl_seconds` 三态：缺省 = 不动；`0`/负数 = 改为永久有效；`>0` = 从现在起 N 秒后到期（**续期就是再传一次**） |
| DELETE | `/admin/api/keys/{id}` | 删除：立即吊销访问；已结算账目保留（组用量不归零，A），有在途请求时拒绝；重复删除返回 404 |
| POST | `/admin/api/keys/{id}/rotate` | 重置密钥：旧 token 立即失效，新 token 只返回一次 |
| GET | `/admin/api/keys/{id}/group` | Key 所属分组 ID（一对一）；未绑定时返回 `{"data": null}` |
| PUT | `/admin/api/keys/{id}/group` | 绑定/解绑所属分组；body `{"group_id": <number>}`，`0` = 解绑；**目标组必须是聚合组**（v23），绑成员组 = 400 `group_not_callable`（`message` 写明「成员组不能直接对外提供服务」）；组不存在或已停用 = 400 `key_invalid`；负数 = 400 `invalid_request` |
| GET | `/admin/api/keys/{id}/connection` | **接入信息**：把「其他软件该填什么」一次算全（Base URL、派生地址、凭据、可用模型清单、所属分组、问题清单）。详见下节 |

### 应用型凭据（客户端不保存密钥）

`keyless: true` 创建的记录**没有可用密钥**：`token_digest` 里是创建时即丢弃的随机值
（明文不存在于任何地方），响应里的 `token` 是空串，`token_hint` 也是空。
客户端改用请求头 **`X-App-Id: app_<22 位随机>`** 认证，头里的值是**公开的**。

```bash
curl http://<relay>:8000/v1/chat/completions \
  -H "X-App-Id: app_Otp8F7hhj8w6RsKRnSLRqA" \
  -H "Content-Type: application/json" \
  -d '{"model":"provider/model","messages":[{"role":"user","content":"Hello"}]}'
```

适用场景：客户端里**不能保存任何密钥**（写在客户端里的密钥迟早被提取），
但又希望「用户拿不到能带到别处去的凭据」。代价是应用标识公开 ——
**能访问到网关的人都能调用**，所以真正的边界在「谁能访问到这个网关」（内网 / 防火墙 /
反向代理鉴权）。给这类记录配好并发与额度上限，可让最坏情况有界。

安全约束（已由测试钉住）：

- 密钥型 Key 的 `public_id` 为空，**不能**用应用标识这条路认证；反之应用型凭据也无法用 Bearer 进来（互不越界，避免权限提升）。
- 请求同时给出 `Authorization` 与 `X-App-Id` 时：**先按密钥认证，密钥无效再回落到应用标识**
  （兼容那些强制发送 `Authorization` 的 OpenAI 兼容客户端）；密钥「已过期」则直接回 `token_expired`，不回落到应用标识。
- 停用 / 过期 / 额度 / 并发对两条路一致生效。

### 接入信息

管理台 Key 列表每行的「接入信息」按钮走这个接口。它解决的是「客户端该填什么」：
客户端密钥的明文只在创建/重置时返回一次（库里只有 HMAC 摘要），Base URL 与模型名又
分散在运行配置和模型页，让运维自己拼极易拼错（Base URL 少带 `/v1`、模型名填成上游名），
而拼错的表现是客户端 400/404 —— 从错误信息里看不出是填错了还是服务坏了。

```json
{
  "data": {
    "key_id": 1,
    "name": "写作软件",
    "enabled": true,
    "token_hint": "crk_Vm8U…",
    "token_label": "key-9fA2c1",
    "token_visible": false,
    "auth_mode": "token",
    "app_id": "",
    "expired": false,
    "expires_at_utc": "2026-09-25T05:00:00Z",
    "expires_in_secs": 82800,
    "group": {"id": 2, "name": "写作组", "enabled": true, "rate_milli": 1000},
    "models": ["deepseek/deepseek-chat"],
    "base_url": "http://10.190.164.209:8000/v1",
    "models_url": "http://10.190.164.209:8000/v1/models",
    "chat_url": "http://10.190.164.209:8000/v1/chat/completions",
    "issues": [],
    "warnings": [],
    "ready": true
  }
}
```

- `models` 与公共 `GET /v1/models` **同源**（store 侧共用一份 SQL），因此不会出现
  「管理台说已授权、客户端却拉不到」的漂移。
- `base_url` 按**访问管理台的方式**推断（尊重 `X-Forwarded-Proto`）。从 `127.0.0.1`
  打开管理台时它是回环地址，给别的设备用要换成该机器的局域网 IP —— 管理台会就此提示。
- `token_visible` 恒为 `false`：明文无法从库里还原，需要明文只有「重置密钥」一条路。
- `auth_mode` 为 `token`（凭密钥）或 `app`（应用型凭据）：后者 `app_id` 是公开的应用标识、
  `token_hint` 为空（确实没有密钥）。前端据此切换「填什么」的说明与示例。
- `expired` / `expires_at_utc` / `expires_in_secs` 是**短时令牌**的有效期：`expires_at_utc` 为 `null`
  表示永久有效（此时 `expires_in_secs` 也是 `null`）。已过期时 `issues` 里会点明客户端会收到
  `401 token_expired`；剩余不足 24 小时时进 `warnings` 提醒提前续期。
- `issues` 是会直接导致客户端连不上的问题（Key 停用 → 401、未绑分组 → `model_not_allowed`、
  分组全停用或无已定价模型 → 模型清单为空）；`warnings` 不影响连通性。`ready = issues 为空`。
- 未知 Key 返回 404，非 GET 返回 405（该路由在 `keyItem` 内分发，不会被当成无效 ID 判 400）。

**用量统计口径**（`GET /admin/api/keys` 返回，与限额检查一致，含未结算预留）：

- `UsedTokens`：已消耗 Token 总数
- `UsedAmountMicroyuan`：已消耗金额（微元，已计入分组倍率）
- `RequestCount`：请求次数
- `ConcurrencyLimit`：该 Key 的并发上限（最小 1，没有「不限」）
- `ActiveConcurrency`：此刻在跑的请求数（`status='reserved'` 的行数），与并发闸门同口径，
  管理台据此显示「在跑 / 上限」，满额标红

金额限额传参单位为**元**（`money_limit_yuan_total`），存储单位为微元（`AmountLimitMicroyuan`）。达到任一总配额后新请求会被拒（429 `key_token_quota_exceeded` / `key_amount_quota_exceeded`），**Key 不会自动停用**，调大限额或窗口轮换后自动恢复。

### 请求记录

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/requests` | 请求元数据列表（不含 prompt/回复/密钥），支持 `request_id`、`status`、`protocol`、`provider_id`、`model_id`、`from`、`to`、`limit`（1..100）和 `cursor` 筛选；返回 `{data,next_cursor}`，游标使用当前页最后一条记录，加载下一页不会重复或遗漏 |
| GET | `/admin/api/requests/{request_id}` | 单条请求诊断详情：真实 `upstream_status`、`error_code`、`failure_stage`、`usage_source`、`incomplete_reason`、四类耗时、客户端关联 ID、用量与报价快照 |

请求记录保留 `status`（`completed`、`failed`、`aborted`、`rejected`、`reserved`）、`protocol`（`chat`/`responses`）和结算诊断字段。流式请求只有收到协议终态才会记为完成；缺少上游 usage 时 `usage_source=estimated`，输入 token 使用预留时的冻结估算。

---

## 公共 API（OpenAI 兼容）

### GET /v1/models

返回当前客户端 Key 被授权的、启用且已定价的模型。认证：`Authorization: Bearer crk_...`。

### POST /v1/chat/completions

支持普通 JSON 与 SSE 流式。计费流程：预留（按输入估算 × 分组倍率）→ 上游转发 → 按实际用量结算。

```bash
curl http://<relay-ip>:8000/v1/chat/completions \
  -H "Authorization: Bearer crk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"渠道名/模型名","messages":[{"role":"user","content":"Hello"}]}'
```

---

## 数据库迁移版本

| 版本 | 内容 |
|---|---|
| v1 | 初始表结构 |
| v2 | 计费账目列：`requests.billing_date_bj`（北京时间自然日）、`reserved_tokens`/`charged_tokens`、`key_reservations.request_id` 及相关索引 |
| v3 | `model_groups.rate_milli INTEGER NOT NULL DEFAULT 1000 CHECK (>0)`（分组倍率） |
| v4 | `client_keys.token_label`（非敏感标识 `key-xxxxxx`） |
| v5 | 回填存量 Key 的 token_label |
| v6 | `client_keys.token_hint`（脱敏缩略 `crk_xxxx…`） |
| v7 | `models.cached_input_price_microyuan`（缓存输入单价）；`requests.cached_input_tokens`（请求缓存命中量） |
| v8 | `group_providers` 表替代 `group_models`：模型组改为**管理渠道**，历史组内模型成员折算为其所属渠道 |
| v9 | 取消「Key 总配额用满即自动停用」，恢复历史遗留的 `disabled_reason='quota_exhausted'` |
| v10 | `group_monitor_results`（分组可用性监测历史） |
| v11 | `client_keys.expires_at_utc`（短时令牌有效期；`NULL` = 永久有效，历史 Key 不受影响） |
| v12 | `client_keys.public_id`（应用型凭据的公开标识；`NULL` = 只能凭密钥认证，历史 Key 不受影响） |
| v13 | 请求诊断、客户端关联 ID、协议/usage 来源/不完整原因、耗时快照、报价快照及请求筛选索引 |
| v14 | `civic_plugins`（外部 WASM 插件清单、状态与运行计数） |
| v15 | `providers.protocol`（上游协议：openai / anthropic / gemini） |
| v16 | `client_keys.group_id`（密钥与分组一对一），账目表冗余 `group_id` 并按旧 `key_groups` 回填 |
| v17 | 删 `key_groups`，补分组维度的额度窗口索引 |
| v18 | `model_groups` 加 10 列分组额度（4 窗口 × Token/金额 + 周期设置） |
| v19 | **故障转移供应商**：`model_pools`（单模型类别）+ `model_pool_members`（成员与优先级）。纯新增表，不改动任何既有表 |
| v20 | `key_reservations` 重建为独立账本（去掉 `key_id` / `model_id` 外键），并按 `requests` 补回历史账目：删 Key / 模型 / 渠道不再清零组额度，保留期也不再裁剪账目 |
| v21 | `providers` 加 6 列生效时段（`schedule_mode` 等），默认 `always` —— 既有渠道升级后行为不变 |
| v25 | `providers` 加 `schedule_windows`（JSON 多段，默认空）—— 旧的单段列继续写第一段，历史数据不需要回填 |
| v22 | 聚合模型组：`model_groups.kind`（默认 `normal`）、`group_compositions`（聚合组 → 成员组，只允许一层）、`requests.source_group_id` 与 `key_reservations.source_group_id`（真正提供该模型的组）。纯新增，既有组与账目行为不变 |
| v23 | **只有聚合组对外**：把「没有被任何聚合组包含」的 `normal` 组改成 `aggregate`（升级前它们本来就是独立对外的组，不改会让既有 Key 全部失效）；成员关系里的 `normal` 组保留成员身份。无结构变更 |
| v26 | `model_groups` 加 `concurrency_limit`（并发上限，`NULL` = 不限），并建 `key_reservations(source_group_id,status)` 索引供计数。纯新增，既有组不限并发，升级后行为不变 |
| v24 | **一个渠道只能属于一个组**：把历史上重复挂到多个组的渠道收敛成一条（保留**最早建立**的归属，rowid 最小），再建唯一索引 `group_providers(provider_id)`。重复的那条关系过去从未被计费（用量恒为 0），删掉不影响账目；若某个组因此变成 0 个渠道，管理台会直接显示「0 渠道」 |

迁移在启动时自动应用，记录在 `schema_migrations` 表。
