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
	if err := repo.ReplaceGroupModels(context.Background(), g.ID, []int64{model.ID}); err != nil {
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
	if err := repo.SettleRequest(context.Background(), reservation.ID, 5, 6, 1000, "completed"); err != nil {
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
