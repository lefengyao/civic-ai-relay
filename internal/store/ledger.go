package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// RequestMessage contains the user-visible string fields that contribute to a
// conservative input-token estimate. The strings themselves are never stored.
type RequestMessage struct {
	Role, Content, Name, ToolName, ToolCallID string
}

type ReserveInput struct {
	RequestID                                 string
	KeyID, ModelID, ProviderID                int64
	StartedAt                                 time.Time
	Stream                                    bool
	InputText                                 string
	Messages                                  []RequestMessage
	StringFields                              []string
	InputTokens                               int64
	OutputTokenCeiling                        int64
	MaxOutputTokens                           int64
	InputPriceMicroyuan, OutputPriceMicroyuan int64
	RPMLimit, TokenLimit5H, TokenLimitDaily   int64
}

type RequestReservation struct {
	ID, KeyReservationID                    int64
	RequestID                               string
	StartedAt                               time.Time
	ReservedTokens, ReservedAmountMicroyuan int64
}

type QuotaError struct{ Code string }

func (e *QuotaError) Error() string {
	if e == nil {
		return "quota exceeded"
	}
	return e.Code
}

type TrendBucket struct {
	StartedAtUTC string `json:"started_at_utc"`
	Count        int    `json:"count"`
}

func estimateInputTokens(in ReserveInput) int64 {
	if in.InputTokens > 0 {
		return in.InputTokens
	}
	bytes, runes := 0, 0
	add := func(value string) { bytes += len(value); runes += utf8.RuneCountInString(value) }
	add(in.InputText)
	for _, m := range in.Messages {
		add(m.Role)
		add(m.Content)
		add(m.Name)
		add(m.ToolName)
		add(m.ToolCallID)
	}
	for _, value := range in.StringFields {
		add(value)
	}
	if bytes == 0 {
		return 1
	}
	byBytes := (bytes + 3) / 4
	if runes > byBytes {
		return int64(runes)
	}
	return int64(byBytes)
}

func ceilPrice(tokens, price int64) (int64, error) {
	if tokens < 0 || price < 0 {
		return 0, errors.New("token and price values must be non-negative")
	}
	if tokens == 0 || price == 0 {
		return 0, nil
	}
	if tokens > (math.MaxInt64-999999)/price {
		return 0, errors.New("reservation amount overflow")
	}
	return (tokens*price + 999999) / 1000000, nil
}

func reserveAmount(inputTokens, outputTokens, inputPrice, outputPrice int64) (int64, error) {
	in, err := ceilPrice(inputTokens, inputPrice)
	if err != nil {
		return 0, err
	}
	out, err := ceilPrice(outputTokens, outputPrice)
	if err != nil {
		return 0, err
	}
	if in > math.MaxInt64-out {
		return 0, errors.New("reservation amount overflow")
	}
	return in + out, nil
}

func billingDate(t time.Time) string {
	return t.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
}

