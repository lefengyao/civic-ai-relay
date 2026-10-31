# civic-ai-relay 对接 OpenCode Go 修复清单

> 结论：**不是模型、参数或密钥的问题，是缺 HTTP 请求头 `x-opencode-session`。**
> OpenCode Go 网关对每个推理请求强制校验该头（2026-09-05 起），缺失即返回
> `400 {"type":"MissingSessionID"}`，与 API Key 是否有效无关。
>
> 影响仓库：`E:\生产项目\项目\TrailblazerStation\civic-ai-relay`
> 参考实现：DSH 侧 `pi-ai/dist/providers/opencode-headers.js`（见第 3 节）

---

## 1. 实测证据（2026-10-28，假 key 探测，只看错误类型）

请求体统一为：

```json
{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":false}
```

| # | 请求头 | 响应 |
|---|---|---|
| A | 仅 `Authorization` | **400** `{"type":"error","error":{"type":"MissingSessionID","message":"Request is missing x-opencode-session and cannot be routed efficiently."}}` |
| B | `Authorization` + `x-opencode-session: <uuid>` + 自定义 UA | **401** `{"error":{"type":"server_error","message":"Upstream request failed: Invalid credential"}}` |
| C | B 但 UA 改回 `Go-http-client/1.1` | **401**（UA 不硬拦，只是合规项） |
| D | B 再加 `store:false`、`max_completion_tokens`、`stream_options` | **401**（鉴权先于参数校验，参数问题无法在此阶段验证） |

A→B 的差别就是唯一的根因：**带头之后请求才进到验 key 阶段**。

复现命令：

```bash
# 400 MissingSessionID
curl -s -i -X POST https://opencode.ai/inference/go/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-dummy-invalid" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}'

# 401（说明头生效了）
curl -s -i -X POST https://opencode.ai/inference/go/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-dummy-invalid" \
  -H "x-opencode-session: 11111111-2222-3333-4444-555555555555" \
  -H "User-Agent: civic-ai-relay/0.3.8" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}'
```

opencode 官方文档《Go → Where can I use it?》明确要求客户端：

1. 发典型 coding agent 流量；
2. **用自己的 user-agent 自报身份**，不要用通用 SDK / HTTP 库名；
3. **每条会话在 `x-opencode-session` 里带一个稳定会话 ID**（用于路由与 prompt cache）。
   文档 "Known problematic clients" 里点名 DeepSeek Harness 就是"部分模型路径没带 session"。

---

## 2. 为什么 DSH 直连能通、走本网关不通

DSH 的 opencode 适配逻辑全在 pi-ai 包里，共 4 处特殊处理：

| 位置（pi-ai 1.3.0） | 行为 | 走自研网关后 |
|---|---|---|
| `dist/providers/opencode-headers.js` | `withOpenCodeSessionHeader()` 包装 opencode / opencode-go 的全部四种协议，发请求前注入 `x-opencode-session = options.sessionId` | **失效**：该包装按 provider id 挂载，路由名不是 `opencode*` 就不注入 |
| `dist/providers/data/opencode-go.json` | baseUrl `https://opencode.ai/inference/go`（anthropic 类）/ `.../go/v1`（openai、responses、google 类） | 无影响（网关自行拼路径） |
| `dist/api/openai-completions.js` `getCompat()` | `provider==="opencode" \|\| baseUrl.includes("opencode.ai")` → 判为非标准端点：`supportsStore:false`（不发 `store`）、`supportsDeveloperRole:false`、`maxTokensField:"max_tokens"` | **失效**：baseUrl 变成网关地址，退化成"标准 OpenAI"，改发 `store:false` + `max_completion_tokens` |
| 同上（thinking 回放） | `provider==="opencode-go"` 时把 thinking 签名 `reasoning` 改写为 `reasoning_content` | **失效**（多轮带思考历史的请求可能被上游判参数非法） |

而本网关的 `internal/upstream/client.go` 在 `request()` 里只做了：

```go
req.Header.Set("Content-Type", "application/json")
if c.apiKey != "" {
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
}
```

**入站头一个都不转发，自己也不造会话头** → opencode 收到的是 `Go-http-client/1.1` + 无 session → 400。

---

