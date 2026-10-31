package relay

import (
	"context"
	"errors"
	"time"
)

const settlementTimeout = 5 * time.Second

func settlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), settlementTimeout)
}

func retryableSettlement(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	code := coded.Code() & 0xff
	return code == 5 || code == 6
}

func settleWithRetry(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for _, wait := range []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond} {
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		err = fn(ctx)
		if err == nil || !retryableSettlement(err) {
			return err
		}
	}
	return err
}