func (s *Store) ReserveRequest(ctx context.Context, in ReserveInput) (RequestReservation, error) {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if strings.TrimSpace(in.RequestID) == "" || in.KeyID <= 0 || in.ModelID <= 0 || in.ProviderID <= 0 {
		return RequestReservation{}, errors.New("request metadata is required")
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = time.Now().UTC()
	}
	if in.InputTokens < 0 {
		return RequestReservation{}, errors.New("input token estimate must be non-negative")
	}
	in.StartedAt = in.StartedAt.UTC()
	inputTokens := estimateInputTokens(in)
	outputTokens := in.OutputTokenCeiling
	if outputTokens <= 0 {
		outputTokens = in.MaxOutputTokens
	}
	if outputTokens <= 0 {
		return RequestReservation{}, errors.New("output token ceiling must be positive")
	}
	reservedTokens := inputTokens + outputTokens
	if reservedTokens < inputTokens {
		return RequestReservation{}, errors.New("reservation token overflow")
	}
	for name, value := range map[string]int64{"rpm_limit": in.RPMLimit, "token_limit_5h": in.TokenLimit5H, "token_limit_daily": in.TokenLimitDaily} {
		if value <= 0 {
			return RequestReservation{}, fmt.Errorf("%s must be positive", name)
		}
	}
	started := in.StartedAt.Format(time.RFC3339Nano)
	date := billingDate(in.StartedAt)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RequestReservation{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RequestReservation{}, err
	}
	rollback := func(e error) (RequestReservation, error) {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return RequestReservation{}, e
	}
	var enabled, concurrency int
	var tokenLimit, amountLimit sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT enabled,concurrency_limit,token_limit,amount_limit_microyuan FROM client_keys WHERE id=?`, in.KeyID).Scan(&enabled, &concurrency, &tokenLimit, &amountLimit); err != nil {
		return rollback(err)
	}
	if enabled != 1 {
		return rollback(errors.New("client key is disabled"))
	}
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM key_reservations WHERE key_id=? AND status='reserved'`, in.KeyID).Scan(&active); err != nil {
		return rollback(err)
	}
	if active >= concurrency {
		return rollback(&QuotaError{Code: "concurrency_exceeded"})
	}
	var inputPrice, outputPrice sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT m.input_price_microyuan,m.output_price_microyuan FROM key_groups kg JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_models gm ON gm.group_id=g.id JOIN models m ON m.id=gm.model_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE kg.key_id=? AND m.id=? AND p.id=? LIMIT 1`, in.KeyID, in.ModelID, in.ProviderID).Scan(&inputPrice, &outputPrice); err != nil {
		return rollback(err)
	}
	if !inputPrice.Valid || !outputPrice.Valid {
		return rollback(errors.New("model is not authorized for key"))
	}
	amount, err := reserveAmount(inputTokens, outputTokens, inputPrice.Int64, outputPrice.Int64)
	if err != nil {
		return rollback(err)
	}
	var rpm int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE created_at_utc>=datetime(?, '-60 seconds') AND created_at_utc<=?`, started, started).Scan(&rpm); err != nil {
		return rollback(err)
	}
	if rpm >= int(in.RPMLimit) {
		return rollback(&QuotaError{Code: "rpm_exceeded"})
	}
	var fiveHour, daily int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND created_at_utc>=datetime(?, '-5 hours') AND created_at_utc<=?`, started, started).Scan(&fiveHour); err != nil {
		return rollback(err)
	}
	if fiveHour > in.TokenLimit5H-reservedTokens {
		return rollback(&QuotaError{Code: "token_quota_exceeded"})
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND billing_date_bj=? AND created_at_utc<=?`, date, started).Scan(&daily); err != nil {
		return rollback(err)
	}
	if daily > in.TokenLimitDaily-reservedTokens {
		return rollback(&QuotaError{Code: "token_quota_exceeded"})
	}
	var keyTokens, keyAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_tokens+reserved_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE key_id=? AND status IN ('reserved','completed','failed','aborted')`, in.KeyID).Scan(&keyTokens, &keyAmount); err != nil {
		return rollback(err)
	}
	if tokenLimit.Valid && keyTokens > tokenLimit.Int64-reservedTokens {
		return rollback(&QuotaError{Code: "key_token_quota_exceeded"})
	}
	if amountLimit.Valid && keyAmount > amountLimit.Int64-amount {
		return rollback(&QuotaError{Code: "key_amount_quota_exceeded"})
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO requests(request_id,key_id,model_id,provider_id,status,input_tokens,reserved_tokens,amount_microyuan,streamed,billing_date_bj,created_at_utc) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, in.RequestID, in.KeyID, in.ModelID, in.ProviderID, "reserved", inputTokens, reservedTokens, amount, boolInt(in.Stream), date, started)
	if err != nil {
		return rollback(err)
	}
	requestRowID, err := result.LastInsertId()
	if err != nil {
		return rollback(err)
	}
	result, err = conn.ExecContext(ctx, `INSERT INTO key_reservations(request_id,billing_date_bj,key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (?,?,?,?,?,?,?,?)`, in.RequestID, date, in.KeyID, in.ModelID, reservedTokens, amount, "reserved", started)
	if err != nil {
		return rollback(err)
	}
	reservationID, err := result.LastInsertId()
	if err != nil {
		return rollback(err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(err)
	}
	return RequestReservation{ID: requestRowID, KeyReservationID: reservationID, RequestID: in.RequestID, StartedAt: in.StartedAt, ReservedTokens: reservedTokens, ReservedAmountMicroyuan: amount}, nil
}

func (s *Store) CancelRequest(ctx context.Context, reservationID int64, status string, httpStatus int) error {
	if reservationID <= 0 {
		return errors.New("reservation ID is required")
	}
	if status == "" {
		status = "rejected"
	}
	if status != "rejected" && status != "aborted" && status != "failed" {
		return errors.New("cancel status must be rejected, aborted, or failed")
	}
	return s.finishRequest(ctx, reservationID, 0, 0, 0, status, httpStatus)
}

func (s *Store) SettleRequest(ctx context.Context, reservationID, inputTokens, outputTokens, amount int64, status string) error {
	if reservationID <= 0 || inputTokens < 0 || outputTokens < 0 || amount < 0 {
		return errors.New("invalid settlement")
	}
	if status == "" {
		status = "completed"
	}
	return s.finishRequest(ctx, reservationID, inputTokens, outputTokens, amount, status, 200)
}

func (s *Store) finishRequest(ctx context.Context, requestID, inputTokens, outputTokens, amount int64, status string, httpStatus int) error {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if status != "completed" && status != "failed" && status != "aborted" && status != "rejected" {
		return errors.New("invalid settlement status")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	fail := func(e error) error { _, _ = conn.ExecContext(context.Background(), "ROLLBACK"); return e }
	var keyID, keyReservationID int64
	var requestToken string
	var current string
	if err := conn.QueryRowContext(ctx, `SELECT key_id,request_id FROM requests WHERE id=? AND status='reserved'`, requestID).Scan(&keyID, &requestToken); err != nil {
		return fail(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT id,status FROM key_reservations WHERE request_id=? AND status='reserved' ORDER BY id DESC LIMIT 1`, requestToken).Scan(&keyReservationID, &current); err != nil {
		return fail(err)
	}
	chargedTokens, chargedAmount := inputTokens+outputTokens, amount
	if _, err := conn.ExecContext(ctx, `UPDATE key_reservations SET charged_tokens=?,charged_amount_microyuan=?,reserved_tokens=0,reserved_amount_microyuan=0,status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, chargedAmount, status, nowUTC(), keyReservationID); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE requests SET reserved_tokens=0,charged_tokens=?,input_tokens=?,output_tokens=?,amount_microyuan=?,status=?,upstream_status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, inputTokens, outputTokens, chargedAmount, status, httpStatus, nowUTC(), requestID); err != nil {
		return fail(err)
	}
	var tokenLimit, amountLimit sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT token_limit,amount_limit_microyuan FROM client_keys WHERE id=?`, keyID).Scan(&tokenLimit, &amountLimit); err != nil {
		return fail(err)
	}
	var totalTokens, totalAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_tokens),0),COALESCE(SUM(charged_amount_microyuan),0) FROM key_reservations WHERE key_id=? AND status IN ('completed','failed','aborted')`, keyID).Scan(&totalTokens, &totalAmount); err != nil {
		return fail(err)
	}
	if (tokenLimit.Valid && totalTokens >= tokenLimit.Int64) || (amountLimit.Valid && totalAmount >= amountLimit.Int64) {
		if _, err := conn.ExecContext(ctx, `UPDATE client_keys SET enabled=0,disabled_reason='quota_exhausted',updated_at_utc=? WHERE id=?`, nowUTC(), keyID); err != nil {
			return fail(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	return nil
}

func (s *Store) OccupiedTokens(ctx context.Context, startedAt time.Time) int64 {
	var total int64
	started := startedAt.UTC().Format(time.RFC3339Nano)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND created_at_utc>=datetime(?, '-5 hours') AND created_at_utc<=?`, started, started).Scan(&total)
	return total
}

