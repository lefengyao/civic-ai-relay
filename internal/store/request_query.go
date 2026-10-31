package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type RequestFilter struct {
	RequestID  string
	Status     string
	Protocol   string
	ProviderID int64
	ModelID    int64
	BeforeID   int64
	FromUTC    *time.Time
	ToUTC      *time.Time
	Limit      int
}

type AnalyticsFilter struct {
	Days     int
	FromDate string
	ToDate   string
	Bucket   string
	Metric   string
	ModelID  int64
	ModelIDs []int64
	Status   string
}

type RequestPage struct {
	Data       []map[string]any `json:"data"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

var ErrInvalidRequestQuery = errors.New("invalid request query")

func normalizedLimit(limit int) (int, error) {
	if limit == 0 {
		return 50, nil
	}
	if limit < 1 || limit > 100 {
		return 0, fmt.Errorf("%w: limit must be 1..100", ErrInvalidRequestQuery)
	}
	return limit, nil
}

func EncodeRequestCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func DecodeRequestCursor(value string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) == 0 {
		return 0, fmt.Errorf("%w: invalid cursor", ErrInvalidRequestQuery)
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: invalid cursor", ErrInvalidRequestQuery)
	}
	return id, nil
}

const requestColumns = `id,request_id,model_id,provider_id,status,input_tokens,output_tokens,cached_input_tokens,amount_microyuan,upstream_status,streamed,created_at_utc,finished_at_utc,error_code,failure_stage,usage_source,protocol,client_request_id,incomplete_reason,duration_ms,upstream_header_ms,first_event_ms,last_upstream_ms,quote_input_microyuan,quote_output_microyuan,quote_cached_microyuan,quote_rate_milli`

func scanRequestRow(scanner interface{ Scan(...any) error }) (int64, map[string]any, error) {
	var id int64
	var requestID, status, created string
	var modelID, providerID, input, output, cached, amount, upstream sql.NullInt64
	var duration, header, firstEvent, lastUpstream, quoteInput, quoteOutput, quoteCached, quoteRate sql.NullInt64
	var errorCode, failureStage, usageSource, protocol, clientRequestID, incompleteReason string
	var streamed int
	var finished sql.NullString
	if err := scanner.Scan(&id, &requestID, &modelID, &providerID, &status, &input, &output, &cached, &amount, &upstream, &streamed, &created, &finished, &errorCode, &failureStage, &usageSource, &protocol, &clientRequestID, &incompleteReason, &duration, &header, &firstEvent, &lastUpstream, &quoteInput, &quoteOutput, &quoteCached, &quoteRate); err != nil {
		return 0, nil, err
	}
	return id, map[string]any{
		"request_id": requestID, "model_id": modelID.Int64, "provider_id": providerID.Int64, "status": status,
		"input_tokens": input.Int64, "output_tokens": output.Int64, "cached_input_tokens": cached.Int64,
		"amount_microyuan": amount.Int64, "upstream_status": nullableNumber(upstream), "streamed": streamed == 1,
		"created_at_utc": created, "finished_at_utc": finished.String,
		"error_code": errorCode, "failure_stage": failureStage, "usage_source": usageSource,
		"protocol": protocol, "client_request_id": clientRequestID, "incomplete_reason": incompleteReason,
		"duration_ms": nullableNumber(duration), "upstream_header_ms": nullableNumber(header),
		"first_event_ms": nullableNumber(firstEvent), "last_upstream_ms": nullableNumber(lastUpstream),
		"quote_input_microyuan": nullableNumber(quoteInput), "quote_output_microyuan": nullableNumber(quoteOutput),
		"quote_cached_microyuan": nullableNumber(quoteCached), "quote_rate_milli": nullableNumber(quoteRate),
	}, nil
}

func (s *Store) QueryRequests(ctx context.Context, filter RequestFilter) (RequestPage, error) {
	limit, err := normalizedLimit(filter.Limit)
	if err != nil {
		return RequestPage{}, err
	}
	if filter.ProviderID < 0 || filter.ModelID < 0 || filter.BeforeID < 0 {
		return RequestPage{}, fmt.Errorf("%w: negative ID", ErrInvalidRequestQuery)
	}
	if filter.FromUTC != nil && filter.ToUTC != nil && filter.FromUTC.After(*filter.ToUTC) {
		return RequestPage{}, fmt.Errorf("%w: from must not be after to", ErrInvalidRequestQuery)
	}
	args := make([]any, 0, 10)
	where := []string{"1=1"}
	if filter.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, filter.RequestID)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Protocol != "" {
		where = append(where, "protocol = ?")
		args = append(args, filter.Protocol)
	}
	if filter.ProviderID > 0 {
		where = append(where, "provider_id = ?")
		args = append(args, filter.ProviderID)
	}
	if filter.ModelID > 0 {
		where = append(where, "model_id = ?")
		args = append(args, filter.ModelID)
	}
	if filter.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, filter.BeforeID)
	}
	if filter.FromUTC != nil {
		where = append(where, "created_at_utc >= ?")
		args = append(args, filter.FromUTC.UTC().Format(time.RFC3339Nano))
	}
	if filter.ToUTC != nil {
		where = append(where, "created_at_utc <= ?")
		args = append(args, filter.ToUTC.UTC().Format(time.RFC3339Nano))
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return RequestPage{}, err
	}
	defer rows.Close()
	data := make([]map[string]any, 0, limit)
	var cursorID int64
	hasMore := false
	for rows.Next() {
		id, item, scanErr := scanRequestRow(rows)
		if scanErr != nil {
			return RequestPage{}, scanErr
		}
		if len(data) < limit {
			data = append(data, item)
			cursorID = id
		} else {
			hasMore = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return RequestPage{}, err
	}
	page := RequestPage{Data: data}
	if hasMore && cursorID > 0 {
		page.NextCursor = EncodeRequestCursor(cursorID)
	}
	return page, nil
}

func (s *Store) GetRequest(ctx context.Context, requestID string) (map[string]any, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("%w: request_id is required", ErrInvalidRequestQuery)
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE request_id = ?`, requestID)
	_, item, err := scanRequestRow(row)
	return item, err
}
