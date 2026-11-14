# Civic Relay

Civic Relay 是一个单进程 Go 服务，将多个 OpenAI 兼容上游统一转发给局域网客户端。它支持供应商、模型、模型组、随机客户端 Key、并发限制、Token/金额配额、SSE 流式响应和中文管理端。

## 当前能力

### 转发与计费

- `GET /v1/models`：只返回当前客户端 Key 被授权的启用且已定价模型。
- `POST /v1/chat/completions`：支持普通 JSON 和流式 SSE。
- `POST /v1/responses`：OpenAI Responses API 兼容层——请求转换为 chat/completions 转发上游，支持 `instructions`、字符串或数组 `input`、`max_output_tokens`、流式（标准 response.created/output_text.delta/response.completed 事件序列）；计费与 chat/completions 同口径。暂不支持工具调用。
- 每个响应带服务端生成的 `X-Request-Id`；错误体和流内终态复用该 ID。客户端可用 `X-Client-Request-Id` 作为关联标签，管理端请求诊断页按 `request_id` 查看真实上游状态、错误码、失败阶段、usage 来源和耗时。
- 每个模型独立计价（输入/输出/缓存输入分别计价，单位：元/百万 token）。
- 缓存输入计费：解析上游 `usage.prompt_tokens_details.cached_tokens`，命中部分按「缓存输入单价」计费；未配置时回退输入价，填 0 表示缓存免费。非流式与流式共用同一计价实现（`relay.PriceUsage`）。
- 每个模型组独立倍率（`rate_milli` 千分比，1000 = 1.0×）；请求时按 Key 所属分组的倍率折算，模型原始单价存库不改写，只在预留与结算时折算（缓存单价同样受倍率约束）。
- **只有聚合模型组对外（v0.10.0）**：组分成两类 —— **聚合模型组**（唯一能被客户端 Key 绑定、对外提供调用）与**成员组**（只有渠道 + 独立额度，不对外，只能被聚合组包含）。聚合组 = 自有渠道 ∪ 成员组的渠道 ∪ 直接包含的故障转移供应商（池，只授权池内模型，v0.14.0），Key 绑聚合组即可用上全部成员组模型。**一笔请求同时扣聚合组与成员组两边额度**（任一组窗口用满即 429，错误里写明是哪个组），倍率取 Key 绑定组、两边金额一致；账本仍只有一行（`group_id` + `source_group_id`），全局统计不会翻倍。只允许一层（聚合组只能包含成员组），聚合组拥有自己的倍率与九项额度。**成员组本身不对外**：即使库里有历史绑定，转发与记账两条路径也都不放行。

### 故障转移供应商（模型池）

与「供应商」并列的第二类供应商形式，适合「一个模型名背后挂多家上游」的场景：

- 每条记录**只对应一个对外模型名**（单一模型类别），下面挂已有供应商下的模型，数组顺序即优先级。客户端照常填这个模型名，无需感知背后有几个渠道。
- 成员出现连接失败 / 超时 / 5xx / 429 / 上游凭据失效 / 模型下架等故障时，按优先级**自动切到下一个可用成员**；400/422、客户端取消、整体时长超限不切换（换了也没用）。失败成员在进程内冷却 60 秒，后续请求优先走健康成员。
- 流式请求**只在响应头之前切换**，已开始输出后不会重发内容（避免重复文本）。一次请求只做一次预留与结算，多次尝试共用 `MAX_STREAM_DURATION`。
- **对外只有一个模型名（v0.14.1）**：池名是唯一入口，池成员模型**不出现在 `/v1/models`**、也不能按自己的名字直接调用（会返回 `model_not_allowed`）。客户端只填池名，网关在池内按优先级挑一个可用成员转发；成员所属渠道不在生效时段（见下）时自动跳过，等价于临时下线。池名只在「本组确实包含它、且至少一个成员此刻可路由」时出现在 `/v1/models`。
- **池按模型授权，不按渠道（v0.14.0）**：聚合组可以直接包含若干个池（`group_pools`），此时组拿到的**只有池里勾选的那几个模型**，成员渠道仍归它原来的组，渠道下别的模型不会跟着进来，也不会改动渠道归属。想让某个组只用池里的模型，就把池挂到组上，而不是把池成员的渠道勾进「自有渠道」——渠道是整条授权的，两者混用时管理台会当场提示。
- 计费按**实际成功成员**的单价与分组倍率结算，请求日志的 `model_id` / `provider_id` 也指向实际成员；预留仍按首选成员估算。接口与边界见 [docs/admin-api.md](docs/admin-api.md)。

