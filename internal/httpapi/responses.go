package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/requestmeta"
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
	token, appID := bearer(r), appIdentifier(r)
	if token == "" && appID == "" {
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
	if sid := strings.TrimSpace(r.Header.Get(upstream.SessionIDHeader)); sid != "" {
		r = r.WithContext(upstream.WithSessionID(r.Context(), sid))
	}
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, http.StatusBadRequest, "model_required")
		return
	}
	if trace := requestmeta.FromContext(r.Context()); trace != nil {
		trace.SetModel(model)
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
	req := relay.Request{Token: token, AppID: appID, Model: model, Payload: payload, Messages: relayMessages, InputText: string(encoded), Stream: stream, MaxTokens: intValue(body["max_output_tokens"]), Protocol: "responses"}
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
	prepared, err := h.service.PrepareStream(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	lease := prepared.Lease
	responseID, messageID := newID("resp"), newID("msg")
	created := time.Now().Unix()
	var full strings.Builder
	var usageEvent upstream.Usage
	var incompleteReason string
	outcome := relay.Outcome{Status: "aborted", ErrorCode: "client_canceled", FailureStage: "stream", Protocol: lease.Protocol()}
	var session *streamSession
	defer func() {
		if session != nil {
			session.Close()
		} else {
			prepared.Cancel()
		}
		reportedUsage := usageEvent.TotalTokens > 0
		if !reportedUsage && full.Len() > 0 {
			usageEvent = upstream.Usage{CompletionTokens: (full.Len() + 1) / 2, TotalTokens: full.Len()}
		}
		outcome.InputTokens = int64(usageEvent.PromptTokens)
		if outcome.InputTokens == 0 {
			outcome.InputTokens = lease.LeaseInputTokens()
		}
		outcome.OutputTokens = int64(usageEvent.CompletionTokens)
		outcome.CachedInputTokens = int64(usageEvent.PromptTokensDetails.CachedTokens)
		outcome.IncompleteReason = incompleteReason
		outcome.AmountMicroyuan = relay.PriceUsage(usageEvent, prepared.Model)
		if reportedUsage {
			outcome.UsageSource = "upstream"
		} else if full.Len() > 0 {
			outcome.UsageSource = "estimated"
		} else {
			outcome.UsageSource = "unknown"
		}
		if err := lease.Close(r.Context(), outcome); err != nil {
			logging.Errorf("responses settlement_failed request_id=%s status=%s error_code=%s failure_stage=%s upstream_status=%d", lease.RequestID(), outcome.Status, outcome.ErrorCode, outcome.FailureStage, outcome.HTTPStatus)
		}
	}()
	session, err = newStreamSession(w, r, prepared, h.streamAdmission)
	if err != nil {
		outcome.ErrorCode, outcome.FailureStage = "downstream_write_failed", "downstream"
		return
	}
	send := func(event string, payload map[string]any) bool {
		payload["type"] = event
		encoded, _ := json.Marshal(payload)
		return session.write([]byte("event: "+event+"\ndata: "+string(encoded)+"\n\n")) == nil
	}
	writeFailure := func() {
		outcome.Status, outcome.ErrorCode, outcome.FailureStage = "aborted", "downstream_write_failed", "downstream"
	}
	var done, headerReceived bool
	var readErr error
	var finishReason string
	for {
		result := session.Next()
		switch result.kind {
		case streamHeader:
			headerReceived = true
			if trace := requestmeta.FromContext(r.Context()); trace != nil {
				trace.MarkHeader()
			}
			outcome.HTTPStatus = result.response.StatusCode
			if !send("response.created", map[string]any{"response": map[string]any{
				"id": responseID, "object": "response", "created_at": created, "status": "in_progress", "model": publicModel, "output": []any{},
			}}) || !send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{
				"type": "message", "id": messageID, "status": "in_progress", "role": "assistant", "content": []any{},
			}}) || !send("response.content_part.added", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{
				"type": "output_text", "text": "", "annotations": []any{},
			}}) {
				writeFailure()
				return
			}
		case streamFrame:
			if result.frame.Comment {
				if err := session.write(result.frame.Raw); err != nil {
					writeFailure()
					return
				}
				continue
			}
			dataPayload := bytes.TrimSpace(result.frame.Data)
			if bytes.Equal(dataPayload, []byte("[DONE]")) {
				done = true
				goto finished
			}
			if len(dataPayload) == 0 {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *upstream.Usage `json:"usage"`
			}
			if json.Unmarshal(dataPayload, &chunk) != nil {
				readErr = errors.New("invalid upstream event")
				goto finished
			}
			if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
				usageEvent = *chunk.Usage
			}
			if len(chunk.Choices) > 0 {
				if trace := requestmeta.FromContext(r.Context()); trace != nil {
					trace.MarkFirstEvent()
				}
				if chunk.Choices[0].FinishReason != "" {
					finishReason = chunk.Choices[0].FinishReason
					if finishReason == "length" {
						incompleteReason = "max_output_tokens"
					}
				}
				delta := chunk.Choices[0].Delta.Content
				if delta != "" {
					full.WriteString(delta)
					if !send("response.output_text.delta", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "delta": delta}) {
						writeFailure()
						return
					}
				}
			}
		case streamReadError:
			readErr = result.err
			goto finished
		case streamWriteError:
			writeFailure()
			return
		case streamAdmissionError:
			outcome.Status, outcome.ErrorCode = "aborted", "memory_limit_exceeded"
			if !sendResponseFailure(send, responseID, publicModel, created, messageID, full.String(), outcome.ErrorCode, "response.failed", lease.RequestID()) {
				writeFailure()
			}
			return
		case streamCanceled:
			outcome.Status, outcome.ErrorCode = "aborted", "client_canceled"
			return
		}
	}

