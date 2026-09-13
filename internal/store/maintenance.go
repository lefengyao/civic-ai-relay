package store

import (
	"context"
	"errors"
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
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	now := nowUTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE key_reservations SET charged_tokens=0,charged_amount_microyuan=0,reserved_tokens=0,reserved_amount_microyuan=0,status='aborted',finished_at_utc=? WHERE status='reserved' AND created_at_utc<=? AND request_id IN (SELECT request_id FROM requests WHERE status='reserved' AND created_at_utc<=?)`, now, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	reaped, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET status='aborted',reserved_tokens=0,upstream_status=0,finished_at_utc=? WHERE status='reserved' AND created_at_utc<=?`, now, cutoff); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reaped, nil
}