### 安全

- 供应商 API Key 使用 AES-256-GCM 加密保存，管理接口只返回是否已配置。
- 客户端 Key 使用 `crk_` 前缀，数据库只保存 HMAC 摘要，明文只在创建/重置时返回一次。
- 密钥任何字符不会出现在列表接口：脱敏缩略（`crk_xxxx…`）与非敏感标识（`key-xxxxxx`）通过独立接口 `/admin/api/keys/hints` 提供（admin_test 强制）。
- 请求记录只保存元数据，不保存 prompt、回复内容、Authorization 或上游 Key。

### 配额与限制

- 成员组**管理渠道**：组内挂供应商（渠道），但不直接对外；被聚合组包含后，其渠道下所有已定价启用模型随聚合组授权（去重并集）。「查看 / 编辑」弹窗内展示每个成员渠道的模型与输入/输出/缓存单价。
- **一个渠道只能属于一个组（v0.11.0）**：渠道是「上游地址 + 凭据」的实体，挂进两个组就会让同一份上游账号受两份额度约束，而账目只能记到一个组上——必然有一边永远显示 0 用量。要按班次/场景分别限流，就为同一家上游建两条渠道（两个账号）。后端在保存时返回 400 `provider_in_use` 并指出原分组，数据库还有唯一索引兜底；管理台的渠道勾选列表会把已在别组的渠道置灰。
- **成员组并发上限（v0.13.0）**：额度管「一段时间用掉多少」，并发管「同一时刻几笔在跑」。上游账号常见限制是并发数，瞬时打满时额度还宽裕、请求已经开始 429 —— 给成员组设 `concurrency_limit` 后，名额满时新请求直接 429（文案写明哪个组、在跑几个、上限多少），前面的请求结算后立即恢复。计数按账本行的 `source_group_id`（与额度扣减同口径），在预留事务内判定，不会超发；与全局并发上限互不换算，两者都要满足。
- **三道并发闸各判各的（v0.13.1 复查）**：全局 `GLOBAL_CONCURRENCY_LIMIT`、Key 自己的 `concurrency_limit`（最小 1）、分组 `concurrency_limit` 同时生效，任何一道满都 429，数字之间不换算。Key 那道的上限改完**立刻生效**（内存计数器每次取名额都会同步新值，调大立即放行、调小只拦新请求），拒绝文案会写明是哪把 Key、在跑几个、上限多少；密钥列表显示「在跑 / 上限」，与分组列表同口径（只数 `status='reserved'` 的账本行）。
- **额度按分组分配（v0.5.0 起）**：4 个窗口 × `Token / 金额` —— `5 小时`（**滚动**）、`当日`（**北京时间自然日**）、`本周`（**北京时间自然周，周一起**）、`自定义周期`（天数 + 起始时间）。额度在管理台「聚合模型组 / 成员组 → 查看 / 编辑」里按组维护，判定时读的是**密钥所属的聚合组**与**真正提供该模型的成员组**这两行（事务内读取，改完下一次请求即生效），全部在 SQLite 事务中判定。自定义周期要**同时**设置「周期长度」和至少一项「周期限额」才生效：生效期间不计入周限额；**周期用满即关闭服务**（429 拒绝，用量不会归零重算），调大限额后自动恢复；周期到期后按周限额接管。分组额度取代了旧版的全局 `TOKEN_LIMIT_*` / `AMOUNT_LIMIT_*` / `PERIOD_LIMIT_*` 配置项——升级时这些残留键会被自动灌进每个现有分组（只填未设过的窗口），然后从 relay.env 移除，所以老部署不会因为升级变成「全部不限」。额度账本是独立表（v20）：不受 `RETENTION_DAYS` 裁剪，也不会因为删除 Key / 模型 / 渠道而清零——删一把 Key 再建一把，组用量继续累计。
- **不限**：额度项填 `0`（或留空）表示不限；出厂默认**所有分组都不限**，新部署不会因为一个出厂限额就在第一次请求被拒。
- **先欠费后停服**：判定只看「该窗口是否已用满」，不再要求「已用 + 本次预留 ≤ 限额」。只要还有余量就放行，哪怕本次请求会让它略微超出；用到限额之后才拒绝后续请求。限额调大或窗口轮换后**自动恢复**，无需人工干预。
- 额度拒绝返回 **429** 并带具体窗口与数字（`token_quota_exceeded` / `amount_quota_exceeded` / `key_token_quota_exceeded` / `key_amount_quota_exceeded`），不会被误当成服务端故障而触发客户端重试。
- **不再因额度自动停用 Key**（v0.3.2 起）：旧行为会把 Key 置为 `enabled=0`，调大限额后也不恢复，只会让人以为"明明还有余量却用不了"。停用现在只能手动操作，额度控制由逐请求判定负责。
- 运维加固：启动时及每 5 分钟自动回收崩溃残留的预留（防止并发槽位与 5h 配额永久泄漏）；每日按 `RETENTION_DAYS` 自动清理过期账目；HTTP 服务带读超时（Slowloris 防护）与空闲连接回收；SIGINT/SIGTERM 优雅停机，先排空在途请求（含 SSE 流）再落盘退出；RSS 超过 `MEMORY_LIMIT_MB` 时拒绝新公共请求并中止进行中的流；提供免认证的 `GET /healthz`（进程存活，不触达数据库）与 `GET /readyz`（数据库就绪）探针，可直接用于容器与反向代理健康检查；启动日志打印构建版本（`civic-relay <version> (commit <hash>, built <date>)`），便于确认线上运行的镜像版本。
- 管理端「运行配置」保存后原子写回 relay.env 并**热应用**（配额、输出上限、并发闸门、内存保护、日志级别、监测与同步间隔都即时生效）；仅 `HOST`/`PORT`/`DB_PATH` 需要重启（换 `RELAY_ENCRYPTION_KEY` 也必须重启）。渠道密钥轮换或地址修改后上游连接缓存自动驱逐。
- 自动同步上游模型：`MODEL_AUTO_SYNC`（默认**关闭**）+ `MODEL_SYNC_INTERVAL`（默认 `30m`）。开启后按间隔遍历所有启用渠道拉取模型目录，把上游新增的模型导入进来。导入的模型一律是**停用 + 未定价**，需要运维定价并启用后才会授权给客户端——自动同步只负责「发现」，不绕过定价环节，也不会悄悄改变对外可用的模型列表。
- **流式时长由 `MAX_STREAM_DURATION` 决定**（默认 30 分钟，`0` = 不限，**改完即时生效**）：上游 HTTP 客户端**刻意不设总超时**。响应头等待由 `UPSTREAM_HEADER_TIMEOUT` 控制（留空时沿用 `connect + read` 兼容计算），`UPSTREAM_IDLE_TIMEOUT` 控制上游流的空闲间隔，`SSE_HEARTBEAT_INTERVAL` 控制下行保活注释。Chat 流只有 `[DONE]` 才计为 completed，Responses 流使用 `response.completed` / `response.incomplete` / `response.failed` 终态；请求记录保留真实 upstream status 和失败阶段。⚠️ 上游连接/头/读写/池超时在进程启动时应用，修改后需重启容器。
- 分级日志：`LOG_LEVEL`（`DEBUG` / `INFO` / `WARN` / `ERROR`，默认 `INFO`，写错会在保存/启动阶段被拒）。调到 `DEBUG` 会额外输出**逐请求访问日志**（方法、路径、状态码、耗时、来源地址），用来回答「客户端说报错了，但容器日志里什么都没有」。访问日志**不记查询串、不记请求头、不记请求体**——有客户端会把密钥放在 `?api_key=`，提示词也属于用户隐私，与本项目「账本只存元数据」的口径一致。
- 已删除的遗留配置：`UPSTREAM_BASE_URL` / `UPSTREAM_API_KEY`（单上游时代遗留，渠道现已全部存在数据库里、密钥加密存储）与 `DOCS_ENABLED`（服务端从来没有 docs 路由）。旧 relay.env 里残留的这几行会被当作未知键保留但不再读取。
- 管理端概览只显示 RPM、并发与近一小时成功率（限额已按组分配，全局视图里没有可展示的限额）；模型组列表按三个主窗口（5h/当日/本周）中最紧张的那个给出用量百分比，≥80% 标黄、已用满标红「已欠费」；每组「概览」弹窗按窗口展示 Token 与金额的「已用 / 本组限额」。Key 列表实时展示已用 Token / 已用金额 / 请求次数（口径与限额检查一致，含未结算预留）。
- 默认进程 RSS 软限制为 200 MB；Linux、Windows 均提供读取实现。

