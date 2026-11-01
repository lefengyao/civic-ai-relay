package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func setupGroupScope(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	repo := newTestStore(t)
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(100)
	model, err := repo.CreateModel(ctx, NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return repo, provider.ID, model.ID
}

// 未绑分组的密钥没有可授权的模型，预留必须被拒——沿用计价查询的
// not-authorized 出口，但给出可诊断的错误消息而不是裸的 ErrNoRows。
func TestReserveRejectsUnboundKey(t *testing.T) {
	repo, _, modelID := setupGroupScope(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, modelID, "unbound", 600))
	if err == nil || !strings.Contains(err.Error(), "model is not authorized for key") {
		t.Fatalf("err = %v", err)
	}
}

// 切换分组不能搬历史：账目行的 group_id 记录请求发生时的归属。
func TestSwitchingGroupDoesNotMoveHistory(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-a", 600)); err != nil {
		t.Fatal(err)
	}
	var inA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&inA); err != nil {
		t.Fatal(err)
	}
	if inA != 1 {
		t.Fatalf("A 组预留数 = %d", inA)
	}

	if err := repo.SetKeyGroup(ctx, key.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	// 历史行不动
	var stillA int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupA.ID).Scan(&stillA); err != nil {
		t.Fatal(err)
	}
	if stillA != 1 {
		t.Fatalf("切换分组搬动了历史：A 组行数变成 %d", stillA)
	}
	// requests 表同样记录请求发生时的归属
	var reqGroupA int64
	if err := repo.db.QueryRow(`SELECT group_id FROM requests WHERE request_id='in-a'`).Scan(&reqGroupA); err != nil {
		t.Fatal(err)
	}
	if reqGroupA != groupA.ID {
		t.Fatalf("requests.group_id = %d, want %d", reqGroupA, groupA.ID)
	}
	// 新请求记到新组
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "in-b", 600)); err != nil {
		t.Fatal(err)
	}
	var inB int
	if err := repo.db.QueryRow(`SELECT count(*) FROM key_reservations WHERE key_id=? AND group_id=?`, key.ID, groupB.ID).Scan(&inB); err != nil {
		t.Fatal(err)
	}
	if inB != 1 {
		t.Fatalf("B 组预留数 = %d", inB)
	}
	var reqGroupB int64
	if err := repo.db.QueryRow(`SELECT group_id FROM requests WHERE request_id='in-b'`).Scan(&reqGroupB); err != nil {
		t.Fatal(err)
	}
	if reqGroupB != groupB.ID {
		t.Fatalf("requests.group_id after switch = %d, want %d", reqGroupB, groupB.ID)
	}
}

// 计价倍率取自唯一所属组（走预留事务里的报价查询，不只是 KeyModelRateMilli）。
func TestReserveQuotesRateFromSingleGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	premium, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "premium", RateMilli: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, premium.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, premium.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "quoted", 600))
	if err != nil {
		t.Fatal(err)
	}
	if res.QuoteRateMilli != 1500 {
		t.Fatalf("QuoteRateMilli = %d, want 1500", res.QuoteRateMilli)
	}
}

// 分组额度取代全局额度后，窗口判定的数据来源是分组行：A 组用满不影响 B 组。
func TestGroupQuotasAreIndependentAcrossGroups(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	// A 组当日 Token 限额 50：一次预留（约 608）就把窗口灌满
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{TokenLimitDaily: intPtr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "a-first", 600)); err != nil {
		t.Fatalf("A 组首笔应按先欠费后停服放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "a-second", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "daily" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want daily token_quota_exceeded", err)
	}
	// B 组没有限额：同样的负载完全不受 A 组影响
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "b-first", 600)); err != nil {
		t.Fatalf("B 组不应被 A 组的额度咬住：%v", err)
	}
	// 错误 detail 带分组信息（spec 第七节：运维要知道是哪个组咬住的）
	if quota.Group == "" || quota.GroupID != groupA.ID {
		t.Fatalf("quota detail 缺分组信息：group=%q id=%d", quota.Group, quota.GroupID)
	}
}