## 3. 需要更新的文件清单

| # | 文件 | 改什么 | 必须 |
|---|---|---|---|
| 1 | `internal/upstream/session.go`（新增） | 会话头名常量、ctx 透传、稳定 ID 派生、host 判定 | 必须 |
| 2 | `internal/upstream/options.go` | `Options` 新增 `UserAgent`、`UpstreamSessionHeader` | 必须 |
| 3 | `internal/upstream/client.go` | `Client` 保存 UA / 会话头配置；`request()` 里设置 UA + 注入会话头 | 必须 |
| 4 | `internal/httpapi/public.go` | `chatCompletions()` 入口把入站 `x-opencode-session` 放进 ctx | 必须 |
| 5 | `internal/httpapi/responses.go` | `responsesAPI()` 同上 | 必须 |
| 6 | `internal/httpapi/cors.go` | 默认 `Access-Control-Allow-Headers` 补 `X-Opencode-Session` | 建议 |
| 7 | `internal/upstream/session_test.go`（新增） | 派生稳定性 / 透传优先级 / host 判定单测 | 必须 |
| 8 | `internal/upstream/client_test.go` | 断言发往上游的请求带上两个头 | 必须 |
| 9 | `cmd/civic-relay/main.go` | `upstream.Options{...}` 里传入新配置（若走环境变量则自动，见下） | 视方案 |
| 10 | `internal/config/settings.go` | 可选：`UPSTREAM_USER_AGENT`、`UPSTREAM_SESSION_HEADER` 热配置 | 可选 |
| 11 | `README.md` / `docs/` | 记录"opencode 类上游需要会话头"这一约束 | 建议 |

> `dist/civic-relay-v0.3.8/` 是历史构建快照，**不要手工改**，重新构建会生成新快照。

---

## 4. 具体改动

### 4.1 新增 `internal/upstream/session.go`

```go
package upstream

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// SessionIDHeader 是 OpenCode Go 网关要求的会话路由头。
// 缺失时上游直接 400 MissingSessionID（与 API Key 无关）。
const SessionIDHeader = "x-opencode-session"

// DefaultUserAgent 自报身份。opencode 明确要求不要用通用 SDK/HTTP 库名，
// 因此绝不能留空让 Go 填 "Go-http-client/1.1"。
const DefaultUserAgent = "civic-ai-relay/1.0"

type sessionKey struct{}

// WithSessionID 把下游带来的会话标识放进 ctx，供上游转发时透传。
// 下游没带时不要调用，交给 deriveSessionID 兜底。
func WithSessionID(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionIDFromContext 读出下游透传的会话标识。
func SessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionKey{}).(string)
	return strings.TrimSpace(v)
}

// isOpenCodeEndpoint 只在真正指向 opencode.ai 的主机上启用（含子域）。
func isOpenCodeEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

// deriveSessionID 给无法提供会话标识的客户端派生一个稳定 ID：
// 同一会话的首条 user 消息在每一轮请求里都不变，因此哈希稳定；
// 不同会话几乎不会碰撞。输出按 UUID v5 形状，避免上游对格式挑剔。
//
// 已知退化：DSH 做上下文压缩后首条 user 消息会变，该会话的缓存亲和会冷一次。
// 想彻底避免就让客户端送 x-opencode-session（见第 6 节）。
func deriveSessionID(apiKey string, payload any) string {
	seed := apiKey + "\x00" + sessionFingerprint(payload)
	sum := sha1.Sum([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(sum[0:4]),
		hex.EncodeToString(sum[4:6]),
		hex.EncodeToString(sum[6:8]),
		hex.EncodeToString(sum[8:10]),
		hex.EncodeToString(sum[10:16]),
	)
}

// sessionFingerprint 取「model + 首条 user 消息正文」作为会话指纹。
func sessionFingerprint(payload any) string {
	body, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	model, _ := body["model"].(string)
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		if text := contentText(msg["content"]); text != "" {
			return model + "\x00" + text
		}
	}
	return model
}

func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}
```

### 4.2 `internal/upstream/options.go`

