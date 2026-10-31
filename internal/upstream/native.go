package upstream

// Native protocol adapters. They translate provider-native requests and
// responses to Civic's internal OpenAI-compatible contract. This keeps quota,
// logging, and the public API independent from vendor wire formats.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"civic-ai-relay/internal/requestmeta"
)

func (c *Client) requestNative(ctx context.Context, method, endpoint string, headers http.Header, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{Code: "upstream_request_invalid"}
		}
		body = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		cancel()
		return nil, &Error{Code: "upstream_connection_failed", Stage: "connect", Cause: err}
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return nil, &Error{Code: "client_canceled", Stage: "request", Cause: err}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &Error{Code: "request_duration_exceeded", Stage: "duration", Cause: err}
		}
		return nil, &Error{Code: "upstream_connection_failed", Stage: "connect", Cause: err}
	}
	if trace := requestmeta.FromContext(ctx); trace != nil {
		trace.MarkHeader()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cancel()
		_ = resp.Body.Close()
		return nil, nativeHTTPError(resp.StatusCode)
	}
	resp.Body = newTimedBody(requestCtx, cancel, resp.Body, resp.StatusCode, c.options.IdleTimeout)
	return resp, nil
}

func nativeHTTPError(status int) *Error {
	code := "upstream_http_error"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		code = "upstream_authentication_failed"
	case http.StatusTooManyRequests:
		code = "upstream_rate_limited"
	case 400, 404, 422:
		code = "upstream_request_rejected"
	case 500, 502, 503, 504:
		code = "upstream_unavailable"
	}
	return &Error{Code: code, Status: status, Stage: "header"}
}

func (c *Client) nativeEndpoint(path string) string {
	base := strings.TrimRight(c.baseURL, "/")
	for _, prefix := range []string{"/v1beta", "/v1"} {
		if strings.HasSuffix(base, prefix) && strings.HasPrefix(path, prefix+"/") {
			return base + strings.TrimPrefix(path, prefix)
		}
	}
	return base + path
}

func (c *Client) anthropicHeaders() http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("x-api-key", c.apiKey)
	h.Set("anthropic-version", "2023-06-01")
	return h
}

func (c *Client) geminiHeaders() http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("x-goog-api-key", c.apiKey)
	return h
}

func (c *Client) chatNative(ctx context.Context, payload map[string]any) ([]byte, error) {
	switch c.protocol {
	case "anthropic":
		body := openAIToAnthropic(payload)
		endpoint := c.nativeEndpoint("/v1/messages")
		if strings.HasSuffix(strings.TrimRight(c.baseURL, "/"), "/messages") {
			endpoint = strings.TrimRight(c.baseURL, "/")
		}
		resp, err := c.requestNative(ctx, http.MethodPost, endpoint, c.anthropicHeaders(), body)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, responseReadError(ctx, err, resp.StatusCode)
		}
		if err := nativeResponseError(data); err != nil {
			return nil, err
		}
		return anthropicToOpenAI(data), nil
	case "gemini":
		model, _ := payload["model"].(string)
		endpoint := c.nativeEndpoint("/v1beta/models/" + urlPathEscape(model) + ":generateContent")
		resp, err := c.requestNative(ctx, http.MethodPost, endpoint, c.geminiHeaders(), openAIToGemini(payload))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, responseReadError(ctx, err, resp.StatusCode)
		}
		if err := nativeResponseError(data); err != nil {
			return nil, err
		}
		return geminiToOpenAI(data), nil
	default:
		return nil, &Error{Code: "unsupported_upstream_protocol"}
	}
}

func (c *Client) listModelsNative(ctx context.Context) ([]byte, error) {
	var endpoint string
	var headers http.Header
	switch c.protocol {
	case "anthropic":
		endpoint, headers = c.nativeEndpoint("/v1/models?limit=100"), c.anthropicHeaders()
	case "gemini":
		endpoint, headers = c.nativeEndpoint("/v1beta/models?pageSize=200"), c.geminiHeaders()
	default:
		return c.ListModels(ctx)
	}
	resp, err := c.requestNative(ctx, http.MethodGet, endpoint, headers, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, responseReadError(ctx, err, resp.StatusCode)
	}
	switch c.protocol {
	case "anthropic":
		return normalizeAnthropicModels(data), nil
	case "gemini":
		return normalizeGeminiModels(data), nil
	default:
		return data, nil
	}
}