// 每组各自独立周期：A 组周期用满切周限额，B 组的周期不受影响。
func TestGroupPeriodsAreIndependent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	// A 组：周期 50 tokens（起点自动填 now）+ 本周 50。
	// 周期用满后按既有语义切到「本周」窗口（periodQuotaTransition 的交接），
	// 所以第 2 笔被放行、第 3 笔才被本周额度挡下——两笔都算在 A 组自己的账上。
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{
		TokenLimitPeriod: intPtr(50),
		TokenLimitWeekly: intPtr(50),
		PeriodLimitDays:  daysPtr(3),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "period-a-1", 600)); err != nil {
		t.Fatalf("A 组周期首笔应放行：%v", err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "period-a-2", 600)); err != nil {
		t.Fatalf("A 组周期用满后应切到本周窗口继续放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "period-a-3", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "weekly" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want weekly token_quota_exceeded", err)
	}
	if quota.GroupID != groupA.ID {
		t.Fatalf("quota 归属 = #%d, want A 组 #%d", quota.GroupID, groupA.ID)
	}
	// 周期是 A 组的状态：没有配周期也没有额度的 B 组完全不受影响
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "period-b-1", 600)); err != nil {
		t.Fatalf("B 组不应感知 A 组的周期：%v", err)
	}
}

// 8 个维度里既有测试没覆盖的三条：5h Token、5h 金额、本周金额。
func TestFiveHourTokenWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{TokenLimit5H: intPtr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-token-1", 600)); err != nil {
		t.Fatalf("首笔应按先欠费后停服放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-token-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "5h" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want 5h token_quota_exceeded", err)
	}
}

func TestFiveHourAmountWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{AmountLimit5H: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-amount-1", 600)); err != nil {
		t.Fatalf("首笔应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "five-amount-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "5h" || quota.Code != "amount_quota_exceeded" || quota.Metric != "amount" {
		t.Fatalf("err = %v, want 5h amount_quota_exceeded", err)
	}
}

func TestWeeklyAmountWindowBlocksAfterItIsSpent(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{AmountLimitWeekly: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "week-amount-1", 600)); err != nil {
		t.Fatalf("首笔应放行：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "week-amount-2", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "weekly" || quota.Code != "amount_quota_exceeded" {
		t.Fatalf("err = %v, want weekly amount_quota_exceeded", err)
	}
}

// 周期起点自动填充之后，周期额度必须真的生效——这是 settings.go 那个
// 「只填天数、起点留空」的坑在分组层的对应防线。
func TestAutoFilledPeriodStartTakesEffect(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, group.ID, UpdateModelGroup{
		TokenLimitPeriod: intPtr(50),
		TokenLimitWeekly: intPtr(50),
		PeriodLimitDays:  daysPtr(3),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "auto-period-1", 600)); err != nil {
		t.Fatalf("首笔应放行：%v", err)
	}
	// 判别信号：起点被自动填充 → 第一笔落在周期内 → 周期用满后切到本周窗口
	// （从切换点起算），第二笔仍放行。若起点没被填充，周期不生效、本周窗口从
	// 周一算起，第二笔会立刻被本周额度拒掉——正是这里要防的静默失效。
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "auto-period-2", 600)); err != nil {
		t.Fatalf("起点自动填充后第二笔应切到本周窗口放行，实际被拒：%v", err)
	}
	_, err = repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "auto-period-3", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "weekly" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want weekly token_quota_exceeded（周期交接后的本周额度）", err)
	}
}