func (s *Store) RecentRequests(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,model_id,provider_id,status,input_tokens,output_tokens,amount_microyuan,upstream_status,streamed,created_at_utc,finished_at_utc FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var requestID, status, created string
		var modelID, providerID, input, output, amount, upstream sql.NullInt64
		var streamed int
		var finished sql.NullString
		if err := rows.Scan(&requestID, &modelID, &providerID, &status, &input, &output, &amount, &upstream, &streamed, &created, &finished); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"request_id": requestID, "model_id": modelID.Int64, "provider_id": providerID.Int64, "status": status, "input_tokens": input.Int64, "output_tokens": output.Int64, "amount_microyuan": amount.Int64, "upstream_status": upstream.Int64, "streamed": streamed == 1, "created_at_utc": created, "finished_at_utc": finished.String})
	}
	return out, rows.Err()
}

// Healthcheck performs a short write transaction so callers detect both a
// closed database and a blocked SQLite writer before accepting traffic.
func (s *Store) Healthcheck(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SELECT 1"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	_, err = conn.ExecContext(ctx, "ROLLBACK")
	return err
}

// Prune removes old request and reservation metadata. The cutoff is supplied
// by the caller so retention policy remains outside the persistence layer.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	cutoffText := cutoff.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE created_at_utc < ?`, cutoffText)
	if err != nil {
		return 0, err
	}
	// Reservations may include legacy rows without a matching request.
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_reservations WHERE created_at_utc < ?`, cutoffText); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Overview returns metadata-only counters used by the administrator console.
// It deliberately accepts limits as arguments so configuration remains outside
// the store and no credentials or prompts can enter the response.
func (s *Store) Overview(ctx context.Context, now time.Time, rpmLimit, fiveHourLimit, dailyLimit int64) (map[string]any, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	minuteStart := now.Add(-time.Minute).Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	hourStart := now.Add(-time.Hour)
	date := billingDate(now)
	var rpm int
	var fiveHour, daily int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE created_at_utc>=? AND created_at_utc<=?`, minuteStart, nowText).Scan(&rpm); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND created_at_utc>=? AND created_at_utc<=?`, fiveHourStart, nowText).Scan(&fiveHour); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND billing_date_bj=? AND created_at_utc<=?`, date, nowText).Scan(&daily); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM requests WHERE created_at_utc>=? AND created_at_utc<=? GROUP BY status`, hourStart.Format(time.RFC3339Nano), nowText)
	if err != nil {
		return nil, err
	}
	outcomes := map[string]int{"completed": 0, "failed": 0, "aborted": 0, "rejected": 0, "reserved": 0}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := outcomes[status]; ok {
			outcomes[status] = count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	trend := make([]TrendBucket, 12)
	for i := range trend {
		trend[i].StartedAtUTC = hourStart.Add(time.Duration(i) * 5 * time.Minute).Format(time.RFC3339)
	}
	trendRows, err := s.db.QueryContext(ctx, `SELECT created_at_utc FROM requests WHERE created_at_utc>=? AND created_at_utc<=?`, hourStart.Format(time.RFC3339Nano), nowText)
	if err != nil {
		return nil, err
	}
	for trendRows.Next() {
		var started string
		if err := trendRows.Scan(&started); err != nil {
			trendRows.Close()
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, started)
		if err != nil {
			continue
		}
		bucket := int(parsed.Sub(hourStart).Minutes() / 5)
		if bucket < 0 {
			bucket = 0
		}
		if bucket > 11 {
			bucket = 11
		}
		trend[bucket].Count++
	}
	trendRows.Close()
	recent, err := s.RecentRequests(ctx, 10)
	if err != nil {
		return nil, err
	}
	denominator := outcomes["completed"] + outcomes["failed"]
	errorRate := 0.0
	if denominator > 0 {
		errorRate = float64(outcomes["failed"]) / float64(denominator)
	}
	return map[string]any{
		"generated_at_utc": nowText,
		"rpm":              map[string]any{"used": rpm, "limit": rpmLimit},
		"five_hour":        map[string]any{"used_tokens": fiveHour, "limit": fiveHourLimit},
		"daily":            map[string]any{"used_tokens": daily, "limit": dailyLimit},
		"last_hour":        map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"trend":            trend,
		"recent":           recent,
	}, nil
}
