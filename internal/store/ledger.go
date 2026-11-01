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
//
// 数据来源（v18 起）：只有 RPMLimit 仍来自全局配置（ReserveInput 传入），
// 其余九个额度值与周期参数都由 ReserveRequest 在事务内从密钥所属的
// model_groups 行读出后填入——配置是进程级快照，热更新时会出现「判定用旧
// 限额、写入在新事务」的错位，而且每组额度不同，本来就传不进一个全局结构体。
// 载体形状没改是为了让窗口判定函数一行不用动。
type QuotaLimits struct {
	RPMLimit                            int64
	Token5H, TokenDaily, TokenWeekly    int64
	Amount5H, AmountDaily, AmountWeekly int64 // 微元
	TokenPeriod, AmountPeriod           int64
	PeriodStart                         time.Time
	PeriodDays                          int
}

// QuotaError 携带判定明细，供 HTTP 层返回可读原因与正确的状态码。
type QuotaError struct {
	Code   string // token_quota_exceeded / amount_quota_exceeded / key_* / rpm_exceeded / concurrency_exceeded
	Scope  string // 5h / daily / weekly / period / key / ""
	Metric string // tokens / amount / ""
	Used   int64  // tokens 或微元，取决于 Metric
	Limit  int64
	// 额度归属从全局变成分组后，运维必须能看出是哪个组的额度咬住了请求。
	Group   string
	GroupID int64
}

func (e *QuotaError) Error() string {
	if e == nil {
		return "quota exceeded"
	}
	return e.Code
}

