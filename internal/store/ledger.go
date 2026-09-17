package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// QuotaLimits 是一次预留判定需要的全部限额。任一字段为 0 表示该窗口不限。
// 三个窗口的语义：5h 为滚动五小时；daily 为北京时间自然日；weekly 为北京时间
// 自然周（周一 00:00 CST 起）。
type QuotaLimits struct {
	RPMLimit                            int64
	Token5H, TokenDaily, TokenWeekly    int64
	Amount5H, AmountDaily, AmountWeekly int64 // 微元
}

// QuotaError 携带判定明细，供 HTTP 层返回可读原因与正确的状态码。
type QuotaError struct {
	Code   string // token_quota_exceeded / amount_quota_exceeded / key_* / rpm_exceeded / concurrency_exceeded
	Scope  string // 5h / daily / weekly / key / ""
	Metric string // tokens / amount / ""
	Used   int64  // tokens 或微元，取决于 Metric
	Limit  int64
}

func (e *QuotaError) Error() string {
	if e == nil {
		return "quota exceeded"
	}
	return e.Code
}

// Detail 生成给客户端看的中文说明。判定口径是"先欠费后停服"——用到限额之后
// 才拒，所以文案要说清"已欠费"，否则客户端会以为是临时故障而反复重试。
func (e *QuotaError) Detail() string {
	if e == nil {
		return ""
	}
	window := map[string]string{
		"5h":     "5 小时窗口",
		"daily":  "当日额度（北京时间）",
		"weekly": "本周额度（北京时间，自周一起）",
		"key":    "该 Key 的总额度",
	}[e.Scope]
	if window == "" {
		window = e.Scope
	}
	metric := "Token"
	if e.Metric == "amount" {
		metric = "金额"
	}
	format := func(v int64) string {
		if e.Metric == "amount" {
			return "¥" + strconv.FormatFloat(float64(v)/1e6, 'f', 2, 64)
		}
		return strconv.FormatInt(v, 10) + " tokens"
	}
	return fmt.Sprintf("%s%s已用满（已用 %s，限额 %s）。当前处于欠费状态，请等待窗口轮换或在管理台调大限额", window, metric, format(e.Used), format(e.Limit))
}

type RequestReservation struct {
	ID, KeyReservationID                    int64
	RequestID                               string
	StartedAt                               time.Time
	ReservedTokens, ReservedAmountMicroyuan int64
}

// ReserveInput 描述一次预扣所需的输入规模信息。字符串字段只参与预估，
// 不会落库。
type ReserveInput struct {
	RequestID                                 string
	KeyID, ModelID, ProviderID                int64
	StartedAt                                 time.Time
	Stream                                    bool
	InputText                                 string
	StringFields                              []string
	InputTokens                               int64
	OutputTokenCeiling                        int64
	MaxOutputTokens                           int64
	InputPriceMicroyuan, OutputPriceMicroyuan int64
	Limits                                    QuotaLimits
}

type TrendBucket struct {
	StartedAtUTC string `json:"started_at_utc"`
	Count        int    `json:"count"`
}

// estimateInputTokens 在请求进入上游前做一次保守的输入预估，供配额预留与
// 预扣费使用；实际用量在结算时回填（SettleRequest 会覆盖 reserved_tokens）。
//
// 旧实现把整个 messages 的 JSON 与每条消息的 JSON 都按"字符数 = 1 token"
// 计，一条消息被算了约两遍，对英文比真实分词器保守 4 倍以上，导致小额限额
// 形同虚设。现在只统计一份文本，并按字符类别近似真实分词：
//
//   - 中日韩等全角字符：1.25 token/字（现代分词器对中文约 1~1.5）；
//   - 其余字符（拉丁字母、数字、符号、空白）：1 token / 3.5 字符（英文与
//     代码实测约 3.5~4 字符/token）。
//
// 估算仍略高于真实值，保留"宁可多预留、不超额"的取向。
func estimateInputTokens(in ReserveInput) int64 {
	if in.InputTokens > 0 {
		return in.InputTokens
	}
	// 每条消息的 JSON 已包含整个数组 JSON 的全部内容，二者只取其一，
	// 避免 messages 被重复计入。
	texts := in.StringFields
	if len(texts) == 0 && in.InputText != "" {
		texts = []string{in.InputText}
	}
	var milli int64
	for _, value := range texts {
		for _, r := range value {
			milli += tokenMilli(r)
		}
	}
	return (milli+999)/1000 + perRequestOverheadTokens
}

