package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFinishReservationRollsBackBothLedgerRows(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	ctx := context.Background()
	r, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, "atomic-failure", 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER fail_request_finish BEFORE UPDATE OF status ON requests WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT, 'injected settlement failure'); END`); err != nil {
		t.Fatal(err)
	}
	outcome := Settlement{Status: "completed", InputTokens: 2, OutputTokens: 3, AmountMicroyuan: 5, UpstreamStatus: 200}
	if err := repo.FinishReservation(ctx, r.ID, outcome); err == nil {
		t.Fatal("injected failure did not abort settlement")
	}
	var requestStatus, keyStatus string
	var charged int64
	if err := repo.db.QueryRowContext(ctx, `SELECT status FROM requests WHERE id=?`, r.ID).Scan(&requestStatus); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT status,charged_amount_microyuan FROM key_reservations WHERE id=?`, r.KeyReservationID).Scan(&keyStatus, &charged); err != nil {
		t.Fatal(err)
	}
	if requestStatus != "reserved" || keyStatus != "reserved" || charged != 0 {
		t.Fatalf("partial settlement: request=%s key=%s charged=%d", requestStatus, keyStatus, charged)
	}
	if _, err := repo.db.ExecContext(ctx, `DROP TRIGGER fail_request_finish`); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishReservation(ctx, r.ID, outcome); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestReaperReleasesLegacyOrphanReservation(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	ctx := context.Background()
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	result, err := repo.db.ExecContext(ctx, `INSERT INTO key_reservations(key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (?,?,?,?,?,?)`, key.ID, model.ID, 3, 7, "reserved", old)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := repo.ReapOrphanReservations(ctx, time.Now().UTC().Add(-time.Minute), nil, nil); err != nil || n != 1 {
		t.Fatalf("orphan reap=%d err=%v", n, err)
	}
	var status string
	var tokens, amount int64
	if err := repo.db.QueryRowContext(ctx, `SELECT status,reserved_tokens,reserved_amount_microyuan FROM key_reservations WHERE id=?`, id).Scan(&status, &tokens, &amount); err != nil {
		t.Fatal(err)
	}
	if status != "aborted" || tokens != 0 || amount != 0 {
		t.Fatalf("orphan status=%s tokens=%d amount=%d", status, tokens, amount)
	}
}

func TestReaperReleasesKeyReservationWhoseRequestAlreadyFinished(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	ctx := context.Background()
	input := newReservationArgs(key.ID, model.ID, "orphaned-key", 10)
	input.StartedAt = time.Now().UTC().Add(-time.Hour)
	r, err := repo.ReserveRequest(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE requests SET status='aborted',reserved_tokens=0,amount_microyuan=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.ReapOrphanReservations(ctx, time.Now().UTC().Add(-time.Minute), nil, nil); err != nil || n != 1 {
		t.Fatalf("inconsistent key reservation reap=%d err=%v", n, err)
	}
}

func TestReaperComparesFractionalTimestampsChronologically(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name       string
		startedAt  time.Time
		cutoff     time.Time
		wantReaped int64
		wantStatus string
	}{
		{"fractional timestamp is newer", base.Add(750 * time.Millisecond), base, 0, "reserved"},
		{"whole second is older", base, base.Add(500 * time.Millisecond), 1, "aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, key, model := newReservationFixture(t, 1000, 1_000_000)
			ctx := context.Background()
			input := newReservationArgs(key.ID, model.ID, "fractional-reap", 10)
			input.StartedAt = tc.startedAt
			reservation, err := repo.ReserveRequest(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			reaped, err := repo.ReapOrphanReservations(ctx, tc.cutoff, nil, nil)
			if err != nil || reaped != tc.wantReaped {
				t.Fatalf("reaped=%d err=%v, want %d", reaped, err, tc.wantReaped)
			}
			var requestStatus, keyStatus string
			if err := repo.db.QueryRowContext(ctx, `SELECT status FROM requests WHERE id=?`, reservation.ID).Scan(&requestStatus); err != nil {
				t.Fatal(err)
			}
			if err := repo.db.QueryRowContext(ctx, `SELECT status FROM key_reservations WHERE id=?`, reservation.KeyReservationID).Scan(&keyStatus); err != nil {
				t.Fatal(err)
			}
			if requestStatus != tc.wantStatus || keyStatus != tc.wantStatus {
				t.Fatalf("request=%s key=%s, want %s", requestStatus, keyStatus, tc.wantStatus)
			}
		})
	}
}

func TestFinishReservationConcurrentDuplicateChargesOnce(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	ctx := context.Background()
	r, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, "concurrent-finish", 10))
	if err != nil {
		t.Fatal(err)
	}
	outcome := Settlement{Status: "completed", InputTokens: 2, OutputTokens: 3, AmountMicroyuan: 5, UpstreamStatus: 200}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- repo.FinishReservation(ctx, r.ID, outcome)
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("duplicate finish: %v", err)
		}
	}
	var charged int64
	if err := repo.db.QueryRowContext(ctx, `SELECT charged_amount_microyuan FROM key_reservations WHERE id=?`, r.KeyReservationID).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != 5 {
		t.Fatalf("charged=%d, want once=5", charged)
	}
}

func TestDeletingResourcesCannotEraseActiveSettlement(t *testing.T) {
	for _, target := range []string{"key", "model", "provider"} {
		t.Run(target, func(t *testing.T) {
			repo, key, model := newReservationFixture(t, 1000, 1_000_000)
			ctx := context.Background()
			reservation, err := repo.ReserveRequest(ctx, newReservationArgs(key.ID, model.ID, "active-delete", 10))
			if err != nil {
				t.Fatal(err)
			}
			deleteResource := func() error {
				switch target {
				case "key":
					return repo.DeleteClientKey(ctx, key.ID)
				case "model":
					return repo.DeleteModel(ctx, model.ID)
				default:
					return repo.DeleteProvider(ctx, model.ProviderID)
				}
			}
			if err := deleteResource(); err == nil {
				t.Fatal("deleted resource while its reservation was active")
			}
			if err := repo.FinishReservation(ctx, reservation.ID, Settlement{Status: "completed", InputTokens: 2, OutputTokens: 3, AmountMicroyuan: 5, UpstreamStatus: 200}); err != nil {
				t.Fatalf("active settlement was erased: %v", err)
			}
			if err := deleteResource(); err != nil {
				t.Fatalf("settled resource could not be deleted: %v", err)
			}
		})
	}
}
