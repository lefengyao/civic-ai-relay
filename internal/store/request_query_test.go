package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func queryTestStore(t *testing.T) *Store {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "relay.db"), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func seedRequestQueryRows(t *testing.T, repo *Store, count int) (int64, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	const nowText = "2026-10-03T00:00:00Z"
	provider, err := repo.db.ExecContext(ctx, `INSERT INTO providers(name,base_url,api_key_ciphertext,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?)`, "query-provider", "https://provider.example", []byte("ciphertext"), nowText, nowText)
	if err != nil {
		t.Fatal(err)
	}
	providerID, err := provider.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	model, err := repo.db.ExecContext(ctx, `INSERT INTO models(provider_id,public_name,upstream_name,input_price_microyuan,output_price_microyuan,enabled,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?)`, providerID, "query-model", "query-model", 1, 1, 1, nowText, nowText)
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := model.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		status := "completed"
		protocol := "chat"
		if i%2 == 1 {
			status = "failed"
			protocol = "responses"
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO requests(request_id,model_id,provider_id,status,input_tokens,output_tokens,cached_input_tokens,amount_microyuan,upstream_status,streamed,created_at_utc,finished_at_utc,error_code,failure_stage,usage_source,protocol,client_request_id,incomplete_reason,duration_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"query-"+itoaQuery(int64(i+1)), modelID, providerID, status, i+1, i+2, i%3, i+10, 200, i%2, base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339Nano), base.Add(time.Duration(i+1)*time.Minute).Format(time.RFC3339Nano), "", "", "unknown", protocol, "client-"+itoaQuery(int64(i+1)), "", i+1); err != nil {
			t.Fatal(err)
		}
	}
	return providerID, modelID, base
}

func itoaQuery(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestQueryRequestsPaginatesByDescendingIDWithoutDuplicates(t *testing.T) {
	repo := queryTestStore(t)
	seedRequestQueryRows(t, repo, 60)
	ctx := context.Background()
	page, err := repo.QueryRequests(ctx, RequestFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 20 || page.NextCursor == "" {
		t.Fatalf("first page len=%d cursor=%q", len(page.Data), page.NextCursor)
	}
	seen := make(map[string]bool, 60)
	pageIndex := 0
	for {
		for itemIndex, item := range page.Data {
			id, ok := item["request_id"].(string)
			if !ok || seen[id] {
				t.Fatalf("duplicate or invalid request id %v", item["request_id"])
			}
			seen[id] = true
			want := int64(60 - pageIndex*20 - itemIndex)
			if id != "query-"+itoaQuery(want) {
				t.Fatalf("page order at %d/%d: got %q want query-%d", pageIndex, itemIndex, id, want)
			}
		}
		if page.NextCursor == "" {
			break
		}
		before, err := DecodeRequestCursor(page.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		page, err = repo.QueryRequests(ctx, RequestFilter{BeforeID: before, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		pageIndex++
	}
	if len(seen) != 60 {
		t.Fatalf("pagination returned %d unique rows, want 60", len(seen))
	}
}

func TestQueryRequestsAppliesCombinedFilters(t *testing.T) {
	repo := queryTestStore(t)
	providerID, modelID, base := seedRequestQueryRows(t, repo, 10)
	from := base.Add(4 * time.Minute)
	to := base.Add(4 * time.Minute)
	page, err := repo.QueryRequests(context.Background(), RequestFilter{
		RequestID: "query-5", Status: "completed", Protocol: "chat", ProviderID: providerID, ModelID: modelID,
		FromUTC: &from, ToUTC: &to, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0]["request_id"] != "query-5" {
		t.Fatalf("filtered page=%v", page.Data)
	}
	if page.Data[0]["provider_id"] != providerID || page.Data[0]["model_id"] != modelID {
		t.Fatalf("filtered IDs=%v", page.Data[0])
	}
}

func TestRequestQueryRejectsInvalidCursorAndLimit(t *testing.T) {
	if _, err := DecodeRequestCursor("not-base64"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	for _, limit := range []int{-1, 101} {
		if _, err := normalizedLimit(limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
	if got, err := normalizedLimit(0); err != nil || got != 50 {
		t.Fatalf("normalized default=%d err=%v", got, err)
	}
	repo := queryTestStore(t)
	from := time.Now().UTC()
	to := from.Add(-time.Minute)
	if _, err := repo.QueryRequests(context.Background(), RequestFilter{FromUTC: &from, ToUTC: &to}); err == nil {
		t.Fatal("reversed time range accepted")
	}
	if _, err := repo.QueryRequests(context.Background(), RequestFilter{ProviderID: -1}); err == nil {
		t.Fatal("negative provider id accepted")
	}
}

func TestGetRequestReturnsMetadataOnly(t *testing.T) {
	repo := queryTestStore(t)
	seedRequestQueryRows(t, repo, 1)
	item, err := repo.GetRequest(context.Background(), "query-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"prompt", "body", "secret", "tool", "authorization", "api_key", "key_id"} {
		if _, found := item[forbidden]; found {
			t.Fatalf("metadata includes forbidden field %q: %v", forbidden, item)
		}
	}
	if item["request_id"] != "query-1" || item["status"] != "completed" {
		t.Fatalf("unexpected metadata: %v", item)
	}
	if _, err := repo.GetRequest(context.Background(), "missing"); err == nil {
		t.Fatal("missing request returned no error")
	}
}

func TestRecentRequestsKeepsLegacyDataShape(t *testing.T) {
	repo := queryTestStore(t)
	seedRequestQueryRows(t, repo, 2)
	data, err := repo.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 || data[0]["request_id"] != "query-2" {
		t.Fatalf("recent data=%v", data)
	}
	if data[0]["duration_ms"] != int64(2) {
		t.Fatalf("diagnostic fields missing: %v", data[0])
	}
	if reflect.TypeOf(data).Kind() != reflect.Slice {
		t.Fatalf("legacy data type=%T", data)
	}
}
