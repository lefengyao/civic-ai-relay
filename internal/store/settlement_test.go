package store_test

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func TestFinishReservationIsIdempotentAndRejectsConflicts(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	reservation, err := repo.ReserveRequest(context.Background(), store.ReserveInput{
		RequestID: "settlement-idempotent",
		KeyID:     key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputText: "hi", OutputTokenCeiling: 10, MaxOutputTokens: 10,
		Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := store.Settlement{Status: "completed", InputTokens: 3, OutputTokens: 4, AmountMicroyuan: 7, UpstreamStatus: 200}
	if err := repo.FinishReservation(context.Background(), reservation.ID, outcome); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishReservation(context.Background(), reservation.ID, outcome); err != nil {
		t.Fatalf("same settlement should be idempotent: %v", err)
	}
	conflict := outcome
	conflict.AmountMicroyuan = 8
	if err := repo.FinishReservation(context.Background(), reservation.ID, conflict); !errors.Is(err, store.ErrSettlementConflict) {
		t.Fatalf("conflict error=%v, want ErrSettlementConflict", err)
	}
}

func TestFinishReservationReportsUnknownID(t *testing.T) {
	repo, _, _ := newSettlementFixture(t)
	err := repo.FinishReservation(context.Background(), 999999, store.Settlement{Status: "aborted"})
	if !errors.Is(err, store.ErrReservationNotFound) {
		t.Fatalf("error=%v, want ErrReservationNotFound", err)
	}
}

func TestFinishReservationStoresNullWhenNoUpstreamStatus(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	reservation, err := repo.ReserveRequest(context.Background(), store.ReserveInput{
		RequestID: "settlement-no-upstream",
		KeyID:     key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputText: "hi", OutputTokenCeiling: 10, MaxOutputTokens: 10,
		Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishReservation(context.Background(), reservation.ID, store.Settlement{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["upstream_status"] != nil {
		t.Fatalf("rows=%v, want NULL upstream status", rows)
	}
}

func TestFinishReservationStoresKnownOutcomeMetadata(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	r, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "metadata", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	duration, header := int64(120), int64(30)
	outcome := store.Settlement{
		Status: "failed", InputTokens: 2, OutputTokens: 3, CachedInputTokens: 1,
		AmountMicroyuan: 5, UpstreamStatus: 503, ErrorCode: "upstream_unavailable",
		FailureStage: "upstream", UsageSource: "reported", Protocol: "responses",
		ClientRequestID: "client-123", IncompleteReason: "upstream_failed",
		DurationMS: &duration, HeaderMS: &header,
	}
	if err := repo.FinishReservation(ctx, r.ID, outcome); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.RecentRequests(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["upstream_status"] != int64(503) ||
		rows[0]["error_code"] != "upstream_unavailable" || rows[0]["failure_stage"] != "upstream" ||
		rows[0]["usage_source"] != "reported" || rows[0]["protocol"] != "responses" ||
		rows[0]["client_request_id"] != "client-123" || rows[0]["incomplete_reason"] != "upstream_failed" ||
		rows[0]["duration_ms"] != int64(120) || rows[0]["upstream_header_ms"] != int64(30) ||
		rows[0]["first_event_ms"] != nil {
		t.Fatalf("metadata rows=%v", rows)
	}
}

func newSettlementFixture(t *testing.T) (*store.Store, store.ClientKey, store.Model) {
	t.Helper()
	box, err := secret.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "settlement.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	model, err := repo.CreateModel(ctx, store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	return repo, key, model
}

func TestReservationUsesGroupQuote(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	groups, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rate := int64(2000)
	if _, err := repo.UpdateModelGroup(ctx, groups[0].ID, store.UpdateModelGroup{RateMilli: &rate}); err != nil {
		t.Fatal(err)
	}
	input := store.ReserveInput{
		RequestID: "group-quote", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
	}
	reservation, err := repo.ReserveRequest(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.InputTokens != 1 || reservation.ReservedAmountMicroyuan != 4 || reservation.QuoteInputPriceMicroyuan != 1_000_000 ||
		reservation.QuoteOutputPriceMicroyuan != 1_000_000 || reservation.QuoteCachedInputPriceMicroyuan != 1_000_000 || reservation.QuoteRateMilli != 2000 {
		t.Fatalf("reservation=%+v, want 2x quote and amount 4", reservation)
	}
	rows, err := repo.RecentRequests(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["quote_rate_milli"] != int64(2000) || rows[0]["quote_input_microyuan"] != int64(1_000_000) {
		t.Fatalf("rows=%v", rows)
	}
}

func TestReservationQuoteSurvivesModelPriceUpdate(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	first, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "old-quote", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(2_000_000)
	if _, err := repo.UpdateModel(ctx, model.ID, store.UpdateModel{InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price}); err != nil {
		t.Fatal(err)
	}
	if first.QuoteInputPriceMicroyuan != 1_000_000 || first.ReservedAmountMicroyuan != 2 {
		t.Fatalf("first quote changed after model update: %+v", first)
	}
	if err := repo.FinishReservation(ctx, first.ID, store.Settlement{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "new-quote", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.QuoteInputPriceMicroyuan != 2_000_000 || second.ReservedAmountMicroyuan != 4 {
		t.Fatalf("second quote did not use new price: %+v", second)
	}
}

func TestReaperExcludesActiveAndPendingReservations(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-time.Hour)
	reserve := func(id string) store.RequestReservation {
		t.Helper()
		r, err := repo.ReserveRequest(ctx, store.ReserveInput{
			RequestID: id, KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
			StartedAt: old, InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	active := reserve("active")
	if n, err := repo.ReapOrphanReservations(ctx, time.Now().UTC().Add(-time.Minute), []int64{active.ID}, nil); err != nil || n != 0 {
		t.Fatalf("active reap=%d err=%v", n, err)
	}
	if err := repo.FinishReservation(ctx, active.ID, store.Settlement{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	pending := reserve("pending")
	if n, err := repo.ReapOrphanReservations(ctx, time.Now().UTC().Add(-time.Minute), nil, []int64{pending.ID}); err != nil || n != 0 {
		t.Fatalf("pending reap=%d err=%v", n, err)
	}
	if n, err := repo.ReapOrphanReservations(ctx, time.Now().UTC().Add(-time.Minute), nil, nil); err != nil || n != 1 {
		t.Fatalf("orphan reap=%d err=%v", n, err)
	}
	rows, err := repo.RecentRequests(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" || rows[0]["amount_microyuan"] != int64(0) || rows[0]["error_code"] != "crash_recovered" {
		t.Fatalf("reaped request=%v", rows)
	}
}

func TestPrunePreservesReservedRequests(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	r, err := repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "reserved-prune", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		StartedAt: old, InputTokens: 1, OutputTokenCeiling: 1, Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Prune(ctx, old.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned reserved=%d err=%v", n, err)
	}
	if err := repo.FinishReservation(ctx, r.ID, store.Settlement{Status: "aborted"}); err != nil {
		t.Fatalf("reserved request vanished during prune: %v", err)
	}
}

func TestReservationRejectsCachedQuoteOverflow(t *testing.T) {
	repo, key, model := newSettlementFixture(t)
	ctx := context.Background()
	tooHigh := int64(math.MaxInt64/2 + 1)
	if _, err := repo.UpdateModel(ctx, model.ID, store.UpdateModel{CachedInputPriceMicroyuan: &tooHigh}); err != nil {
		t.Fatal(err)
	}
	groups, err := repo.ListModelGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups=%v err=%v", groups, err)
	}
	rate := int64(2000)
	if _, err := repo.UpdateModelGroup(ctx, groups[0].ID, store.UpdateModelGroup{RateMilli: &rate}); err != nil {
		t.Fatal(err)
	}
	_, err = repo.ReserveRequest(ctx, store.ReserveInput{
		RequestID: "cached-overflow", KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		InputText: "hi", OutputTokenCeiling: 10, MaxOutputTokens: 10,
		Limits: store.QuotaLimits{RPMLimit: 30},
	})
	if err == nil {
		t.Fatal("cached quote overflow was accepted")
	}
	rows, err := repo.RecentRequests(ctx, 1)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed reservation wrote a request: rows=%v err=%v", rows, err)
	}
}