### 管理（中文 Web 控制台，`/admin/`）

- **两种登录主体**：管理员密钥（`X-Admin-Key`）看到全部页面；客户端密钥（`crk_…`）是只读且按分组裁剪的，导航里只剩「运行概览 / 用量分析 / 请求日志」——供应商、成员组等运维页面连入口都不出现（v0.13.2），概览与请求日志也不再有「渠道」列。这只是界面可见性，不是安全边界：真正的隔离由服务端按分组裁剪（跨组一律 404）。
- 供应商（渠道）：增删改查；「模型管理」弹窗统一完成四件事——① 拉取上游 `/v1/models` 勾选导入（可逐个自定义对外名称作为映射，幂等）② 手动添加模型（自由命名 + 映射 + 可选定价）③ 已接入模型列表（含映射与计价）④ 行内编辑（名称/映射/计价/启用）与删除。导入为停用未定价。模型无独立页面，全部通过渠道查看与维护。
- **供应商生效时段**：列表每行「设置时段」可选三种互斥方式——不限（默认）、固定日期区间（到点自动生效/失效）、每天重复时段（**可加多段、每段单独选星期**、可跨零点），时区固定北京时间。比如「工作日 22:00-08:00 + 周末全天」是同一条渠道的一组配置，不用拆成两个上游账号。时段外该渠道下的模型**不出现在 `/v1/models`、也不会被路由**（故障转移池自动跳下一个成员），探活与定时模型同步同样跳过；供应商列表用「未到时段」标签和「停用」区分，到点自动恢复，不需要人去点开关。
- **排除时间段（v0.15.0）**：同一个弹窗里还能加「排除段」——命中即不可用，即使正处在生效时段内。它与上面的三种方式互不影响，所以「平时全天可用、只在每天凌晨 02:00-04:00 上游维护时下线」不用把剩下 22 小时逐段列出来；每段同样能单独选星期。列表里显示成 `不限 · 排除 02:00-04:00`。全天排除（起止相同）会被拒绝——那是「停用」，用开关表达更直白。
- 故障转移供应商（模型池）：与「供应商」并列的独立页面；创建/编辑/删除、启停、从已有供应商模型里勾选成员并用 ↑↓ 调整优先级、单独停用某个成员；列表按优先级展示成员与所属供应商。
- 成员组：增删改查、组内渠道增删（拒绝停用渠道）；**可设并发上限**（v0.13.0，`concurrency_limit`，0/留空 = 不限）——按「真正承接请求的渠道所属组」计数，名额满了 429 `group_concurrency_exceeded`，与全局 `GLOBAL_CONCURRENCY_LIMIT` 各判各的（全局先超同样是超限，两者不需要换算）；列表实时显示「在跑 / 上限」，满额标红；倍率只展示不参与扣费（扣费取 Key 绑定的聚合组倍率），界面上不可改；列表有「被聚合组包含」列，未被任何聚合组包含的组会被显著标出（它的渠道与额度不会被任何请求用到）、**按组设置 4 窗口 × Token/金额额度**（0 = 不限、留空 = 保持原值，金额按元输入）；每组独立「概览」弹窗——RPM、四个窗口的已用/本组限额、进行中请求、近一小时成功率，**仅统计归属本组的请求**（按账目行上的分组归属，与额度扣减同口径，`GET /admin/api/groups/{id}/overview`）。删除分组会提示影响面（被几个聚合组包含；历史上绑在它上面的密钥会一并解绑）。
- 聚合模型组（v0.9.0，v0.10.0 起是**唯一可被 Key 绑定**的组）：独立页面；创建/编辑/删除、包含哪些成员组（可只挂自有渠道，成员组与自有渠道至少要有一个；已在别组的渠道不可选）、独立倍率与九项额度；列表显示成员组、渠道数、额度紧张度与探活状态；删除聚合组不动成员组。客户端 Key 的分组单选只列聚合组，绑成员组会被后端以 400 `group_not_callable` 拒绝。
- 客户端 Key：创建（明文只返回一次，需填并发上限）、重置密钥（旧密钥立即失效）、分组调整（只能绑聚合组）、删除（立即吊销）；列表的「并发」列显示「在跑 / 上限」（满额标红），口径与闸门一致。
- **应用型凭据（客户端里没有密钥）**：创建时「接入方式」选**应用标识**（接口 `keyless: true`），服务端**不产生任何可用密钥**（库里存的是创建时即丢弃的随机摘要，明文不存在于任何地方），客户端只用公开的 `X-App-Id: app_…` 请求头接入。适用于「软件里不能保存密钥、且不想让用户拿到能带走的凭据」的场景 —— 扒开客户端也只有一个公开值。**代价是应用标识公开，能访问到网关的人都能调用**，所以必须让网关只在可信网络内可达，并给该应用设好并发/额度上限；要跨不可信网络又要求客户端零凭据，正确做法是让客户端连你自己的中转服务。
- **短时令牌**：创建/编辑 Key 时可填「有效期」（接口字段 `ttl_seconds`），到点自动失效——**不需要有人记得去吊销**，这是把凭据交给第三方程序/外包/别人机器时的正确姿势。过期返回 401 `token_expired`（与 `invalid_api_key` 分开：前者是「该换新令牌」，后者是「凭据不对」，客户端不会把前者当服务故障重试）；管理台列表显示剩余寿命，不足 24 小时标黄、已过期标红；续期就是再填一次小时数（填 `0` 改为永久有效，**留空则不动**，所以只改名字不会顺手清掉有效期）。到期不会删除 Key，历史账目与用量统计保留。
- **接入信息**：Key 列表每行的「接入信息」弹窗把「其他软件该填什么」一次列全——Base URL 与派生地址、脱敏密钥标识、该 Key 实际可用的模型清单、所属分组，以及会导致连不上的问题清单（Key 停用、未绑分组、没有已定价模型），并附可直接复制的自检命令。模型清单与公共 `/v1/models` 同源，不会出现「管理台说已授权、客户端却拉不到」。
- 分组可用性监测：按 `GROUP_MONITOR_INTERVAL`（默认 `30m`，填 `0` 关闭）定时检查每个分组。每轮先判断配置是否成立（组启用、有启用中的渠道、渠道下有「已启用且已定价」的模型），再对启用渠道打一次上游 `GET /v1/models` 探活（只读、不产生 token 费用）。
- 判定结果分五态：**可用** / **部分渠道不可达**（组仍可服务，但走坏渠道的模型会失败）/ **全部渠道不可达** / **配置不可用**（要去配渠道或定价）/ **已停用**（不算故障，灰色显示）。结果落库并在分组列表展示，非正常状态额外写一条容器日志。
- 监测**只发现与展示，不改变任何服务行为**：不会自动停用渠道、不会拒绝请求。分组列表支持「立即检测全部」，单个分组支持「检测」，概览弹窗内可看最近 10 轮历史，都不必等定时轮询。
- 四类资源均支持删除：删除供应商级联删除其模型；历史账单金额一律保留；重复删除返回 404。
- 根路径 `/` 302 跳转到 `/admin/`。

