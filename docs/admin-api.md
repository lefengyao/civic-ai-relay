# Civic Relay API 参考

> 与 `internal/httpapi/admin.go`、`public.go`、`server.go` 保持同步（2026-09-10 核对）。

所有管理接口挂在 `/admin/api/` 下，认证方式为请求头 `X-Admin-Key: <管理员密钥>`。公共转发接口挂在 `/v1/` 下，认证方式为 `Authorization: Bearer crk_...`。两种密钥不通用。

错误统一返回 `{"error": {"message": "...", "type": "relay_error", "code": "...", "request_id": "..."}}`；`request_id` 同时出现在响应头 `X-Request-Id`，用于把客户端错误、流内错误和账本记录关联起来。服务端始终生成新的 request ID，客户端提供的 `X-Client-Request-Id`（兼容读取 `X-Request-Id`）只作为关联字段。认证失败一律 401，但码分三种：管理端是 `authentication_required`，公共接口是 `invalid_api_key`（令牌不存在或已停用）与 `token_expired`（短时令牌已过期 —— 凭据没错，只是该换新令牌了，客户端不应把它当服务故障重试）。所有列表字段在无数据时返回 `[]` 而非 `null`（含 `data`、`trend`、各组 `provider_ids`/`model_ids` 等），客户端无需额外判空。公共接口的请求体解析错误和上游错误会携带具体原因（`message = code: detail`）；服务层 `invalid_request` 和其他内部错误使用稳定提示，不回传底层错误正文。管理端的渠道建/改失败（`provider_invalid`）同样携带具体原因，并额外附 `code` 字段（`message` 带原因便于管理台直接展示，`code` 保持稳定便于程序匹配）。当进程 RSS 超过 `MEMORY_LIMIT_MB` 时，公共对话请求返回 503 `memory_limit_exceeded`，进行中的流式响应会被中止（按 aborted 结算）；管理接口不受内存保护影响。

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

---

## 管理端 API

### 概览与配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/overview` | 账本概览 + 全局并发（active/limit）。返回 `{rpm{used,limit}, five_hour{used_tokens,limit,used_microyuan,amount_limit}, daily{...}, period{used_tokens,limit,used_microyuan,amount_limit,active,start_utc,end_utc}, weekly{used_tokens,limit,used_microyuan,amount_limit,active,from_utc}, concurrency{active,limit}, last_hour{...}, trend[], recent[]}`；窗口均为「已用 / 限额」，含未结算预留；限额为 `0` 即**不限**（前端显示为「不限」）。周期 active 时周限额暂停，from_utc 记录恢复统计的时间。 |
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

六个固定额度维度：`5 小时`（滚动窗口）、`当日`（北京时间自然日）、`本周`（北京时间自然周，周一起）× `Token` / `金额`；另有可选的自定义周期 Token/金额额度。
配置项为 `TOKEN_LIMIT_5H` / `TOKEN_LIMIT_DAILY` / `TOKEN_LIMIT_WEEKLY` 与
`AMOUNT_LIMIT_5H` / `AMOUNT_LIMIT_DAILY` / `AMOUNT_LIMIT_WEEKLY`（金额单位是**元**，可带小数如 `12.5`，内部换算成微元存储）。

自定义周期使用 `PERIOD_LIMIT_DAYS`、`PERIOD_LIMIT_START`、`TOKEN_LIMIT_PERIOD` 和 `AMOUNT_LIMIT_PERIOD`。周期天数大于 0 时，若开始时间为空则从保存配置时开始。周期有效期间不统计/拦截周限额；周期 Token 或金额任一用满，或到达周期截止日后，后续请求切换到周限额。周期内最后一笔请求仍只计入周期，周限额从交接时刻之后开始累计。管理台的「+1 天」「+7 天」按钮用于延长进行中的周期；周期已结束时从当前时刻新开周期。
另有两项不随窗口轮换的 Key 总额度：`token_limit`、`amount_limit_microyuan`。

**任何一项填 `0` 或留空都表示不限**，出厂默认全部不限。

**判定口径 = 先欠费后停服**：只检查「该窗口是否已用满」，不再要求「已用 + 本次预留 ≤ 限额」。
只要窗口还有余量就放行（本次请求可能让它略微超出），用到限额之后才拒绝后续请求。
调大限额或窗口轮换后**自动恢复**，不需要人工干预。

额度类拒绝一律返回 **429**（不是 500，也不是 401 —— 否则客户端会误判成服务端故障而反复重试）：

| `error.code` | 含义 |
|---|---|
| `token_quota_exceeded` | 某个全局 Token 窗口已用满，`error.message` 写明是 5 小时 / 当日 / 本周 |
| `amount_quota_exceeded` | 某个全局金额窗口已用满 |
| `key_token_quota_exceeded` | Key 的 Token 总额度已用满 |
| `key_amount_quota_exceeded` | Key 的金额总额度已用满 |
| `rpm_exceeded` | 超过 `RPM_LIMIT`（滚动一分钟） |
| `rate_limit_exceeded` | 全局并发或 Key 并发达到上限 |

`error.message` 形如：

