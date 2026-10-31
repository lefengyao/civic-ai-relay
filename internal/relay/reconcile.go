package relay

import (
	"context"
	"sync"
)

type Reconciler struct {
	mu     sync.Mutex
	leases map[int64]*Lease
}

func (r *Reconciler) Add(lease *Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leases == nil {
		r.leases = make(map[int64]*Lease)
	}
	r.leases[lease.reservation.ID] = lease
}

func (r *Reconciler) Remove(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.leases, id)
}

func (r *Reconciler) IDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.leases))
	for id := range r.leases {
		ids = append(ids, id)
	}
	return ids
}

func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	r.mu.Lock()
	leases := make([]*Lease, 0, len(r.leases))
	for _, lease := range r.leases {
		leases = append(leases, lease)
	}
	r.mu.Unlock()
	settled := 0
	var firstErr error
	for _, lease := range leases {
		if ctx.Err() != nil {
			return settled, ctx.Err()
		}
		if err := lease.Close(ctx, Outcome{}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		settled++
	}
	return settled, firstErr
}
