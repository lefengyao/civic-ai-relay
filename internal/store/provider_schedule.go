package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 供应商生效时段的三种方式。always 是默认值，也是升级前所有渠道的状态：
// 只要供应商本身启用，全天都可用。range 适合临时按天采购的渠道（到点自动生效/失效），
// daily 适合「只在夜间/某几个工作日的某个时段」才可用的上游。
const (
	ProviderScheduleAlways = "always"
	ProviderScheduleRange  = "range"
	ProviderScheduleDaily  = "daily"
)

// providerWeekdaysAll 是「每天」的位掩码：bit0=周一 … bit6=周日。
const providerWeekdaysAll = 0x7f

// maxScheduleWindows 是一条渠道在「每天重复」下最多能配几段。一天只有 24 小时，
// 留这个上限只为拦住脚本误把上千段写进来（判定是逐段线性扫，热路径上不该无界）。
const maxScheduleWindows = 24

// ScheduleWindow 是「每天重复」方式下的一段可用区间（北京时间第几分钟）。
//
// start == end 表示「全天」；start > end 表示跨零点（如 22:00-08:00）。多段之间
// 允许重叠——重叠不改变结果，只是冗余，保存时会去重。
//
// Weekdays 是**这一段自己的**星期掩码（bit0=周一 … bit6=周日；0 或 127 = 每天）。
// 每段独立而不是全局，是因为最常见的组合恰恰是「工作日只在夜里、周末全天」——
// 全局星期会逼运维把它拆成两条渠道，而在归属唯一的规则下那意味着两个上游账号。
type ScheduleWindow struct {
	StartMinute int
	EndMinute   int
	Weekdays    int
}

// ProviderSchedule 是渠道的生效时段。零值等价于 always。
//
// 时区口径统一用北京时间（与「当日额度」等窗口一致）：range 的起止时间入库前
// 归一化成 UTC RFC3339，daily 的分钟数与星期则按北京时间解释。
type ProviderSchedule struct {
	Mode        string
	StartUTC    string // range：RFC3339 UTC，空字符串表示不限起点
	EndUTC      string // range：RFC3339 UTC，空字符串表示不限终点
	StartMinute int    // daily：0..1439，北京时间
	EndMinute   int
	Weekdays    int              // daily：bit0=周一 … bit6=周日；0 或 127 都表示每天
	Windows     []ScheduleWindow // daily：多段；空 = 回落到 StartMinute/EndMinute 单段（v25 之前的数据）
	// ExcludeWindows 是「排除时间段」（v28）：命中任一段即不可用，**与 Mode 无关**。
	//
	// 之所以独立于三种生效方式，是因为「每天凌晨上游维护」这类需求跟「平时到底
	// 用哪种方式限定可用性」是正交的两件事：不限 + 排除夜里两小时、夜间窗口 +
	// 排除周三下午，都要能表达。夹在包含判定里做只能是「包含减排除」，写成独立
	// 判定既好读，也不会在 always 分支上被漏掉。
	ExcludeWindows []ScheduleWindow
}