finished:
	if !headerReceived {
		outcome.Status, outcome.ErrorCode, outcome.HTTPStatus = streamOpenFailure(readErr, r.Context())
		outcome.FailureStage = "upstream"
		if outcome.Status == "failed" && !sendResponseFailure(send, responseID, publicModel, created, messageID, full.String(), outcome.ErrorCode, "response.failed", lease.RequestID()) {
			writeFailure()
		}
		return
	}
	responseEvent := ""
	outcome.Status, outcome.ErrorCode, responseEvent = upstream.ResponsesBridgeTerminal(done, finishReason, readErr, r.Context().Err() != nil || errors.Is(readErr, context.Canceled))
	if readErr != nil && !errors.Is(readErr, io.EOF) && readErr.Error() == "invalid upstream event" {
		outcome.ErrorCode = "upstream_event_invalid"
	}
	if outcome.Status == "failed" && outcome.FailureStage == "stream" {
		outcome.FailureStage = "upstream"
	}
	if outcome.Status == "completed" {
		outcome.FailureStage = ""
	}
	text := full.String()
	if outcome.Status == "completed" {
		if !send("response.output_text.done", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "text": text}) ||
			!send("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{
				"type": "message", "id": messageID, "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
			}}) ||
			!send("response.completed", map[string]any{"response": map[string]any{
				"id": responseID, "object": "response", "created_at": created, "status": "completed", "model": publicModel,
				"output": []map[string]any{{
					"type": "message", "id": messageID, "status": "completed", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
				}},
				"usage": map[string]any{
					"input_tokens": usageEvent.PromptTokens, "output_tokens": usageEvent.CompletionTokens, "total_tokens": usageEvent.TotalTokens,
				},
			}}) {
			writeFailure()
		}
	} else if responseEvent != "" && !sendResponseFailure(send, responseID, publicModel, created, messageID, text, outcome.ErrorCode, responseEvent, lease.RequestID()) {
		writeFailure()
	}
}

func sendResponseFailure(send func(string, map[string]any) bool, responseID, model string, created int64, messageID, text, code, event, requestID string) bool {
	status := "failed"
	if event == "response.incomplete" {
		status = "incomplete"
	}
	return send(event, map[string]any{"response": map[string]any{
		"id": responseID, "object": "response", "created_at": created, "status": status, "model": model,
		"error": map[string]any{"code": code, "message": "response stream interrupted", "request_id": requestID},
		"output": []map[string]any{{
			"type": "message", "id": messageID, "status": "incomplete", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
	}})
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
