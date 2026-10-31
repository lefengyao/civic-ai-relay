# 阶段3：智能体协议兼容实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. User may explicitly choose superpowers:subagent-driven-development instead. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Responses 的工具调用、工具结果和推理选项得到准确处理，既有 Chat 渠道继续可用，具备 Responses 能力的渠道可以原生转发。

**Architecture:** 供应商显式选择 bridge/native，两条路径共用准入、价格、结算、心跳和诊断。首版保持无状态完整上下文请求；不支持的能力在预留前明确拒绝。桥接只转换可表达的语义，原生保留未知事件。

**Tech Stack:** Go、net/http、encoding/json、SQLite、阶段2的 SSEReader/流会话、httptest。

---

依赖：[阶段1](2026-10-03-civic-relay-01-settlement.md)、[阶段2](2026-10-03-civic-relay-02-streaming.md)、[阶段4](2026-10-03-civic-relay-04-diagnostics.md)。本轮仅规划；所有进度框留空。执行前阅读[总计划](2026-10-03-civic-relay-hardening.md)。

协议依据：[Responses 迁移指南](https://developers.openai.com/api/docs/guides/migrate-to-responses)中的 Items/call_id 与无状态回传方式，以及[流式事件指南](https://developers.openai.com/api/docs/guides/streaming-responses)中的独立终态。实现时以官方当前事件字段为准，将使用的最小样本保存为测试夹具。

## Task 1：渠道模式与原生传输入口

**Files:**

- Modify: `internal/store/schema.go`、`internal/store/providers.go`、`internal/relay/providers.go`、`internal/httpapi/admin.go`、`web/admin.html`。
- Create: `internal/upstream/responses.go`、`internal/relay/responses.go`。
- Test: `internal/store/providers_test.go`、`internal/upstream/responses_test.go`、`internal/relay/responses_test.go`。

- [ ] **Step 1：写模式和传输回归。** `TestProviderResponsesModeMigration` 从 v13 升级，断言所有旧供应商为 bridge，密钥仍可解密。`TestNativeResponsesRequest` 用 httptest 上游断言路径 `/v1/responses`、模型名替换、store=false、工具和 reasoning Items 保留，Bearer 只含供应商密钥。`TestResponsesRejectStatefulFields` 覆盖 previous_response_id/conversation/background/store=true；无上游调用、无预留。
- [ ] **Step 2：确认当前代码缺失入口。** 运行 `go test -count=1 ./internal/store ./internal/upstream ./internal/relay -run 'Test(ProviderResponsesModeMigration|NativeResponsesRequest|ResponsesRejectStatefulFields)' -v`。预期先因缺少字段/接口失败，修复后同一命令 PASS。
- [ ] **Step 3：追加 v14，保留旧上游接口。** 供应商 CRUD 校验只接受 bridge/native；UI 使用中文“兼容转换 / 原生 Responses”，默认兼容转换。SQLite 与扩展接口为：

```sql
ALTER TABLE providers ADD COLUMN responses_mode TEXT NOT NULL DEFAULT 'bridge'
    CHECK (responses_mode IN ('bridge', 'native'));
```

```go
// internal/relay/responses.go; optional extension to existing UpstreamClient
type ResponsesClient interface {
    ResponsesJSON(context.Context, map[string]any) ([]byte, error)
    ResponsesStream(context.Context, map[string]any) (*http.Response, error)
}

// internal/httpapi/responses_bridge.go
func requireStateless(body map[string]any) error {
    for _, key := range []string{"previous_response_id", "conversation"} {
        if v, ok := body[key]; ok && v != nil {
            return fmt.Errorf("unsupported_capability: %s", key)
        }
    }
    for _, key := range []string{"background", "store"} {
        if v, ok := body[key]; ok && v != nil {
            flag, valid := v.(bool)
            if !valid { return fmt.Errorf("invalid_request: %s", key) }
            if flag { return fmt.Errorf("unsupported_capability: %s", key) }
        }
    }
    body["store"] = false
    return nil
}
```

先校验字段类型；background/store 非 bool/null 也返回 invalid_request。上述 helper 只接收已解码的新请求 map，不共享或修改缓存对象。状态字段错误对客户端为 400，并给出字段名，禁止静默删除。

- [ ] **Step 4：复用传输和生命周期。** 在 upstream.Client 复用现有 c.request 的鉴权、错误分类、Body.Close/超时逻辑，ResponsesStream 不能添加 Chat 的 stream_options。relay.Request 新增 `Protocol string`，只接受 chat/responses_bridge/responses_native；Service 根据它选择 Chat 或 Responses 扩展方法。使用现有 Begin/PreparedStream，不能另建不计费的 HTTP 代理路径。Registry 缓存淘汰也覆盖 responses_mode 修改。
- [ ] **Step 5：验证并保留检查点。** 运行上述命令及 `go test -count=1 ./internal/httpapi`。本地检查点建议名 `feat: add explicit native responses transport`；只提交本任务文件。

## Task 2：可核对的输入映射与能力拒绝

**Files:**

- Create: `internal/httpapi/responses_bridge.go`、`internal/httpapi/responses_input_test.go`。
- Modify: `internal/httpapi/responses.go`、`internal/relay/service.go`、`internal/store/ledger.go`（现有输入估算函数 estimateInputTokens）。
- Fixture: `internal/httpapi/testdata/responses/tool-roundtrip.json`。

- [ ] **Step 1：固定两轮输入夹具。** 第一轮包含 instructions、用户文本、一个函数工具、tool_choice、parallel_tool_calls 和 reasoning.effort。第二轮 input 同时包含第一轮用户消息、function_call 和 function_call_output；断言 call_id 不变化，参数 JSON 字符串不被二次编码。再覆盖两条并行调用、重复 call_id、未知结果引用、图片与 reasoning.encrypted_content。

```json
{
  "model": "agent-public",
  "store": false,
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "查一下虚构城市的天气"}]},
    {"type": "function_call", "call_id": "call_weather_1", "name": "weather", "arguments": "{\"city\":\"测试城\"}"},
    {"type": "function_call_output", "call_id": "call_weather_1", "output": "晴，20度"}
  ],
  "tools": [{"type": "function", "name": "weather", "description": "返回测试天气", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}, "strict": true}],
  "reasoning": {"effort": "medium"}
}
```

- [ ] **Step 2：运行失败用例。** `go test -count=1 ./internal/httpapi -run 'TestResponses(Input|Capability|Tools)' -v`。当前应在工具结果被过滤、reasoning 丢失等断言处失败。
- [ ] **Step 3：抽取返回错误的映射器。** 定义 `BridgeRequest(body map[string]any) (map[string]any, error)`，替代 responsesInputToMessages 的无错误过滤。函数定义转换的完整规则：

```go
func bridgeFunctionTool(tool map[string]any) (map[string]any, error) {
    if tool["type"] != "function" {
        return nil, errors.New("unsupported_capability: tools.type")
    }
    name, ok := tool["name"].(string)
    if !ok || strings.TrimSpace(name) == "" {
        return nil, errors.New("invalid_request: tools.name")
    }
    function := map[string]any{"name": name}
    for _, key := range []string{"description", "parameters", "strict"} {
        if value, exists := tool[key]; exists {
            function[key] = value
        }
    }
    return map[string]any{"type": "function", "function": function}, nil
}
```

字符串 input 映射 user；instructions 映射首条 system。message 只接受受支持的文本内容块和合法 role。连续 function_call 合为一条 assistant.tool_calls，`id=call_id`；function_call_output 映射 `role=tool, tool_call_id=call_id, content=output`。检查重复/未知引用，并保持用户输入顺序。output 可以是字符串；非字符串工具输出首版明确拒绝，不能 fmt.Sprint 生成无效文本。

tool_choice 的 auto/none/required 原样映射，命名 function choice 转换到 Chat 嵌套 function.name；parallel_tool_calls 保留 bool。reasoning 仅接受 effort 并映射 reasoning_effort；summary/encrypted_content、图片、内建工具和不能表达的字段返回 unsupported_capability。定义 bridge 支持字段白名单，遇到未支持的非空字段报告字段名，不能沿用现有“只取几个字段、其他全丢”的做法。native 仅限制上述有状态字段及网关安全限制，不套 bridge 白名单。

- [ ] **Step 4：把估算与出站预算接上。** 准入估算包含 messages、tool definitions 和 tool call arguments，不把工具历史算成空输入；native 对 Items 与 tools 估算，未知内容标记 estimated。实际出站 max_tokens/max_output_tokens 必须采用准入批准的输出上限，避免只限制预留却放行无限输出。不在 DB/日志保存输入。运行上述命令及阶段1预留/价格测试，预期 PASS。
- [ ] **Step 5：保留检查点。** 建议 `feat: bridge responses tools and reasoning without silent loss`。

## Task 3：输出 Items、工具参数分片与真实终态

**Files:**

- Create: `internal/httpapi/responses_output.go`、`internal/httpapi/responses_output_test.go`、`internal/upstream/responses_usage_test.go`。
- Modify: `internal/httpapi/responses.go`、`internal/httpapi/stream_session.go`、`internal/upstream/responses.go`。

- [ ] **Step 1：写流和非流回归。** 用两个交错的 tool_calls.index 分片返回 ID/name/arguments，断言 output_item.added、function_call_arguments.delta/done、output_item.done 和最终 output 的 ID/参数一致；文本与函数调用同时出现时顺序可核对。坏 JSON、无终态 EOF、上游 failed/incomplete、length、下游写失败分别断言不会出现 response.completed。native 注释/多行/未知事件按完整帧保留。
- [ ] **Step 2：确认当前只输出文本。** `go test -count=1 ./internal/httpapi ./internal/upstream -run 'TestResponses(Output|Stream|Terminal|Usage)' -v`。预期当前工具输出缺失；新增完成后 PASS。
- [ ] **Step 3：实现有界工具聚合和事件写入。** 定义以下类型放在 responses_output.go，所有写操作仍由阶段2单写循环完成：

```go
type BridgeCall struct {
    ItemID string
    CallID string
    Name string
    Arguments strings.Builder
    OutputIndex int
    Added bool
}

func responseFunctionItem(call *BridgeCall, status string) map[string]any {
    return map[string]any{
        "id": call.ItemID, "type": "function_call", "status": status,
        "call_id": call.CallID, "name": call.Name,
        "arguments": call.Arguments.String(),
    }
}
```

按 Chat choice=0/tool_calls.index 聚合，item ID 由网关生成一次，call_id 取上游工具 ID；若 ID 分片，完整形成后才生成 added。每条调用参数与文本累计受现有 max_body/memory guard 控制，并设单请求最多128个工具调用；超过限制取消、失败结算。必须有有效 name 和 call_id，不可在未知引用时补随机 ID。响应 n 首版只接受未传或1。

reasoning_content 仅作为 reasoning summary_text Item 与对应摘要事件映射，并在能力说明中标明为兼容摘要；不伪造 encrypted_content。只有当该内容实际存在才创建 Item。非流和流输出复用同一 Item 构造逻辑，避免一条路径丢工具。

- [ ] **Step 4：按协议认定终态并归一用量。** Chat bridge 必须读到 DONE；finish_reason=length 生成 response.incomplete/max_output_tokens，其余正常结束生成 completed；DONE前坏流生成 failed。native 以 completed/failed/incomplete 事件为终态；终态后取消读取，不等待连接永久关闭。所有 failed/incomplete 只结算一次。原生 usage 的 input_tokens/output_tokens/input_tokens_details.cached_tokens 归一到账本；reasoning_tokens 属于 output_tokens 的细分，不重复加计；cached_tokens 限制到 [0,input_tokens]。用量缺失仍按阶段2 estimated/unknown 规则。改回 response 对象与已知事件中公开 model 名，不改 ID/call_id；未知事件原样保留，不把 provider body 打日志。
- [ ] **Step 5：验证并保留检查点。** 上述命令、`go test -count=1 ./internal/httpapi ./internal/relay` 均 PASS。建议 `feat: preserve responses items and terminal events`。

## Task 4：客户端闭环与发布验收

**Files:**

- Create: `internal/httpapi/responses_roundtrip_test.go`、`scripts/test_responses_integration.py`。
- Modify: `README.md`、`docs/api-compatibility.md`（现有 docs 被忽略，执行时确认需交付的说明进入发布文档或 README）。

- [ ] **Step 1：用真实 HTTP 请求完成两轮。** Go httptest 启动完整 PublicHandler/临时 Store/虚构上游，分别测试 bridge/native × stream/nonstream。客户端第一轮读取 weather 调用，在本地执行固定返回函数，第二轮完整回传上下文和结果，断言最终文本、call_id、两次独立结算、同一公开报价。加入 Key/App-ID、两把 Key 相同模型、原生拒绝跨响应 ID 续接的用例。
- [ ] **Step 2：补独立进程验收。** Python unittest 启动构建后的本地二进制及假上游，使用标准库 http.client 逐事件读取，确保 HTTP 层没有缓冲到终态；端口为临时端口，不读取生产配置或调用真实模型。`python -m unittest scripts.test_responses_integration -v` 预期四个模式闭环 PASS，并确认 stderr 无提示词、工具参数和密钥。
- [ ] **Step 3：执行阶段门禁。** `go test -count=1 ./...`、`go vet ./...`、上述 Python 命令。Linux 执行 `go test -race -count=1 ./...`。管理端检查保存模式、重载、旧渠道默认值。兼容说明逐项列出 bridge/native、stateless、支持参数、失败终态，不能声称所有 Responses 能力全兼容。
- [ ] **Step 4：交付检查点。** 建议 `test: validate responses tool roundtrips`。在总计划记录实际命令结果和缺失的执行环境；通过后再进入阶段5。

## 阶段验收

- [ ] 不再静默丢 tools、reasoning、function_call_output。
- [ ] bridge/native 的四条两轮闭环通过，call_id 不被重新生成。
- [ ] 无终态/非法流无伪成功；原生未知帧保留。
- [ ] store=false 与有状态字段拒绝有测试；没有聊天内容持久化。
- [ ] 新模式从 v13 迁移后保持旧配置、密钥、Key/App-ID 与定价行为。
