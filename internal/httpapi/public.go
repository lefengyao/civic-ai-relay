package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/requestmeta"
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
	r = ensureRequestTrace(w, r)
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

// appIdentifier 读取公开的应用标识（X-App-Id）。它不是密钥，只是告诉服务端
// 「我是哪个应用」——给那些**不能保存密钥**的客户端用（密钥写在客户端里迟早被提取）。
// 与 Bearer 二选一，同给时以密钥为准。
func appIdentifier(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-App-Id"))
}

func (h *PublicHandler) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token, appID := bearer(r), appIdentifier(r)
	if token == "" && appID == "" {
		writeError(w, http.StatusUnauthorized, "invalid_api_key")
		return
	}
	models, err := h.service.Models(r.Context(), token, appID)
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
	var payload map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	if sid := strings.TrimSpace(r.Header.Get(upstream.SessionIDHeader)); sid != "" {
		r = r.WithContext(upstream.WithSessionID(r.Context(), sid))
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, http.StatusBadRequest, "model_required")
		return
	}
	if trace := requestmeta.FromContext(r.Context()); trace != nil {
		trace.SetModel(model)
	}
	if _, ok := payload["messages"].([]any); !ok {
		writeError(w, http.StatusBadRequest, "messages_required")
		return
	}
	req := relay.Request{Token: token, AppID: appID, Model: model, Payload: payload, InputText: messageText(payload["messages"]), Stream: boolValue(payload["stream"]), MaxTokens: intValue(payload["max_tokens"]), MaxCompletionTokens: intValue(payload["max_completion_tokens"])}
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
	prepared, err := h.service.PrepareStream(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	lease := prepared.Lease
	outputChars := int64(0)
	var usageEvent upstream.Usage
	var incompleteReason string
	var finishReason string
	outcome := relay.Outcome{Status: "aborted", ErrorCode: "client_canceled", FailureStage: "stream", Protocol: lease.Protocol()}
	var session *streamSession
	defer func() {
		if session != nil {
			session.Close()
		} else {
			prepared.Cancel()
		}
		reportedUsage := usageEvent.TotalTokens > 0
		if !reportedUsage && outputChars > 0 {
			usageEvent = upstream.Usage{CompletionTokens: int((outputChars + 1) / 2), TotalTokens: int(outputChars)}
		}
		outcome.InputTokens = int64(usageEvent.PromptTokens)
		outcome.OutputTokens = int64(usageEvent.CompletionTokens)
		outcome.CachedInputTokens = int64(usageEvent.PromptTokensDetails.CachedTokens)
		if outcome.InputTokens == 0 {
			outcome.InputTokens = lease.LeaseInputTokens()
		}
		outcome.IncompleteReason = incompleteReason
		outcome.AmountMicroyuan = relay.PriceUsage(usageEvent, prepared.Model)
		if reportedUsage {
			outcome.UsageSource = "upstream"
		} else if outputChars > 0 {
			outcome.UsageSource = "estimated"
		} else {
			outcome.UsageSource = "unknown"
		}
		if err := lease.Close(r.Context(), outcome); err != nil {
			logging.Errorf("stream settlement_failed request_id=%s status=%s error_code=%s failure_stage=%s upstream_status=%d", lease.RequestID(), outcome.Status, outcome.ErrorCode, outcome.FailureStage, outcome.HTTPStatus)
		}
	}()
	session, err = newStreamSession(w, r, prepared, h.streamAdmission)
	if err != nil {
		outcome.ErrorCode, outcome.FailureStage = "downstream_write_failed", "downstream"
		return
	}
	var done, headerReceived bool
	var readErr error
	for {
		result := session.Next()
		switch result.kind {
		case streamHeader:
			headerReceived = true
			if trace := requestmeta.FromContext(r.Context()); trace != nil {
				trace.MarkHeader()
			}
			outcome.HTTPStatus = result.response.StatusCode
		case streamFrame:
			if err := session.write(result.frame.Raw); err != nil {
				outcome.Status, outcome.ErrorCode, outcome.FailureStage = "aborted", "downstream_write_failed", "downstream"
				return
			}
			event := upstream.ParseEvent(result.frame.Raw)
			if event.Invalid {
				readErr = errors.New("invalid upstream event")
				goto finished
			}
			outputChars += int64(event.OutputCharacters)
			if event.OutputCharacters > 0 || event.Usage.TotalTokens > 0 {
				if trace := requestmeta.FromContext(r.Context()); trace != nil {
					trace.MarkFirstEvent()
				}
			}
			if event.FinishReason != "" {
				finishReason = event.FinishReason
			}
			if event.Usage.TotalTokens > 0 {
				usageEvent = event.Usage
			}
			if bytes.Equal(bytes.TrimSpace(result.frame.Data), []byte("[DONE]")) {
				done = true
				goto finished
			}
		case streamReadError:
			readErr = result.err
			goto finished
		case streamWriteError:
			outcome.Status, outcome.ErrorCode, outcome.FailureStage = "aborted", "downstream_write_failed", "downstream"
			return
		case streamAdmissionError:
			outcome.Status, outcome.ErrorCode = "aborted", "memory_limit_exceeded"
			if err := writeChatStreamError(session, outcome.ErrorCode, lease.RequestID()); err != nil {
				outcome.ErrorCode, outcome.FailureStage = "downstream_write_failed", "downstream"
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
	} else {
		outcome.Status, outcome.ErrorCode = upstream.ChatTerminal(done, readErr, r.Context().Err() != nil || errors.Is(readErr, context.Canceled))
	}
	if finishReason == "length" && done && outcome.Status == "completed" {
		incompleteReason = "max_output_tokens"
	}
	if readErr != nil && readErr.Error() == "invalid upstream event" {
		outcome.ErrorCode = "upstream_event_invalid"
	}
	if outcome.Status == "failed" && outcome.FailureStage == "stream" {
		outcome.FailureStage = "upstream"
	}
	if outcome.Status == "completed" {
		outcome.FailureStage = ""
	}
	if outcome.Status == "failed" {
		if err := writeChatStreamError(session, outcome.ErrorCode, lease.RequestID()); err != nil {
			outcome.Status, outcome.ErrorCode, outcome.FailureStage = "aborted", "downstream_write_failed", "downstream"
		}
	}
}

func (h *PublicHandler) writeServiceError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "relay_error"
	// Service errors may be wrapped or joined with database, transport, or
	// provider causes. Those causes are useful in server-side diagnostics, but
	// their text can contain paths, URLs, or other implementation details. Keep
	// the default public detail stable and opt in only to the two structured
	// error types whose messages are deliberately safe for clients.
	detail := "request could not be completed"
	var quota *store.QuotaError
	var upstreamErr *upstream.Error
	switch {
	case errors.Is(err, relay.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "invalid_api_key"
	case errors.Is(err, relay.ErrTokenExpired):
		// 短时令牌到期：同样是 401，但错误码不同 —— 客户端据此能提示「令牌过期，
		// 请换新的」，而不是把「凭据不对」和「该续期了」混为一谈反复重试。
		status, code = http.StatusUnauthorized, "token_expired"
		detail = "客户端令牌已过期，请向管理员申请新的令牌"
	case errors.Is(err, relay.ErrModelNotAllowed):
		status, code = http.StatusBadRequest, "model_not_allowed"
	case errors.Is(err, relay.ErrSettlementUnavailable):
		status, code = http.StatusServiceUnavailable, "settlement_unavailable"
		detail = "request settlement is temporarily unavailable"
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
		// upstream.Error.Error deliberately exposes only the stable code and
		// status; its Cause is retained for server-side unwrapping and logging.
		detail = upstreamErr.Error()
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
