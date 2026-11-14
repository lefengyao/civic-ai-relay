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

// 管理台把 Key 的并发上限调大/调小，必须**立刻**生效：旧实现只在 gate 首次创建时
// 取 limit，改完要等这把 Key 的在跑请求全部结束（entry 被回收）才轮到新值——
// 调大后仍被旧值挡着，调小后又能多放几个进来。
func TestKeyGatesFollowsLimitChanges(t *testing.T) {
	gates := NewKeyGates()
	if !gates.TryAcquire(7, 1) {
		t.Fatal("first lease should pass")
	}
	if gates.TryAcquire(7, 1) {
		t.Fatal("limit 1 should reject the second lease")
	}
	// 调大到 2：不用等在跑的那个结束，第二个名额立刻可用。
	if !gates.TryAcquire(7, 2) {
		t.Fatal("raised limit did not take effect while a lease is in flight")
	}
	// 调小到 1：在跑的两个不被中断，但第三个必须被拒。
	if gates.TryAcquire(7, 1) {
		t.Fatal("lowered limit did not take effect while leases are in flight")
	}
	gates.Release(7)
	gates.Release(7)
	if !gates.TryAcquire(7, 1) {
		t.Fatal("slot was not restored after releasing both leases")
	}
}
