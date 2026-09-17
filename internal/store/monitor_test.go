package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGroupMonitorTargetsDescribesGroupsAndProviders(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	price := int64(1000)
	priced, err := repo.CreateProvider(ctx, NewProvider{Name: "priced", BaseURL: "https://a.example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModel(ctx, NewModel{ProviderID: priced.ID, PublicName: "a/m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 未定价的模型不算可用
	bare, err := repo.CreateProvider(ctx, NewProvider{Name: "bare", BaseURL: "https://b.example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModel(ctx, NewModel{ProviderID: bare.ID, PublicName: "b/m", UpstreamName: "m", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{priced.ID, bare.ID}); err != nil {
		t.Fatal(err)
	}
	// 一个渠道都没有的组也必须出现在快照里，否则界面上它永远停在「尚未检测」
	empty, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "empty"})
	if err != nil {
		t.Fatal(err)
	}

	targets, err := repo.GroupMonitorTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2（含没有渠道的组）", len(targets))
	}
	byID := map[int64]GroupMonitorTarget{}
	for _, target := range targets {
		byID[target.GroupID] = target
	}
	first := byID[group.ID]
	if !first.Enabled || first.GroupName != "g" || len(first.Providers) != 2 {
		t.Fatalf("group target = %#v", first)
	}
	if got := first.PricedModelCount(); got != 1 {
		t.Fatalf("priced models = %d, want 1（bare 渠道的模型没定价）", got)
	}
	providers := map[string]MonitorProvider{}
	for _, p := range first.Providers {
		providers[p.Name] = p
	}
	if providers["priced"].PricedModels != 1 || providers["bare"].PricedModels != 0 {
		t.Fatalf("provider priced counts = %#v", providers)
	}
	blank := byID[empty.ID]
	if len(blank.Providers) != 0 || blank.PricedModelCount() != 0 || !blank.Enabled {
		t.Fatalf("empty group target = %#v", blank)
	}
}

func TestGroupMonitorResultRoundTripAndHistory(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		result := GroupMonitorResult{
			GroupID: group.ID, Status: GroupStatusOK, Detail: "第 " + string(rune('1'+i)) + " 轮",
			TotalProviders: 2, FailedProviders: i % 2, LatencyMS: int64(10 * i),
			Trigger: "auto", CreatedAtUTC: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
		}
		if err := repo.SaveGroupMonitorResult(ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := repo.LatestGroupMonitorResult(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Detail != "第 5 轮" || latest.GroupName != "g" || latest.LatencyMS != 40 {
		t.Fatalf("latest = %#v", latest)
	}
	history, err := repo.GroupMonitorHistory(ctx, group.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history len = %d, want 3", len(history))
	}
	if history[0].Detail != "第 5 轮" || history[2].Detail != "第 3 轮" {
		t.Fatalf("history must be newest-first: %#v", history)
	}
	// 默认触发方式是定时轮询
	if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: group.ID, Status: GroupStatusOK}); err != nil {
		t.Fatal(err)
	}
	manual, err := repo.LatestGroupMonitorResult(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Trigger != "auto" {
		t.Fatalf("trigger = %q, want auto by default", manual.Trigger)
	}
	if manual.CreatedAtUTC == "" {
		t.Fatal("timestamp must be filled in when omitted")
	}

	all, err := repo.LatestGroupMonitorResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[group.ID].Detail != "" {
		t.Fatalf("latest map = %#v, want exactly one entry for the group", all)
	}
}

func TestGroupMonitorResultValidatesCounters(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: 0, Status: GroupStatusOK}); err == nil {
		t.Fatal("expected an error for a missing group ID")
	}
	if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: group.ID, Status: GroupStatusOK, TotalProviders: 1, FailedProviders: 2}); err == nil {
		t.Fatal("expected an error when failed > total")
	}
}

// 删掉分组必须把它的监测历史一并带走，否则 latest 查询会去 JOIN 一个不存在的组。
func TestDeletingGroupCascadesMonitorResults(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: group.ID, Status: GroupStatusOK}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.DeleteModelGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	history, err := repo.GroupMonitorHistory(ctx, group.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("history survived the group deletion: %#v", history)
	}
	all, err := repo.LatestGroupMonitorResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("latest map = %#v, want empty", all)
	}
}

func TestPruneGroupMonitorResultsByAgeAndPerGroupCap(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: group.ID, Status: GroupStatusOK, CreatedAtUTC: old.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	// 超过每组上限的部分
	for i := 0; i < monitorRowsPerGroup+5; i++ {
		if err := repo.SaveGroupMonitorResult(ctx, GroupMonitorResult{GroupID: group.ID, Status: GroupStatusOK, CreatedAtUTC: recent.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := repo.PruneGroupMonitorResults(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	if removed <= 0 {
		t.Fatalf("pruned = %d, want the over-cap rows to be deleted", removed)
	}
	history, err := repo.GroupMonitorHistory(ctx, group.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) > monitorRowsPerGroup {
		t.Fatalf("history = %d rows, want at most %d", len(history), monitorRowsPerGroup)
	}
	for _, row := range history {
		if strings.HasPrefix(row.CreatedAtUTC, "2026-01-01") {
			t.Fatalf("row older than the cutoff survived: %#v", row)
		}
	}
}
