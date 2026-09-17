package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

type PublicHandler struct {
	service      *relay.Service
	maxBodyBytes int64
	// admission 在接受公共对话请求前调用（如 RSS 软保护），非 nil 且返回
	// 错误时以 503 拒绝；管理端不受影响。
	admission func() error
	// streamAdmission 在流式转发过程中周期调用，超限时中止流并按 aborted 结算。
	streamAdmission func() error
}

func NewPublicHandler(service *relay.Service, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 8 * 1024 * 1024
	}
	h := &PublicHandler{service: service, maxBodyBytes: maxBodyBytes}
	return h
}

// SetAdmission 注册请求准入检查（例如 memory.Guard.PublicAdmission）。
func (h *PublicHandler) SetAdmission(fn func() error) { h.admission = fn }

// SetStreamAdmission 注册流式过程中的周期检查（例如 memory.Guard.StreamContinue）。
func (h *PublicHandler) SetStreamAdmission(fn func() error) { h.streamAdmission = fn }

// ServeHTTP normalizes the request path before dispatching. Both
// "/v1/chat/completions" and the variants clients actually send in the wild
// (double "/v1" prefix, missing prefix, trailing slash) reach the same handler.
func (h *PublicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	path = strings.TrimPrefix(path, "/v1")
	path = strings.TrimPrefix(path, "/v1") // 客户端 Base URL 已带 /v1 时会拼出双重前缀
	switch path {
	case "/models":
		h.models(w, r)
	case "/chat/completions":
		h.chatCompletions(w, r)
	case "/responses":
		h.responsesAPI(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "unsupported endpoint " + r.URL.Path + ": use /v1/chat/completions or /v1/responses (POST) or /v1/models (GET)",
			"type":    "relay_error",
			"code":    "not_found",
		}})
	}
}

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func (h *PublicHandler) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "invalid_api_key")
		return
	}
	models, err := h.service.Models(r.Context(), token)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{"id": model.PublicName, "object": "model", "owned_by": "civic-relay"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *PublicHandler) chatCompletions(w http.ResponseWriter, r *http.Request) {
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
	var payload map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, http.StatusBadRequest, "model_required")
		return
	}
	if _, ok := payload["messages"].([]any); !ok {
		writeError(w, http.StatusBadRequest, "messages_required")
		return
	}
	req := relay.Request{Token: token, Model: model, Payload: payload, InputText: messageText(payload["messages"]), Stream: boolValue(payload["stream"]), MaxTokens: intValue(payload["max_tokens"]), MaxCompletionTokens: intValue(payload["max_completion_tokens"])}
	if messages, ok := payload["messages"].([]any); ok {
		req.Messages = messages
	}
	if req.Stream {
		h.stream(w, r, req)
		return
	}
	data, err := h.service.Chat(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h *PublicHandler) stream(w http.ResponseWriter, r *http.Request, req relay.Request) {
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
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	outputChars := int64(0)
	lines := 0
	var usageEvent upstream.Usage
	amount := int64(0)
	status := "completed"
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			status = "aborted"
			break
		}
		if flusher != nil {
			flusher.Flush()
		}
		event := upstream.ParseEvent([]byte(line + "\n\n"))
		outputChars += int64(event.OutputCharacters)
		if event.Usage.TotalTokens > 0 {
			usageEvent = event.Usage
		}
		if h.streamAdmission != nil && lines%256 == 255 {
			if err := h.streamAdmission(); err != nil {
				status = "aborted"
				break
			}
		}
		lines++
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		status = "failed"
	}
	if usageEvent.TotalTokens == 0 && outputChars > 0 {
		// Upstream never reported usage; estimate output tokens from
		// streamed characters so quota and ledger still record something.
		usageEvent = upstream.Usage{PromptTokens: int(usageEvent.PromptTokens), CompletionTokens: int((outputChars + 1) / 2), TotalTokens: int(outputChars)}
	}
	amount = relay.PriceUsage(usageEvent, model)
	_ = lease.Close(r.Context(), relay.Outcome{Status: status, InputTokens: int64(usageEvent.PromptTokens), OutputTokens: int64(usageEvent.CompletionTokens), CachedInputTokens: int64(usageEvent.PromptTokensDetails.CachedTokens), AmountMicroyuan: amount, HTTPStatus: 200})
	return
}

func (h *PublicHandler) writeServiceError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "relay_error"
	detail := err.Error()
	var quota *store.QuotaError
	var upstreamErr *upstream.Error
	switch {
	case errors.Is(err, relay.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "invalid_api_key"
	case errors.Is(err, relay.ErrModelNotAllowed):
		status, code = http.StatusBadRequest, "model_not_allowed"
	case errors.Is(err, relay.ErrGlobalConcurrencyExceeded), errors.Is(err, relay.ErrKeyConcurrencyExceeded):
		status, code = http.StatusTooManyRequests, "rate_limit_exceeded"
	case errors.Is(err, relay.ErrInvalidRequest):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.As(err, &quota):
		// 额度用满必须是 429 且带具体窗口与数字。旧实现没有这一分支，QuotaError
		// 落到 default 返回 500 relay_error，客户端会当成服务端故障反复重试。
		status, code = http.StatusTooManyRequests, quota.Code
		if text := quota.Detail(); text != "" {
			detail = text
		}
	case errors.As(err, &upstreamErr):
		status, code = http.StatusBadGateway, upstreamErr.Code
	}
	writeErrorDetail(w, status, code, detail)
}

func boolValue(value any) bool { result, _ := value.(bool); return result }
func intValue(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		n, _ := number.Int64()
		return n
	case int64:
		return number
	}
	return 0
}
func messageText(value any) string { encoded, _ := json.Marshal(value); return string(encoded) }
