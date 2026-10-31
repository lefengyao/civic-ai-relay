package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

var (
	ErrSettlementConflict  = errors.New("settlement outcome conflicts with finalized request")
	ErrReservationNotFound = errors.New("reservation not found")
)

type Settlement struct {
	Status            string
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	AmountMicroyuan   int64
	UpstreamStatus    int
	ErrorCode         string
	FailureStage      string
	UsageSource       string
	Protocol          string
	ClientRequestID   string
	IncompleteReason  string
	DurationMS        *int64
	HeaderMS          *int64
	FirstEventMS      *int64
	LastUpstreamMS    *int64
}

func (s *Store) FinishReservation(ctx context.Context, reservationID int64, outcome Settlement) error {
	if reservationID <= 0 || outcome.InputTokens < 0 || outcome.OutputTokens < 0 ||
		outcome.CachedInputTokens < 0 || outcome.AmountMicroyuan < 0 || outcome.UpstreamStatus < 0 ||
		outcome.InputTokens > math.MaxInt64-outcome.OutputTokens {
		return errors.New("invalid settlement")
	}
	if outcome.Status != "completed" && outcome.Status != "failed" && outcome.Status != "aborted" && outcome.Status != "rejected" {
		return errors.New("invalid settlement status")
	}
	if outcome.UsageSource == "" {
		outcome.UsageSource = "unknown"
	}
	if outcome.Protocol == "" {
		outcome.Protocol = "chat"
	}
	for _, elapsed := range []*int64{outcome.DurationMS, outcome.HeaderMS, outcome.FirstEventMS, outcome.LastUpstreamMS} {
		if elapsed != nil && *elapsed < 0 {
			return errors.New("settlement duration must be non-negative")
		}
	}

	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := func(e error) error {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return e
	}

	var requestToken, status, errorCode, failureStage, usageSource, protocol, clientRequestID, incompleteReason string
	var input, output, cached, amount int64
	var upstreamStatus, duration, header, firstEvent, lastUpstream sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT request_id,status,input_tokens,output_tokens,cached_input_tokens,amount_microyuan,upstream_status,error_code,failure_stage,usage_source,protocol,client_request_id,incomplete_reason,duration_ms,upstream_header_ms,first_event_ms,last_upstream_ms FROM requests WHERE id=?`, reservationID).Scan(
		&requestToken, &status, &input, &output, &cached, &amount, &upstreamStatus,
		&errorCode, &failureStage, &usageSource, &protocol, &clientRequestID, &incompleteReason,
		&duration, &header, &firstEvent, &lastUpstream,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rollback(ErrReservationNotFound)
	}
	if err != nil {
		return rollback(err)
	}
	if status != "reserved" {
		if status == outcome.Status && input == outcome.InputTokens && output == outcome.OutputTokens &&
			cached == outcome.CachedInputTokens && amount == outcome.AmountMicroyuan &&
			upstreamStatusEqual(upstreamStatus, outcome.UpstreamStatus) && errorCode == outcome.ErrorCode &&
			failureStage == outcome.FailureStage && usageSource == outcome.UsageSource &&
			protocol == outcome.Protocol && clientRequestID == outcome.ClientRequestID &&
			incompleteReason == outcome.IncompleteReason && nullableIntEqual(duration, outcome.DurationMS) &&
			nullableIntEqual(header, outcome.HeaderMS) && nullableIntEqual(firstEvent, outcome.FirstEventMS) &&
			nullableIntEqual(lastUpstream, outcome.LastUpstreamMS) {
			return rollback(nil)
		}
		return rollback(ErrSettlementConflict)
	}

	var keyReservationID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM key_reservations WHERE request_id=? AND status='reserved' ORDER BY id DESC LIMIT 1`, requestToken).Scan(&keyReservationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(fmt.Errorf("%w: key reservation for request %d", ErrReservationNotFound, reservationID))
		}
		return rollback(err)
	}
	finished := nowUTC()
	chargedTokens := outcome.InputTokens + outcome.OutputTokens
	result, err := conn.ExecContext(ctx, `UPDATE key_reservations SET charged_tokens=?,charged_amount_microyuan=?,reserved_tokens=0,reserved_amount_microyuan=0,status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, outcome.AmountMicroyuan, outcome.Status, finished, keyReservationID)
	if err != nil {
		return rollback(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return rollback(err)
		}
		return rollback(ErrSettlementConflict)
	}
	result, err = conn.ExecContext(ctx, `UPDATE requests SET reserved_tokens=0,charged_tokens=?,input_tokens=?,output_tokens=?,cached_input_tokens=?,amount_microyuan=?,status=?,upstream_status=?,error_code=?,failure_stage=?,usage_source=?,protocol=?,client_request_id=?,incomplete_reason=?,duration_ms=?,upstream_header_ms=?,first_event_ms=?,last_upstream_ms=?,finished_at_utc=? WHERE id=? AND status='reserved'`,
		chargedTokens, outcome.InputTokens, outcome.OutputTokens, outcome.CachedInputTokens, outcome.AmountMicroyuan,
		outcome.Status, upstreamStatusValue(outcome.UpstreamStatus), outcome.ErrorCode, outcome.FailureStage,
		outcome.UsageSource, outcome.Protocol, outcome.ClientRequestID, outcome.IncompleteReason,
		outcome.DurationMS, outcome.HeaderMS, outcome.FirstEventMS, outcome.LastUpstreamMS, finished, reservationID)
	if err != nil {
		return rollback(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return rollback(err)
		}
		return rollback(ErrSettlementConflict)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(err)
	}
	return nil
}

func upstreamStatusValue(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

func upstreamStatusEqual(saved sql.NullInt64, incoming int) bool {
	return (!saved.Valid && incoming == 0) || (saved.Valid && saved.Int64 == int64(incoming))
}

func nullableIntEqual(saved sql.NullInt64, incoming *int64) bool {
	return (!saved.Valid && incoming == nil) || (saved.Valid && incoming != nil && saved.Int64 == *incoming)
}