const (
	milliPerWideToken  = 1250 // 全角字符 1.25 token/字
	milliPerNarrowChar = 286  // 3.5 字符 1 token（约 0.286 token/字符）

	// perRequestOverheadTokens 是上游分词器给每条请求加的固定角色/分隔开销
	// （BOS、role 标记、消息分隔符等实测约 3~5 token）。旧实现完全没算它，
	// 短请求会被低估到 1 token，导致预留不足、结算时才发现超额。
	perRequestOverheadTokens = 4
)

// tokenMilli 返回单个字符的预估 token 数，单位是千分之一 token，
// 便于用整数累加后再取整。
func tokenMilli(r rune) int64 {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK 部首/标点/假名/注音/统一表意
		return milliPerWideToken
	case r >= 0xA000 && r <= 0xD7FF: // 彝文、谚文音节等
		return milliPerWideToken
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容表意
		return milliPerWideToken
	case r >= 0xFE30 && r <= 0xFE4F: // CJK 兼容形式
		return milliPerWideToken
	case r >= 0xFF00 && r <= 0xFFEF: // 全角/半角形式
		return milliPerWideToken
	case r >= 0x1F000: // 符号、表情与 CJK 扩展 B 起
		return milliPerWideToken
	}
	return milliPerNarrowChar
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

// 计费窗口一律按北京时间切分（日/周），与账目口径一致。
var beijingZone = time.FixedZone("Asia/Shanghai", 8*60*60)

func billingDate(t time.Time) string {
	return t.In(beijingZone).Format("2006-01-02")
}

