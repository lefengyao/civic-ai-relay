package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 删 Key 不能带走组额度账目：额度是按 group_id 统计的，旧表 key_id 上的
// ON DELETE CASCADE 让「删一把 Key」等于把整组的周期/总额度用量清零。
func TestDeletingKeyKeepsGroupQuotaUsage(t *testing.T) {
	repo, key, model, group := newReservationFixture(t, 1_000_000, 1_000_000)
	ctx := context.Background()
	setConcurrency(t, repo, key.ID, 8)
	setGroupLimits(t, repo, group, QuotaLimits{
		TokenPeriod: 100, PeriodStart: time.Now().UTC().Add(-time.Hour), PeriodDays: 60,
	})
	reservation, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, "deleted-key-1", 100))
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	// 结算成 50 tokens：周期限额 100，还剩一半。
	if err := repo.SettleRequest(ctx, reservation.ID, 30, 20, 0, 50, "completed"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := repo.DeleteClientKey(ctx, key.ID); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	overview, err := repo.GroupOverview(ctx, group.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	period := overview["period"].(map[string]any)
	if got := period["used_tokens"].(int64); got != 50 {
		t.Fatalf("period used after key deletion = %d, want 50 (账目不能随 Key 删除)", got)
	}

	// 新 Key 仍在同一组：账目继续累计，不该「从 0 开始」。
	replacement, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k2", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatalf("create replacement key: %v", err)
	}
	if err := repo.SetKeyGroup(ctx, replacement.ID, group.ID); err != nil {
		t.Fatalf("bind replacement key: %v", err)
	}
	if _, err := repo.ReserveRequest(ctx, newReservationArgs(replacement.ID, model.ID, "deleted-key-2", 100)); err != nil {
		t.Fatalf("second reservation below the period limit: %v", err)
	}
	// 已用 50 + 本次预留 108 ≥ 100，下一笔必须被周期额度挡下，而不是重新计数。
	_, err = repo.ReserveRequest(ctx, newReservationArgs(replacement.ID, model.ID, "deleted-key-3", 100))
	var quota *QuotaError
	if !errors.As(err, &quota) || quota.Scope != "period" {
		t.Fatalf("err = %v, want period quota_exceeded after key replacement", err)
	}
}

// 保留期（默认 7 天）不能裁剪额度账目：自定义周期可以长达 60 天，
// 否则周期用量会随时间自己减少，等于额度被悄悄重置。
func TestPruneKeepsLedgerForLongQuotaWindows(t *testing.T) {
	repo, key, model, group := newReservationFixture(t, 1_000_000, 1_000_000)
	ctx := context.Background()
	setConcurrency(t, repo, key.ID, 8)
	setGroupLimits(t, repo, group, QuotaLimits{
		TokenPeriod: 1000, PeriodStart: time.Now().UTC().AddDate(0, 0, -40), PeriodDays: 60,
	})
	args := newReservationArgs(key.ID, model.ID, "old-ledger-row", 100)
	args.StartedAt = time.Now().UTC().AddDate(0, 0, -30)
	reservation, err := repo.ReserveRequest(ctx, args)
	if err != nil {
		t.Fatalf("reservation 30 days ago: %v", err)
	}
	if err := repo.SettleRequest(ctx, reservation.ID, 400, 400, 0, 800, "completed"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	removed, err := repo.Prune(ctx, time.Now().UTC().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("pruned requests = %d, want 1（请求明细仍按保留期清理）", removed)
	}
	overview, err := repo.GroupOverview(ctx, group.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if got := overview["period"].(map[string]any)["used_tokens"].(int64); got != 800 {
		t.Fatalf("period used after prune = %d, want 800（账目不能被保留期裁掉）", got)
	}
}
