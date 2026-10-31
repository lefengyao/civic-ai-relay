package upstream

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"civic-ai-relay/internal/requestmeta"
)

type Options struct {
	ConnectTimeout time.Duration
	HeaderTimeout  time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	PoolTimeout    time.Duration
	IdleTimeout    time.Duration
	UserAgent      string
	// Empty auto-detects OpenCode Go by upstream host; "-" disables it.
	UpstreamSessionHeader string
}

func effectiveHeaderTimeout(explicit, connect, legacyRead time.Duration) time.Duration {
	if explicit > 0 {
		return explicit
	}
	return connect + legacyRead
}

type timedBody struct {
	body        io.ReadCloser
	ctx         context.Context
	cancel      context.CancelFunc
	status      int
	idle        time.Duration
	mu          sync.Mutex
	timer       *time.Timer
	stopContext func() bool
	timedOut    bool
	done        bool
	closeOnce   sync.Once
	closeErr    error
}

func newTimedBody(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser, status int, idle time.Duration) *timedBody {
	b := &timedBody{body: body, ctx: ctx, cancel: cancel, status: status, idle: idle}
	if idle > 0 {
		b.timer = time.AfterFunc(idle, b.expire)
	}
	b.stopContext = context.AfterFunc(ctx, func() { _ = b.closeUnderlying() })
	return b
}

func (b *timedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		if trace := requestmeta.FromContext(b.ctx); trace != nil {
			trace.MarkUpstream()
		}
	}
	b.mu.Lock()
	if n > 0 && !b.done && !b.timedOut && b.timer != nil {
		b.timer.Reset(b.idle)
	}
	timedOut := b.timedOut
	if errors.Is(err, io.EOF) && !timedOut && b.ctx.Err() == nil {
		b.done = true
		if b.timer != nil {
			b.timer.Stop()
		}
		b.stopContext()
	}
	b.mu.Unlock()
	if errors.Is(err, io.EOF) && !timedOut {
		b.cancel()
	}
	if timedOut {
		return n, &Error{Code: "upstream_idle_timeout", Status: b.status, Stage: "idle", Cause: context.DeadlineExceeded}
	}
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if errors.Is(err, context.Canceled) || errors.Is(b.ctx.Err(), context.Canceled) {
		return n, &Error{Code: "client_canceled", Status: b.status, Stage: "request", Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(b.ctx.Err(), context.DeadlineExceeded) {
		return n, &Error{Code: "request_duration_exceeded", Status: b.status, Stage: "duration", Cause: err}
	}
	return n, &Error{Code: "upstream_stream_read_failed", Status: b.status, Stage: "body", Cause: err}
}

func (b *timedBody) Close() error {
	b.mu.Lock()
	b.done = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.stopContext()
	b.mu.Unlock()
	b.cancel()
	return b.closeUnderlying()
}

func (b *timedBody) expire() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.timedOut = true
	b.mu.Unlock()
	b.cancel()
	_ = b.closeUnderlying()
}

func (b *timedBody) closeUnderlying() error {
	b.closeOnce.Do(func() { b.closeErr = b.body.Close() })
	return b.closeErr
}
