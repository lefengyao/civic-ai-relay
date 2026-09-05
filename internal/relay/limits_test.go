package relay

import "testing"

func TestLoweringLimitKeepsExistingLeaseButRejectsNewLease(t *testing.T) {
	gate := NewGate(2)
	if !gate.TryAcquire() || !gate.TryAcquire() {
		t.Fatal("initial leases")
	}
	gate.SetLimit(1)
	if gate.TryAcquire() {
		t.Fatal("new request admitted")
	}
	gate.Release()
	gate.Release()
	if !gate.TryAcquire() {
		t.Fatal("slot was not restored")
	}
}

func TestKeyGatesRemovesIdleEntries(t *testing.T) {
	gates := NewKeyGates()
	if !gates.TryAcquire(42, 1) {
		t.Fatal("lease not acquired")
	}
	if gates.TryAcquire(42, 1) {
		t.Fatal("key limit ignored")
	}
	gates.Release(42)
	if gates.Size() != 0 {
		t.Fatalf("idle gate retained: %d", gates.Size())
	}
}