### 外部 WASM 插件

管理台的「WASM 插件」页支持上传、校验、启用、停用、启动和删除外部插件包。插件包为 ZIP，包含 `manifest.json` 与 `module.wasm`；启动时由 wazero 在无宿主导入的独立实例中执行，清单可声明入口、权限和内存/超时上限。插件默认保存到 `PLUGIN_DIR=data/plugins`，该目录位于数据卷内。完整接口见 [docs/admin-api.md](docs/admin-api.md)。
- **请求诊断**：管理端「请求日志」支持状态、渠道、模型、协议、时间和 request ID 筛选，游标加载更多，并可查看单条请求的真实上游状态、错误码、失败阶段、usage 来源、报价与耗时快照。

## 本地启动

需要 Go 1.27 或更新版本。配置文件不放在仓库内：

```powershell
$env:CIVIC_RELAY_CONFIG_FILE = "C:\ProgramData\CivicRelay\relay.env"
go run ./cmd/civic-relay
```

首次启动会创建 `relay.env` 和一次性 `bootstrap-admin-key.txt`。读取管理员 Key 后，请妥善保存并删除 bootstrap 文件。管理端地址：

```text
http://127.0.0.1:8000/admin
```

局域网测试时将 `HOST` 设置为 `0.0.0.0`，然后用服务器局域网 IP 访问。先在管理端添加供应商与模型，建一个成员组把渠道装进去，再建一个聚合模型组包含它（或直接给聚合组挂渠道），最后创建客户端 Key 并绑定这个聚合组。

