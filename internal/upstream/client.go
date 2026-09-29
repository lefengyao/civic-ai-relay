// Package upstream implements the small OpenAI-compatible transport boundary.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string {
	if e == nil {
		return "upstream request failed"
	}
	if e.Status > 0 {
		return fmt.Sprintf("%s (status %d)", e.Code, e.Status)
	}
	return e.Code
}

func New(baseURL, apiKey string, connect, read, write, pool time.Duration) *Client {
	if connect <= 0 {
		connect = 10 * time.Second
	}
	if read <= 0 {
		read = 300 * time.Second
	}
	if write <= 0 {
		write = 30 * time.Second
	}
	if pool <= 0 {
		pool = 10 * time.Second
	}
	transport := &http.Transport{
		MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: pool,
		ResponseHeaderTimeout: connect + read,
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		// ⚠️ 刻意**不设** http.Client.Timeout。它是「整个请求」的总超时，**包含读取
		// 响应体的全部时间**，对流式回答等于一道固定的硬闸：默认 connect+read+write
		// = 10+300+30 = 340 秒，任何超过它的流式对话都会在那一刻被无声掐断，
		// 而记录里只留下 upstream_status=200 + 零 token 的 failed，运维完全看不出
		// 是超时（2026-09-25 线上真实故障）。服务端配的 MAX_STREAM_DURATION 也因此
		// 形同虚设。现在职责拆开：
		//   - 「上游迟迟不响应」由 Transport.ResponseHeaderTimeout（connect+read）拦；
		//   - 「这条流跑太久」由 MAX_STREAM_DURATION 在 relay 层用 context 拦；
		//   - 「客户端断开」由请求自己的 ctx 取消。
		http: &http.Client{Transport: transport},
	}
}

func (c *Client) endpoint(path string) string {
	base := strings.TrimRight(c.baseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + path
	}
	return base + "/v1" + path
}

func (c *Client) request(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{Code: "upstream_request_invalid"}
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, &Error{Code: "upstream_connection_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return nil, &Error{Code: "upstream_timeout"}
		}
		return nil, &Error{Code: "upstream_connection_failed"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		code := "upstream_http_error"
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "upstream_authentication_failed"
		case http.StatusTooManyRequests:
			code = "upstream_rate_limited"
		case 400, 404, 422:
			code = "upstream_request_rejected"
		case 500, 502, 503, 504:
			code = "upstream_unavailable"
		}
		return nil, &Error{Code: code, Status: resp.StatusCode}
	}
	return resp, nil
}

func (c *Client) ListModels(ctx context.Context) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Code: "upstream_response_invalid"}
	}
	return data, nil
}

func (c *Client) ChatJSON(ctx context.Context, payload map[string]any) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Code: "upstream_response_invalid"}
	}
	return data, nil
}

func (c *Client) Stream(ctx context.Context, payload map[string]any) (*http.Response, error) {
	copyPayload := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		copyPayload[key] = value
	}
	options, ok := copyPayload["stream_options"].(map[string]any)
	if !ok || options == nil {
		options = make(map[string]any)
	}
	options["include_usage"] = true
	copyPayload["stream_options"] = options
	copyPayload["stream"] = true
	return c.request(ctx, http.MethodPost, "/chat/completions", copyPayload)
}
