package relay

import (
	"context"
	"io"
	"sync"
)

type ActiveReservations struct {
	mu  sync.RWMutex
	ids map[int64]struct{}
}

func (a *ActiveReservations) Register(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ids == nil {
		a.ids = make(map[int64]struct{})
	}
	a.ids[id] = struct{}{}
}

func (a *ActiveReservations) Remove(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.ids, id)
}

func (a *ActiveReservations) IDs() []int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ids := make([]int64, 0, len(a.ids))
	for id := range a.ids {
		ids = append(ids, id)
	}
	return ids
}

type activeUpstream struct {
	cancel context.CancelFunc
	body   io.Closer
}

func (s *Service) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *Service) BeginDraining() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
}

func (s *Service) setUpstream(id int64, cancel context.CancelFunc, body io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstream[id] = activeUpstream{cancel: cancel, body: body}
}

func (s *Service) forgetUpstream(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.upstream, id)
}

func (s *Service) CancelActive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	upstreams := make([]activeUpstream, 0, len(s.upstream))
	for _, request := range s.upstream {
		upstreams = append(upstreams, request)
	}
	s.mu.Unlock()
	for _, request := range upstreams {
		if request.cancel != nil {
			request.cancel()
		}
		if request.body != nil {
			_ = request.body.Close()
		}
	}
}

func (s *Service) ActiveReservationIDs() []int64 {
	if s == nil {
		return nil
	}
	return s.active.IDs()
}

func (s *Service) PendingReservationIDs() []int64 {
	if s == nil {
		return nil
	}
	return s.pending.IDs()
}

func (s *Service) ReconcilePending(ctx context.Context) (int, error) {
	if s == nil {
		return 0, nil
	}
	return s.pending.RunOnce(ctx)
}