客户端使用：

```bash
curl http://<relay-ip>:8000/v1/chat/completions \
  -H "Authorization: Bearer crk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"provider/model","messages":[{"role":"user","content":"Hello"}]}'
```

客户端只需要三样东西：**Base URL**、**API Key**、**模型名**。在管理台的 Key 列表点某一行的「接入信息」，弹窗会把三样一次列全，并给出可直接复制的自检命令——不必自己拼（拼错的表现通常是客户端 400/404，很难反推）。给其他软件接入的完整说明（各客户端填法、错误码对照、密钥安全边界、防火墙）见 [docs/client-integration.md](docs/client-integration.md)。

客户端 Base URL 填 `http://<relay-ip>:8000/v1`（末尾不要多带或漏掉 `/v1`）。服务端对常见路径变体做了容错：`/v1/v1/...`（多一层前缀）、`/chat/completions`（少前缀）、尾斜杠均自动归一处理；其余未知端点返回 JSON 404 并提示支持的路径。已实现的对话端点有两个：`POST /v1/chat/completions` 与 `POST /v1/responses`（Responses API 兼容层，暂不支持工具调用）。若对话 404 但能拉取模型，多半是客户端把模型名填错成带空格的值，或使用了本服务未实现的端点（如 `/v1/embeddings`）。

