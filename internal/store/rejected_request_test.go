package store

import (
	"database/sql"
	"testing"
	"time"
)

// 入口拒绝（请求体过大、不是合法 JSON）必须真的落进 requests 表，否则
// 「客户端收到 413、管理台却查不到」这个盲区依然存在。
func TestRecordRejectedRequestPersists(t *testing.T) {
	repo := newTestStore(t)
	ctx := t.Context()

	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 11, 14, 7, 30, 0, 0, time.UTC)
	if err := repo.RecordRejectedRequest(ctx, RejectedRequest{
		RequestID: "relay-rejected-1",
		KeyID:     key.ID,
		Protocol:  "chat",
		ErrorCode: "request_too_large",
		StartedAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	page, err := repo.QueryRequests(ctx, RequestFilter{Status: "rejected", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("按 status=rejected 应查到 1 条，得到 %d", len(page.Data))
	}
	item := page.Data[0]
	if item["request_id"] != "relay-rejected-1" {
		t.Fatalf("request_id = %v", item["request_id"])
	}
	// 没读到 body，所以没有模型、没有渠道、没有 token 计数 —— 必须是 0/NULL，
	// 不能因为默认值缺失而让用量统计凭空多出一次输入。
	if item["status"] != "rejected" || item["error_code"] != "request_too_large" || item["failure_stage"] != "ingest" {
		t.Fatalf("行内容 = %+v", item)
	}
	if item["input_tokens"] != int64(0) || item["output_tokens"] != int64(0) || item["amount_microyuan"] != int64(0) {
		t.Fatalf("拒绝行不应有任何用量：%+v", item)
	}
	if item["model_id"] != int64(0) || item["provider_id"] != int64(0) {
		t.Fatalf("拒绝行不应挂模型/渠道：%+v", item)
	}

	var keyID sql.NullInt64
	var billingDate string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT key_id,billing_date_bj FROM requests WHERE request_id=?`, "relay-rejected-1").Scan(&keyID, &billingDate); err != nil {
		t.Fatal(err)
	}
	if !keyID.Valid || keyID.Int64 != key.ID {
		t.Fatalf("key_id = %+v，应记下带凭据的调用方", keyID)
	}
	// 计费窗口按北京时间切：UTC 07:30 属于北京时间当天 15 点。
	if billingDate != "2026-11-14" {
		t.Fatalf("billing_date_bj = %q", billingDate)
	}
}

// 认不出凭据时 key_id 必须是 NULL（而不是 0）：外键指向 client_keys(id)，
// 写 0 会直接被约束挡下，整条记录都丢了。
func TestRecordRejectedRequestWithoutKey(t *testing.T) {
	repo := newTestStore(t)
	ctx := t.Context()

	if err := repo.RecordRejectedRequest(ctx, RejectedRequest{
		RequestID: "relay-rejected-anon",
		ErrorCode: "invalid_request",
	}); err != nil {
		t.Fatal(err)
	}

	var keyID sql.NullInt64
	var stage, protocol string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT key_id,failure_stage,protocol FROM requests WHERE request_id=?`, "relay-rejected-anon").Scan(&keyID, &stage, &protocol); err != nil {
		t.Fatal(err)
	}
	if keyID.Valid {
		t.Fatalf("匿名拒绝的 key_id 应为 NULL，得到 %+v", keyID)
	}
	// 未显式指定阶段时默认 ingest：与上游失败区分开。
	if stage != "ingest" || protocol != "" {
		t.Fatalf("stage=%q protocol=%q", stage, protocol)
	}
}

// 没有 reservation 的拒绝行不能被「回收残留预留」之类的维护任务当成异常处理，
// 也不能被并发统计误算成「有请求在跑」。
func TestRecordRejectedRequestDoesNotOccupyConcurrency(t *testing.T) {
	repo := newTestStore(t)
	ctx := t.Context()

	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordRejectedRequest(ctx, RejectedRequest{
		RequestID: "relay-rejected-conc",
		KeyID:     key.ID,
		ErrorCode: "request_too_large",
	}); err != nil {
		t.Fatal(err)
	}

	active, err := repo.ActiveReservationsByKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active[key.ID] != 0 {
		t.Fatalf("拒绝行不应占用并发：%+v", active)
	}
	page, err := repo.QueryRequests(ctx, RequestFilter{Status: "reserved", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 0 {
		t.Fatalf("不应出现 reserved 行：%+v", page.Data)
	}
}

// request_id 是账本主键（UNIQUE）：重复写入必须报错而不是静默覆盖，
// 否则同一请求被记两遍会污染统计。
func TestRecordRejectedRequestRejectsDuplicateID(t *testing.T) {
	repo := newTestStore(t)
	ctx := t.Context()

	in := RejectedRequest{RequestID: "relay-rejected-dup", ErrorCode: "invalid_request"}
	if err := repo.RecordRejectedRequest(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordRejectedRequest(ctx, in); err == nil {
		t.Fatal("重复 request_id 应返回错误")
	}
	if err := repo.RecordRejectedRequest(ctx, RejectedRequest{RequestID: "  ", ErrorCode: "invalid_request"}); err == nil {
		t.Fatal("空 request_id 应返回错误")
	}
}
