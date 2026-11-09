package relay

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

// 故障转移供应商（模型池）的运行时策略。
//
// 冷却只影响「优先试谁」，不影响「能不能试」：所有候选在每次请求里都会被尝试，
// 只是最近失败过的成员被排到后面。全都在冷却中时仍按配置顺序走，避免因为一次
// 抖动就把整个池锁死。冷却只存在进程内存里，重启即清零，不写库、不改变配置。
const poolCooldownDuration = 60 * time.Second

type poolHealth struct {
	mu    sync.Mutex
	until map[int64]time.Time
}

func (h *poolHealth) mark(modelID int64, now time.Time) {
	if modelID <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil {
		h.until = make(map[int64]time.Time)
	}
	h.until[modelID] = now.Add(poolCooldownDuration)
}

func (h *poolHealth) clear(modelID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.until, modelID)
}

func (h *poolHealth) snapshot() map[int64]time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[int64]time.Time, len(h.until))
	for id, until := range h.until {
		out[id] = until
	}
	return out
}

// orderPoolCandidates 是稳定排序：未冷却的候选保持原优先级顺序在前，冷却中的
// 成员按原顺序排后。纯函数，便于测试。
func orderPoolCandidates(models []store.Model, cooldown map[int64]time.Time, now time.Time) []store.Model {
	out := make([]store.Model, 0, len(models))
	deferred := make([]store.Model, 0)
	for _, model := range models {
		if until, ok := cooldown[model.ID]; ok && until.After(now) {
			deferred = append(deferred, model)
			continue
		}
		out = append(out, model)
	}
	return append(out, deferred...)
}

func poolFailureCode(err error) string {
	var upstreamErr *upstream.Error
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Code
	}
	return "unknown"
}

// poolClientError 把「拿不到该成员的上游客户端」（渠道配置被删、密钥解不开等）
// 归一成可切换的上游故障，让池里其它成员仍有机会服务。
func poolClientError(err error) error {
	return &upstream.Error{Code: "upstream_connection_failed", Cause: err}
}

// retryablePoolFailure 只把「这个成员不可用」的故障判为可切换。
//
// 不切换的情况同样重要：400/422 是请求体本身有问题（换供应商也一样错），
// 客户端取消和整体时长超限也救不回来；贸然切换只会放大上游流量与账单噪声。
// 401/403/404 视为可切换：上游凭据失效、模型下架正是故障转移要兜住的场景。
func retryablePoolFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var upstreamErr *upstream.Error
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.Code {
	case "client_canceled", "downstream_write_failed", "upstream_request_invalid", "request_duration_exceeded":
		return false
	}
	switch upstreamErr.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return false
	}
	if upstreamErr.Status >= 500 || upstreamErr.Status == http.StatusTooManyRequests ||
		upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden ||
		upstreamErr.Status == http.StatusNotFound {
		return true
	}
	switch upstreamErr.Code {
	case "upstream_connection_failed", "upstream_connect_timeout", "upstream_header_timeout",
		"upstream_unavailable", "upstream_rate_limited", "upstream_authentication_failed",
		"upstream_response_invalid", "upstream_stream_read_failed":
		return true
	}
	return false
}
