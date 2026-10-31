package store

import (
	"context"
	"testing"
	"time"
)

func TestAnalyticsFiltersRangeStatusModelAndBuckets(t *testing.T) {
	repo, _, model := newReservationFixture(t, 100000, 100000000)
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	seed := func(id, status string, at time.Time, tokens, amount int64) {
		t.Helper()
		_, err := repo.db.ExecContext(ctx, `INSERT INTO requests(request_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES(?,?,?,?,?,?,?,?)`, id, model.ID, 1, status, tokens, 0, amount, at.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("analytics-recent-ok", "completed", now.Add(-2*time.Hour), 10, 100)
	seed("analytics-recent-fail", "failed", now.Add(-3*time.Hour), 7, 30)
	seed("analytics-old", "completed", now.Add(-10*24*time.Hour), 50, 500)

	data, err := repo.Analytics(ctx, now, AnalyticsFilter{Days: 30, Bucket: "hour", Metric: "tokens", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	series := data["series"].([]map[string]any)
	if len(series) != 721 {
		t.Fatalf("30-day hourly buckets = %d, want all 721 hours including empty buckets", len(series))
	}
	if series[0]["time"] != "2026-09-28T12:00:00Z" {
		t.Fatalf("oldest bucket = %#v", series[0]["time"])
	}
	if series[0]["value"] != int64(0) || series[1]["value"] != int64(0) || series[717]["value"] != int64(7) || series[718]["value"] != int64(10) {
		t.Fatalf("hourly values should include request data and zero-filled gaps: first=%#v gap=%#v failed=%#v completed=%#v", series[0]["value"], series[1]["value"], series[717]["value"], series[718]["value"])
	}
	models := data["models"].([]map[string]any)
	if len(models) != 1 || models[0]["count"] != int64(3) || models[0]["tokens"] != int64(67) {
		t.Fatalf("models = %#v", models)
	}
	filtered, err := repo.Analytics(ctx, now, AnalyticsFilter{Days: 7, Bucket: "day", Metric: "requests", Status: "failed", ModelID: model.ID})
	if err != nil {
		t.Fatal(err)
	}
	filteredSeries := filtered["series"].([]map[string]any)
	var failedPoint map[string]any
	for _, point := range filteredSeries {
		if point["failed"] == int64(1) {
			failedPoint = point
			break
		}
	}
	if len(filteredSeries) != 8 || failedPoint == nil || failedPoint["value"] != int64(1) {
		t.Fatalf("filtered series = %#v", filteredSeries)
	}
}

func TestAnalyticsRejectsInvalidRange(t *testing.T) {
	repo := newTestStore(t)
	for _, filter := range []AnalyticsFilter{{Days: 31}, {Days: 7, Bucket: "minute"}, {Days: 7, Metric: "latency"}, {Days: 7, Status: "unknown"}, {FromDate: "2026-10-01"}, {FromDate: "2026-09-01", ToDate: "2026-10-02"}, {FromDate: "2026-10-03", ToDate: "2026-10-02"}} {
		if _, err := repo.Analytics(context.Background(), time.Now(), filter); err == nil {
			t.Fatalf("filter %#v should be rejected", filter)
		}
	}
}

func TestAnalyticsCalendarRangeUsesBeijingDaysAndZeroFills(t *testing.T) {
	repo, _, model := newReservationFixture(t, 100000, 100000000)
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	seedAt := time.Date(2026, 10, 28, 0, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO requests(request_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,created_at_utc) VALUES(?,?,?,?,?,?,?,?)`, "analytics-calendar", model.ID, 1, "completed", 12, 8, 2400, seedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	data, err := repo.Analytics(ctx, now, AnalyticsFilter{FromDate: "2026-10-27", ToDate: "2026-10-28", Bucket: "day", Metric: "tokens"})
	if err != nil {
		t.Fatal(err)
	}
	series := data["series"].([]map[string]any)
	if len(series) != 2 || series[0]["time"] != "2026-10-27" || series[0]["value"] != int64(0) || series[1]["time"] != "2026-10-28" || series[1]["value"] != int64(20) {
		t.Fatalf("calendar series = %#v", series)
	}
	if data["days"] != 2 || data["from_date"] != "2026-10-27" || data["to_date"] != "2026-10-28" {
		t.Fatalf("calendar range metadata = %#v", data)
	}
}
