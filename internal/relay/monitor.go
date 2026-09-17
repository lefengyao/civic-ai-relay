package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

// ModelProbe 是监测所需的唯一上游能力：拉一次模型目录，用来判断渠道是否可达。
// 之所以用 GET /v1/models 而不是发一次对话请求：它是只读元数据接口，不产生
// token 费用，也几乎不占上游速率额度。
type ModelProbe interface {
	PreviewModels(ctx context.Context, providerID int64) ([]string, error)
}

const (
	// monitorProbeTimeout 是单次渠道探活的超时。探测的是元数据接口，不该套用
	// 上游那套分钟级的读取超时——否则一个挂掉的渠道就能拖住整轮监测。
	monitorProbeTimeout = 15 * time.Second
	// monitorMaxConcurrentProbes 限制并发探活数，渠道多时避免一口气打爆上游限流。
	monitorMaxConcurrentProbes = 4
)

// Monitor 按固定间隔检查每个分组是否还能正常服务。
//
// 定位：**只发现与记录，不改变服务行为**。它不会停用渠道、不会拒绝请求，
// 只把结果落库并写一条日志，由运维决定怎么处理。
type Monitor struct {
	store  *store.Store
	probes ModelProbe
	now    func() time.Time
}

func NewMonitor(repo *store.Store, probes ModelProbe) *Monitor {
	return &Monitor{store: repo, probes: probes, now: func() time.Time { return time.Now().UTC() }}
}

// RunOnce 监测所有分组，返回已落盘的结果数。分组之间并发执行，单个分组出错
// 不影响其它分组。
func (m *Monitor) RunOnce(ctx context.Context, trigger string) (int, error) {
	targets, err := m.store.GroupMonitorTargets(ctx)
	if err != nil {
		return 0, err
	}
	results := make([]store.GroupMonitorResult, len(targets))
	sem := make(chan struct{}, monitorMaxConcurrentProbes)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target store.GroupMonitorTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = m.evaluate(ctx, target, trigger)
		}(i, target)
	}
	wg.Wait()
	saved := 0
	for _, result := range results {
		if err := m.store.SaveGroupMonitorResult(ctx, result); err != nil {
			return saved, err
		}
		m.report(result)
		saved++
	}
	return saved, nil
}

// RunGroup 立即监测单个分组（管理台的「立即检测」）。
func (m *Monitor) RunGroup(ctx context.Context, groupID int64) (store.GroupMonitorResult, error) {
	if groupID <= 0 {
		return store.GroupMonitorResult{}, errors.New("group ID is required")
	}
	targets, err := m.store.GroupMonitorTargets(ctx)
	if err != nil {
		return store.GroupMonitorResult{}, err
	}
	for _, target := range targets {
		if target.GroupID != groupID {
			continue
		}
		result := m.evaluate(ctx, target, "manual")
		if err := m.store.SaveGroupMonitorResult(ctx, result); err != nil {
			return store.GroupMonitorResult{}, err
		}
		m.report(result)
		return result, nil
	}
	return store.GroupMonitorResult{}, fmt.Errorf("分组 #%d 不存在（可能已被删除，请刷新页面）", groupID)
}

// evaluate 先查配置是否成立，再对启用渠道做上游探活。配置不成立时**不做探活**：
// 既省请求，也避免用"上游可达"掩盖"这个组压根没配好"。
func (m *Monitor) evaluate(ctx context.Context, target store.GroupMonitorTarget, trigger string) store.GroupMonitorResult {
	started := m.now()
	result := store.GroupMonitorResult{
		GroupID:      target.GroupID,
		GroupName:    target.GroupName,
		Status:       store.GroupStatusOK,
		Trigger:      trigger,
		CreatedAtUTC: started.Format(time.RFC3339Nano),
	}
	finish := func(status, detail string) store.GroupMonitorResult {
		result.Status = status
		result.Detail = detail
		result.LatencyMS = m.now().Sub(started).Milliseconds()
		return result
	}
	if !target.Enabled {
		return finish(store.GroupStatusDisabled, "分组已停用，跳过探活")
	}
	providers := target.EnabledProviders()
	if len(providers) == 0 {
		return finish(store.GroupStatusConfigUnavailable, "组内没有启用中的渠道")
	}
	if target.PricedModelCount() == 0 {
		return finish(store.GroupStatusConfigUnavailable, "组内渠道下没有「已启用且已定价」的模型，客户端拿不到任何可用模型")
	}
	result.TotalProviders = len(providers)
	failures := make([]string, 0, len(providers))
	for _, provider := range providers {
		if err := m.probe(ctx, provider.ID); err != nil {
			failures = append(failures, provider.Name+"："+probeReason(err))
		}
	}
	result.FailedProviders = len(failures)
	switch len(failures) {
	case 0:
		return finish(store.GroupStatusOK, fmt.Sprintf("%d 个启用渠道全部可达", len(providers)))
	case len(providers):
		return finish(store.GroupStatusUpstreamUnreachable, "全部启用渠道探活失败 —— "+strings.Join(failures, "；"))
	default:
		return finish(store.GroupStatusDegraded, fmt.Sprintf(
			"%d/%d 个渠道探活失败，走这些渠道的模型会请求失败 —— %s",
			len(failures), len(providers), strings.Join(failures, "；")))
	}
}

func (m *Monitor) probe(ctx context.Context, providerID int64) error {
	probeCtx, cancel := context.WithTimeout(ctx, monitorProbeTimeout)
	defer cancel()
	_, err := m.probes.PreviewModels(probeCtx, providerID)
	return err
}

// report 只在非正常状态下打日志。正常与主动停用不刷日志，避免把容器日志淹掉。
func (m *Monitor) report(r store.GroupMonitorResult) {
	switch r.Status {
	case store.GroupStatusOK, store.GroupStatusDisabled:
		return
	}
	logging.Warnf("group monitor: 分组「%s」状态 %s —— %s", r.GroupName, r.Status, r.Detail)
}

// probeReason 把上游错误翻成运维能直接照着处理的说明。error 只会是
// upstream.Error 这类自带码，不会带出上游 Key（Authorization 头从不回显）。
func probeReason(err error) string {
	if err == nil {
		return ""
	}
	var upstreamErr *upstream.Error
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.Code {
		case "upstream_authentication_failed":
			return "上游拒绝认证，检查该渠道的上游 API Key 是否失效"
		case "upstream_rate_limited":
			return "被上游限流（429）"
		case "upstream_timeout":
			return "连接或读取超时（网络不通，或上游无响应）"
		case "upstream_connection_failed":
			return "无法建立连接（地址错误、DNS 失败或端口不通）"
		case "upstream_unavailable":
			return fmt.Sprintf("上游返回 %d（服务端故障）", upstreamErr.Status)
		case "upstream_request_rejected":
			return fmt.Sprintf("上游拒绝请求（%d），该地址可能不提供 /v1/models", upstreamErr.Status)
		}
		if upstreamErr.Status > 0 {
			return fmt.Sprintf("%s（HTTP %d）", upstreamErr.Code, upstreamErr.Status)
		}
		return upstreamErr.Code
	}
	return truncateReason(err.Error())
}

// truncateReason 把任意错误压成一行、限制长度：上游可能回一大段 HTML，
// 原样写进 detail 会撑爆管理台表格。
func truncateReason(text string) string {
	text = strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	runes := []rune(text)
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return text
}