// billingWeekStart 返回 t 所在北京时间自然周的起点（周一 00:00 CST），
// 以 UTC 返回，供 [start, now] 区间查询使用。用区间而非新增落库列，
// 是为了避免迁移：key_reservations.created_at_utc 已有索引。
func billingWeekStart(t time.Time) time.Time {
	local := t.In(beijingZone)
	offset := (int(local.Weekday()) + 6) % 7 // 周一 = 0
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, beijingZone).AddDate(0, 0, -offset)
	return start.UTC()
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
	if in.Limits.RPMLimit <= 0 {
		return RequestReservation{}, errors.New("rpm_limit must be positive")
	}
	started := in.StartedAt.Format(time.RFC3339Nano)
	date := billingDate(in.StartedAt)
	weekStart := billingWeekStart(in.StartedAt).Format(time.RFC3339Nano)
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
	if err := conn.QueryRowContext(ctx, `SELECT m.input_price_microyuan,m.output_price_microyuan FROM key_groups kg JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.id=? AND m.provider_id=gp.provider_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE kg.key_id=? AND p.id=? LIMIT 1`, in.ModelID, in.KeyID, in.ProviderID).Scan(&inputPrice, &outputPrice); err != nil {
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
	// 用 julianday 做数值比较：datetime() 输出空格分隔格式，与 RFC3339 存储串
	// 直接字符串比较时 'T' > ' '，会把窗口起点同一天的历史记录全部误判为窗口内。
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE julianday(created_at_utc)>=julianday(?,'-60 seconds') AND julianday(created_at_utc)<=julianday(?)`, started, started).Scan(&rpm); err != nil {
		return rollback(err)
	}
	if rpm >= int(in.Limits.RPMLimit) {
		return rollback(&QuotaError{Code: "rpm_exceeded", Scope: "rpm", Metric: "requests", Used: int64(rpm), Limit: in.Limits.RPMLimit})
	}
	// ── 额度判定：先欠费后停服 ──
	//
	// 只要求"窗口还没用满"，不再要求"窗口已用 + 本次预留 <= 限额"。旧口径会把
	// "还剩一些、但不够本次输入估算 + 输出上限"的请求整体拒掉：运维看到概览里
	// 明明还剩额度，客户端却报额度不足，还得靠读代码才能解释。现在改成先放过去、
	// 用完即欠费，之后的请求才被挡下——欠费状态随窗口轮换或调大限额自动解除。
	//
	// 预留额度仍然照算（用于金额预扣与并发账目），只是不再参与"能不能发"的判断，
	// 所以单次请求可能略微超出限额，多出的部分由后续请求被拒来兜底。
	overdrawn := func(scope, code string, used, limit int64) error {
		if limit > 0 && used >= limit {
			return &QuotaError{Code: code, Scope: scope, Metric: "tokens", Used: used, Limit: limit}
		}
		return nil
	}
	overdrawnAmount := func(scope, code string, used, limit int64) error {
		if limit > 0 && used >= limit {
			return &QuotaError{Code: code, Scope: scope, Metric: "amount", Used: used, Limit: limit}
		}
		return nil
	}
	var usedTokens, usedAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND julianday(created_at_utc)>=julianday(?,'-5 hours') AND julianday(created_at_utc)<=julianday(?)`, started, started).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if err := overdrawn("5h", "token_quota_exceeded", usedTokens, in.Limits.Token5H); err != nil {
		return rollback(err)
	}
	if err := overdrawnAmount("5h", "amount_quota_exceeded", usedAmount, in.Limits.Amount5H); err != nil {
		return rollback(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND billing_date_bj=? AND created_at_utc<=?`, date, started).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if err := overdrawn("daily", "token_quota_exceeded", usedTokens, in.Limits.TokenDaily); err != nil {
		return rollback(err)
	}
	if err := overdrawnAmount("daily", "amount_quota_exceeded", usedAmount, in.Limits.AmountDaily); err != nil {
		return rollback(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND created_at_utc>=? AND created_at_utc<=?`, weekStart, started).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if err := overdrawn("weekly", "token_quota_exceeded", usedTokens, in.Limits.TokenWeekly); err != nil {
		return rollback(err)
	}
	if err := overdrawnAmount("weekly", "amount_quota_exceeded", usedAmount, in.Limits.AmountWeekly); err != nil {
		return rollback(err)
	}
	// Key 总额度不随窗口轮换，用满即欠费。这里只拒绝请求，不再自动停用 Key：
	// 永久停用会让"明明调大了限额却仍然用不了"，而且必须人工恢复，比欠费即拒更难排查。
	var keyTokens, keyAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_tokens+reserved_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE key_id=? AND status IN ('reserved','completed','failed','aborted')`, in.KeyID).Scan(&keyTokens, &keyAmount); err != nil {
		return rollback(err)
	}
	if tokenLimit.Valid {
		if err := overdrawn("key", "key_token_quota_exceeded", keyTokens, tokenLimit.Int64); err != nil {
			return rollback(err)
		}
	}
	if amountLimit.Valid {
		if err := overdrawnAmount("key", "key_amount_quota_exceeded", keyAmount, amountLimit.Int64); err != nil {
			return rollback(err)
		}
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
	return s.finishRequest(ctx, reservationID, 0, 0, 0, 0, status, httpStatus)
}

func (s *Store) SettleRequest(ctx context.Context, reservationID, inputTokens, outputTokens, cachedInputTokens, amount int64, status string) error {
	if reservationID <= 0 || inputTokens < 0 || outputTokens < 0 || cachedInputTokens < 0 || amount < 0 {
		return errors.New("invalid settlement")
	}
	if status == "" {
		status = "completed"
	}
	return s.finishRequest(ctx, reservationID, inputTokens, outputTokens, cachedInputTokens, amount, status, 200)
}

func (s *Store) finishRequest(ctx context.Context, requestID, inputTokens, outputTokens, cachedInputTokens, amount int64, status string, httpStatus int) error {
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
	var keyReservationID int64
	var requestToken string
	var current string
	if err := conn.QueryRowContext(ctx, `SELECT request_id FROM requests WHERE id=? AND status='reserved'`, requestID).Scan(&requestToken); err != nil {
		return fail(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT id,status FROM key_reservations WHERE request_id=? AND status='reserved' ORDER BY id DESC LIMIT 1`, requestToken).Scan(&keyReservationID, &current); err != nil {
		return fail(err)
	}
	chargedTokens, chargedAmount := inputTokens+outputTokens, amount
	if _, err := conn.ExecContext(ctx, `UPDATE key_reservations SET charged_tokens=?,charged_amount_microyuan=?,reserved_tokens=0,reserved_amount_microyuan=0,status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, chargedAmount, status, nowUTC(), keyReservationID); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE requests SET reserved_tokens=0,charged_tokens=?,input_tokens=?,output_tokens=?,cached_input_tokens=?,amount_microyuan=?,status=?,upstream_status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, inputTokens, outputTokens, cachedInputTokens, chargedAmount, status, httpStatus, nowUTC(), requestID); err != nil {
		return fail(err)
	}
	// 这里刻意不再把 Key 自动停用（旧行为：总配额用满即 enabled=0 +
	// disabled_reason='quota_exhausted'）。停用是永久性的，运维调大限额后 Key
	// 仍然用不了，只会让人以为"明明还有余量却提示无法使用"。额度管控改由
	// ReserveRequest 在每次请求时判定：欠费即拒，限额调大或窗口轮换后自动恢复。
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	return nil
}

func (s *Store) OccupiedTokens(ctx context.Context, startedAt time.Time) int64 {
	var total int64
	started := startedAt.UTC().Format(time.RFC3339Nano)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND julianday(created_at_utc)>=julianday(?,'-5 hours') AND julianday(created_at_utc)<=julianday(?)`, started, started).Scan(&total)
	return total
}

