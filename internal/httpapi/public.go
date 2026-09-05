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
}

func NewPublicHandler(service *relay.Service, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 8 * 1024 * 1024
	}
	h := &PublicHandler{service: service, maxBodyBytes: maxBodyBytes}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", h.models)
	mux.HandleFunc("/v1/chat/completions", h.chatCompletions)
	return mux
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
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	var payload map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
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
	inputTokens, outputTokens := int64(0), int64(0)
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
			inputTokens = int64(event.Usage.PromptTokens)
			outputTokens = int64(event.Usage.CompletionTokens)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		status = "failed"
	}
	if outputTokens == 0 && outputChars > 0 {
		outputTokens = (outputChars + 1) / 2
	}
	amount = priceForStream(model, inputTokens, outputTokens)
	_ = lease.Close(r.Context(), relay.Outcome{Status: status, InputTokens: inputTokens, OutputTokens: outputTokens, AmountMicroyuan: amount, HTTPStatus: 200})
}

func priceForStream(model store.Model, inputTokens, outputTokens int64) int64 {
	inputPrice, outputPrice := int64(0), int64(0)
	if model.InputPriceMicroyuan != nil {
		inputPrice = *model.InputPriceMicroyuan
	}
	if model.OutputPriceMicroyuan != nil {
		outputPrice = *model.OutputPriceMicroyuan
	}
	return pricePartHTTP(inputTokens, inputPrice) + pricePartHTTP(outputTokens, outputPrice)
}

func pricePartHTTP(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	return (tokens*price + 999999) / 1000000
}

func (h *PublicHandler) writeServiceError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "relay_error"
	switch {
	case errors.Is(err, relay.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "invalid_api_key"
	case errors.Is(err, relay.ErrModelNotAllowed):
		status, code = http.StatusBadRequest, "model_not_allowed"
	case errors.Is(err, relay.ErrGlobalConcurrencyExceeded), errors.Is(err, relay.ErrKeyConcurrencyExceeded):
		status, code = http.StatusTooManyRequests, "rate_limit_exceeded"
	case errors.Is(err, relay.ErrInvalidRequest):
		status, code = http.StatusBadRequest, "invalid_request"
	default:
		var upstreamErr *upstream.Error
		if errors.As(err, &upstreamErr) {
			status, code = http.StatusBadGateway, upstreamErr.Code
		}
	}
	writeError(w, status, code)
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
