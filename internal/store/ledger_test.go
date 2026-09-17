package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func newReservationFixture(t *testing.T, tokenLimit, amountLimit int64) (repo *Store, key ClientKey, model Model) {
	t.Helper()
	repo = newTestStore(t)
	p, err := repo.CreateProvider(context.Background(), NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	model, err = repo.CreateModel(context.Background(), NewModel{ProviderID: p.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	g, err := repo.CreateModelGroup(context.Background(), NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(context.Background(), g.ID, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	// Leave room for the conservative prompt estimate used by reservations.
	tokenLimit += 100
	key, err = repo.CreateClientKey(context.Background(), NewClientKey{Name: "k", ConcurrencyLimit: 2, TokenLimit: &tokenLimit, AmountLimitMicroyuan: &amountLimit})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(context.Background(), key.ID, []int64{g.ID}); err != nil {
		t.Fatal(err)
	}
	return repo, key, model
}

func newReservationArgs(keyID, modelID int64, requestID string, tokens int64) ReserveInput {
	return ReserveInput{
		RequestID:            requestID,
		KeyID:                keyID,
		ModelID:              modelID,
		ProviderID:           1,
		StartedAt:            time.Now().UTC(),
		Stream:               true,
		InputText:            "prompt text",
		OutputTokenCeiling:   tokens,
		MaxOutputTokens:      tokens,
		InputPriceMicroyuan:  1000,
		OutputPriceMicroyuan: 1000,
		// 窗口限额给得很宽，只有测试显式覆盖的窗口才会成为约束。
		Limits: QuotaLimits{RPMLimit: 30, Token5H: 100000, TokenDaily: 20000, TokenWeekly: 1000000},
	}
}

// setConcurrency 把 Key 的并发上限调高，让额度判定成为唯一约束。
func setConcurrency(t *testing.T, repo *Store, keyID int64, limit int) {
	t.Helper()
	if _, err := repo.UpdateClientKey(context.Background(), keyID, UpdateClientKey{ConcurrencyLimit: &limit}); err != nil {
		t.Fatal(err)
	}
}

// 先欠费后停服：只要窗口/总额度还没用满就放行（哪怕"已用 + 本次预留"已经越过
// 限额），用到限额之后才拒绝。旧口径要求预留必须整体塞得下，导致"还剩额度却提示
// 额度不足"，这是本次改造要消灭的现象。
func TestOverdraftIsAllowedUntilQuotaIsSpent(t *testing.T) {
	repo, key, model := newReservationFixture(t, 600, 1_000_000) // Key token 总额度 = 700
	setConcurrency(t, repo, key.ID, 8)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		// 每次预留 inputTokens(8) + 600 = 608；两次累计 1216 已经越过 700，
		// 但每一次发起时窗口都还没用满，所以都应该被放行。
		if _, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, fmt.Sprintf("overdraft-%d", i), 600)); err != nil {
			t.Fatalf("reservation %d should have been allowed to overdraft: %v", i, err)
		}
	}
	// 额度已欠费，后续请求才被挡住。
	_, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, "overdraft-blocked", 600))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Code != "key_token_quota_exceeded" {
		t.Fatalf("err = %v, want key_token_quota_exceeded once the quota is spent", err)
	}
	if quota.Scope != "key" || quota.Metric != "tokens" || quota.Limit != 700 {
		t.Fatalf("quota detail = %#v, want scope=key metric=tokens limit=700", quota)
	}
	if quota.Detail() == "" {
		t.Fatal("quota error must carry a human-readable detail")
	}
}

// 并发预留必须串行化：Key 并发上限为 1 时两个同时发起的请求只能有一个进入。
// 旧的"只允许一个最终配额占用"断言依赖已被移除的预留口径，改为直接验证并发闸门
// ——它同样依赖 reservationMu + BEGIN IMMEDIATE 的串行化。
func TestConcurrentReservationsRespectKeyConcurrency(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	setConcurrency(t, repo, key.ID, 1)
	args := newReservationArgs(key.ID, model.ID, "request-concurrent", 10)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input := args
			input.RequestID = fmt.Sprintf("request-%d", i)
			_, err := repo.ReserveRequest(context.Background(), input)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	succeeded, rejected := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		var quota *QuotaError
		if !errors.As(err, &quota) || quota.Code != "concurrency_exceeded" {
			t.Fatalf("unexpected rejection: %v", err)
		}
		rejected++
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded=%d rejected=%d, want exactly 1 of each", succeeded, rejected)
	}
}

