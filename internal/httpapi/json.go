package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	if requestID := strings.TrimSpace(w.Header().Get("X-Request-Id")); requestID != "" {
		if payload, ok := value.(map[string]any); ok {
			if errorValue, ok := payload["error"].(map[string]any); ok {
				if _, exists := errorValue["request_id"]; !exists {
					errorValue["request_id"] = requestID
				}
			}
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeErrorPayload(w, status, code, code)
}

// writeErrorDetail 在标准错误结构上附加人类可读的具体原因，message 保留
// 原始错误文本，code 保持稳定的机器可读标识。
func writeErrorDetail(w http.ResponseWriter, status int, code, detail string) {
	message := code
	if detail != "" {
		message = code + ": " + detail
	}
	writeErrorPayload(w, status, code, message)
}

func writeErrorPayload(w http.ResponseWriter, status int, code, message string) {
	errorValue := map[string]any{"message": message, "type": "relay_error", "code": code}
	if requestID := strings.TrimSpace(w.Header().Get("X-Request-Id")); requestID != "" {
		errorValue["request_id"] = requestID
	}
	writeJSON(w, status, map[string]any{"error": errorValue})
}
