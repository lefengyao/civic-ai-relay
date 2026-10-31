// Package upstream implements the small OpenAI-compatible transport boundary.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"civic-ai-relay/internal/requestmeta"
)

type Client struct {
	baseURL       string
	apiKey        string
	http          *http.Client
	options       Options
	userAgent     string
	sessionHeader string
	protocol      string
}

type Error struct {
	Code   string
	Status int
	Stage  string
	Cause  error
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

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func New(baseURL, apiKey string, connect, read, write, pool time.Duration) *Client {
	return NewWithOptions(baseURL, apiKey, Options{
		ConnectTimeout: connect, ReadTimeout: read, WriteTimeout: write, PoolTimeout: pool,
	})
}

// NewWithProtocol selects a native upstream protocol while preserving Civic's
// internal OpenAI-compatible response contract.
func NewWithProtocol(baseURL, apiKey, protocol string, options Options) *Client {
	client := NewWithOptions(baseURL, apiKey, options)
	client.protocol, _ = normalizeProtocol(protocol)
	return client
}

func normalizeProtocol(protocol string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "openai":
		return "openai", nil
	case "anthropic", "gemini":
		return strings.ToLower(strings.TrimSpace(protocol)), nil
	default:
		return "", fmt.Errorf("unsupported upstream protocol %q", protocol)
	}
}

func NewWithOptions(baseURL, apiKey string, options Options) *Client {
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = 10 * time.Second
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = 300 * time.Second
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = 30 * time.Second
	}
	if options.PoolTimeout <= 0 {
		options.PoolTimeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: options.ConnectTimeout}
	transport := &http.Transport{
		MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: options.PoolTimeout,
		DialContext: dialer.DialContext, TLSHandshakeTimeout: options.ConnectTimeout,
		ResponseHeaderTimeout: effectiveHeaderTimeout(options.HeaderTimeout, options.ConnectTimeout, options.ReadTimeout),
	}
	userAgent := strings.TrimSpace(options.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	sessionHeader := strings.TrimSpace(options.UpstreamSessionHeader)
	switch {
	case sessionHeader == "-":
		sessionHeader = ""
	case sessionHeader == "" && isOpenCodeEndpoint(baseURL):
		sessionHeader = SessionIDHeader
	}
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		options:   options,
		userAgent: userAgent, sessionHeader: sessionHeader,
		protocol: "openai",
		// ⚠️ 刻意**不设** http.Client.Timeout。它是「整个请求」的总超时，**包含读取
		// 响应体的全部时间**，对流式回答等于一道固定的硬闸：默认 connect+read+write
		// = 10+300+30 = 340 秒，任何超过它的流式对话都会在那一刻被无声掐断，
		// 而记录里只留下 upstream_status=200 + 零 token 的 failed，运维完全看不出
		// 是超时（2026-09-25 线上真实故障）。服务端配的 MAX_STREAM_DURATION 也因此
		// 形同虚设。现在职责拆开：
		//   - 「上游迟迟不响应」由 Transport.ResponseHeaderTimeout 拦；
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
	var wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wroteRequest.Store(true)
		}
	}}
	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(requestCtx, trace), method, c.endpoint(path), body)
	if err != nil {
		cancel()
		return nil, &Error{Code: "upstream_connection_failed", Stage: "connect", Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.sessionHeader != "" {
		session := SessionIDFromContext(ctx)
		if session == "" {
			session = deriveSessionID(c.apiKey, payload)
		}
		req.Header.Set(c.sessionHeader, session)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return nil, &Error{Code: "client_canceled", Stage: "request", Cause: err}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &Error{Code: "request_duration_exceeded", Stage: "duration", Cause: err}
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			if wroteRequest.Load() {
				return nil, &Error{Code: "upstream_header_timeout", Stage: "header", Cause: err}
			}
			return nil, &Error{Code: "upstream_connect_timeout", Stage: "connect", Cause: err}
		}
		return nil, &Error{Code: "upstream_connection_failed", Stage: "connect", Cause: err}
	}
	if trace := requestmeta.FromContext(ctx); trace != nil {
		trace.MarkHeader()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cancel()
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
		return nil, &Error{Code: code, Status: resp.StatusCode, Stage: "header"}
	}
	resp.Body = newTimedBody(requestCtx, cancel, resp.Body, resp.StatusCode, c.options.IdleTimeout)
	return resp, nil
}

func (c *Client) ListModels(ctx context.Context) ([]byte, error) {
	if c.protocol != "" && c.protocol != "openai" {
		return c.listModelsNative(ctx)
	}
	resp, err := c.request(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if trace := requestmeta.FromContext(ctx); trace != nil {
			trace.MarkUpstream()
		}
	}
	if err != nil {
		return nil, responseReadError(ctx, err, resp.StatusCode)
	}
	return data, nil
}

func (c *Client) ChatJSON(ctx context.Context, payload map[string]any) ([]byte, error) {
	if c.protocol != "" && c.protocol != "openai" {
		return c.chatNative(ctx, payload)
	}
	resp, err := c.request(ctx, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if trace := requestmeta.FromContext(ctx); trace != nil {
			trace.MarkUpstream()
		}
	}
	if err != nil {
		return nil, responseReadError(ctx, err, resp.StatusCode)
	}
	return data, nil
}

func responseReadError(ctx context.Context, err error, status int) *Error {
	var upstreamErr *Error
	if errors.As(err, &upstreamErr) && upstreamErr.Code != "upstream_stream_read_failed" {
		return upstreamErr
	}
	code, stage := "upstream_response_invalid", "body"
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		code, stage = "client_canceled", "request"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		code, stage = "request_duration_exceeded", "duration"
	}
	return &Error{Code: code, Status: status, Stage: stage, Cause: err}
}

func (c *Client) Stream(ctx context.Context, payload map[string]any) (*http.Response, error) {
	if c.protocol != "" && c.protocol != "openai" {
		return c.streamNative(ctx, payload)
	}
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