```
token_quota_exceeded: 5 小时窗口Token已用满（已用 5,000,000 tokens，限额 5,000,000）。当前处于欠费状态，请等待窗口轮换或在管理台调大限额
```

> 额度用满**不会**停用 Key。Key 停用只能手动操作；停用后公共接口返回 401 `invalid_api_key`。
> 旧版本会在 Key 总额度用满时自动置 `enabled=0`，该行为已移除，升级时的迁移会把历史遗留的
> `disabled_reason='quota_exhausted'` 恢复为启用。

### 供应商（渠道）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/providers` | 列表（不返回明文 Key，仅返回是否已配置） |
| POST | `/admin/api/providers` | 创建；body `{name, base_url, api_key}` |
| PUT | `/admin/api/providers/{id}` | 更新（nil 字段保持原值）。⚠️ **body 用 PascalCase**：`{Name, BaseURL, APIKey, Enabled}` —— `store.UpdateProvider` 没有 json tag，靠 `encoding/json` 的大小写不敏感匹配对上（与 POST 的 snake_case 不一致，管理台就是这么发的）。改成 snake_case 会**静默变成空更新**（不报错也不生效），契约由 `admin_test.go::TestAdminUpdateProviderAcceptsPascalCasePayload` 钉住 |
| DELETE | `/admin/api/providers/{id}` | 删除，**级联删除该渠道下的所有模型**；重复删除返回 404 |
| GET | `/admin/api/providers/{id}/remote-models` | 拉取上游 `/v1/models` 预览，返回 `{upstream_name, public_name, exists}`，不导入 |
| POST | `/admin/api/providers/{id}/models/import` | 批量导入；body `{"models": [{upstream_name, public_name?}]}`。public_name 缺省为 `渠道名/上游名`。**幂等**：已存在的 public_name 跳过；导入结果为停用 + 未定价，需到模型页定价并启用后才能加入分组 |

供应商 `base_url` 存库时自动去掉 `/v1` 后缀，请求上游时自动补回。

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

### 模型组

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/groups` | 列表（含 rate_milli） |
| POST | `/admin/api/groups` | 创建；body `{name, rate?, rate_milli?, provider_ids?}` |
| PUT | `/admin/api/groups/{id}` | 更新 `{name?, enabled?, rate?, rate_milli?}` |
| DELETE | `/admin/api/groups/{id}` | 删除；重复删除返回 404 |
| GET | `/admin/api/groups/{id}/providers` | 组内渠道 ID 列表 |
| PUT | `/admin/api/groups/{id}/providers` | **整体替换**组内渠道；body `{"provider_ids": [...]}`；拒绝停用渠道 |
| GET | `/admin/api/groups/{id}/overview` | **分组概览**：仅统计走组内渠道的请求。返回 `{rpm.used, five_hour{used_tokens,used_microyuan}, daily{used_tokens,used_microyuan}, weekly{used_tokens,used_microyuan}, last_hour{completed,failed,aborted,rejected,reserved,error_rate}, active, generated_at_utc, group_id}`；令牌/金额含未结算预留；限额为全局配置，分组不设限额 |
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
| `config_unavailable` | 配置层面不成立（无启用渠道，或渠道下没有已启用且已定价的模型） | 去配渠道或定价；**此时不做探活** |
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
| POST | `/admin/api/keys` | 创建；body `{name, max_concurrency, token_limit_total?, money_limit_yuan_total?, group_ids?, ttl_seconds?, keyless?}`。**明文 token 只在本次响应返回一次**。`ttl_seconds > 0` 时创建为**短时令牌**（从现在起 N 秒后自动失效），缺省/0/负数 = 永久有效。`keyless: true` 时创建**应用型凭据**：不产生任何可用密钥（响应里 `token` 为空串），改用公开的应用标识接入，见下节 |
| GET | `/admin/api/keys/hints` | 脱敏缩略 `crk_xxxx…` + 非敏感标识 `key-xxxxxx`（独立接口，保证列表永不泄露密钥字符） |
| PUT | `/admin/api/keys/{id}` | 更新 `{name?, enabled?, concurrency_limit?, token_limit_total?, money_limit_yuan_total?, ttl_seconds?}`；**nil 字段保持原值**。`ttl_seconds` 三态：缺省 = 不动；`0`/负数 = 改为永久有效；`>0` = 从现在起 N 秒后到期（**续期就是再传一次**） |
| DELETE | `/admin/api/keys/{id}` | 删除：立即吊销访问；历史账目金额保留；重复删除返回 404 |
| POST | `/admin/api/keys/{id}/rotate` | 重置密钥：旧 token 立即失效，新 token 只返回一次 |
| GET | `/admin/api/keys/{id}/groups` | Key 所属分组 ID 列表 |
| PUT | `/admin/api/keys/{id}/groups` | **整体替换**所属分组；body `{"group_ids": [...]}` |
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
    "groups": [{"id": 2, "name": "写作组", "enabled": true, "rate_milli": 1000}],
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

金额限额传参单位为**元**（`money_limit_yuan_total`），存储单位为微元（`AmountLimitMicroyuan`）。达到任一总配额后 Key 自动停用，`DisabledReason = quota_exhausted`。

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

迁移在启动时自动应用，记录在 `schema_migrations` 表。
