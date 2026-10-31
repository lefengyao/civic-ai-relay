package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func nativeOptions() Options {
	return Options{ConnectTimeout: time.Second, HeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second}
}

func TestAnthropicAdapterUsesNativeAuthAndNormalizesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "anthropic-secret" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			t.Fatalf("request path/auth = %s/%q/%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["max_tokens"] == nil || body["messages"] == nil {
			t.Fatalf("native body = %#v", body)
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer server.Close()
	client := NewWithProtocol(server.URL, "anthropic-secret", "anthropic", nativeOptions())
	data, err := client.ChatJSON(context.Background(), map[string]any{"model": "claude-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["object"] != "chat.completion" || got["usage"].(map[string]any)["prompt_tokens"] != float64(3) {
		t.Fatalf("normalized response = %#v", got)
	}
}

func TestNativeStreamTranslatesAnthropicUsageToolsAndTerminalReason(t *testing.T) {
	body := &nativeSSEBody{protocol: "anthropic"}
	frames := []SSEFrame{
		{Data: []byte(`{"type":"message_start","message":{"usage":{"input_tokens":7}}}`)},
		{Data: []byte(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}}`)},
		{Data: []byte(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}`)},
		{Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`)},
	}
	var translated strings.Builder
	for _, frame := range frames {
		translated.Write(body.translate(frame))
	}
	data := translated.String()
	for _, expected := range []string{`"prompt_tokens":7`, `"completion_tokens":3`, `"finish_reason":"tool_calls"`, `"id":"tool_1"`, `"arguments":"{\"q\":1}"`} {
		if !strings.Contains(data, expected) {
			t.Errorf("translated stream missing %s: %s", expected, data)
		}
	}
}

func TestNativeStreamTranslatesGeminiUsageToolsAndErrors(t *testing.T) {
	body := &nativeSSEBody{protocol: "gemini"}
	first := body.translate(SSEFrame{Data: []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":1}}}]}}],"usageMetadata":{"promptTokenCount":8}}`)})
	second := body.translate(SSEFrame{Data: []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":4,"cachedContentTokenCount":2}}`)})
	if strings.Count(string(first)+string(second), `"id":"call_0"`) != 1 {
		t.Fatalf("tool call id should be emitted once: %s%s", first, second)
	}
	for _, expected := range []string{`"prompt_tokens":8`, `"completion_tokens":4`, `"cached_tokens":2`, `"finish_reason":"stop"`} {
		if !strings.Contains(string(first)+string(second), expected) {
			t.Errorf("Gemini stream missing %s: %s%s", expected, first, second)
		}
	}
	errFrame := body.translate(SSEFrame{Data: []byte(`{"error":{"code":429,"message":"quota exceeded"}}`)})
	var upstreamErr *Error
	if !strings.Contains(string(errFrame), `"error"`) || !errors.As(body.terminalErr, &upstreamErr) || upstreamErr.Code != "upstream_stream_error" {
		t.Fatalf("native error was not normalized: frame=%s err=%#v", errFrame, body.terminalErr)
	}
}

func TestGeminiStreamUsesNativeSSEEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "gemini-secret" {
			t.Fatalf("stream endpoint/auth = %s?%s/%q", r.URL.Path, r.URL.RawQuery, r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n")
	}))
	defer server.Close()
	client := NewWithProtocol(server.URL, "gemini-secret", "gemini", nativeOptions())
	resp, err := client.Stream(context.Background(), map[string]any{"model": "gemini-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":"hello"`) || !strings.Contains(string(data), `"prompt_tokens":2`) || !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("translated Gemini stream = %s", data)
	}
}

func TestGeminiAdapterUsesNativePathAndNormalizesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:generateContent" || r.Header.Get("x-goog-api-key") != "gemini-secret" {
			t.Fatalf("path/auth = %s/%q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3}}`)
	}))
	defer server.Close()
	client := NewWithProtocol(server.URL, "gemini-secret", "gemini", nativeOptions())
	data, err := client.ChatJSON(context.Background(), map[string]any{"model": "gemini-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello from gemini") || !strings.Contains(string(data), `"prompt_tokens":4`) {
		t.Fatalf("normalized response = %s", data)
	}
}

func TestNativeJSONErrorResponseIsReturnedAsUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"quota exceeded"}}`)
	}))
	defer server.Close()
	client := NewWithProtocol(server.URL, "secret", "gemini", nativeOptions())
	_, err := client.ChatJSON(context.Background(), map[string]any{"model": "m", "messages": []any{}})
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "upstream_response_error" {
		t.Fatalf("error = %#v", err)
	}
}

func TestNativeStreamTranslatesAnthropicTextEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	client := NewWithProtocol(server.URL, "secret", "anthropic", nativeOptions())
	resp, err := client.Stream(context.Background(), map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"content":"hi"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("translated stream = %s", body)
	}
}
