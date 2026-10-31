# 客户端接入指南

给「其他软件」接进 Civic Relay 用。客户端只需要三样东西：**Base URL**、**API Key**、**模型名**。
上游渠道的 API Key 全程留在服务端，客户端永远拿不到。

---

## 一、服务端先备好（一次性，在管理台 `/admin/` 里做）

| 步骤 | 在哪 | 说明 |
|---|---|---|
| ① 加渠道 | 供应商（渠道）→ 新建 | 填上游地址与**上游 API Key**。地址必须带协议头（`https://` 或私网 `http://`），不要带 `/v1`，服务端存库时会自动剥掉、请求时自动补回 |
| ② 定价并启用模型 | 渠道行的「模型管理」 | 拉取上游模型 → 勾选导入 → 在列表里逐个「编辑」填单价并启用。**未定价或未启用的模型不会授权给任何客户端** |
| ③ 建分组 | 模型组 → 新建 | 把渠道选进去。分组是权限边界：Key 只能看到「所属分组内渠道」下的模型 |
| ④ 建客户端 Key | 客户端 Key → 新建 | 勾选分组，**明文密钥只显示这一次**，立即复制保存 |

> 客户端侧「一个软件一个 Key、一个 Key 一个分组」是最省心的用法：某个软件的权限或额度需要调整时，只动它自己的 Key 与分组，不影响别的软件；需要吊销时删 Key 即可，旧密钥立刻失效。

## 二、客户端填什么