// GroupOverview 的限额来自分组行，且统计口径是「归属该组的请求」（group_id 列），
// 不再是「走组内渠道的请求」——共享渠道不再被重复计入多个组。
func TestGroupOverviewCarriesGroupLimits(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ModelGroup{groupA, groupB} {
		if err := repo.ReplaceGroupProviders(ctx, g.ID, []int64{providerID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.UpdateModelGroup(ctx, groupA.ID, UpdateModelGroup{TokenLimitDaily: intPtr(5000)}); err != nil {
		t.Fatal(err)
	}
	keyA, err := repo.CreateClientKey(ctx, NewClientKey{Name: "ka", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyA.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(keyA.ID, modelID, "ov-a", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, res.ID, 10, 20, 0, 30, "completed"); err != nil {
		t.Fatal(err)
	}
	ov, err := repo.GroupOverview(ctx, groupA.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ov["daily"].(map[string]any)["limit"]; got != int64(5000) {
		t.Fatalf("daily limit = %v, want 5000（来自分组行）", got)
	}
	// keyB 的请求不属于 A 组，即使渠道共享：B 组的组概览看得到，A 组看不到
	keyB, err := repo.CreateClientKey(ctx, NewClientKey{Name: "kb", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, keyB.ID, groupB.ID); err != nil {
		t.Fatal(err)
	}
	resB, err := repo.ReserveRequest(ctx, newReservationArgs(keyB.ID, modelID, "ov-b", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, resB.ID, 7, 8, 0, 15, "completed"); err != nil {
		t.Fatal(err)
	}
	ovA, err := repo.GroupOverview(ctx, groupA.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ovA["daily"].(map[string]any)["used_tokens"]; got != int64(30) {
		t.Fatalf("A 组 daily tokens = %v, want 30（B 组的 15 不计入）", got)
	}
	ovB, err := repo.GroupOverview(ctx, groupB.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got := ovB["daily"].(map[string]any)["used_tokens"]; got != int64(15) {
		t.Fatalf("B 组 daily tokens = %v, want 15", got)
	}
	if got := ovB["group_name"]; got != "b" {
		t.Fatalf("group_name = %v, want b", got)
	}
}

// 删组影响面：删除前能数出将解绑多少把密钥（spec 5.1 末尾）。
func TestCountKeysInGroup(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setupGroupScope(t)
	groupA, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := repo.CountKeysInGroup(ctx, groupA.ID)
	if err != nil || n != 0 {
		t.Fatalf("n = %d, %v", n, err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = repo.CountKeysInGroup(ctx, groupA.ID); err != nil || n != 1 {
		t.Fatalf("n = %d, %v", n, err)
	}
}

// 分组列表的紧张度：只有三个主窗口的 Token 限额参与，全部不限时为 0。
func TestGroupUsageBucketsGroupsByOwnership(t *testing.T) {
	ctx := context.Background()
	repo, providerID, modelID := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, modelID, "bucket", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(ctx, res.ID, 10, 20, 0, 30, "completed"); err != nil {
		t.Fatal(err)
	}
	five, daily, weekly, err := repo.GroupUsageBuckets(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if daily[group.ID][0] != 30 || five[group.ID][0] != 30 || weekly[group.ID][0] != 30 {
		t.Fatalf("buckets = five %v daily %v weekly %v, want 30 each", five[group.ID], daily[group.ID], weekly[group.ID])
	}
}

// 播种把全局额度值灌进分组「仍为 NULL」的列：跑两次结果一致，
// 已有值的列不被覆盖（写库成功但 relay.env 删键失败而重跑时，不覆盖运维的手工修改）。
func TestSeedGroupQuotasFillsOnlyNullColumns(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	if _, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g1"}); err != nil {
		t.Fatal(err)
	}
	g2, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	// g2 预先手工设过 5h=999：播种不得覆盖
	if _, err := repo.UpdateModelGroup(ctx, g2.ID, UpdateModelGroup{TokenLimit5H: intPtr(999)}); err != nil {
		t.Fatal(err)
	}
	seed := GlobalQuotaSeed{Token5H: 100000, TokenDaily: 20000}
	n, err := repo.SeedGroupQuotas(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded = %d, want 2", n)
	}
	if _, err := repo.SeedGroupQuotas(ctx, seed); err != nil {
		t.Fatalf("重跑播种出错：%v", err)
	}
	listed, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range listed {
		if g.TokenLimit5H == nil {
			t.Fatalf("%s 未被播种", g.Name)
		}
		want := int64(100000)
		if g.ID == g2.ID {
			want = 999
		}
		if *g.TokenLimit5H != want {
			t.Fatalf("%s token_limit_5h = %d, want %d", g.Name, *g.TokenLimit5H, want)
		}
		if g.TokenLimitDaily == nil || *g.TokenLimitDaily != 20000 {
			t.Fatalf("%s token_limit_daily = %v", g.Name, g.TokenLimitDaily)
		}
		if g.TokenLimitWeekly != nil {
			t.Fatalf("%s 未涉及的窗口被误设：%v", g.Name, g.TokenLimitWeekly)
		}
	}
}