```go
type Options struct {
	ConnectTimeout time.Duration
	HeaderTimeout  time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	PoolTimeout    time.Duration
	IdleTimeout    time.Duration

	// UserAgent 留空时用 DefaultUserAgent；绝不留给 http.Client 默认值。
	UserAgent string
	// UpstreamSessionHeader 留空表示按 host 自动判定（opencode.ai → x-opencode-session）；
	// 显式设置可覆盖镜像/反代场景，设为 "-" 表示强制关闭。
	UpstreamSessionHeader string
}
```

### 4.3 `internal/upstream/client.go`

结构体加两个字段：

```go
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	options Options
	userAgent     string
	sessionHeader string // "" 表示该上游不需要会话头
}
```

`NewWithOptions` 结尾处（构造返回值的地方）：

```go
userAgent := strings.TrimSpace(options.UserAgent)
if userAgent == "" {
	userAgent = DefaultUserAgent
}
sessionHeader := strings.TrimSpace(options.UpstreamSessionHeader)
switch {
case sessionHeader == "-":
	sessionHeader = ""
case sessionHeader == "" && isOpenCodeEndpoint(baseURL):
	sessionHeader = SessionIDHeader
}
return &Client{
	baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	apiKey:  strings.TrimSpace(apiKey),
	options: options,
	http:    &http.Client{Transport: transport},

	userAgent:     userAgent,
	sessionHeader: sessionHeader,
}
```

`request()` 里紧跟 `Content-Type` 之后插入：

```go
req.Header.Set("Content-Type", "application/json")
req.Header.Set("User-Agent", c.userAgent) // 不能空：opencode 拒绝通用库名
if c.sessionHeader != "" {
	session := SessionIDFromContext(ctx)
	if session == "" {
		session = deriveSessionID(c.apiKey, payload) // GET /models 时 payload 为 nil，退化为按 key 稳定
	}
	req.Header.Set(c.sessionHeader, session)
}
if c.apiKey != "" {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}
```

> 只在 `request()` 一处改，`ChatJSON` / `Stream` / `ListModels` 自动覆盖（三条都走它）。

### 4.4 `internal/httpapi/public.go`

在 `chatCompletions()` 里、`json.Decode` 之后尽早透传（`stream` 分支也用 `r.Context()`，所以放前面即可）：

```go
if sid := strings.TrimSpace(r.Header.Get(upstream.SessionIDHeader)); sid != "" {
	r = r.WithContext(upstream.WithSessionID(r.Context(), sid))
}
```

`internal/httpapi/responses.go` 的 `responsesAPI()` 同样位置加同一段（responses 会被转换成 chat/completions 再发上游，因此同样需要）。

### 4.5 `internal/httpapi/cors.go`

默认列表补上自定义头（浏览器直连场景才会用到）：

```go
w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Admin-Key, X-App-Id, X-Opencode-Session")
```

### 4.6 单测（`internal/upstream/session_test.go` 新建）

必须覆盖 4 条：

1. `deriveSessionID` 对同一 payload 稳定、对不同首条消息不同、输出是 36 字符 UUID 形状；
2. 显式 `UpstreamSessionHeader` 时 httptest 上游能收到该头（host 不是 opencode.ai 也能测）；
3. ctx 里已有会话 ID 时**优先用透传值**，不再派生；
4. `isOpenCodeEndpoint`：`opencode.ai` / `x.opencode.ai` 为真；`opencode.ai.evil.com`、`notopencode.ai`、`http://127.0.0.1` 为假。

`client_test.go` 增一例：断言发往 httptest 的请求同时带 `User-Agent: civic-ai-relay/...` 与 `x-opencode-session`。

---

## 5. 配置项（可选，但建议做成热配置）

`internal/config/settings.go` 加两个可热更新字段，写进 `values` / `apply` / diff 三段：

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `UPSTREAM_USER_AGENT` | `civic-ai-relay/<version>` | 自报身份；空值回落到默认，**不允许**落成 `Go-http-client/1.1` |
| `UPSTREAM_SESSION_HEADER` | 空（自动按 host 判定） | 显式指定头名（镜像/反代/其它同类网关）；`-` 表示关闭 |

不改 settings 也行：`cmd/civic-relay/main.go` 里 `upstream.Options{...}` 直接写死 `UserAgent` 即可，只是需要重启生效。

---