// ActiveAt 判断 now 是否落在生效时段内。它是全链路唯一的时段口径：
// /v1/models、请求路由、故障转移成员筛选、探活、模型同步都用它，
// 避免出现「客户端看不到但管理台说可用」这类难查的漂移。
func (s ProviderSchedule) ActiveAt(now time.Time) bool {
	local := now.In(beijingZone)
	minute := local.Hour()*60 + local.Minute()
	// 排除段优先（v28）：命中即不可用，不看生效方式。没有排除段时这一步是空转。
	for _, window := range s.ExcludeWindows {
		if dailyWindowActive(window, local, minute) {
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(s.Mode)) {
	case ProviderScheduleRange:
		// 起点含、终点不含：区间 [start, end)
		if start, err := time.Parse(time.RFC3339, s.StartUTC); err == nil && now.Before(start) {
			return false
		}
		if end, err := time.Parse(time.RFC3339, s.EndUTC); err == nil && !now.Before(end) {
			return false
		}
		return true
	case ProviderScheduleDaily:
		// 多段（v25）任一命中即可用。没配多段时回落到单段列——那是 v25 之前的
		// 数据形态，也兼容直接写库的老脚本。
		if len(s.Windows) == 0 {
			legacy := ScheduleWindow{StartMinute: s.StartMinute, EndMinute: s.EndMinute, Weekdays: s.Weekdays}
			return dailyWindowActive(legacy, local, minute)
		}
		for _, window := range s.Windows {
			if dailyWindowActive(window, local, minute) {
				return true
			}
		}
		return false
	default: // always / 空值 / 无法识别的历史值一律当不限，绝不让脏数据把渠道永久停掉
		return true
	}
}

// dailyWindowActive 判断「某一段」在 day 的 minute 时刻是否生效。
//
// 跨零点的时段（如 22:00-08:00）：凌晨那一段归属「前一天」，星期也按前一天算
// —— 否则「周一夜间」会在周二凌晨突然失效。星期取自这一段自己（v25 起每段
// 独立），所以「工作日夜间」与「周末全天」可以并存在同一条渠道上。
func dailyWindowActive(window ScheduleWindow, day time.Time, minute int) bool {
	start, end := window.StartMinute, window.EndMinute
	anchor := day
	if start > end && minute < end {
		anchor = day.AddDate(0, 0, -1)
	}
	if !scheduleWeekdayAllows(window.Weekdays, anchor) {
		return false
	}
	if start == end {
		return true // 起止相同 = 全天，只受星期约束
	}
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

// scheduleWeekdayAllows 判断某一天是否被位掩码选中。0 与 127 都表示每天。
func scheduleWeekdayAllows(mask int, day time.Time) bool {
	if mask == 0 || mask == providerWeekdaysAll {
		return true
	}
	bit := 1 << uint((int(day.Weekday())+6)%7) // 周一=bit0 … 周日=bit6
	return mask&bit != 0
}

// Schedule 把 Provider 上的散列字段收成一个值类型，方便判定与测试。
func (p Provider) Schedule() ProviderSchedule {
	return ProviderSchedule{
		Mode:           p.ScheduleMode,
		StartUTC:       p.ScheduleStartUTC,
		EndUTC:         p.ScheduleEndUTC,
		StartMinute:    p.ScheduleStartMinute,
		EndMinute:      p.ScheduleEndMinute,
		Weekdays:       p.ScheduleWeekdays,
		Windows:        p.ScheduleWindows,
		ExcludeWindows: p.ScheduleExcludeWindows,
	}
}

// ActiveAt 等价于 p.Schedule().ActiveAt(now)，只是省掉调用方的临时变量。
func (p Provider) ActiveAt(now time.Time) bool { return p.Schedule().ActiveAt(now) }

// ProviderScheduleInput 是管理端提交的原始时段（字符串形式，未归一化）。
// 指针语义由调用方掌握：整段提交才算「改时段」，只改名字时不能顺手把时段清掉。
type ProviderScheduleInput struct {
	Mode       string
	Start      string // range：RFC3339 或 2006-01-02T15:04[:05] 或 2006-01-02（按北京时间解释）
	End        string // 同上
	DailyStart string // daily："HH:MM"（单段写法，v25 之前的老请求体）
	DailyEnd   string // daily："HH:MM"
	Weekdays   []int  // daily：1=周一 … 7=周日；为空或全选 = 每天
	// Windows 是 daily 的多段写法（v25）。nil = 没传，回落到 DailyStart/DailyEnd；
	// 空切片 = 明确要求「一段都不留」，直接报错而不是静默变成全天。
	Windows []ScheduleWindowInput
	// ExcludeWindows 是 v28 的排除段。空/nil 都表示不排除——这与 Windows 的
	// 「空切片即报错」刻意不同：排除段本来就可以一段都没有。
	ExcludeWindows []ScheduleWindowInput
}

// ScheduleWindowInput 是多段时段的单段入参，钟点用 "HH:MM" 表达。
type ScheduleWindowInput struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// Weekdays：1=周一 … 7=周日；为空或全选 = 这一段每天都生效。
	Weekdays []int `json:"weekdays"`
}

// NormalizeProviderSchedule 校验并归一化时段输入。错误信息会直接显示在管理台，
// 所以要说清「该改成什么」，而不是只回 invalid_schedule。
func NormalizeProviderSchedule(in ProviderScheduleInput) (ProviderSchedule, error) {
	// 排除段先归一化：它独立于生效方式，三种分支都要带上（v28）。
	exclude, err := normalizeExcludeWindows(in.ExcludeWindows)
	if err != nil {
		return ProviderSchedule{}, err
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" || mode == ProviderScheduleAlways {
		return ProviderSchedule{Mode: ProviderScheduleAlways, ExcludeWindows: exclude}, nil
	}
	switch mode {
	case ProviderScheduleRange:
		start, err := parseScheduleInstant(in.Start, "开始时间")
		if err != nil {
			return ProviderSchedule{}, err
		}
		end, err := parseScheduleInstant(in.End, "结束时间")
		if err != nil {
			return ProviderSchedule{}, err
		}
		if start != "" && end != "" {
			// start/end 已由 parseScheduleInstant 归一化成 RFC3339，这里的解析在正常
			// 情况下不会失败；仍然显式检查 err，避免将来改格式时把解析失败误报成
			// 「结束时间必须晚于开始时间」这种指错方向的提示。
			startAt, startErr := time.Parse(time.RFC3339, start)
			endAt, endErr := time.Parse(time.RFC3339, end)
			if startErr != nil || endErr != nil {
				return ProviderSchedule{}, fmt.Errorf("时段无法归一化（%q → %q），请检查时间格式后重试", start, end)
			}
			if !startAt.Before(endAt) {
				return ProviderSchedule{}, fmt.Errorf("结束时间必须晚于开始时间（当前：%s → %s）", formatScheduleInstant(start), formatScheduleInstant(end))
			}
		}
		return ProviderSchedule{Mode: ProviderScheduleRange, StartUTC: start, EndUTC: end, ExcludeWindows: exclude}, nil
	case ProviderScheduleDaily:
		windows, err := normalizeScheduleWindows(in)
		if err != nil {
			return ProviderSchedule{}, err
		}
		// 第一段同步写进单段列（含它自己的星期）：旧版本回滚后仍能看到一个能用的
		// 时段，而不是整条渠道被读成「00:00-00:00 全天」。
		return ProviderSchedule{
			Mode:           ProviderScheduleDaily,
			StartMinute:    windows[0].StartMinute,
			EndMinute:      windows[0].EndMinute,
			Weekdays:       windows[0].Weekdays,
			Windows:        windows,
			ExcludeWindows: exclude,
		}, nil
	default:
		return ProviderSchedule{}, fmt.Errorf("不支持的生效方式 %q：可选 always（不限）、range（固定区间）、daily（每天重复）", in.Mode)
	}
}

// normalizeScheduleWindows 解析多段时段：去重、按开始时间排序、限制段数。
// 每段带自己的星期（v25）；老的单段请求体则用顶层的 Weekdays 作为它的星期。
func normalizeScheduleWindows(in ProviderScheduleInput) ([]ScheduleWindow, error) {
	if in.Windows == nil {
		// 老请求体：只有一段。空串按 0 处理，于是 00:00-00:00 = 全天。
		start, err := parseScheduleClock(in.DailyStart, "每天的开始时间")
		if err != nil {
			return nil, err
		}
		end, err := parseScheduleClock(in.DailyEnd, "每天的结束时间")
		if err != nil {
			return nil, err
		}
		mask, err := scheduleWeekdayMask(in.Weekdays)
		if err != nil {
			return nil, err
		}
		return []ScheduleWindow{{StartMinute: start, EndMinute: end, Weekdays: mask}}, nil
	}
	if len(in.Windows) == 0 {
		return nil, fmt.Errorf("「每天重复时段」至少要留一段；要全天可用请把生效方式改成「不限」")
	}
	if len(in.Windows) > maxScheduleWindows {
		return nil, fmt.Errorf("时段最多 %d 段，收到 %d 段", maxScheduleWindows, len(in.Windows))
	}
	out := make([]ScheduleWindow, 0, len(in.Windows))
	seen := make(map[ScheduleWindow]bool, len(in.Windows))
	for index, item := range in.Windows {
		window, err := parseScheduleWindow(item, fmt.Sprintf("第 %d 段", index+1))
		if err != nil {
			return nil, err
		}
		if seen[window] {
			continue
		}
		seen[window] = true
		out = append(out, window)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("「每天重复时段」至少要留一段；要全天可用请把生效方式改成「不限」")
	}
	// 按钟点排序：管理台回显稳定，人肉比对时也不用在脑子里重排。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMinute != out[j].StartMinute {
			return out[i].StartMinute < out[j].StartMinute
		}
		return out[i].EndMinute < out[j].EndMinute
	})
	return out, nil
}