func (s *Store) RecentRequests(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,model_id,provider_id,status,input_tokens,output_tokens,cached_input_tokens,amount_microyuan,upstream_status,streamed,created_at_utc,finished_at_utc FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]map[string]any, 0)
	for rows.Next() {
		var requestID, status, created string
		var modelID, providerID, input, output, cached, amount, upstream sql.NullInt64
		var streamed int
		var finished sql.NullString
		if err := rows.Scan(&requestID, &modelID, &providerID, &status, &input, &output, &cached, &amount, &upstream, &streamed, &created, &finished); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"request_id": requestID, "model_id": modelID.Int64, "provider_id": providerID.Int64, "status": status, "input_tokens": input.Int64, "output_tokens": output.Int64, "cached_input_tokens": cached.Int64, "amount_microyuan": amount.Int64, "upstream_status": upstream.Int64, "streamed": streamed == 1, "created_at_utc": created, "finished_at_utc": finished.String})
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

// GroupOverview mirrors Overview but scopes every counter to requests that
// went through providers currently attached to the given group. Token totals
// read key_reservations (the authoritative ledger), joined back to requests
// for the provider filter. No limits are applied here: quotas are global.
func (s *Store) GroupOverview(ctx context.Context, groupID int64, now time.Time) (map[string]any, error) {
	if groupID <= 0 {
		return nil, errors.New("group ID is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	minuteStart := now.Add(-time.Minute).Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	weekStart := billingWeekStart(now).Format(time.RFC3339Nano)
	hourStart := now.Add(-time.Hour).Format(time.RFC3339Nano)
	date := billingDate(now)
	scope := `provider_id IN (SELECT provider_id FROM group_providers WHERE group_id=?)`
	var rpm int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE `+scope+` AND created_at_utc>=? AND created_at_utc<=?`, groupID, minuteStart, nowText).Scan(&rpm); err != nil {
		return nil, err
	}
	var fiveHour, fiveHourAmount, daily, dailyAmount, weekly, weeklyAmount int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(kr.reserved_tokens+kr.charged_tokens),0),COALESCE(SUM(kr.charged_amount_microyuan+kr.reserved_amount_microyuan),0) FROM key_reservations kr JOIN requests r ON r.request_id=kr.request_id WHERE kr.request_id IS NOT NULL AND kr.status IN ('reserved','completed','failed','aborted') AND `+scope+` AND kr.created_at_utc>=? AND kr.created_at_utc<=?`, groupID, fiveHourStart, nowText).Scan(&fiveHour, &fiveHourAmount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(kr.reserved_tokens+kr.charged_tokens),0),COALESCE(SUM(kr.charged_amount_microyuan+kr.reserved_amount_microyuan),0) FROM key_reservations kr JOIN requests r ON r.request_id=kr.request_id WHERE kr.request_id IS NOT NULL AND kr.status IN ('reserved','completed','failed','aborted') AND `+scope+` AND kr.billing_date_bj=? AND kr.created_at_utc<=?`, groupID, date, nowText).Scan(&daily, &dailyAmount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(kr.reserved_tokens+kr.charged_tokens),0),COALESCE(SUM(kr.charged_amount_microyuan+kr.reserved_amount_microyuan),0) FROM key_reservations kr JOIN requests r ON r.request_id=kr.request_id WHERE kr.request_id IS NOT NULL AND kr.status IN ('reserved','completed','failed','aborted') AND `+scope+` AND kr.created_at_utc>=? AND kr.created_at_utc<=?`, groupID, weekStart, nowText).Scan(&weekly, &weeklyAmount); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM requests WHERE `+scope+` AND created_at_utc>=? AND created_at_utc<=? GROUP BY status`, groupID, hourStart, nowText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	outcomes := map[string]int{"completed": 0, "failed": 0, "aborted": 0, "rejected": 0, "reserved": 0}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		if _, ok := outcomes[status]; ok {
			outcomes[status] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE `+scope+` AND status='reserved'`, groupID).Scan(&active); err != nil {
		return nil, err
	}
	denominator := outcomes["completed"] + outcomes["failed"]
	errorRate := 0.0
	if denominator > 0 {
		errorRate = float64(outcomes["failed"]) / float64(denominator)
	}
	return map[string]any{
		"group_id":         groupID,
		"generated_at_utc": nowText,
		"rpm":              map[string]any{"used": rpm},
		"five_hour":        map[string]any{"used_tokens": fiveHour, "used_microyuan": fiveHourAmount},
		"daily":            map[string]any{"used_tokens": daily, "used_microyuan": dailyAmount},
		"weekly":           map[string]any{"used_tokens": weekly, "used_microyuan": weeklyAmount},
		"last_hour":        map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"active":           active,
	}, nil
}