## Docker 运行

镜像多阶段构建，基于 distroless，以非 root（UID `65532`）运行，根文件系统只读，内置健康检查。配置与数据库放在卷里，仓库目录不参与运行。

### 方式一：命名卷（默认，跨平台零准备）

```bash
docker compose up -d --build
docker compose ps          # 等待 STATUS 变为 healthy
docker compose logs -f
```

首次启动会在 `civic-relay-config` 卷内生成 `relay.env` 与一次性的 `bootstrap-admin-key.txt`：

```bash
docker compose exec civic-relay cat /app/config/bootstrap-admin-key.txt
docker compose exec civic-relay rm /app/config/bootstrap-admin-key.txt
```

管理端地址 `http://127.0.0.1:8000/admin`，局域网内其他设备用 `http://<本机IP>:8000/admin`。端口默认绑定所有网卡（等价 `8000:8000`）；只允许本机访问时设 `CIVIC_RELAY_BIND=127.0.0.1`。注意这是明文 HTTP，管理台与客户端 Key 都会明文传输，只在可信内网使用，对外提供访问必须经 HTTPS 反向代理。

### 中文运维脚本（推荐日常使用）

不用记 docker 命令，脚本会自动定位 Docker Desktop 的 `docker.exe`：

```bash
./scripts/docker-ops.sh 帮助        # 全部命令
./scripts/docker-ops.sh 启动        # 构建并启动，等它变 healthy
./scripts/docker-ops.sh 探针        # /healthz、/readyz、容器健康一眼看完
./scripts/docker-ops.sh 备份        # 停容器后完整备份配置与数据库
./scripts/docker-ops.sh 看卷        # distroless 无 shell，用辅助容器查看卷内容
```