// Detail 生成给客户端看的中文说明。判定口径是"先欠费后停服"——用到限额之后
// 才拒，所以文案要说清"已欠费"，否则客户端会以为是临时故障而反复重试。
// 额度归属变成分组之后，运维必须能看出是哪个组的额度咬住的（spec 第七节）。
func (e *QuotaError) Detail() string {
	if e == nil {
		return ""
	}
	window := map[string]string{
		"5h":     "5 小时窗口",
		"daily":  "当日额度（北京时间）",
		"weekly": "本周额度（北京时间，自周一起）",
		"period": "自定义周期额度",
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
	detail := fmt.Sprintf("%s%s已用满（已用 %s，限额 %s）", window, metric, format(e.Used), format(e.Limit))
	if e.Group != "" {
		detail += fmt.Sprintf("，分组「%s」#%d", e.Group, e.GroupID)
	}
	return detail + "。当前处于欠费状态，请等待窗口轮换或在管理台调大限额"
}

type RequestReservation struct {
	ID, KeyReservationID, ModelID           int64
	RequestID                               string
	StartedAt                               time.Time
	InputTokens                             int64
	ReservedTokens, ReservedAmountMicroyuan int64
	QuoteInputPriceMicroyuan                int64
	QuoteOutputPriceMicroyuan               int64
	QuoteCachedInputPriceMicroyuan          int64
	QuoteRateMilli                          int64
}

// ReserveInput 描述一次预扣所需的输入规模信息。字符串字段只参与预估，
// 不会落库。
//
// ProviderID 只作「请求派发渠道」的记录落库，不参与授权判定——授权由
// Key 所属分组在事务内重查模型归属决定，两者在管理员改配置的极窄窗口内
// 可能不同，以事务内结果为准。
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

func priceWithRate(price, rate int64) (int64, error) {
	if price < 0 || rate <= 0 {
		return 0, errors.New("invalid price quote")
	}
	if price > math.MaxInt64/rate {
		return 0, errors.New("price quote overflow")
	}
	return price * rate / 1000, nil
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

func periodActive(now, start time.Time, days int, tokenLimit, amountLimit int64) bool {
	return days > 0 && (tokenLimit > 0 || amountLimit > 0) && !start.IsZero() && !now.Before(start) && now.Before(start.AddDate(0, 0, days))
}

func weeklyWindowStart(now time.Time, limits QuotaLimits) (time.Time, bool) {
	weekStart := billingWeekStart(now)
	if periodActive(now, limits.PeriodStart, limits.PeriodDays, limits.TokenPeriod, limits.AmountPeriod) {
		return now, true
	}
	if !limits.PeriodStart.IsZero() && limits.PeriodDays > 0 && (limits.TokenPeriod > 0 || limits.AmountPeriod > 0) {
		periodEnd := limits.PeriodStart.AddDate(0, 0, limits.PeriodDays)
		if periodEnd.After(weekStart) && !periodEnd.After(now) {
			return periodEnd, false
		}
	}
	return weekStart, false
}

// 边界一律用 julianday 数值比较，不做字符串比较：period_limit_start 可能来自
// 配置/前端（秒级 RFC3339，无小数位），账目行的 created_at_utc 是 RFC3339Nano，
// 文本序里 "…48Z" 会大于 "…48.219Z"——同秒内的行会被整段排除在窗口外。
// 周期起点改成「分组创建时自动填 now」之后这条路径必然踩到，所以必须换比较方式。
// groupID 收窄统计口径：额度按组归属（账目行落库时的 group_id），切换分组
// 不搬历史，也不会让「别的组用掉的量」咬住本组。
func periodQuotaTransition(ctx context.Context, conn *sql.Conn, limits QuotaLimits, groupID int64, started string) (time.Time, int64, int64, error) {
	if limits.PeriodDays <= 0 || limits.PeriodStart.IsZero() {
		return time.Time{}, 0, 0, nil
	}
	periodStart := limits.PeriodStart.Format(time.RFC3339Nano)
	periodEnd := limits.PeriodStart.AddDate(0, 0, limits.PeriodDays).Format(time.RFC3339Nano)
	var usedTokens, usedAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND group_id=? AND julianday(created_at_utc)>=julianday(?) AND julianday(created_at_utc)<julianday(?) AND julianday(created_at_utc)<=julianday(?)`, groupID, periodStart, periodEnd, started).Scan(&usedTokens, &usedAmount); err != nil {
		return time.Time{}, 0, 0, err
	}
	if (limits.TokenPeriod <= 0 || usedTokens < limits.TokenPeriod) && (limits.AmountPeriod <= 0 || usedAmount < limits.AmountPeriod) {
		return time.Time{}, usedTokens, usedAmount, nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT created_at_utc, SUM(reserved_tokens+charged_tokens) OVER (ORDER BY created_at_utc, id ROWS UNBOUNDED PRECEDING), SUM(charged_amount_microyuan+reserved_amount_microyuan) OVER (ORDER BY created_at_utc, id ROWS UNBOUNDED PRECEDING) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND group_id=? AND julianday(created_at_utc)>=julianday(?) AND julianday(created_at_utc)<julianday(?) AND julianday(created_at_utc)<=julianday(?) ORDER BY created_at_utc, id`, groupID, periodStart, periodEnd, started)
	if err != nil {
		return time.Time{}, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var tokens, amount int64
		if err := rows.Scan(&raw, &tokens, &amount); err != nil {
			return time.Time{}, 0, 0, err
		}
		if (limits.TokenPeriod > 0 && tokens >= limits.TokenPeriod) || (limits.AmountPeriod > 0 && amount >= limits.AmountPeriod) {
			t, parseErr := time.Parse(time.RFC3339Nano, raw)
			if parseErr != nil {
				return time.Time{}, 0, 0, parseErr
			}
			return t, usedTokens, usedAmount, rows.Err()
		}
	}
	return time.Time{}, usedTokens, usedAmount, rows.Err()
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
	// 额度来自密钥所属分组：必须在这个事务里读，不能从配置传——配置是进程级
	// 快照，热更新时判定与写入会错位。RPMLimit 仍是全局的（in.Limits）。
	var limits QuotaLimits
	limits.RPMLimit = in.Limits.RPMLimit
	var groupID int64
	var groupName string
	var periodDays int64
	var periodStartRaw sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT k.enabled,k.concurrency_limit,k.token_limit,k.amount_limit_microyuan,COALESCE(k.group_id,0),COALESCE(g.name,''),COALESCE(g.token_limit_5h,0),COALESCE(g.amount_limit_5h,0),COALESCE(g.token_limit_daily,0),COALESCE(g.amount_limit_daily,0),COALESCE(g.token_limit_weekly,0),COALESCE(g.amount_limit_weekly,0),COALESCE(g.period_limit_days,0),COALESCE(g.token_limit_period,0),COALESCE(g.amount_limit_period,0),COALESCE(g.period_limit_start,'') FROM client_keys k LEFT JOIN model_groups g ON g.id=k.group_id WHERE k.id=?`, in.KeyID).
		Scan(&enabled, &concurrency, &tokenLimit, &amountLimit, &groupID, &groupName,
			&limits.Token5H, &limits.Amount5H, &limits.TokenDaily, &limits.AmountDaily,
			&limits.TokenWeekly, &limits.AmountWeekly, &periodDays, &limits.TokenPeriod,
			&limits.AmountPeriod, &periodStartRaw); err != nil {
		return rollback(err)
	}
	limits.PeriodDays = int(periodDays)
	if periodStartRaw.Valid && periodStartRaw.String != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, periodStartRaw.String); parseErr == nil {
			limits.PeriodStart = parsed
		}
	}
	// 本周窗口的起点取决于分组自己的周期状态（周期在用则窗口退化为「本周期」），
	// 所以必须等分组额度读出来之后才算。
	weekStartTime, activePeriod := weeklyWindowStart(in.StartedAt, limits)
	weekStart := weekStartTime.Format(time.RFC3339Nano)
	weekStartOperator := ">="
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
	// 未绑分组的密钥没有可授权的模型：与「模型不在组内」同一个错误出口。
	// 顺序不能动——停用/并发的错误优先级保持与旧版一致。
	if groupID == 0 {
		return rollback(errors.New("model is not authorized for key"))
	}
	var inputPrice, outputPrice, cachedPrice, rate sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT m.input_price_microyuan,m.output_price_microyuan,COALESCE(m.cached_input_price_microyuan,m.input_price_microyuan),g.rate_milli FROM model_groups g JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.id=? AND m.provider_id=gp.provider_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE g.id=? AND g.enabled=1`, in.ModelID, groupID).Scan(&inputPrice, &outputPrice, &cachedPrice, &rate); err != nil {
		return rollback(err)
	}
	if !inputPrice.Valid || !outputPrice.Valid || !cachedPrice.Valid || !rate.Valid {
		return rollback(errors.New("model is not authorized for key"))
	}
	effectiveInput, err := priceWithRate(inputPrice.Int64, rate.Int64)
	if err != nil {
		return rollback(err)
	}
	effectiveOutput, err := priceWithRate(outputPrice.Int64, rate.Int64)
	if err != nil {
		return rollback(err)
	}
	if _, err := priceWithRate(cachedPrice.Int64, rate.Int64); err != nil {
		return rollback(err)
	}
	amount, err := reserveAmount(inputTokens, outputTokens, effectiveInput, effectiveOutput)
	if err != nil {
		return rollback(err)
	}
	var rpm int
	// 用 julianday 做数值比较：datetime() 输出空格分隔格式，与 RFC3339 存储串
	// 直接字符串比较时 'T' > ' '，会把窗口起点同一天的历史记录全部误判为窗口内。
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE julianday(created_at_utc)>=julianday(?,'-60 seconds') AND julianday(created_at_utc)<=julianday(?)`, started, started).Scan(&rpm); err != nil {
		return rollback(err)
	}
	if rpm >= int(limits.RPMLimit) {
		return rollback(&QuotaError{Code: "rpm_exceeded", Scope: "rpm", Metric: "requests", Used: int64(rpm), Limit: limits.RPMLimit})
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
			return &QuotaError{Code: code, Scope: scope, Metric: "tokens", Used: used, Limit: limit, Group: groupName, GroupID: groupID}
		}
		return nil
	}
	overdrawnAmount := func(scope, code string, used, limit int64) error {
		if limit > 0 && used >= limit {
			return &QuotaError{Code: code, Scope: scope, Metric: "amount", Used: used, Limit: limit, Group: groupName, GroupID: groupID}
		}
		return nil
	}
	var usedTokens, usedAmount int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND julianday(created_at_utc)>=julianday(?,'-5 hours') AND julianday(created_at_utc)<=? AND group_id=?`, started, started, groupID).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if err := overdrawn("5h", "token_quota_exceeded", usedTokens, limits.Token5H); err != nil {
		return rollback(err)
	}
	if err := overdrawnAmount("5h", "amount_quota_exceeded", usedAmount, limits.Amount5H); err != nil {
		return rollback(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND billing_date_bj=? AND created_at_utc<=? AND group_id=?`, date, started, groupID).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if err := overdrawn("daily", "token_quota_exceeded", usedTokens, limits.TokenDaily); err != nil {
		return rollback(err)
	}
	if err := overdrawnAmount("daily", "amount_quota_exceeded", usedAmount, limits.AmountDaily); err != nil {
		return rollback(err)
	}
	if activePeriod {
		transition, periodTokens, periodAmount, err := periodQuotaTransition(ctx, conn, limits, groupID, started)
		if err != nil {
			return rollback(err)
		}
		if !transition.IsZero() {
			activePeriod = false
			weekStart = transition.Format(time.RFC3339Nano)
			weekStartOperator = ">"
		} else if err := overdrawn("period", "token_quota_exceeded", periodTokens, limits.TokenPeriod); err != nil {
			return rollback(err)
		} else if err := overdrawnAmount("period", "amount_quota_exceeded", periodAmount, limits.AmountPeriod); err != nil {
			return rollback(err)
		}
	}
	weeklyQuery := `SELECT COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND created_at_utc` + weekStartOperator + `? AND created_at_utc<=? AND group_id=?`
	if err := conn.QueryRowContext(ctx, weeklyQuery, weekStart, started, groupID).Scan(&usedTokens, &usedAmount); err != nil {
		return rollback(err)
	}
	if !activePeriod {
		if err := overdrawn("weekly", "token_quota_exceeded", usedTokens, limits.TokenWeekly); err != nil {
			return rollback(err)
		}
		if err := overdrawnAmount("weekly", "amount_quota_exceeded", usedAmount, limits.AmountWeekly); err != nil {
			return rollback(err)
		}
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
	result, err := conn.ExecContext(ctx, `INSERT INTO requests(request_id,key_id,model_id,provider_id,group_id,status,input_tokens,reserved_tokens,amount_microyuan,streamed,billing_date_bj,created_at_utc,quote_input_microyuan,quote_output_microyuan,quote_cached_microyuan,quote_rate_milli) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.RequestID, in.KeyID, in.ModelID, in.ProviderID, groupID, "reserved", inputTokens, reservedTokens, amount, boolInt(in.Stream), date, started, inputPrice.Int64, outputPrice.Int64, cachedPrice.Int64, rate.Int64)
	if err != nil {
		return rollback(err)
	}
	requestRowID, err := result.LastInsertId()
	if err != nil {
		return rollback(err)
	}
	result, err = conn.ExecContext(ctx, `INSERT INTO key_reservations(request_id,billing_date_bj,key_id,model_id,group_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (?,?,?,?,?,?,?,?,?)`, in.RequestID, date, in.KeyID, in.ModelID, groupID, reservedTokens, amount, "reserved", started)
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
	return RequestReservation{
		ID: requestRowID, KeyReservationID: reservationID, ModelID: in.ModelID, RequestID: in.RequestID, StartedAt: in.StartedAt,
		InputTokens:    inputTokens,
		ReservedTokens: reservedTokens, ReservedAmountMicroyuan: amount,
		QuoteInputPriceMicroyuan: inputPrice.Int64, QuoteOutputPriceMicroyuan: outputPrice.Int64,
		QuoteCachedInputPriceMicroyuan: cachedPrice.Int64, QuoteRateMilli: rate.Int64,
	}, nil
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
	return s.FinishReservation(ctx, reservationID, Settlement{Status: status, UpstreamStatus: httpStatus, UsageSource: "unknown"})
}