func TestWeeklyTokenWindowBlocksAfterItIsSpent(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1_000_000, 1_000_000)
	setConcurrency(t, repo, key.ID, 8)
	ctx := context.Background()
	args := newReservationArgs(key.ID, model.ID, "weekly-1", 600)
	args.Limits.TokenWeekly = 50 // 远小于单次预留，但首次应放行
	if _, err := repo.ReserveRequest(ctx, args); err != nil {
		t.Fatalf("first request must pass under the overdraft rule: %v", err)
	}
	args.RequestID = "weekly-2"
	_, err := repo.ReserveRequest(ctx, args)
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "weekly" || quota.Code != "token_quota_exceeded" {
		t.Fatalf("err = %v, want weekly token_quota_exceeded", err)
	}
}

func TestDailyAmountWindowBlocksAfterItIsSpent(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1_000_000, 1_000_000)
	setConcurrency(t, repo, key.ID, 8)
	ctx := context.Background()
	args := newReservationArgs(key.ID, model.ID, "amount-1", 600)
	args.Limits.AmountDaily = 1 // 单次预留就会产生 >= 1 微元
	if _, err := repo.ReserveRequest(ctx, args); err != nil {
		t.Fatalf("first request must pass under the overdraft rule: %v", err)
	}
	args.RequestID = "amount-2"
	_, err := repo.ReserveRequest(ctx, args)
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "daily" || quota.Code != "amount_quota_exceeded" || quota.Metric != "amount" {
		t.Fatalf("err = %v, want daily amount_quota_exceeded", err)
	}
}

// 限额为 0 表示不限：给 0 时任何请求都不应被额度挡住。
func TestZeroLimitsMeanUnlimited(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1_000_000, 1_000_000)
	setConcurrency(t, repo, key.ID, 8)
	ctx := context.Background()
	args := newReservationArgs(key.ID, model.ID, "unlimited", 600)
	args.Limits = QuotaLimits{RPMLimit: 30}
	for i := 0; i < 3; i++ {
		args.RequestID = fmt.Sprintf("unlimited-%d", i)
		if _, err := repo.ReserveRequest(ctx, args); err != nil {
			t.Fatalf("request %d rejected while every window is unlimited: %v", i, err)
		}
	}
}

func TestCancelledReservationReleasesGlobalAndKeyQuota(t *testing.T) {
	repo, key, model := newReservationFixture(t, 100, 1_000_000)
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-cancel", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CancelRequest(context.Background(), reservation.ID, "rejected", 429); err != nil {
		t.Fatal(err)
	}
	if got := repo.OccupiedTokens(context.Background(), reservation.StartedAt); got != 0 {
		t.Fatalf("occupied = %d", got)
	}
	if _, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-after-cancel", 100)); err != nil {
		t.Fatalf("quota was not released: %v", err)
	}
}

func TestRecentRequestMetadataContainsNoPromptOrCredential(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	_, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-redacted", 10))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := repo.RecentRequests(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(rows)
	if strings.Contains(got, "prompt text") || strings.Contains(got, key.Token) {
		t.Fatalf("sensitive data retained: %s", got)
	}
}

