package httpapi

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": code, "type": "relay_error", "code": code}})
}

// writeErrorDetail 在标准错误结构上附加人类可读的具体原因，message 保留
// 原始错误文本，code 保持稳定的机器可读标识。
func writeErrorDetail(w http.ResponseWriter, status int, code, detail string) {
	message := code
	if detail != "" {
		message = code + ": " + detail
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "relay_error", "code": code}})
}
