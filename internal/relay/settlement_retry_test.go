package relay

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteCodeError int

func (e sqliteCodeError) Error() string { return "sqlite error" }
func (e sqliteCodeError) Code() int     { return int(e) }

func TestSettlementContextSurvivesCanceledAndExpiredParent(t *testing.T) {
	for _, parent := range []context.Context{
		func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}(),
		func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			return ctx
		}(),
	} {
		ctx, cancel := settlementContext(parent)
		if err := ctx.Err(); err != nil {
			cancel()
			t.Fatalf("cleanup context already canceled: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
			cancel()
			t.Fatalf("cleanup deadline=%v, ok=%v", deadline, ok)
		}
		cancel()
	}
}

func TestSettlementRetryOnlyForSQLiteBusyAndLocked(t *testing.T) {
	for _, code := range []int{5, 6, 261, 262} {
		calls := 0
		err := settleWithRetry(context.Background(), func(context.Context) error {
			calls++
			if calls < 3 {
				return sqliteCodeError(code)
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("code=%d: err=%v calls=%d", code, err, calls)
		}
	}
	calls := 0
	want := errors.New("permanent failure")
	err := settleWithRetry(context.Background(), func(context.Context) error {
		calls++
		return want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("permanent err=%v calls=%d", err, calls)
	}
}

func TestSettlementRetryStopsAtCanceledBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	start := time.Now()
	err := settleWithRetry(ctx, func(context.Context) error {
		calls++
		return sqliteCodeError(5)
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("err=%v calls=%d elapsed=%v", err, calls, time.Since(start))
	}
}

func TestSettlementRetrySurvivesRealSQLiteWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := "file:" + url.PathEscape(filepath.ToSlash(path)) + "?_pragma=busy_timeout(20)"
	first, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.SetMaxOpenConns(1)
	second.SetMaxOpenConns(1)
	if _, err := first.Exec("CREATE TABLE pending (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer first.Exec("ROLLBACK")
	firstFailed := make(chan struct{})
	unlocked := make(chan error, 1)
	go func() {
		<-firstFailed
		_, err := first.Exec("ROLLBACK")
		unlocked <- err
	}()
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = settleWithRetry(ctx, func(ctx context.Context) error {
		attempts++
		_, err := second.ExecContext(ctx, "BEGIN IMMEDIATE")
		if attempts == 1 && err != nil {
			close(firstFailed)
		}
		if err == nil {
			_, _ = second.ExecContext(ctx, "ROLLBACK")
		}
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("retry err=%v attempts=%d, want second attempt success", err, attempts)
	}
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
}