### 方式二：宿主机目录（需要直接备份数据库或审计文件时）

```bash
export CIVIC_RELAY_CONFIG_DIR=/srv/civic-relay/config
export CIVIC_RELAY_DATA_DIR=/srv/civic-relay/data
sudo mkdir -p "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
sudo chown -R 65532:65532 "$CIVIC_RELAY_CONFIG_DIR" "$CIVIC_RELAY_DATA_DIR"
docker compose -f docker-compose.yml -f docker-compose.bind.yml up -d --build
```

容器以 UID `65532` 运行，宿主机目录未 `chown` 会因无写权限启动失败（Windows + Docker Desktop 绑定 Windows 目录不需要，Desktop 会处理属主）。

### 可调参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `CIVIC_RELAY_BIND` | `0.0.0.0` | 宿主机绑定地址；设 `127.0.0.1` 可退回到仅本机可访问 |
| `CIVIC_RELAY_PORT` | `8000` | 宿主机端口 |
| `CIVIC_RELAY_VERSION` | `dev` | 镜像 tag，同时注入 `/healthz` 与启动日志 |
| `TZ` | `Asia/Shanghai` | 容器日志时区（镜像已内置该时区数据）；只影响日志时间戳，配额窗口与账目一律用 UTC |
| `CIVIC_RELAY_CONFIG_DIR` / `CIVIC_RELAY_DATA_DIR` | 无 | 仅绑定卷覆盖文件需要 |

### 健康检查与资源上限

- `GET /healthz`：进程存活探针，刻意不触达数据库（返回 `{"status":"ok","version":"..."}`），避免瞬时数据库锁竞争触发容器重启。
- `GET /readyz`：依赖探针，确认加密数据库仍能应答；不可用时返回 503 `database_unavailable`，编排层可据此摘流量。
- 容器 `HEALTHCHECK` 复用同一个二进制（`civic-relay -healthcheck`），它按 `relay.env` 解析端口，因此改端口后探针会跟着变，不会让容器永久处于 unhealthy。
- `mem_limit` 默认 `256m`，略高于 `MEMORY_LIMIT_MB`（默认 200MB）。这样应用自身的 RSS 软保护会先返回 503，而不是被 cgroup OOM 直接杀进程。调整 `MEMORY_LIMIT_MB` 时请同步调整 `mem_limit`，建议留 25% 余量。
- 停止容器时先发 SIGTERM，进程会排空在途请求（含 SSE 流，最长 15 秒）后落盘退出，`stop_grace_period` 已设 30 秒。

