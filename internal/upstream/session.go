package upstream

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

const SessionIDHeader = "x-opencode-session"
const DefaultUserAgent = "civic-ai-relay/1.0"

type sessionKey struct{}

func WithSessionID(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return strings.TrimSpace(id)
}

func isOpenCodeEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

func deriveSessionID(apiKey string, payload any) string {
	seed := apiKey + "\x00" + sessionFingerprint(payload)
	sum := sha1.Sum([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(sum[0:4]), hex.EncodeToString(sum[4:6]),
		hex.EncodeToString(sum[6:8]), hex.EncodeToString(sum[8:10]),
		hex.EncodeToString(sum[10:16]))
}

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
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, raw := range value {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\x00")
	default:
		return ""
	}
}