// normalizeExcludeWindows 解析排除段（v28）。与包含段的差别只有两点：
// 允许一段都没有；起止相同（= 全天排除）直接拒绝——那等于把渠道永久停掉，
// 有更直白的开关可用，却是手滑最容易配出来的状态。
func normalizeExcludeWindows(items []ScheduleWindowInput) ([]ScheduleWindow, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > maxScheduleWindows {
		return nil, fmt.Errorf("排除时段最多 %d 段，收到 %d 段", maxScheduleWindows, len(items))
	}
	out := make([]ScheduleWindow, 0, len(items))
	for index, item := range items {
		window, err := parseScheduleWindow(item, fmt.Sprintf("排除段 %d", index+1))
		if err != nil {
			return nil, err
		}
		if window.StartMinute == window.EndMinute {
			return nil, fmt.Errorf("排除段 %d 的起止时间相同（%s）= 全天排除，会让这条渠道永久不可用；要停用它请关闭供应商开关", index+1, formatScheduleClock(window.StartMinute))
		}
		out = append(out, window)
	}
	return dedupeScheduleWindows(out), nil
}

// parseScheduleWindow 解析一段「HH:MM → HH:MM + 自己的星期」。label 只用于错误提示，
// 例如「第 1 段」「排除段 2」，让报错能直接指到弹窗里的哪一行。
func parseScheduleWindow(item ScheduleWindowInput, label string) (ScheduleWindow, error) {
	start, err := parseScheduleClock(item.Start, label+"的开始时间")
	if err != nil {
		return ScheduleWindow{}, err
	}
	end, err := parseScheduleClock(item.End, label+"的结束时间")
	if err != nil {
		return ScheduleWindow{}, err
	}
	mask, err := scheduleWeekdayMask(item.Weekdays)
	if err != nil {
		return ScheduleWindow{}, err
	}
	if mask == 0 {
		// scheduleWeekdayMask 对空列表返回「每天」；0 只可能来自显式传入的空掩码，
		// 这里统一成每天，避免一段配置因为星期为空而永远不生效。
		mask = providerWeekdaysAll
	}
	return ScheduleWindow{StartMinute: start, EndMinute: end, Weekdays: mask}, nil
}