在管理台的 Key 列表里点某一行的 **「接入信息」**，弹窗会把三样一次列全，并给出可直接复制的自检命令。
（对应接口 `GET /admin/api/keys/{id}/connection`，见 [admin-api.md](admin-api.md#接入信息)。）

| 项 | 值 | 注意 |
|---|---|---|
| Base URL | `http://<服务器IP>:8000/v1` | 末尾 `/v1` 不能少；多写一层 `/v1/v1` 服务端也能容错，但别依赖这个 |
| API Key | `crk_...` | 放在 `Authorization: Bearer` 头里，**不是**自定义请求头。明文只在创建/重置时出现一次 |
| ↑ 若凭据是**应用型**（见第六节） | `app_...` | 这一项变成公开的**应用标识**，放在 `X-App-Id` 头里，不需要密钥 |
| 模型名 | 例：`deepseek/deepseek-chat` | 用「接入信息」里列出的名字（即**对外名称**），不是上游的原始模型名 |

## 三、在那台机器上先自检

```bash
# 1. 拉模型列表：返回 {"object":"list",...} 说明地址与密钥都对
curl http://10.190.164.209:8000/v1/models \
  -H "Authorization: Bearer crk_你的密钥"

# 2. 发一句话
curl http://10.190.164.209:8000/v1/chat/completions \
  -H "Authorization: Bearer crk_你的密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"你好"}]}'

# 3. 流式（OpenAI 客户端默认走这条）
curl -N http://10.190.164.209:8000/v1/chat/completions \
  -H "Authorization: Bearer crk_你的密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

### 常见客户端怎么填

| 客户端 | 填法 |
|---|---|
| Cherry Studio / ChatBox / NextChat 等 | 服务商类型选 **OpenAI**（或「自定义 OpenAI 兼容」），API 地址填 `http://<IP>:8000/v1`，密钥填 `crk_...`，模型在服务端拉取或手填「对外名称」 |
| OpenAI Python SDK | `OpenAI(base_url="http://<IP>:8000/v1", api_key="crk_...")` |
| OpenAI Node SDK | `new OpenAI({ baseURL: "http://<IP>:8000/v1", apiKey: "crk_..." })` |
| 自己写的程序 | 直接 POST `/v1/chat/completions`，请求体与 OpenAI 一致；错误处理见下表 |

已实现的端点：`GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/responses`（Responses API 兼容层，暂不支持工具调用）。
未实现 `/v1/embeddings` 等其它端点，调用会返回 JSON 404 并提示支持的路径。

流式客户端应忽略以 `:` 开头的 SSE 注释；它们是保活心跳，不是模型输出，也不包含用量。
每个响应都带服务端生成的 `X-Request-Id`；错误 JSON 的 `error.request_id`、Chat 流内错误对象和 Responses 失败事件复用同一值。客户端可发送 `X-Client-Request-Id` 作为关联标签，服务端会把它写入管理端请求详情，但仍以服务端 ID 作为账本主键。
Chat 流只有收到 `data: [DONE]` 才算正常结束；连接提前 EOF 或读取失败应显示为中断，不能把已收到的部分文本当作完整回答。HTTP 200 只表示流已建立，最终状态以 `[DONE]` 和管理端记录为准。
`finish_reason: "length"` 后若仍收到 `[DONE]`，表示 Chat 请求正常结束但回答达到输出上限。
Responses 流则以 `response.completed`、`response.incomplete` 或 `response.failed` 区分终态，不能只看最初的 HTTP 200；事件中的 `response.error.request_id` 与响应头保持一致。

### 网页 / 浏览器类客户端：先解决跨域

relay 的公共接口**不返回 CORS 头**（`OPTIONS /v1/models` 直接 405）。所以网页从**其它源**——
`file://` 双击打开、或另一个端口的静态服务器——直接 `fetch("/v1/...")` 会被浏览器同源策略拦掉，
典型表现是控制台报 CORS 错、请求看起来根本没发出去（服务端日志里也没有）。

两种做法：

1. **同源反向代理（推荐，生产环境也是这个套路）**：让页面和 `/v1` 处在同一个源下。
   仓库里有一个最小实现可以直接用：
   ```bash
   python _e2e/relay_demo_server.py      # 一个页面 + 把 /v1/* 转发到 127.0.0.1:8000
   # 浏览器打开 http://127.0.0.1:8088/ ，页面上选模型、发消息即可
   ```
   Nginx 同理：页面与接口放同一个 server 块里，`location /v1/ { proxy_pass http://127.0.0.1:8000; }`。
2. **给服务端加 CORS 头**：需要改代码（当前版本未实现）。注意这会让你的 relay 对**任意网页**开放，
   而浏览器里的密钥是明文可见的 —— 只在完全可信的环境下这么做。

## 四、出错了对照这张表

| 现象 | `error.code` | 原因与处理 |
|---|---|---|
| 401 | `authentication_required` / `invalid_api_key` | 密钥错、被停用，或没放进 `Authorization: Bearer`。到管理台看该 Key 的状态 |
| 401 | `token_expired` | **令牌过期**（短时令牌到期）。凭据本身没错，换一个新令牌即可 —— 不要按「服务故障」重试 |
| 400 | `model_required` / `messages_required` | 请求体问题：缺 `model` 或缺 `messages` |
| 400 | `model_not_allowed` | 模型名写错，或该模型没授权给这个 Key（不在所属分组的渠道下 / 未定价 / 未启用）。用 `/v1/models` 校对名字 |
| 404 | `not_found` | Base URL 少了 `/v1`，或用了未实现的端点 |
| 429 | `rate_limit_exceeded` | 全局或该 Key 的并发到顶 |
| 429 | `rpm_exceeded` | 超过 `RPM_LIMIT`（滚动一分钟） |
| 429 | `token_quota_exceeded` / `amount_quota_exceeded` | 管理端的 5 小时 / 当日 / 本周额度用满。**这是欠费，不是服务坏了**，不要重试；等窗口轮换或在管理台调大限额后自动恢复 |
| 429 | `key_token_quota_exceeded` / `key_amount_quota_exceeded` | 该 Key 自己的总额度用满，同上 |
| 502 | `upstream_*` | 上游拒绝或不可达（余额、限流、地址、上游密钥失效）。用分组列表的「检测」看是哪个渠道挂了 |
| 503 | `settlement_unavailable` | 结算存储暂时不可用；服务端会保留请求诊断并进行有限重试，客户端可按 `request_id` 联系运维 |
| 503 | `memory_limit_exceeded` | 服务端 RSS 超过 `MEMORY_LIMIT_MB` 的自我保护，稍后重试 |

额度与限流类错误一律 **429** 且 `message` 里带窗口与具体数字，客户端应把它当作「明确拒绝」而不是「服务端故障」——按服务端故障重试只会白烧额度。

## 五、关于密钥安全

这是把客户端交给别人用时最该说清的一件事：

- **上游渠道的 API Key 只存在于服务端数据库**，用 AES-256-GCM 加密保存；所有管理接口只返回「是否已配置」，从不返回明文。
- **客户端拿到的 `crk_...` 是与上游完全无关的独立密钥**，数据库里只存它的 HMAC 摘要——连管理员也无法从库里还原明文。
- 因此**泄露客户端密钥 ≠ 泄露上游密钥**：最坏情况是别人能通过这个 Key 消耗你给它的额度，而无法拿到渠道凭据、也无法绕过分组去看别的模型。
- 出事时的处置：管理台对该 Key 点「重置密钥」（旧密钥立即失效）或直接删除。两者都只影响这一个客户端。
- 请求记录只保存元数据（模型、用量、状态、耗时），**不保存 prompt、回复内容和任何密钥**。

## 六、应用型凭据：客户端里没有密钥

前面两种方式，客户端都持有某个凭据。如果你连**凭据本身**都不想放进客户端，用**应用型凭据**：

- 创建客户端凭据时把「接入方式」选成**应用标识**（接口 `keyless: true`）。
- 服务端**不产生任何可用密钥**：库里那条记录的密钥摘要是创建时即丢弃的随机值，它的明文不存在于任何地方（不是"加密保存"，是**从来不存在**）。
- 客户端只需要一个公开的**应用标识**，放在请求头里：

```bash
curl http://<relay>:8000/v1/chat/completions \
  -H "X-App-Id: app_xxxxxxxxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo/mimo-v2.5","messages":[{"role":"user","content":"你好"}]}'
```

**用户拿到什么也没用。** 应用标识不是密钥：扒开前端、反编译客户端，最多拿到这个公开值，而它没有对应的密钥可以"带出去"填进别的软件。这正是"防止用户把 key 拿到应用外去用"的做法——因为不存在可拿的 key。

### 代价必须说清楚

**应用标识是公开的，所以能访问到这个网关的人都能用它调用。** 这种模式下：

- 真正的门是「**谁能访问到网关**」：请让网关只在可信网络内可达（内网 / 防火墙 / 反向代理鉴权 / VPN），不要直接裸奔在公网。
- 给这个应用设好**并发与额度上限**（`max_concurrency`、`token_limit_total`、`money_limit_total`）：万一标识被转发出去，损失有界。
- 如果必须跨不可信网络、又要求"客户端零凭据"，正确做法是让客户端连**你自己的中转服务**，由中转持有网关凭据 —— 那时网关凭据在你自己的服务器上，客户端侧依然什么都没有。

### 同时给出两种凭据时

**先按密钥认证，密钥无效再回落到应用标识。** 这样兼容那些强制发送 `Authorization`（哪怕里填的是占位符）的 OpenAI 兼容客户端，不会因为多带了一个无效密钥就把合法的应用标识一起废掉。

### 其他性质

- 停用（`enabled: false`）对两条路立即生效；用量、额度、并发、有效期机制与密钥型完全一致。
- 也支持短时令牌：给应用标识设有效期，到点自动失效（见下一节）。
- 管理台列表里这类记录显示为「应用标识」，不会让人去找一个并不存在的密钥。

## 七、短时令牌：给程序 / 客户端发会过期的凭据

前文示例用的都是「永久有效」的客户端 Key。**当凭据要交给第三方程序、外包或别人的机器时，
应该改用短时令牌**：创建 Key 时填一个有效期，到点自动失效，不依赖任何人记得去吊销。

为什么这比「永久 Key + 出事再吊销」好：

- **泄露窗口有限**：令牌只在有效期内可用，过期即作废。永久 Key 一旦泄露，除非有人发现并手动吊销，否则一直有效。
- **不依赖人工兜底**：忘记吊销、管理员休假、交接遗漏 —— 这些都不再是风险点。
- **过期是机器可读的**：返回 401 `token_expired`（而不是含混的 `invalid_api_key`），
  客户端能明确提示「凭据过期，请换新的」，而不是把「凭据错」和「服务坏」混在一起反复重试。

```bash
# 创建 24 小时后失效的令牌（返回的 token 只显示这一次）
curl -X POST http://<relay>:8000/admin/api/keys \
  -H "X-Admin-Key: <管理员密钥>" -H "Content-Type: application/json" \
  -d '{"name":"外包A","max_concurrency":2,"group_ids":[2],"ttl_seconds":86400}'
```

- 到期后管理台的 Key 列表把它标成**已过期**（红色），「接入信息」里也写明客户端会收到 `token_expired`。
- **续期**：编辑该 Key，在「令牌有效期」填新的小时数（从现在起重新计时）；填 `0` = 改为永久有效；
  **留空 = 不动**（所以只改名字不会影响有效期）。接口层对应 `PUT /admin/api/keys/{id}` 的 `ttl_seconds`。
- **过期 ≠ 停用**：过期的 Key 仍是「启用」状态，续期后立即恢复；「停用」是人工动作，续期救不回来。
- 到期**不会**删除 Key，历史账单与用量都保留，便于回溯「这段时间用了多少」。

时长按「泄露后你能承受多久」来选：给外部程序的常见档位是 **24 小时 ~ 30 天**。

> 一个必须提前想清楚的点：**到期意味着客户端会中断**。所以短令牌必须配一条续期流程
> （谁、在什么时间点换新的），否则省下的泄露风险会变成「半夜业务停了」。

## 八、反向代理（Nginx / CDN）必须改的四个设置

relay 的流式响应是 **SSE 长连接**，而 Nginx 的出厂默认值对它是错的。不改的话会出现两种
看起来毫不相关的症状，实际是同一个原因：

| 症状 | 原因 |
|---|---|
| 发出去没反应、一直转圈（像"请求没发出去"） | `proxy_buffering on`（默认）：流被 Nginx 攒在缓冲区里不下发 |
| 回答流到一半就断了 | `proxy_read_timeout 60s`（默认）：两次上游字节之间空闲超过 60 秒后断开 |
| 大请求（长上下文 / 带工具定义）报 413 | `client_max_body_size 1m`（默认）小于服务端的 8MB 上限 |

正确的 `server` 块片段：

```nginx
location /v1/ {
    proxy_pass http://127.0.0.1:8000;
    proxy_http_version 1.1;          # SSE 需要 1.1，否则可能被按 1.0 处理
    proxy_buffering off;             # ★ 必须关，否则流式输出看不见
    proxy_cache off;
    proxy_read_timeout 90s;          # 大于 relay 的 SSE 心跳间隔（默认 15s）
    proxy_send_timeout 3600s;
    proxy_set_header Connection "";  # 清掉 close，保持长连接
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;   # 接入信息据此推断对外 Base URL
    client_max_body_size 16m;
    chunked_transfer_encoding on;
}

# 管理台（普通请求，无需关缓冲）
location /admin/ {
    proxy_pass http://127.0.0.1:8000;
    proxy_set_header X-Admin-Key  $http_x_admin_key;
    client_max_body_size 16m;
}
```

**Cloudflare 等 CDN**：免费版对长连接有约 **100 秒**的硬超时，绕不过去。要用 CDN 就别让它代理流式接口
（给 `/v1/` 单独开一个不经 CDN 的子域，或客户端直连源站）。

验证是否真的配对了：从**客户端那台机器**执行

```bash
curl -N http://<你的域名>/v1/chat/completions \
  -H "Authorization: Bearer crk_..." -H "Content-Type: application/json" \
  -d '{"model":"...","messages":[{"role":"user","content":"从1数到20"}],"stream":true}'
```

- 等待模型输出时可逐条看到 `: relay-heartbeat`，随后逐条出现 `data: {...}`，而不是最后一口气全部出现 → 说明代理没有缓冲 SSE
- 完整跑到 `data: [DONE]` → Chat 流正常结束；仅看到连接关闭而没有 `[DONE]` 表示流中断

`proxy_read_timeout` 是相邻两次上游读取之间的空闲超时，不是整个请求的最长时长。若把 relay 心跳设为 `0`，必须自行把该超时调到大于上游可能持续静默的时间；Nginx 超时不能代替客户端判断终态。

## 九、网络与部署注意

- 服务端默认绑定 `0.0.0.0:8000`（容器映射由 `CIVIC_RELAY_BIND` / `CIVIC_RELAY_PORT` 控制）。
- **Windows 上还要放行防火墙入站 8000**，否则其他设备连不上（本机 `127.0.0.1` 却正常）：
  ```powershell
  New-NetFirewallRule -DisplayName "Civic Relay 8000" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8000
  ```
- 判定「是防火墙还是没起来」：本机 `curl 127.0.0.1:8000/v1/models` 通、其他设备连 `<本机IP>:8000` 超时 → 防火墙问题。
- 这是**明文 HTTP**：只在可信内网用。要给公网或不可信网络用，必须在前面挂 HTTPS 反向代理（客户端 Base URL 换成代理地址，其余不变）。
