package requestmeta

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type Trace struct {
	ID             string
	ClientID       string
	Protocol       string
	Model          string
	Started        time.Time
	mu             sync.Mutex
	HeaderAt       *time.Time
	FirstEventAt   *time.Time
	LastUpstreamAt *time.Time
}

func (t *Trace) SetModel(model string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.Model = SafeClientID(model)
	t.mu.Unlock()
}

func (t *Trace) ModelName() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Model
}

type Snapshot struct {
	DurationMS     *int64
	HeaderMS       *int64
	FirstEventMS   *int64
	LastUpstreamMS *int64
}

type traceKey struct{}

func TraceFromContext(ctx context.Context) *Trace { return FromContext(ctx) }

func NewTrace(id, clientID, protocol string, started time.Time) *Trace {
	if strings.TrimSpace(id) == "" {
		id = NewID()
	}
	if started.IsZero() {
		started = time.Now()
	}
	return &Trace{ID: id, ClientID: SafeClientID(clientID), Protocol: protocol, Started: started}
}

func NewID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "relay-" + time.Now().UTC().Format("20060102T150405.000000000Z07:00")
	}
	return "relay-" + hex.EncodeToString(raw)
}

func SafeClientID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return ""
		}
	}
	return value
}

func WithTrace(ctx context.Context, trace *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, trace)
}

func FromContext(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(traceKey{}).(*Trace)
	return trace
}

func (t *Trace) MarkHeader()     { t.mark(&t.HeaderAt, false) }
func (t *Trace) MarkFirstEvent() { t.mark(&t.FirstEventAt, false) }
func (t *Trace) MarkUpstream()   { t.mark(&t.LastUpstreamAt, true) }

func (t *Trace) mark(slot **time.Time, update bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if *slot == nil || update {
		now := time.Now()
		*slot = &now
	}
}

func (t *Trace) Snapshot(now time.Time) Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return Snapshot{
		DurationMS: millisSince(t.Started, &now), HeaderMS: millisSince(t.Started, t.HeaderAt),
		FirstEventMS: millisSince(t.Started, t.FirstEventAt), LastUpstreamMS: millisSince(t.Started, t.LastUpstreamAt),
	}
}

func millisSince(start time.Time, at *time.Time) *int64 {
	if at == nil {
		return nil
	}
	n := at.Sub(start).Milliseconds()
	if n < 0 {
		n = 0
	}
	return &n
}