// dedupeScheduleWindows 去重并按开始钟点排序：管理台回显稳定，人肉比对时也不用
// 在脑子里重排（排除段与包含段共用同一套展示顺序）。
func dedupeScheduleWindows(windows []ScheduleWindow) []ScheduleWindow {
	out := make([]ScheduleWindow, 0, len(windows))
	seen := make(map[ScheduleWindow]bool, len(windows))
	for _, window := range windows {
		if seen[window] {
			continue
		}
		seen[window] = true
		out = append(out, window)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMinute != out[j].StartMinute {
			return out[i].StartMinute < out[j].StartMinute
		}
		return out[i].EndMinute < out[j].EndMinute
	})
	return out
}

// encodeScheduleWindows 把多段时段序列化成库里的一列。空切片存空串——「没配多段」
// 与「配了一段 00:00-00:00」是两件事，不能混。
func encodeScheduleWindows(windows []ScheduleWindow) string {
	if len(windows) == 0 {
		return ""
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return ""
	}
	return string(raw)
}

// decodeScheduleWindows 解析库里的多段时段。坏数据一律当作「没配多段」，让判定
// 回落到单段列，而不是把渠道永久判成不在时段（那是运维最难自查的一种全站 404）。
func decodeScheduleWindows(raw string) []ScheduleWindow {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	var decoded []ScheduleWindow
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil
	}
	out := make([]ScheduleWindow, 0, len(decoded))
	for _, window := range decoded {
		if window.StartMinute < 0 || window.StartMinute > 1439 || window.EndMinute < 0 || window.EndMinute > 1439 {
			continue
		}
		// 0 与越界值都归一成「每天」：0 来自没有这个字段的旧 JSON，保留两种表示
		// 会让管理台回显出现「一段全不选」这种读不懂的状态。
		if window.Weekdays <= 0 || window.Weekdays > providerWeekdaysAll {
			window.Weekdays = providerWeekdaysAll
		}
		out = append(out, window)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseScheduleInstant 接受带时区的 RFC3339，也接受写死的本地时间（按北京时间解释），
// 统一输出 UTC RFC3339 字符串；空串表示该侧不限。
func parseScheduleInstant(raw, label string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	// 管理台的 datetime-local 不带时区，按北京时间（与额度窗口同一口径）解释。
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, text, beijingZone); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("%s格式不对（%q）：请填 2026-11-13T20:00 这样的北京时间，或带时区的 RFC3339", label, raw)
}