func (s *Store) SettleRequest(ctx context.Context, reservationID, inputTokens, outputTokens, cachedInputTokens, amount int64, status string) error {
	if status == "" {
		status = "completed"
	}
	return s.FinishReservation(ctx, reservationID, Settlement{
		Status: status, InputTokens: inputTokens, OutputTokens: outputTokens,
		CachedInputTokens: cachedInputTokens, AmountMicroyuan: amount, UpstreamStatus: 200,
	})
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
	page, err := s.QueryRequests(ctx, RequestFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Data, nil
}

func nullableNumber(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
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
	result, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE created_at_utc < ? AND status <> 'reserved'`, cutoffText)
	if err != nil {
		return 0, err
	}
	// Reservations may include legacy rows without a matching request.
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_reservations WHERE created_at_utc < ? AND status <> 'reserved'`, cutoffText); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// GroupOverview 返回单个分组的用量与额度。统计口径是「归属该组的请求」
// （账目行的 group_id，由阶段 A 落下）——必须与额度扣减同口径，否则管理台
// 显示的「已用/限额」和实际判定用的数字对不上；共享渠道不再被重复计入多个组。
// 限额来自分组行（v18），与 ReserveRequest 的判定来源一致。
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
	hourStart := now.Add(-time.Hour).Format(time.RFC3339Nano)
	date := billingDate(now)
	var limits QuotaLimits
	var groupName string
	var periodDays int64
	var periodStartRaw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(name,''),COALESCE(token_limit_5h,0),COALESCE(amount_limit_5h,0),COALESCE(token_limit_daily,0),COALESCE(amount_limit_daily,0),COALESCE(token_limit_weekly,0),COALESCE(amount_limit_weekly,0),COALESCE(period_limit_days,0),COALESCE(token_limit_period,0),COALESCE(amount_limit_period,0),COALESCE(period_limit_start,'') FROM model_groups WHERE id=?`, groupID).
		Scan(&groupName, &limits.Token5H, &limits.Amount5H, &limits.TokenDaily, &limits.AmountDaily,
			&limits.TokenWeekly, &limits.AmountWeekly, &periodDays, &limits.TokenPeriod,
			&limits.AmountPeriod, &periodStartRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// 组不存在时按「空概览」处理（沿用旧行为）：管理台在组被删掉的瞬间
		// 仍可能在轮询这个端点，返回 500 只会制造误报。
		return nil, err
	}
	limits.PeriodDays = int(periodDays)
	if periodStartRaw.Valid && periodStartRaw.String != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, periodStartRaw.String); parseErr == nil {
			limits.PeriodStart = parsed
		}
	}
	var rpm int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE group_id=? AND julianday(created_at_utc)>=julianday(?) AND julianday(created_at_utc)<=julianday(?)`, groupID, minuteStart, nowText).Scan(&rpm); err != nil {
		return nil, err
	}
	fiveHour, fiveHourAmount, err := s.windowUsage(ctx, `group_id=? AND created_at_utc>=? AND created_at_utc<=?`, groupID, fiveHourStart, nowText)
	if err != nil {
		return nil, err
	}
	daily, dailyAmount, err := s.windowUsage(ctx, `group_id=? AND billing_date_bj=? AND created_at_utc<=?`, groupID, date, nowText)
	if err != nil {
		return nil, err
	}
	// 周期窗口的边界同样交给 periodQuotaTransition 判定：它与扣减同口径，
	// 周期用满后本周窗口自动退化为「从交接点算起」。
	weekStart, periodActiveNow := weeklyWindowStart(now, limits)
	var periodTransition time.Time
	if periodActiveNow {
		conn, connErr := s.db.Conn(ctx)
		if connErr != nil {
			return nil, connErr
		}
		periodTransition, _, _, connErr = periodQuotaTransition(ctx, conn, limits, groupID, nowText)
		conn.Close()
		if connErr != nil {
			return nil, connErr
		}
		if !periodTransition.IsZero() {
			periodActiveNow = false
			weekStart = periodTransition
		}
	}
	weekFilter := `group_id=? AND created_at_utc>=? AND created_at_utc<=?`
	if !periodTransition.IsZero() {
		weekFilter = `group_id=? AND created_at_utc>? AND created_at_utc<=?`
	}
	weekly, weeklyAmount, err := s.windowUsage(ctx, weekFilter, groupID, weekStart.Format(time.RFC3339Nano), nowText)
	if err != nil {
		return nil, err
	}
	periodStartText, periodEndText := "", ""
	var periodTokens, periodAmount int64
	if limits.PeriodDays > 0 && !limits.PeriodStart.IsZero() {
		periodStartText = limits.PeriodStart.Format(time.RFC3339Nano)
		periodEndText = limits.PeriodStart.AddDate(0, 0, limits.PeriodDays).Format(time.RFC3339Nano)
		periodTokens, periodAmount, err = s.windowUsage(ctx, `group_id=? AND julianday(created_at_utc)>=julianday(?) AND julianday(created_at_utc)<julianday(?)`, groupID, periodStartText, periodEndText)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM requests WHERE group_id=? AND created_at_utc>=? AND created_at_utc<=? GROUP BY status`, groupID, hourStart, nowText)
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
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE group_id=? AND status='reserved'`, groupID).Scan(&active); err != nil {
		return nil, err
	}
	denominator := outcomes["completed"] + outcomes["failed"]
	errorRate := 0.0
	if denominator > 0 {
		errorRate = float64(outcomes["failed"]) / float64(denominator)
	}
	return map[string]any{
		"group_id":         groupID,
		"group_name":       groupName,
		"generated_at_utc": nowText,
		"rpm":              map[string]any{"used": rpm, "limit": limits.RPMLimit},
		"five_hour":        map[string]any{"used_tokens": fiveHour, "limit": limits.Token5H, "used_microyuan": fiveHourAmount, "amount_limit": limits.Amount5H},
		"daily":            map[string]any{"used_tokens": daily, "limit": limits.TokenDaily, "used_microyuan": dailyAmount, "amount_limit": limits.AmountDaily},
		"weekly":           map[string]any{"used_tokens": weekly, "limit": limits.TokenWeekly, "used_microyuan": weeklyAmount, "amount_limit": limits.AmountWeekly, "active": !periodActiveNow, "from_utc": weekStart.Format(time.RFC3339Nano)},
		"period": map[string]any{"used_tokens": periodTokens, "limit": limits.TokenPeriod, "used_microyuan": periodAmount, "amount_limit": limits.AmountPeriod, "active": periodActiveNow, "start_utc": periodStartText, "end_utc": periodEndText, "transition_utc": func() string {
			if periodTransition.IsZero() {
				return ""
			}
			return periodTransition.Format(time.RFC3339Nano)
		}()},
		"last_hour": map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"active":    active,
	}, nil
}

