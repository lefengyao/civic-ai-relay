package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Analytics aggregates request metadata for the admin dashboard. It never reads
// prompts or response bodies, and every dimension is bounded to 30 days.
func (s *Store) Analytics(ctx context.Context, now time.Time, filter AnalyticsFilter) (map[string]any, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if filter.Days == 0 {
		filter.Days = 30
	}
	if filter.Days < 1 || filter.Days > 30 {
		return nil, errors.New("days must be between 1 and 30")
	}
	if filter.Bucket == "" || filter.Bucket == "auto" {
		if filter.Days <= 1 {
			filter.Bucket = "hour"
		} else {
			filter.Bucket = "day"
		}
	}
	if filter.Bucket != "hour" && filter.Bucket != "day" {
		return nil, errors.New("bucket must be hour or day")
	}
	if filter.Metric == "" {
		filter.Metric = "requests"
	}
	if filter.Metric != "requests" && filter.Metric != "tokens" && filter.Metric != "amount" {
		return nil, errors.New("metric must be requests, tokens or amount")
	}
	if filter.ModelID < 0 {
		return nil, errors.New("model_id must be positive")
	}
	modelIDs := append([]int64(nil), filter.ModelIDs...)
	if len(modelIDs) == 0 && filter.ModelID > 0 {
		modelIDs = append(modelIDs, filter.ModelID)
	}
	if len(modelIDs) > 20 {
		return nil, errors.New("at most 20 models may be selected")
	}
	for _, id := range modelIDs {
		if id <= 0 {
			return nil, errors.New("model_id must be positive")
		}
	}
	if filter.Status != "" && filter.Status != "all" && filter.Status != "completed" && filter.Status != "failed" && filter.Status != "aborted" && filter.Status != "rejected" && filter.Status != "reserved" {
		return nil, errors.New("unsupported status")
	}
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	from, to := now.Add(-time.Duration(filter.Days)*24*time.Hour), now
	if filter.FromDate != "" || filter.ToDate != "" {
		if filter.FromDate == "" || filter.ToDate == "" {
			return nil, errors.New("from_date and to_date must be provided together")
		}
		start, err := time.ParseInLocation("2006-01-02", filter.FromDate, location)
		if err != nil {
			return nil, errors.New("from_date must use YYYY-MM-DD")
		}
		end, err := time.ParseInLocation("2006-01-02", filter.ToDate, location)
		if err != nil {
			return nil, errors.New("to_date must use YYYY-MM-DD")
		}
		if end.Before(start) || end.Sub(start) >= 31*24*time.Hour {
			return nil, errors.New("date range must be between 1 and 31 days")
		}
		if start.After(now.In(location)) {
			return nil, errors.New("date range cannot start in the future")
		}
		from, to = start.UTC(), end.Add(24*time.Hour).Add(-time.Nanosecond).UTC()
		if to.After(now) {
			to = now
		}
		filter.Days = int(end.Sub(start)/(24*time.Hour)) + 1
	}
	fromText, toText := from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)
	dateExpr := `strftime('%Y-%m-%d', r.created_at_utc)`
	if filter.FromDate != "" {
		dateExpr = `strftime('%Y-%m-%d', r.created_at_utc, '+8 hours')`
	}
	if filter.Bucket == "hour" {
		dateExpr = `strftime('%Y-%m-%dT%H:00:00Z', r.created_at_utc)`
		if filter.FromDate != "" {
			dateExpr = `strftime('%Y-%m-%dT%H:00:00Z', r.created_at_utc, '+8 hours')`
		}
	}
	where := `r.created_at_utc>=? AND r.created_at_utc<=?`
	args := []any{fromText, toText}
	if filter.GroupID > 0 {
		where += ` AND r.group_id=?`
		args = append(args, filter.GroupID)
	}
	if len(modelIDs) > 0 {
		where += ` AND r.model_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(modelIDs)), ",") + `)`
		for _, id := range modelIDs {
			args = append(args, id)
		}
	}
	if filter.Status != "" && filter.Status != "all" {
		where += ` AND r.status=?`
		args = append(args, filter.Status)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+dateExpr+`,r.status,COUNT(*),COALESCE(SUM(r.input_tokens+r.output_tokens),0),COALESCE(SUM(r.amount_microyuan),0) FROM requests r WHERE `+where+` GROUP BY 1,2 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	buckets := map[string]map[string]int64{}
	for rows.Next() {
		var slot, status string
		var requests, tokens, amount int64
		if err := rows.Scan(&slot, &status, &requests, &tokens, &amount); err != nil {
			rows.Close()
			return nil, err
		}
		if buckets[slot] == nil {
			buckets[slot] = map[string]int64{}
		}
		buckets[slot][status+"_requests"] += requests
		buckets[slot][status+"_tokens"] += tokens
		buckets[slot][status+"_amount_microyuan"] += amount
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	ordered := make([]map[string]any, 0, len(buckets))
	pointFor := func(slot string, metrics map[string]int64) map[string]any {
		value := int64(0)
		switch filter.Metric {
		case "requests":
			for _, status := range []string{"completed", "failed", "aborted", "rejected", "reserved"} {
				value += metrics[status+"_requests"]
			}
		case "tokens":
			value = metrics["completed_tokens"] + metrics["failed_tokens"] + metrics["aborted_tokens"]
		case "amount":
			value = metrics["completed_amount_microyuan"] + metrics["failed_amount_microyuan"] + metrics["aborted_amount_microyuan"]
		}
		point := map[string]any{
			"time": slot, "value": value,
			"completed": metrics["completed_requests"], "failed": metrics["failed_requests"],
			"aborted": metrics["aborted_requests"], "rejected": metrics["rejected_requests"],
			"reserved":         metrics["reserved_requests"],
			"tokens":           metrics["completed_tokens"] + metrics["failed_tokens"] + metrics["aborted_tokens"],
			"amount_microyuan": metrics["completed_amount_microyuan"] + metrics["failed_amount_microyuan"] + metrics["aborted_amount_microyuan"],
		}
		for _, status := range []string{"completed", "failed", "aborted", "rejected", "reserved"} {
			point[status+"_tokens"] = metrics[status+"_tokens"]
			point[status+"_amount_microyuan"] = metrics[status+"_amount_microyuan"]
		}
		return point
	}
	// Return a complete time axis so charts do not collapse quiet periods or
	// imply that adjacent samples were consecutive. Empty buckets carry zeros.
	step := 24 * time.Hour
	start := from.UTC().Truncate(24 * time.Hour)
	end := now.UTC().Truncate(24 * time.Hour)
	if filter.FromDate != "" {
		localFrom, localTo := from.In(location), to.In(location)
		start = time.Date(localFrom.Year(), localFrom.Month(), localFrom.Day(), 0, 0, 0, 0, location)
		end = time.Date(localTo.Year(), localTo.Month(), localTo.Day(), 0, 0, 0, 0, location)
	}
	if filter.Bucket == "hour" {
		step = time.Hour
		if filter.FromDate != "" {
			localFrom, localTo := from.In(location), to.In(location)
			start = time.Date(localFrom.Year(), localFrom.Month(), localFrom.Day(), localFrom.Hour(), 0, 0, 0, location)
			end = time.Date(localTo.Year(), localTo.Month(), localTo.Day(), localTo.Hour(), 0, 0, 0, location)
		} else {
			start = from.UTC().Truncate(time.Hour)
			end = now.UTC().Truncate(time.Hour)
		}
	}
	for slot := start; !slot.After(end); slot = slot.Add(step) {
		key := slot.Format("2006-01-02")
		if filter.FromDate != "" {
			key = slot.In(location).Format("2006-01-02")
		}
		if filter.Bucket == "hour" {
			key = slot.Format("2006-01-02T15:04:05Z")
			if filter.FromDate != "" {
				key = slot.In(location).Format("2006-01-02T15:04:05Z")
			}
		}
		ordered = append(ordered, pointFor(key, buckets[key]))
	}

	modelWhere := `r.created_at_utc>=? AND r.created_at_utc<=?`
	modelArgs := []any{fromText, toText}
	if filter.GroupID > 0 {
		modelWhere += ` AND r.group_id=?`
		modelArgs = append(modelArgs, filter.GroupID)
	}
	if filter.Status != "" && filter.Status != "all" {
		modelWhere += ` AND r.status=?`
		modelArgs = append(modelArgs, filter.Status)
	}
	if len(modelIDs) > 0 {
		modelWhere += ` AND r.model_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(modelIDs)), ",") + `)`
		for _, id := range modelIDs {
			modelArgs = append(modelArgs, id)
		}
	}
	modelRows, err := s.db.QueryContext(ctx, `SELECT r.model_id,COALESCE(m.public_name,''),COUNT(*),COALESCE(SUM(r.input_tokens+r.output_tokens),0),COALESCE(SUM(r.amount_microyuan),0) FROM requests r LEFT JOIN models m ON m.id=r.model_id WHERE `+modelWhere+` GROUP BY r.model_id,m.public_name ORDER BY COUNT(*) DESC,r.model_id`, modelArgs...)
	if err != nil {
		return nil, err
	}
	models := make([]map[string]any, 0)
	for modelRows.Next() {
		// 删除渠道会级联删模型，历史请求的 model_id 可空，必须按可空值读取。
		var id sql.NullInt64
		var count, tokens, amount int64
		var name string
		if err := modelRows.Scan(&id, &name, &count, &tokens, &amount); err != nil {
			modelRows.Close()
			return nil, err
		}
		models = append(models, map[string]any{"model_id": nullableNumber(id), "model": name, "count": count, "tokens": tokens, "amount_microyuan": amount})
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return nil, err
	}
	modelRows.Close()
	return map[string]any{"from_utc": fromText, "to_utc": toText, "from_date": filter.FromDate, "to_date": filter.ToDate, "days": filter.Days, "bucket": filter.Bucket, "metric": filter.Metric, "series": ordered, "models": models}, nil
}