func (c *Client) streamNative(ctx context.Context, payload map[string]any) (*http.Response, error) {
	switch c.protocol {
	case "anthropic":
		body := openAIToAnthropic(payload)
		body["stream"] = true
		endpoint := c.nativeEndpoint("/v1/messages")
		if strings.HasSuffix(strings.TrimRight(c.baseURL, "/"), "/messages") {
			endpoint = strings.TrimRight(c.baseURL, "/")
		}
		resp, err := c.requestNative(ctx, http.MethodPost, endpoint, c.anthropicHeaders(), body)
		if err != nil {
			return nil, err
		}
		resp.Body = &nativeSSEBody{source: NewSSEReader(resp.Body, 1<<20), close: resp.Body.Close, protocol: c.protocol}
		return resp, nil
	case "gemini":
		model, _ := payload["model"].(string)
		endpoint := c.nativeEndpoint("/v1beta/models/" + urlPathEscape(model) + ":streamGenerateContent?alt=sse")
		resp, err := c.requestNative(ctx, http.MethodPost, endpoint, c.geminiHeaders(), openAIToGemini(payload))
		if err != nil {
			return nil, err
		}
		resp.Body = &nativeSSEBody{source: NewSSEReader(resp.Body, 1<<20), close: resp.Body.Close, protocol: c.protocol}
		return resp, nil
	default:
		return nil, &Error{Code: "unsupported_upstream_protocol"}
	}
}

type nativeSSEBody struct {
	source           *SSEReader
	close            func() error
	protocol         string
	pending          []byte
	done             bool
	inputTokens      int
	completionTokens int
	cachedTokens     int
	cacheWriteTokens int
	toolIndices      map[int]nativeTool
	geminiTools      map[string]nativeTool
	nextGeminiTool   int
	terminalErr      error
}

type nativeTool struct {
	ID    string
	Name  string
	Index int
}

func (b *nativeSSEBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 && !b.done {
		if b.terminalErr != nil {
			return 0, b.terminalErr
		}
		frame, err := b.source.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if b.terminalErr != nil {
					return 0, b.terminalErr
				}
				b.pending = []byte("data: [DONE]\n\n")
				b.done = true
				break
			}
			return 0, err
		}
		b.pending = b.translate(frame)
	}
	if len(b.pending) == 0 {
		if b.terminalErr != nil {
			return 0, b.terminalErr
		}
		return 0, io.EOF
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *nativeSSEBody) Close() error { return b.close() }

func (b *nativeSSEBody) translate(frame SSEFrame) []byte {
	if len(frame.Data) == 0 {
		return frame.Raw
	}
	switch b.protocol {
	case "anthropic":
		return b.translateAnthropic(frame.Data)
	case "gemini":
		var raw map[string]any
		if json.Unmarshal(frame.Data, &raw) == nil && raw["error"] != nil {
			b.terminalErr = nativeStreamError(raw["error"])
		}
		return b.translateGeminiFrame(frame.Data)
	default:
		return frame.Raw
	}
}

func (b *nativeSSEBody) translateAnthropic(data []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	typ, _ := raw["type"].(string)
	var out map[string]any
	switch typ {
	case "error":
		b.terminalErr = nativeStreamError(raw["error"])
		return nativeErrorEvent(raw["error"])
	case "message_start":
		message, _ := raw["message"].(map[string]any)
		usage, _ := message["usage"].(map[string]any)
		b.inputTokens = int(number(usage["input_tokens"]))
		b.cachedTokens = int(number(usage["cache_read_input_tokens"]))
		b.cacheWriteTokens = int(number(usage["cache_creation_input_tokens"]))
	case "content_block_start":
		block, _ := raw["content_block"].(map[string]any)
		if block["type"] == "tool_use" {
			index := int(number(raw["index"]))
			if b.toolIndices == nil {
				b.toolIndices = make(map[int]nativeTool)
			}
			tool := nativeTool{ID: stringValue(block["id"]), Name: stringValue(block["name"]), Index: len(b.toolIndices)}
			b.toolIndices[index] = tool
			out = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": ""}}}}}}}
		}
	case "content_block_delta":
		delta, _ := raw["delta"].(map[string]any)
		switch delta["type"] {
		case "text_delta":
			if text := stringValue(delta["text"]); text != "" {
				out = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}}
			}
		case "input_json_delta":
			index := int(number(raw["index"]))
			tool := b.toolIndices[index]
			out = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "id": tool.ID, "function": map[string]any{"arguments": stringValue(delta["partial_json"])}}}}}}}
		}
	case "message_delta":
		delta, _ := raw["delta"].(map[string]any)
		choice := map[string]any{"index": 0, "delta": map[string]any{}}
		if reason := stringValue(delta["stop_reason"]); reason != "" {
			choice["finish_reason"] = anthropicFinishReason(reason)
		}
		out = map[string]any{"choices": []any{choice}}
		if usage, ok := raw["usage"].(map[string]any); ok {
			completion := int(number(usage["output_tokens"]))
			out["usage"] = map[string]any{"prompt_tokens": b.inputTokens, "completion_tokens": completion, "total_tokens": b.inputTokens + completion, "prompt_tokens_details": map[string]any{"cached_tokens": b.cachedTokens}, "cache_creation_input_tokens": b.cacheWriteTokens}
		}
	case "message_stop":
		return []byte("data: [DONE]\n\n")
	}
	if out == nil {
		return nil
	}
	return openAIEvent(out)
}

