package store

import (
	"context"
	"strings"
	"testing"
)

func TestNewKeyIsReturnedOnceAndStoredAsDigest(t *testing.T) {
	repo := newTestStore(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "alice", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Token, "crk_") {
		t.Fatalf("token = %q", key.Token)
	}
	rows, err := repo.ListClientKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join([]string{rows[0].Token, rows[0].Name}, " "), key.Token) || rows[0].Token != "" {
		t.Fatal("plaintext token persisted or listed")
	}
	var digest []byte
	if err := repo.db.QueryRow("SELECT token_digest FROM client_keys WHERE id = ?", key.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if string(digest) == key.Token || len(digest) == 0 {
		t.Fatal("invalid token digest")
	}
}

// 结算必须幂等：同一笔预留被重复结算要报错，不能重复计费。
// 注意这里刻意不再断言"Key 用满即自动停用"——v0.3.2 起已移除该行为（旧行为会
// 让运维调大限额后 Key 依然用不了），额度判定改由每次请求时进行。
func TestSettlementIsIdempotent(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-idempotent", 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 3, 4, 0, 700, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 3, 4, 0, 700, "completed"); err == nil {
		t.Fatal("double settlement accepted")
	}
	current, err := repo.GetClientKey(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled {
		t.Fatalf("settlement must not disable the key any more: %#v", current)
	}
}

func ptrInt64(v int64) *int64 { return &v }
