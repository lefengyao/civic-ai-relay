// Package relay contains process-local admission controls.
package relay

import "sync"

type Gate struct {
	mu     sync.Mutex
	limit  int
	active int
}

func NewGate(limit int) *Gate {
	if limit < 1 {
		limit = 1
	}
	return &Gate{limit: limit}
}

func (g *Gate) TryAcquire() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= g.limit {
		return false
	}
	g.active++
	return true
}

func (g *Gate) Release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	g.mu.Unlock()
}

func (g *Gate) SetLimit(limit int) {
	if g == nil {
		return
	}
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	g.limit = limit
	g.mu.Unlock()
}

func (g *Gate) Limit() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}
func (g *Gate) Active() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

type KeyGates struct {
	mu    sync.Mutex
	gates map[int64]*keyGate
}

type keyGate struct {
	gate *Gate
	refs int
}

func NewKeyGates() *KeyGates { return &KeyGates{gates: make(map[int64]*keyGate)} }

func (ks *KeyGates) TryAcquire(keyID int64, limit int) bool {
	if ks == nil || keyID <= 0 {
		return false
	}
	if limit < 1 {
		limit = 1
	}
	ks.mu.Lock()
	entry := ks.gates[keyID]
	if entry == nil {
		entry = &keyGate{gate: NewGate(limit)}
		ks.gates[keyID] = entry
	}
	entry.refs++
	ks.mu.Unlock()
	if entry.gate.TryAcquire() {
		return true
	}
	ks.mu.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(ks.gates, keyID)
	}
	ks.mu.Unlock()
	return false
}

func (ks *KeyGates) Release(keyID int64) {
	if ks == nil {
		return
	}
	ks.mu.Lock()
	entry := ks.gates[keyID]
	if entry == nil {
		ks.mu.Unlock()
		return
	}
	entry.gate.Release()
	entry.refs--
	if entry.refs == 0 && entry.gate.Active() == 0 {
		delete(ks.gates, keyID)
	}
	ks.mu.Unlock()
}

func (ks *KeyGates) SetLimit(keyID int64, limit int) {
	if ks == nil || keyID <= 0 {
		return
	}
	ks.mu.Lock()
	entry := ks.gates[keyID]
	if entry == nil {
		entry = &keyGate{gate: NewGate(limit)}
		ks.gates[keyID] = entry
	}
	entry.gate.SetLimit(limit)
	ks.mu.Unlock()
}

func (ks *KeyGates) Size() int {
	if ks == nil {
		return 0
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return len(ks.gates)
}

func (ks *KeyGates) Active(keyID int64) int {
	if ks == nil {
		return 0
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if entry := ks.gates[keyID]; entry != nil {
		return entry.gate.Active()
	}
	return 0
}
