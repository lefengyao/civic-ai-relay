package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ReapStaleReservations finishes reservations that never settled, for example
// because the process crashed mid-request. Without reaping, every such request
// permanently leaks one concurrency slot (key_reservations.status stays
// 'reserved') and inflates the 5-hour token window forever.
//
// maxAge should exceed the longest legitimate request lifetime: the configured
// MAX_STREAM_DURATION plus a safety margin. Aborted rows keep their rows but
// zero the reserved tokens so quota is released; the matching requests rows are
// marked 'aborted' so operators can see what happened.
func (s *Store) ReapStaleReservations(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, errors.New("max age must be positive")
	}
	return s.ReapOrphanReservations(ctx, time.Now().UTC().Add(-maxAge), nil, nil)
}

func (s *Store) ReapOrphanReservations(ctx context.Context, cutoff time.Time, activeIDs, pendingIDs []int64) (int64, error) {
	if cutoff.IsZero() {
		return 0, errors.New("cutoff is required")
	}
	cutoffText := cutoff.UTC().Format(time.RFC3339Nano)
	now := nowUTC()
	excluded := make(map[int64]struct{}, len(activeIDs)+len(pendingIDs))
	for _, id := range activeIDs {
		if id > 0 {
			excluded[id] = struct{}{}
		}
	}
	for _, id := range pendingIDs {
		if id > 0 {
			excluded[id] = struct{}{}
		}
	}
	exclusion := ""
	exclusionArgs := make([]any, 0, len(excluded))
	if len(excluded) > 0 {
		placeholders := make([]string, 0, len(excluded))
		for id := range excluded {
			placeholders = append(placeholders, "?")
			exclusionArgs = append(exclusionArgs, id)
		}
		exclusion = " AND id NOT IN (" + strings.Join(placeholders, ",") + ")"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// SQLite date functions resolve to milliseconds; strict comparison avoids
	// reaping a newer timestamp rounded into the cutoff's millisecond.
	keyArgs := append([]any{now, cutoffText, cutoffText}, exclusionArgs...)
	result, err := tx.ExecContext(ctx, `UPDATE key_reservations SET charged_tokens=0,charged_amount_microyuan=0,reserved_tokens=0,reserved_amount_microyuan=0,status='aborted',finished_at_utc=? WHERE status='reserved' AND julianday(created_at_utc)<julianday(?) AND (NOT EXISTS (SELECT 1 FROM requests WHERE requests.request_id=key_reservations.request_id AND requests.status='reserved') OR request_id IN (SELECT request_id FROM requests WHERE status='reserved' AND julianday(created_at_utc)<julianday(?)`+exclusion+`))`, keyArgs...)
	if err != nil {
		return 0, err
	}
	reaped, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	requestArgs := append([]any{now, cutoffText}, exclusionArgs...)
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET status='aborted',input_tokens=0,output_tokens=0,cached_input_tokens=0,charged_tokens=0,reserved_tokens=0,amount_microyuan=0,upstream_status=NULL,error_code='crash_recovered',usage_source='unknown',finished_at_utc=? WHERE status='reserved' AND julianday(created_at_utc)<julianday(?)`+exclusion, requestArgs...); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reaped, nil
}
