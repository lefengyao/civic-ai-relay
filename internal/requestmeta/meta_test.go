package requestmeta

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTraceContextAndSafeClientID(t *testing.T) {
	started := time.Now().Add(-time.Second)
	trace := NewTrace("relay-test", "client\nsecret", "chat", started)
	ctx := WithTrace(context.Background(), trace)
	if got := FromContext(ctx); got != trace || trace.ID != "relay-test" || trace.ClientID != "" {
		t.Fatalf("trace=%+v context=%v", trace, got)
	}
	trace.MarkHeader()
	trace.MarkFirstEvent()
	trace.MarkUpstream()
	snapshot := trace.Snapshot(time.Now())
	if snapshot.HeaderMS == nil || snapshot.FirstEventMS == nil || snapshot.LastUpstreamMS == nil || snapshot.DurationMS == nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if strings.Contains(trace.ClientID, "\n") {
		t.Fatalf("unsafe client id=%q", trace.ClientID)
	}
}

func TestTraceMissingPhaseIsNil(t *testing.T) {
	trace := NewTrace("id", "", "chat", time.Now())
	snapshot := trace.Snapshot(time.Now())
	if snapshot.HeaderMS != nil || snapshot.FirstEventMS != nil || snapshot.LastUpstreamMS != nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
