package store

import (
	"fmt"
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
	Weekdays    int // daily：bit0=周一 … bit6=周日；0 或 127 都表示每天
}

// ActiveAt 判断 now 是否落在生效时段内。它是全链路唯一的时段口径：
// /v1/models、请求路由、故障转移成员筛选、探活、模型同步都用它，
// 避免出现「客户端看不到但管理台说可用」这类难查的漂移。
func (s ProviderSchedule) ActiveAt(now time.Time) bool {
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
		local := now.In(beijingZone)
		minute := local.Hour()*60 + local.Minute()
		start, end := s.StartMinute, s.EndMinute
		// 跨零点的时段（如 22:00-08:00）：凌晨那一段归属「前一天」，
		// 星期也按前一天算 —— 否则「周一夜间」会在周二凌晨突然失效。
		day := local
		if start > end && minute < end {
			day = local.AddDate(0, 0, -1)
		}
		if mask := s.Weekdays; mask != 0 && mask != providerWeekdaysAll {
			bit := 1 << uint((int(day.Weekday())+6)%7) // 周一=bit0 … 周日=bit6
			if mask&bit == 0 {
				return false
			}
		}
		if start == end {
			return true // 起止相同 = 全天，只受星期约束
		}
		if start < end {
			return minute >= start && minute < end
		}
		return minute >= start || minute < end
	default: // always / 空值 / 无法识别的历史值一律当不限，绝不让脏数据把渠道永久停掉
		return true
	}
}

// Schedule 把 Provider 上的散列字段收成一个值类型，方便判定与测试。
func (p Provider) Schedule() ProviderSchedule {
	return ProviderSchedule{
		Mode:        p.ScheduleMode,
		StartUTC:    p.ScheduleStartUTC,
		EndUTC:      p.ScheduleEndUTC,
		StartMinute: p.ScheduleStartMinute,
		EndMinute:   p.ScheduleEndMinute,
		Weekdays:    p.ScheduleWeekdays,
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
	DailyStart string // daily："HH:MM"
	DailyEnd   string // daily："HH:MM"
	Weekdays   []int  // daily：1=周一 … 7=周日；为空或全选 = 每天
}

// NormalizeProviderSchedule 校验并归一化时段输入。错误信息会直接显示在管理台，
// 所以要说清「该改成什么」，而不是只回 invalid_schedule。
func NormalizeProviderSchedule(in ProviderScheduleInput) (ProviderSchedule, error) {
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" || mode == ProviderScheduleAlways {
		return ProviderSchedule{Mode: ProviderScheduleAlways}, nil
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
			startAt, _ := time.Parse(time.RFC3339, start)
			endAt, _ := time.Parse(time.RFC3339, end)
			if !startAt.Before(endAt) {
				return ProviderSchedule{}, fmt.Errorf("结束时间必须晚于开始时间（当前：%s → %s）", formatScheduleInstant(start), formatScheduleInstant(end))
			}
		}
		return ProviderSchedule{Mode: ProviderScheduleRange, StartUTC: start, EndUTC: end}, nil
	case ProviderScheduleDaily:
		start, err := parseScheduleClock(in.DailyStart, "每天的开始时间")
		if err != nil {
			return ProviderSchedule{}, err
		}
		end, err := parseScheduleClock(in.DailyEnd, "每天的结束时间")
		if err != nil {
			return ProviderSchedule{}, err
		}
		mask, err := scheduleWeekdayMask(in.Weekdays)
		if err != nil {
			return ProviderSchedule{}, err
		}
		return ProviderSchedule{Mode: ProviderScheduleDaily, StartMinute: start, EndMinute: end, Weekdays: mask}, nil
	default:
		return ProviderSchedule{}, fmt.Errorf("不支持的生效方式 %q：可选 always（不限）、range（固定区间）、daily（每天重复）", in.Mode)
	}
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