// parseScheduleClock 把 "HH:MM" 解析成北京时间当天第几分钟。
func parseScheduleClock(raw, label string) (int, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, nil
	}
	hourText, minuteText, ok := strings.Cut(text, ":")
	if !ok {
		return 0, fmt.Errorf("%s格式不对（%q）：请填 HH:MM，例如 22:00", label, raw)
	}
	hour, hourErr := strconv.Atoi(strings.TrimSpace(hourText))
	minute, minuteErr := strconv.Atoi(strings.TrimSpace(minuteText))
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%s超出范围（%q）：小时 0-23、分钟 0-59", label, raw)
	}
	return hour*60 + minute, nil
}

// scheduleWeekdayMask 把 [1..7] 的星期列表转成位掩码。空或全选都记为「每天」。
func scheduleWeekdayMask(days []int) (int, error) {
	if len(days) == 0 {
		return providerWeekdaysAll, nil
	}
	mask := 0
	for _, day := range days {
		if day < 1 || day > 7 {
			return 0, fmt.Errorf("星期取值必须是 1（周一）到 7（周日），收到 %d", day)
		}
		mask |= 1 << uint(day-1)
	}
	if mask == providerWeekdaysAll {
		return providerWeekdaysAll, nil
	}
	return mask, nil
}

// formatScheduleClock 只用于错误提示：把「北京时间第几分钟」显示成 HH:MM。
func formatScheduleClock(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// formatScheduleInstant 只用于错误提示：把 UTC 显示成北京时间的可读形式。
func formatScheduleInstant(value string) string {
	if value == "" {
		return "不限"
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.In(beijingZone).Format("2006-01-02 15:04")
}