## 6. 客户端侧（DSH）建议配置

网关改完后，400 会消失；但 DSH 走自研路由时，pi-ai 会按"标准 OpenAI"处理，可能撞第二道坎。建议在 DSH 的这条路由上显式还原 opencode 的兼容语义：

```yaml
providers:
  civic:
    api: openai-completions
    baseURL: http://<gateway-host>:<port>/v1
    apiKeyEnv: CIVIC_RELAY_KEY
    compat:
      supportsStore: false          # 不再发 store
      supportsDeveloperRole: false  # 用 system 而不是 developer
      maxTokensField: max_tokens    # 与 opencode 目录默认一致
    models:
      - id: deepseek-v4.1-flash
```

两种"真会话 ID"方案（二选一，可选）：

- **A（推荐，零客户端改动）**：用第 4.1 节的派生逻辑，网关自己保证"每会话一个稳定 ID"。代价：会话被上下文压缩后亲和会冷一次。
- **B（最贴近官方行为）**：客户端提供 `x-opencode-session`，网关只负责透传。DSH 侧可装一个按 host/provider 注入会话头的插件（社区有 `dsh-my-opencode-session-header` 一类），或自写插件读取 harness 的 `sessionId` 注入。网关侧第 4.4 节已经支持透传。

拿到外部会话 ID 后，日志里建议记 `session_id_hash`（sha1 前 8 位）而不是原值，便于排障又不泄露。

---

## 7. 上线与验证

```bash
# 1. 单测 + 构建
go test ./internal/upstream/... ./internal/httpapi/...
go build ./...

# 2. 重建镜像并起服务（按现有 docker-compose 流程）
docker compose up -d --build

# 3. 端到端：通过网关打一次 opencode 上游
curl -s -i -X POST http://<gateway>/v1/chat/completions \
  -H "Authorization: Bearer <civic-relay 的 token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"<公开模型名>","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

验收标准：

| 检查点 | 期望 |
|---|---|
| 网关响应 | 200（或上游业务错误），**不再是 400** |
| 上游错误体 | 若仍是 400，必须是除 `MissingSessionID` 之外的其它 type |
| 数据库 `requests` 表 | `upstream_status` 不再是 400；`error_code` 不再是 `upstream_request_rejected` |
| 流式 | SSE 首帧正常，`stream_options.include_usage` 照旧 |
| 日志 | 能看到 `user_agent`、`session_id_hash` 便于复现 |

排查辅助：临时把上游 baseURL 指到一个本地 httptest/echo 服务，直接看网关实际发出的头，比读代码快。

---

## 8. 风险与边界

1. **别用固定常量做会话 ID**。所有会话共用一个 ID 会毁掉官方按会话的路由与 prompt cache，官方文档已明确警告；派生值优于常量。
2. **派生 ID 依赖"首条 user 消息不变"**。DSH 压缩上下文后首条消息会变 → 该会话缓存冷一次，功能不受影响。
3. **UA 不设会被记成 `Go-http-client/1.1`**。目前不硬拦（实测 401 而非 400），但属于官方明确要求的合规项，且流量滥用监测会看这个。
4. **本网关只有 `/v1/chat/completions` 与 `/v1/responses`，没有 `/v1/messages`**。opencode 文档称所有文本模型可通过其它协议翻译访问，所以 anthropic 原生模型（如 `claude-haiku-5-5`、`qwen3.8-*`、`minimax-m3`）走 chat/completions 理论可行，但属**需实测**项；若失败，那是第二个独立问题，不要和 400 混在一起。
5. **DeepSeek V4.1 Flash / V4 Pro 的地区限制**：opencode 控制台需把 Regions 设为 Global，否则会出现非本清单范围内的拒绝。
6. **不要改 `dist/` 下的历史快照**，也不要为了省事在 provider 里塞一个静态 `x-opencode-session` 配置——那是临时 workaround，会把所有会话压成一个。

---

## 9. Phase 2（可选，不进本次范围）

若要长期支持"每个上游都要自定义头"，可在 `providers` 表加 `extra_headers TEXT`（JSON 对象，密文无关），管理台加编辑框，`forProvider` 透传给 `Client`。这属于能力扩展，不是本次 400 的必要条件。