// windowUsage 汇总 key_reservations 在给定条件下的（token, 微元）用量。
// 「未结算的预留也算已用」与限额判定口径保持一致，否则运维看到的数字会比
// 实际生效的宽松。
func (s *Store) windowUsage(ctx context.Context, extra string, args ...any) (int64, int64, error) {
	query := `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted')`
	if extra != "" {
		query += " AND " + extra
	}
	var tokens, amount int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&tokens, &amount); err != nil {
		return 0, 0, err
	}
	return tokens, amount, nil
}

// Overview returns metadata-only counters used by the administrator console.
// It deliberately accepts limits as arguments so configuration remains outside
// the store and no credentials or prompts can enter the response.
// 限额为 0 表示该窗口不限，原样回传，由前端显示为「不限」。
func (s *Store) Overview(ctx context.Context, now time.Time, limits QuotaLimits) (map[string]any, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	minuteStart := now.Add(-time.Minute).Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	weekStart := billingWeekStart(now).Format(time.RFC3339Nano)
	hourStart := now.Add(-time.Hour)
	date := billingDate(now)
	var rpm int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE created_at_utc>=? AND created_at_utc<=?`, minuteStart, nowText).Scan(&rpm); err != nil {
		return nil, err
	}
	fiveTokens, fiveAmount, err := s.windowUsage(ctx, `created_at_utc>=? AND created_at_utc<=?`, fiveHourStart, nowText)
	if err != nil {
		return nil, err
	}
	dayTokens, dayAmount, err := s.windowUsage(ctx, `billing_date_bj=? AND created_at_utc<=?`, date, nowText)
	if err != nil {
		return nil, err
	}
	weekTokens, weekAmount, err := s.windowUsage(ctx, `created_at_utc>=? AND created_at_utc<=?`, weekStart, nowText)
	if err != nil {
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
		"rpm":              map[string]any{"used": rpm, "limit": limits.RPMLimit},
		"five_hour":        map[string]any{"used_tokens": fiveTokens, "limit": limits.Token5H, "used_microyuan": fiveAmount, "amount_limit": limits.Amount5H},
		"daily":            map[string]any{"used_tokens": dayTokens, "limit": limits.TokenDaily, "used_microyuan": dayAmount, "amount_limit": limits.AmountDaily},
		"weekly":           map[string]any{"used_tokens": weekTokens, "limit": limits.TokenWeekly, "used_microyuan": weekAmount, "amount_limit": limits.AmountWeekly},
		"last_hour":        map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"trend":            trend,
		"recent":           recent,
	}, nil
}