func translateNativeFrame(frame SSEFrame, protocol string) []byte {
	if len(frame.Data) == 0 {
		return frame.Raw
	}
	if protocol == "anthropic" {
		return translateAnthropicFrame(frame.Data)
	}
	if protocol == "gemini" {
		return translateGeminiFrame(frame.Data)
	}
	return frame.Raw
}

func translateAnthropicFrame(data []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	typ, _ := raw["type"].(string)
	var out map[string]any
	switch typ {
	case "content_block_delta":
		delta, _ := raw["delta"].(map[string]any)
		if text, _ := delta["text"].(string); text != "" {
			out = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}}
		}
	case "message_delta":
		delta, _ := raw["delta"].(map[string]any)
		stop, _ := delta["stop_reason"].(string)
		out = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": stop}}}
		if usage, ok := raw["usage"].(map[string]any); ok {
			out["usage"] = map[string]any{"prompt_tokens": 0, "completion_tokens": usage["output_tokens"], "total_tokens": usage["output_tokens"]}
		}
	case "message_stop":
		return []byte("data: [DONE]\n\n")
	}
	if out == nil {
		return nil
	}
	encoded, _ := json.Marshal(out)
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}

func (b *nativeSSEBody) translateGeminiFrame(data []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	if raw["error"] != nil {
		return nativeErrorEvent(raw["error"])
	}
	candidate, _ := firstMap(raw["candidates"])
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	delta := map[string]any{}
	for partIndex, item := range parts {
		part, _ := item.(map[string]any)
		if text, _ := part["text"].(string); text != "" {
			delta["content"] = stringValue(delta["content"]) + text
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			args, _ := json.Marshal(call["args"])
			if b.geminiTools == nil {
				b.geminiTools = make(map[string]nativeTool)
			}
			key := fmt.Sprintf("%d:%d", int(number(candidate["index"])), partIndex)
			tool, seen := b.geminiTools[key]
			if !seen {
				tool = nativeTool{ID: fmt.Sprintf("call_%d", b.nextGeminiTool), Name: stringValue(call["name"]), Index: b.nextGeminiTool}
				b.nextGeminiTool++
				b.geminiTools[key] = tool
			}
			toolDelta := map[string]any{"index": tool.Index, "function": map[string]any{"arguments": string(args)}}
			if !seen {
				toolDelta["id"] = tool.ID
				toolDelta["type"] = "function"
				toolDelta["function"].(map[string]any)["name"] = tool.Name
			}
			delta["tool_calls"] = append(anySlice(delta["tool_calls"]), toolDelta)
		}
	}
	var finish string
	if reason := stringValue(candidate["finishReason"]); reason != "" {
		finish = geminiFinishReason(reason)
	}
	if usage, ok := raw["usageMetadata"].(map[string]any); ok {
		b.inputTokens = int(numberOr(usage["promptTokenCount"], float64(b.inputTokens)))
		b.completionTokens = int(numberOr(usage["candidatesTokenCount"], float64(b.completionTokens)))
		if _, ok := usage["candidatesTokenCount"]; !ok {
			total := number(usage["totalTokenCount"])
			if total > 0 {
				b.completionTokens = int(total) - b.inputTokens
			}
		}
		b.cachedTokens = int(numberOr(usage["cachedContentTokenCount"], float64(b.cachedTokens)))
	}
	if len(delta) == 0 && finish == "" && raw["usageMetadata"] == nil {
		return nil
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	out := map[string]any{"choices": []any{choice}}
	if raw["usageMetadata"] != nil {
		out["usage"] = map[string]any{"prompt_tokens": b.inputTokens, "completion_tokens": b.completionTokens, "total_tokens": b.inputTokens + b.completionTokens, "prompt_tokens_details": map[string]any{"cached_tokens": b.cachedTokens}}
	}
	return openAIEvent(out)
}

func translateGeminiFrame(data []byte) []byte {
	return (&nativeSSEBody{protocol: "gemini"}).translateGeminiFrame(data)
}

func anySlice(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	return nil
}

func numberOr(value any, fallback float64) float64 {
	if number, ok := value.(float64); ok {
		return number
	}
	return fallback
}

func nativeStreamError(raw any) error {
	message := "native upstream stream error"
	if detail, ok := raw.(map[string]any); ok {
		if value := stringValue(detail["message"]); value != "" {
			message = value
		}
	}
	return &Error{Code: "upstream_stream_error", Stage: "body", Cause: errors.New(message)}
}

func nativeResponseError(data []byte) error {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil || raw["error"] == nil {
		return nil
	}
	streamErr := nativeStreamError(raw["error"]).(*Error)
	streamErr.Code = "upstream_response_error"
	return streamErr
}

func openAIEvent(out map[string]any) []byte {
	encoded, _ := json.Marshal(out)
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}
func nativeErrorEvent(raw any) []byte {
	encoded, _ := json.Marshal(map[string]any{"error": raw})
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}
func number(raw any) float64 { value, _ := raw.(float64); return value }
func anthropicFinishReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}
func geminiFinishReason(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "STOP":
		return "stop"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

func openAIToAnthropic(payload map[string]any) map[string]any {
	maxTokens := payload["max_tokens"]
	if maxTokens == nil {
		maxTokens = payload["max_completion_tokens"]
	}
	out := map[string]any{"model": payload["model"], "max_tokens": maxTokens, "messages": []any{}}
	if out["max_tokens"] == nil {
		out["max_tokens"] = 8192
	}
	if stream, ok := payload["stream"]; ok {
		out["stream"] = stream
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := payload[key]; ok {
			out[key] = value
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		convertedTools := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			convertedTools = append(convertedTools, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"]})
		}
		out["tools"] = convertedTools
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		fn, _ := choice["function"].(map[string]any)
		switch stringValue(choice["type"]) {
		case "function":
			out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		}
	} else if choice, ok := payload["tool_choice"].(string); ok {
		switch choice {
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "none":
			delete(out, "tools")
		}
	}
	messages, _ := payload["messages"].([]any)
	converted := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" {
			out["system"] = msg["content"]
			continue
		}
		if role == "tool" {
			converted = append(converted, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": msg["tool_call_id"], "content": msg["content"]}}})
			continue
		}
		content := msg["content"]
		if role == "assistant" {
			blocks := make([]any, 0, 1)
			if text := nativeContentText(content); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					args := map[string]any{}
					_ = json.Unmarshal([]byte(stringValue(fn["arguments"])), &args)
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": args})
				}
			}
			if len(blocks) > 0 {
				content = blocks
			}
		}
		converted = append(converted, map[string]any{"role": role, "content": content})
	}
	out["messages"] = converted
	return out
}