// GroupUsageBuckets 返回每个分组在三个主窗口的 (token, 微元) 用量，供分组
// 列表渲染额度紧张标记。周期窗口不参与：它按组独立启停，批量算切换点成本高，
// 是否咬住看各组概览弹窗。
func (s *Store) GroupUsageBuckets(ctx context.Context, now time.Time) (five, daily, weekly map[int64][2]int64, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	fiveHourStart := now.Add(-5 * time.Hour).Format(time.RFC3339Nano)
	date := billingDate(now)
	weekStart := billingWeekStart(now).Format(time.RFC3339Nano)
	five, daily, weekly = make(map[int64][2]int64), make(map[int64][2]int64), make(map[int64][2]int64)
	load := func(into map[int64][2]int64, filter string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, `SELECT group_id,COALESCE(SUM(reserved_tokens+charged_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE request_id IS NOT NULL AND status IN ('reserved','completed','failed','aborted') AND group_id>0 AND `+filter+` GROUP BY group_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var bucket [2]int64
			if err := rows.Scan(&id, &bucket[0], &bucket[1]); err != nil {
				return err
			}
			into[id] = bucket
		}
		return rows.Err()
	}
	if err = load(five, `created_at_utc>=? AND created_at_utc<=?`, fiveHourStart, nowText); err != nil {
		return nil, nil, nil, err
	}
	if err = load(daily, `billing_date_bj=? AND created_at_utc<=?`, date, nowText); err != nil {
		return nil, nil, nil, err
	}
	if err = load(weekly, `created_at_utc>=? AND created_at_utc<=?`, weekStart, nowText); err != nil {
		return nil, nil, nil, err
	}
	return five, daily, weekly, nil
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
//
// 额度不再出现在全局概览里：v18 起额度按分组分配（见 GroupOverview），
// 这里只剩全局限流（RPM）与健康度指标。
func (s *Store) Overview(ctx context.Context, now time.Time, limits QuotaLimits) (map[string]any, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	nowText := now.Format(time.RFC3339Nano)
	minuteStart := now.Add(-time.Minute).Format(time.RFC3339Nano)
	hourStart := now.Add(-time.Hour)
	var rpm int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE created_at_utc>=? AND created_at_utc<=?`, minuteStart, nowText).Scan(&rpm); err != nil {
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
	modelCalls := make([]map[string]any, 0)
	modelRows, err := s.db.QueryContext(ctx, `SELECT r.model_id, COALESCE(m.public_name,''), count(*) FROM requests r LEFT JOIN models m ON m.id=r.model_id WHERE r.created_at_utc>=? AND r.created_at_utc<=? GROUP BY r.model_id,m.public_name ORDER BY count(*) DESC, r.model_id`, now.Add(-24*time.Hour).Format(time.RFC3339Nano), nowText)
	if err != nil {
		return nil, err
	}
	for modelRows.Next() {
		// 删除渠道会级联删掉模型，历史请求的 model_id 因此变成 NULL；
		// 这里必须按可空值读取，否则一条历史记录就能让整个概览（含登录）500。
		var id sql.NullInt64
		var count int64
		var name string
		if err := modelRows.Scan(&id, &name, &count); err != nil {
			modelRows.Close()
			return nil, err
		}
		modelCalls = append(modelCalls, map[string]any{"model_id": nullableNumber(id), "model": name, "count": count})
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return nil, err
	}
	modelRows.Close()
	denominator := outcomes["completed"] + outcomes["failed"]
	errorRate := 0.0
	if denominator > 0 {
		errorRate = float64(outcomes["failed"]) / float64(denominator)
	}
	return map[string]any{
		"generated_at_utc": nowText,
		"rpm":              map[string]any{"used": rpm, "limit": limits.RPMLimit},
		"last_hour":        map[string]any{"completed": outcomes["completed"], "failed": outcomes["failed"], "aborted": outcomes["aborted"], "rejected": outcomes["rejected"], "reserved": outcomes["reserved"], "error_rate": errorRate},
		"trend":            trend,
		"model_calls":      modelCalls,
		"recent":           recent,
	}, nil
}
