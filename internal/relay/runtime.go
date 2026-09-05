package relay

import (
	"context"
	"sync"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/store"
)

type Runtime struct {
	mu       sync.RWMutex
	settings config.Settings
	registry *Registry
	cancel   context.CancelFunc
}

func NewRuntime(settings config.Settings, registry *Registry) *Runtime {
	return &Runtime{settings: settings, registry: registry}
}
func (r *Runtime) Settings() config.Settings       { r.mu.RLock(); defer r.mu.RUnlock(); return r.settings }
func (r *Runtime) Reload(settings config.Settings) { r.mu.Lock(); r.settings = settings; r.mu.Unlock() }
func (r *Runtime) Start(ctx context.Context, prune func(context.Context) error) {
	if prune == nil {
		return
	}
	child, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				_ = prune(child)
			}
		}
	}()
}
func (r *Runtime) Stop() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.mu.Unlock()
}
func every(ctx context.Context, interval time.Duration, work func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work(ctx)
		}
	}
}

var _ = store.Model{}
