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
		RPMLimit:             30,
		TokenLimit5H:         100000,
		TokenLimitDaily:      20000,
	}
}

func TestCompetingReservationsAllowExactlyOneFinalQuotaClaim(t *testing.T) {
	repo, key, model := newReservationFixture(t, 600, 1_000_000)
	args := newReservationArgs(key.ID, model.ID, "request-concurrent", 600)
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
	var errs []error
	for err := range results {
		errs = append(errs, err)
	}
	if len(errs) != 2 || (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("results = %v", errs)
	}
	var quotaErr *QuotaError
	if !errors.As(errs[0], &quotaErr) && !errors.As(errs[1], &quotaErr) {
		t.Fatalf("expected quota error, got %v", errs)
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

func TestSettledRequestChargesActualUsageAndDisablesAtQuota(t *testing.T) {
	repo, key, model := newReservationFixture(t, 100, 1_000_000)
	limit := int64(11)
	if _, err := repo.UpdateClientKey(context.Background(), key.ID, UpdateClientKey{TokenLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	input := newReservationArgs(key.ID, model.ID, "request-settle", 10)
	input.InputTokens = 1
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
	if current.Enabled || current.DisabledReason != "quota_exhausted" {
		t.Fatalf("key was not disabled at quota: %#v", current)
	}
	row, err := repo.RecentRequests(context.Background(), 1)
	if err != nil || len(row) != 1 {
		t.Fatalf("recent rows = %#v, err = %v", row, err)
	}
	if row[0]["cached_input_tokens"] != int64(2) || row[0]["input_tokens"] != int64(5) {
		t.Fatalf("cached tokens not recorded: %#v", row[0])
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
	if daily["used_tokens"].(int64) != 30 || daily["amount_microyuan"].(int64) != 30 {
		t.Fatalf("daily = %#v, want tokens 30 / amount 30", daily)
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
