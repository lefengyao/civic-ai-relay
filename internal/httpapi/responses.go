package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/upstream"
)

// 本文件实现 OpenAI Responses API（POST /v1/responses）的兼容层：
// 把 Responses 请求转换为 chat/completions 转发上游，再把结果转回
// Responses 形状返回客户端。计费与 chat/completions 完全同口径。

type responseID [8]byte

func newID(prefix string) string {
	var raw responseID
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:8])
}

func (h *PublicHandler) responsesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "invalid_api_key")
		return
	}
	if h.admission != nil {
		if err := h.admission(); err != nil {
			writeErrorDetail(w, http.StatusServiceUnavailable, "memory_limit_exceeded", err.Error())
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, http.StatusBadRequest, "model_required")
		return
	}
	input, hasInput := body["input"]
	if !hasInput {
		writeError(w, http.StatusBadRequest, "input_required")
		return
	}
	instructions, _ := body["instructions"].(string)
	messages := responsesInputToMessages(input, instructions)
	if len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "input_required")
		return
	}
	stream, _ := body["stream"].(bool)

	// 组装 chat/completions 载荷；temperature/top_p/stop 等采样参数原样透传
	payload := map[string]any{"model": model, "messages": messages, "stream": stream}
	for _, key := range []string{"temperature", "top_p", "stop", "seed", "user"} {
		if value, ok := body[key]; ok {
			payload[key] = value
		}
	}
	if maxTokens := intValue(body["max_output_tokens"]); maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	encoded, _ := json.Marshal(messages)
	relayMessages := make([]any, len(messages))
	for i, m := range messages {
		relayMessages[i] = m
	}
	req := relay.Request{Token: token, Model: model, Payload: payload, Messages: relayMessages, InputText: string(encoded), Stream: stream, MaxTokens: intValue(body["max_output_tokens"])}
	if stream {
		h.responsesStream(w, r, req, model)
		return
	}
	data, err := h.service.Chat(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	h.writeResponsesResult(w, model, data)
}

// writeResponsesResult 把 chat.completion JSON 转成 response 对象返回。
func (h *PublicHandler) writeResponsesResult(w http.ResponseWriter, model string, data []byte) {
	var chat struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage upstream.Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chat); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_response_invalid")
		return
	}
	text := ""
	if len(chat.Choices) > 0 {
		text = chat.Choices[0].Message.Content
	}
	responseID := newID("resp")
	messageID := newID("msg")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": model,
		"output": []map[string]any{{
			"type": "message", "id": messageID, "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"output_text": text,
		"usage": map[string]any{
			"input_tokens": chat.Usage.PromptTokens, "output_tokens": chat.Usage.CompletionTokens, "total_tokens": chat.Usage.TotalTokens,
		},
	})
}

// responsesStream 把上游 chat SSE 增量转成 Responses 事件流。
func (h *PublicHandler) responsesStream(w http.ResponseWriter, r *http.Request, req relay.Request, publicModel string) {
	response, lease, model, err := h.service.Stream(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	responseID, messageID := newID("resp"), newID("msg")
	created := time.Now().Unix()
	send := func(event string, payload map[string]any) {
		payload["type"] = event
		encoded, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+event+"\ndata: "+string(encoded)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	// response.created 让客户端尽早拿到会话句柄
	send("response.created", map[string]any{"response": map[string]any{
		"id": responseID, "object": "response", "created_at": created, "status": "in_progress", "model": publicModel, "output": []any{},
	}})
	send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{
		"type": "message", "id": messageID, "status": "in_progress", "role": "assistant", "content": []any{},
	}})
	send("response.content_part.added", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{
		"type": "output_text", "text": "", "annotations": []any{},
	}})

	var full strings.Builder
	var usageEvent upstream.Usage
	status := "completed"
	lines := 0
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataPayload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataPayload == "[DONE]" {
			break
		}
		if h.streamAdmission != nil && lines%256 == 255 {
			if err := h.streamAdmission(); err != nil {
				status = "aborted"
				break
			}
		}
		lines++
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *upstream.Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(dataPayload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			usageEvent = *chunk.Usage
		}
		if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == "" {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		full.WriteString(delta)
		send("response.output_text.delta", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "delta": delta})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		status = "failed"
	}
	if usageEvent.TotalTokens == 0 && full.Len() > 0 {
		// 上游未报告 usage 时按流出的字符数估算，保证配额与账本仍有记录
		usageEvent = upstream.Usage{PromptTokens: usageEvent.PromptTokens, CompletionTokens: (full.Len() + 1) / 2, TotalTokens: full.Len()}
	}
	text := full.String()
	send("response.output_text.done", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "text": text})
	send("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{
		"type": "message", "id": messageID, "status": "completed", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}})
	send("response.completed", map[string]any{"response": map[string]any{
		"id": responseID, "object": "response", "created_at": created, "status": status, "model": publicModel,
		"output": []map[string]any{{
			"type": "message", "id": messageID, "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"usage": map[string]any{
			"input_tokens": usageEvent.PromptTokens, "output_tokens": usageEvent.CompletionTokens, "total_tokens": usageEvent.TotalTokens,
		},
	}})
	amount := relay.PriceUsage(usageEvent, model)
	_ = lease.Close(r.Context(), relay.Outcome{Status: status, InputTokens: int64(usageEvent.PromptTokens), OutputTokens: int64(usageEvent.CompletionTokens), CachedInputTokens: int64(usageEvent.PromptTokensDetails.CachedTokens), AmountMicroyuan: amount, HTTPStatus: 200})
}

// responsesInputToMessages 把 Responses 的 instructions + input 转成
// chat/completions 的 messages。非 message 类型（工具调用、推理历史等）忽略。
func responsesInputToMessages(input any, instructions string) []map[string]any {
	var messages []map[string]any
	if strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch value := input.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": value})
		}
	case []any:
		for _, item := range value {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := entry["type"].(string); typ != "" && typ != "message" {
				continue
			}
			role, _ := entry["role"].(string)
			if role == "" {
				role = "user"
			}
			if content := flattenResponseContent(entry["content"]); content != "" {
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		}
	}
	return messages
}

// flattenResponseContent 把 string 或 [{type,text}] 形态的内容归一为纯文本。
func flattenResponseContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, part := range value {
			entry, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := entry["text"].(string); text != "" {
				builder.WriteString(text)
			}
		}
		return builder.String()
	}
	return ""
}
