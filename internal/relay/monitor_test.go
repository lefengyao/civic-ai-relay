package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

// fakeProbe 记录每个渠道被探活的次数，并可按渠道返回指定错误。
type fakeProbe struct {
	mu     sync.Mutex
	calls  map[int64]int
	errs   map[int64]error
	result []string
}

func newFakeProbe() *fakeProbe {
	return &fakeProbe{calls: map[int64]int{}, errs: map[int64]error{}, result: []string{"upstream-model"}}
}

func (f *fakeProbe) PreviewModels(_ context.Context, providerID int64) ([]string, error) {
	f.mu.Lock()
	f.calls[providerID]++
	err := f.errs[providerID]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.result, nil
}

func (f *fakeProbe) callCount(providerID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[providerID]
}

func (f *fakeProbe) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.calls {
		total += n
	}
	return total
}

func (f *fakeProbe) failFor(providerID int64, err error) {
	f.mu.Lock()
	f.errs[providerID] = err
	f.mu.Unlock()
}

func monitorFixture(t *testing.T) (*store.Store, *fakeProbe) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	box, err := secret.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo, newFakeProbe()
}

// addProvider 建一个启用中的渠道，可选挂一个已定价启用的模型。
//
// 注意：渠道只能在**启用状态**下入组（ReplaceGroupProviders 拒绝停用渠道），
// 所以「组里有停用渠道」只能靠先入组、再用 setProviderEnabled 停用产生。
func addProvider(t *testing.T, repo *store.Store, name string, withPricedModel bool) store.Provider {
	t.Helper()
	ctx := context.Background()
	p, err := repo.CreateProvider(ctx, store.NewProvider{Name: name, BaseURL: "https://" + name + ".example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if withPricedModel {
		price := int64(1000)
		if _, err := repo.CreateModel(ctx, store.NewModel{
			ProviderID: p.ID, PublicName: name + "/m", UpstreamName: "m",
			InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func setProviderEnabled(t *testing.T, repo *store.Store, providerID int64, enabled bool) {
	t.Helper()
	if _, err := repo.UpdateProvider(context.Background(), providerID, store.UpdateProvider{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
}

func addGroup(t *testing.T, repo *store.Store, name string, providerIDs []int64, enabled bool) store.ModelGroup {
	t.Helper()
	ctx := context.Background()
	g, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, g.ID, providerIDs); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		no := false
		if _, err := repo.UpdateModelGroup(ctx, g.ID, store.UpdateModelGroup{Enabled: &no}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// 停用的分组不探活：既省请求，也不该把「已停用」这种正常状态报成上游故障。
func TestMonitorReportsDisabledGroupWithoutProbing(t *testing.T) {
	repo, probe := monitorFixture(t)
	p := addProvider(t, repo, "p1", true)
	group := addGroup(t, repo, "停用组", []int64{p.ID}, false)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != store.GroupStatusDisabled {
		t.Fatalf("status = %q, want disabled: %s", result.Status, result.Detail)
	}
	if probe.totalCalls() != 0 {
		t.Fatalf("probe was called %d time(s) for a disabled group", probe.totalCalls())
	}
}

// 配置不成立时不探活：否则「上游可达」会掩盖「这个组压根没配好」。
func TestMonitorConfigChecksShortCircuitProbing(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T, repo *store.Store) int64
		wantSub string
	}{
		{
			name: "组内没有启用中的渠道",
			build: func(t *testing.T, repo *store.Store) int64 {
				p := addProvider(t, repo, "off", true)
				group := addGroup(t, repo, "g", []int64{p.ID}, true)
				setProviderEnabled(t, repo, p.ID, false) // 入组后再停用
				return group.ID
			},
			wantSub: "没有启用中的渠道",
		},
		{
			name: "渠道下没有已定价模型",
			build: func(t *testing.T, repo *store.Store) int64 {
				p := addProvider(t, repo, "noprice", false)
				return addGroup(t, repo, "g", []int64{p.ID}, true).ID
			},
			wantSub: "已启用且已定价",
		},
		{
			name: "只有停用渠道带模型（停用渠道的模型不算可用）",
			build: func(t *testing.T, repo *store.Store) int64 {
				on := addProvider(t, repo, "on", false)
				off := addProvider(t, repo, "off", true)
				group := addGroup(t, repo, "g", []int64{on.ID, off.ID}, true)
				setProviderEnabled(t, repo, off.ID, false)
				return group.ID
			},
			wantSub: "已启用且已定价",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, probe := monitorFixture(t)
			groupID := tc.build(t, repo)
			result, err := NewMonitor(repo, probe).RunGroup(context.Background(), groupID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != store.GroupStatusConfigUnavailable {
				t.Fatalf("status = %q, want config_unavailable: %s", result.Status, result.Detail)
			}
			if !strings.Contains(result.Detail, tc.wantSub) {
				t.Fatalf("detail = %q, want it to mention %q", result.Detail, tc.wantSub)
			}
			if probe.totalCalls() != 0 {
				t.Fatalf("probe was called %d time(s) although the config is unusable", probe.totalCalls())
			}
		})
	}
}

func TestMonitorAllProvidersReachable(t *testing.T) {
	repo, probe := monitorFixture(t)
	first := addProvider(t, repo, "p1", true)
	second := addProvider(t, repo, "p2", true)
	group := addGroup(t, repo, "g", []int64{first.ID, second.ID}, true)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != store.GroupStatusOK || result.TotalProviders != 2 || result.FailedProviders != 0 {
		t.Fatalf("result = %#v, want ok with 2/2 reachable", result)
	}
	if probe.totalCalls() != 2 {
		t.Fatalf("probe calls = %d, want 2", probe.totalCalls())
	}
}

// 部分渠道挂掉时组整体仍然可服务，但走那些渠道的模型会失败——这个区别很重要，
// 所以单独给一个 degraded 状态，而不是简单判成「不可用」。
func TestMonitorDegradedWhenSomeProvidersFail(t *testing.T) {
	repo, probe := monitorFixture(t)
	healthy := addProvider(t, repo, "healthy", true)
	broken := addProvider(t, repo, "broken", true)
	probe.failFor(broken.ID, &upstream.Error{Code: "upstream_authentication_failed", Status: 401})
	group := addGroup(t, repo, "g", []int64{healthy.ID, broken.ID}, true)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != store.GroupStatusDegraded {
		t.Fatalf("status = %q, want degraded: %s", result.Status, result.Detail)
	}
	if result.TotalProviders != 2 || result.FailedProviders != 1 {
		t.Fatalf("counters = %d/%d, want 1 of 2 failed", result.FailedProviders, result.TotalProviders)
	}
	// 报障信息要落到具体渠道名和可行动的原因上，运维才知道去改哪一条
	if !strings.Contains(result.Detail, "broken") || !strings.Contains(result.Detail, "上游 API Key") {
		t.Fatalf("detail = %q, want the failing provider name and a reason", result.Detail)
	}
	if strings.Contains(result.Detail, "healthy") {
		t.Fatalf("detail should not list the healthy provider as failing: %q", result.Detail)
	}
}

func TestMonitorUnreachableWhenEveryProviderFails(t *testing.T) {
	repo, probe := monitorFixture(t)
	first := addProvider(t, repo, "p1", true)
	second := addProvider(t, repo, "p2", true)
	probe.failFor(first.ID, &upstream.Error{Code: "upstream_connection_failed"})
	probe.failFor(second.ID, &upstream.Error{Code: "upstream_timeout"})
	group := addGroup(t, repo, "g", []int64{first.ID, second.ID}, true)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != store.GroupStatusUpstreamUnreachable || result.FailedProviders != 2 {
		t.Fatalf("result = %#v, want upstream_unreachable 2/2", result)
	}
	if !strings.Contains(result.Detail, "无法建立连接") || !strings.Contains(result.Detail, "超时") {
		t.Fatalf("detail = %q, want both failure reasons", result.Detail)
	}
}

// 上游可能回一大段 HTML，detail 必须被压成可控长度，否则会撑爆管理台表格。
func TestMonitorTruncatesHugeUpstreamErrors(t *testing.T) {
	repo, probe := monitorFixture(t)
	p := addProvider(t, repo, "p1", true)
	probe.failFor(p.ID, errors.New(strings.Repeat("测", 2000)))
	group := addGroup(t, repo, "g", []int64{p.ID}, true)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Detail, "\n") {
		t.Fatalf("detail must stay on one line: %q", result.Detail)
	}
	if runes := []rune(result.Detail); len(runes) > 300 {
		t.Fatalf("detail has %d runes, want it truncated", len(runes))
	}
	if !strings.Contains(result.Detail, "…") {
		t.Fatalf("detail = %q, want a truncation marker", result.Detail)
	}
}

// 多行错误只保留第一行：后面的内容多半是 HTML 或堆栈，写进表格只会更难读。
func TestMonitorKeepsOnlyFirstLineOfMultiLineErrors(t *testing.T) {
	repo, probe := monitorFixture(t)
	p := addProvider(t, repo, "p1", true)
	probe.failFor(p.ID, errors.New("connection reset\n<html><body>gateway error</body></html>"))
	group := addGroup(t, repo, "g", []int64{p.ID}, true)

	result, err := NewMonitor(repo, probe).RunGroup(context.Background(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Detail, "connection reset") {
		t.Fatalf("detail = %q, want the first line kept", result.Detail)
	}
	if strings.Contains(result.Detail, "gateway error") {
		t.Fatalf("detail = %q, want the remaining lines dropped", result.Detail)
	}
}

func TestMonitorRunOnceWritesOneRowPerGroup(t *testing.T) {
	repo, probe := monitorFixture(t)
	p := addProvider(t, repo, "p1", true)
	addGroup(t, repo, "g1", []int64{p.ID}, true)
	addGroup(t, repo, "g2", []int64{p.ID}, true)
	// 一个渠道都没有的组也必须被记录，否则界面上它永远是「尚未检测」
	if _, err := repo.CreateModelGroup(context.Background(), store.NewModelGroup{Name: "g3-empty"}); err != nil {
		t.Fatal(err)
	}

	count, err := NewMonitor(repo, probe).RunOnce(context.Background(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("monitored = %d, want 3", count)
	}
	latest, err := repo.LatestGroupMonitorResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 3 {
		t.Fatalf("latest results = %d, want 3", len(latest))
	}
	for id, result := range latest {
		if result.Trigger != "auto" {
			t.Fatalf("group %d trigger = %q, want auto", id, result.Trigger)
		}
		if result.CreatedAtUTC == "" {
			t.Fatalf("group %d has no timestamp", id)
		}
	}
}

func TestMonitorRunGroupRejectsUnknownGroup(t *testing.T) {
	repo, probe := monitorFixture(t)
	if _, err := NewMonitor(repo, probe).RunGroup(context.Background(), 9999); err == nil {
		t.Fatal("expected an error for a missing group")
	}
}

func TestProbeReasonExplainsUpstreamCodes(t *testing.T) {
	cases := []struct {
		err     error
		wantSub string
	}{
		{&upstream.Error{Code: "upstream_authentication_failed", Status: 401}, "上游 API Key"},
		{&upstream.Error{Code: "upstream_rate_limited", Status: 429}, "限流"},
		{&upstream.Error{Code: "upstream_timeout"}, "超时"},
		{&upstream.Error{Code: "upstream_connection_failed"}, "无法建立连接"},
		{&upstream.Error{Code: "upstream_unavailable", Status: 502}, "502"},
		{&upstream.Error{Code: "upstream_request_rejected", Status: 404}, "/v1/models"},
		{errors.New("provider is disabled"), "provider is disabled"},
	}
	for _, tc := range cases {
		got := probeReason(tc.err)
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("probeReason(%v) = %q, want it to mention %q", tc.err, got, tc.wantSub)
		}
	}
	if probeReason(nil) != "" {
		t.Fatal("probeReason(nil) must be empty")
	}
}