func anthropicToOpenAI(data []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return data
	}
	text := ""
	toolCalls := make([]any, 0)
	if blocks, ok := raw["content"].([]any); ok {
		for _, item := range blocks {
			block, _ := item.(map[string]any)
			if value, _ := block["text"].(string); value != "" {
				text += value
			}
			if block["type"] == "tool_use" {
				args, _ := json.Marshal(block["input"])
				toolCalls = append(toolCalls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": string(args)}})
			}
		}
	}
	usage, _ := raw["usage"].(map[string]any)
	in, _ := usage["input_tokens"].(float64)
	out, _ := usage["output_tokens"].(float64)
	message := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	cacheRead := int(number(usage["cache_read_input_tokens"]))
	cacheWrite := int(number(usage["cache_creation_input_tokens"]))
	result := map[string]any{"id": raw["id"], "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": anthropicFinishReason(stringValue(raw["stop_reason"]))}}, "usage": map[string]any{"prompt_tokens": int(in), "completion_tokens": int(out), "total_tokens": int(in + out), "prompt_tokens_details": map[string]any{"cached_tokens": cacheRead}, "cache_creation_input_tokens": cacheWrite}}
	encoded, _ := json.Marshal(result)
	return encoded
}

func openAIToGemini(payload map[string]any) map[string]any {
	out := map[string]any{"contents": []any{}}
	if messages, ok := payload["messages"].([]any); ok {
		contents := make([]any, 0, len(messages))
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "system" {
				out["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": msg["content"]}}}
				continue
			}
			if role == "tool" {
				contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": msg["name"], "response": map[string]any{"result": nativeJSONValue(stringValue(msg["content"]))}}}}})
				continue
			}
			if role == "assistant" {
				role = "model"
			}
			parts := make([]any, 0, 1)
			if text := nativeContentText(msg["content"]); text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					args := map[string]any{}
					_ = json.Unmarshal([]byte(stringValue(fn["arguments"])), &args)
					parts = append(parts, map[string]any{"functionCall": map[string]any{"name": fn["name"], "args": args}})
				}
			}
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
		out["contents"] = contents
	}
	if tools, ok := payload["tools"].([]any); ok {
		declarations := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			declarations = append(declarations, map[string]any{"name": fn["name"], "description": fn["description"], "parameters": fn["parameters"]})
		}
		out["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		fn, _ := choice["function"].(map[string]any)
		if name := stringValue(fn["name"]); name != "" {
			out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []string{name}}}
		}
	} else if choice, ok := payload["tool_choice"].(string); ok {
		mode := "AUTO"
		if choice == "none" {
			mode = "NONE"
		} else if choice == "required" {
			mode = "ANY"
		}
		out["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	}
	config := map[string]any{}
	for source, target := range map[string]string{"max_tokens": "maxOutputTokens", "max_completion_tokens": "maxOutputTokens", "temperature": "temperature", "top_p": "topP"} {
		if value, ok := payload[source]; ok {
			config[target] = value
		}
	}
	if len(config) > 0 {
		out["generationConfig"] = config
	}
	return out
}

func nativeJSONValue(value string) any {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return value
}

func nativeContentText(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var out strings.Builder
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			text, _ := part["text"].(string)
			out.WriteString(text)
		}
		return out.String()
	default:
		return ""
	}
}