完整的容器运行手册（备份恢复、版本升级、排障）见 [docs/docker-deploy.md](docs/docker-deploy.md)。

## Linux systemd

编译并安装二进制到 `/opt/civic-ai-relay/civic-relay`，创建 `civic-relay` 用户和 `/etc/civic-relay`、`/var/lib/civic-relay` 目录，然后安装：

```bash
sudo cp civic-relay-go.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now civic-relay-go
```

服务单元启用 `MemoryMax=200M`、`ProtectSystem=strict`、`ProtectHome=true` 和 `NoNewPrivileges=true`。

## 安全说明

- 不要把 `relay.env`、`bootstrap-admin-key.txt`、数据库文件或真实上游 Key 提交到 Git。
- 管理员 Key 与客户端 Key 不同；管理员接口使用 `X-Admin-Key`，公共接口使用 `Authorization: Bearer`。
- HTTPS 终止应放在反向代理或 VPS 面板；代理到本机 Go 服务的 HTTP 只适用于受信任的本机链路。
- 修改 `RELAY_ENCRYPTION_KEY` 会导致已有供应商 Key 无法解密；数据库与该密钥必须一起备份。

## 测试

```powershell
go test -count=1 ./...
go vet ./...
```

当前 Windows 环境没有 CGO/GCC，`go test -race ./...` 需要在具备 GCC 的环境执行。

### 端到端联调（假上游 + 真实容器）

单元测试覆盖不到容器网络、引导写入的出厂配置、以及管理台热改配置后的行为，这些由
`_e2e/` 下的脚本兜住（本地工具，已 gitignore，不进发布包）：

```powershell
# 先在仓库根目录构建镜像
docker build -f Dockerfile.relay -t civic-relay:v0.3.2 --build-arg VERSION=v0.3.2 .

# 起假上游 → 拉容器 → 建渠道/模型/组/Key → 对话 → 额度对照实验 → 清理
python _e2e\run_e2e.py
```

脚本会自己解析「容器侧可达的宿主机 IP」（`host.docker.internal` 指向的真实地址，
本机实测 `192.168.65.254`），因此不需要手工改地址。假上游必须绑 `0.0.0.0`，
绑 `127.0.0.1` 容器访问不到。

## 打发布包

```powershell
docker build -f Dockerfile.relay -t civic-relay:v0.3.2 `
  --build-arg VERSION=v0.3.2 --build-arg COMMIT=<hash> --build-arg BUILD_DATE=<iso> .
python scripts\package_release.py v0.3.2
```

产物都在 `dist/` 下，其中 `civic-relay-v0.3.2-deploy.tar.gz` 是**上传服务器用的那一个**：
解压即得完整目录（源码 + `.env` + `DEPLOY.md` + 镜像包），服务器上 `docker load -i` 再
`docker compose up -d` 即可，不需要联网构建。

`DEPLOY.md` 由 `docs/DEPLOY.md` 模板渲染（`{{VERSION}}` 占位），改动部署说明请改模板。

## 更多文档

- [docs/changelog.md](docs/changelog.md) — **变更记录（v0.3.4 → v0.11.0）**：每个版本「当时存在的问题 → 修复 → 回归」，以及尚未实现的事项。
- [docs/client-integration.md](docs/client-integration.md) — **客户端接入指南**：其他软件要填什么、怎么自检、错误码对照、代理配置、密钥安全边界。
- [docs/deploy-with-trailblazer.md](docs/deploy-with-trailblazer.md) — **用部署管理器部署/更新本服务**：打包站点 zip、首次接管、Nginx（宝塔）SSE 配置。
- [docs/admin-api.md](docs/admin-api.md) — 完整的管理端与公共 API 参考（路由、请求/响应字段、语义约定）。
- [docs/breakpoint-2026-09-10.md](docs/breakpoint-2026-09-10.md) — 渠道多模型 + 分组倍率功能说明与端到端验证记录。
- [docs/superpowers/](docs/superpowers/) — 历史设计规格与实施计划存档。