// 结算按上游回报的实际用量入账，并覆盖预留。Key 不再被自动停用：额度用满后
// 表现为"后续请求被拒"，而不是把 Key 永久置为停用（旧行为要让运维手工恢复）。
func TestSettledRequestChargesActualUsageAndKeyStaysEnabled(t *testing.T) {
	repo, key, model := newReservationFixture(t, 100, 1_000_000)
	limit := int64(11)
	if _, err := repo.UpdateClientKey(context.Background(), key.ID, UpdateClientKey{TokenLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	input := newReservationArgs(key.ID, model.ID, "request-settle", 10)
	input.InputTokens = 1 // 预留 1 + 10 = 11，刚好等于 Key 总额度
	reservation, err := repo.ReserveRequest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 5, 6, 2, 1000, "completed"); err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetClientKey(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled || current.DisabledReason != "" {
		t.Fatalf("settlement must not disable the key: %#v", current)
	}
	row, err := repo.RecentRequests(context.Background(), 1)
	if err != nil || len(row) != 1 {
		t.Fatalf("recent rows = %#v, err = %v", row, err)
	}
	if row[0]["cached_input_tokens"] != int64(2) || row[0]["input_tokens"] != int64(5) {
		t.Fatalf("cached tokens not recorded: %#v", row[0])
	}
	// 已用 11 >= 限额 11：欠费后新请求被拒（而不是 Key 被停用）。
	_, err = repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-after-settle", 10))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Code != "key_token_quota_exceeded" {
		t.Fatalf("err = %v, want key_token_quota_exceeded after the quota is spent", err)
	}
}

func TestHealthcheckAndPruneOperateOnMetadata(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	if err := repo.Healthcheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-prune", 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Prune(context.Background(), reservation.StartedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestGroupOverviewScopesToGroupProviders(t *testing.T) {
	repo, key, model := newReservationFixture(t, 600, 1_000_000)
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-group-overview", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 10, 20, 5, 30, "completed"); err != nil {
		t.Fatal(err)
	}
	overview, err := repo.GroupOverview(context.Background(), 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	rpm := overview["rpm"].(map[string]any)["used"].(int)
	if rpm != 1 {
		t.Fatalf("rpm used = %d, want 1", rpm)
	}
	if got := overview["five_hour"].(map[string]any)["used_tokens"].(int64); got != 30 {
		t.Fatalf("five_hour tokens = %d, want 30 (input+output after settle)", got)
	}
	daily := overview["daily"].(map[string]any)
	if daily["used_tokens"].(int64) != 30 || daily["used_microyuan"].(int64) != 30 {
		t.Fatalf("daily = %#v, want tokens 30 / microyuan 30", daily)
	}
	weekly := overview["weekly"].(map[string]any)
	if weekly["used_tokens"].(int64) != 30 || weekly["used_microyuan"].(int64) != 30 {
		t.Fatalf("weekly = %#v, want tokens 30 / microyuan 30", weekly)
	}
	lastHour := overview["last_hour"].(map[string]any)
	if lastHour["completed"].(int) != 1 || lastHour["failed"].(int) != 0 {
		t.Fatalf("last_hour = %#v, want completed 1", lastHour)
	}
	if overview["active"].(int) != 0 {
		t.Fatalf("active = %d, want 0", overview["active"])
	}
	// 未包含任何渠道的组不应看到任何统计
	empty, err := repo.GroupOverview(context.Background(), 999, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if empty["rpm"].(map[string]any)["used"].(int) != 0 {
		t.Fatalf("unknown group rpm = %#v, want 0", empty["rpm"])
	}
	if _, err := repo.GroupOverview(context.Background(), 0, time.Now().UTC()); err == nil {
		t.Fatal("expected error for invalid group ID")
	}
}

func TestReapStaleReservationsReleasesQuotaAndConcurrency(t *testing.T) {
	repo, key, model := newReservationFixture(t, 100, 1_000_000)
	// 并发上限调成 1：第二个并发请求必须被拒
	one := 1
	if _, err := repo.UpdateClientKey(context.Background(), key.ID, UpdateClientKey{ConcurrencyLimit: &one}); err != nil {
		t.Fatal(err)
	}
	// fixture 的并发上限为 1：第二个并发请求必须被拒
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-stale", 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-blocked", 10)); err == nil {
		t.Fatal("expected concurrency rejection while first reservation is open")
	}
	time.Sleep(20 * time.Millisecond)
	reaped, err := repo.ReapStaleReservations(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	// 槽位与 5h 配额均已释放：新请求可用
	if _, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-after-reap", 10)); err != nil {
		t.Fatalf("quota/concurrency not released: %v", err)
	}
	// 重复回收不应再次命中
	if again, err := repo.ReapStaleReservations(context.Background(), 10*time.Millisecond); err != nil || again != 0 {
		t.Fatalf("second reap = %d, %v; want 0, nil", again, err)
	}
	_ = reservation
}