func stringValue(value any) string { text, _ := value.(string); return text }

func geminiToOpenAI(data []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return data
	}
	candidate, _ := firstMap(raw["candidates"])
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	text := ""
	toolCalls := make([]any, 0)
	for _, item := range parts {
		part, _ := item.(map[string]any)
		value, _ := part["text"].(string)
		text += value
		if call, ok := part["functionCall"].(map[string]any); ok {
			args, _ := json.Marshal(call["args"])
			toolCalls = append(toolCalls, map[string]any{"id": fmt.Sprintf("call_%d", len(toolCalls)), "type": "function", "function": map[string]any{"name": call["name"], "arguments": string(args)}})
		}
	}
	meta, _ := raw["usageMetadata"].(map[string]any)
	in, _ := meta["promptTokenCount"].(float64)
	out, _ := meta["candidatesTokenCount"].(float64)
	message := map[string]any{"role": "assistant", "content": text}
	finish := geminiFinishReason(stringValue(candidate["finishReason"]))
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	cached := int(number(meta["cachedContentTokenCount"]))
	result := map[string]any{"object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": int(in), "completion_tokens": int(out), "total_tokens": int(in + out), "prompt_tokens_details": map[string]any{"cached_tokens": cached}}}
	encoded, _ := json.Marshal(result)
	return encoded
}

func normalizeAnthropicModels(data []byte) []byte {
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return data
	}
	return modelList(raw.Data, "id")
}
func normalizeGeminiModels(data []byte) []byte {
	var raw struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return data
	}
	for _, item := range raw.Models {
		if name, ok := item["name"].(string); ok {
			item["id"] = strings.TrimPrefix(name, "models/")
		}
	}
	return modelList(raw.Models, "id")
}
func modelList(items []map[string]any, key string) []byte {
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if id, ok := item[key].(string); ok && id != "" {
			data = append(data, map[string]any{"id": id})
		}
	}
	encoded, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return encoded
}
func firstMap(value any) (map[string]any, bool) {
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	item, ok := list[0].(map[string]any)
	return item, ok
}
func urlPathEscape(value string) string {
	return strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23").Replace(value)
}
