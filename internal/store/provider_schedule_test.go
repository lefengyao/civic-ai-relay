package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

var bj = time.FixedZone("Asia/Shanghai", 8*3600)

// at 用北京时间构造一个时刻，测试里读起来和运维看到的配置一致。
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, bj)
}

func rangeSchedule(start, end string) ProviderSchedule {
	return ProviderSchedule{Mode: ProviderScheduleRange, StartUTC: start, EndUTC: end}
}

func dailySchedule(startMinute, endMinute, weekdays int) ProviderSchedule {
	return ProviderSchedule{Mode: ProviderScheduleDaily, StartMinute: startMinute, EndMinute: endMinute, Weekdays: weekdays}
}

func TestProviderScheduleActiveAt(t *testing.T) {
	cases := []struct {
		name     string
		schedule ProviderSchedule
		now      time.Time
		want     bool
	}{
		{"零值按不限", ProviderSchedule{}, at(2026, 11, 13, 10, 0), true},
		{"显式不限", ProviderSchedule{Mode: ProviderScheduleAlways}, at(2026, 11, 13, 3, 0), true},

		// range：起点含、终点不含，空字符串表示该侧不限
		{"区间内", rangeSchedule("2026-11-13T00:00:00Z", "2026-11-14T00:00:00Z"), at(2026, 11, 13, 12, 0), true},
		{"未到起点", rangeSchedule("2026-11-13T12:00:00Z", "2026-11-14T00:00:00Z"), at(2026, 11, 13, 19, 59), false},
		{"恰好起点", rangeSchedule("2026-11-13T12:00:00Z", "2026-11-14T00:00:00Z"), at(2026, 11, 13, 20, 0), true},
		{"恰好终点", rangeSchedule("2026-11-13T12:00:00Z", "2026-11-14T00:00:00Z"), at(2026, 11, 14, 8, 0), false},
		{"只有起点且已过", rangeSchedule("2026-11-13T00:00:00Z", ""), at(2026, 11, 13, 9, 0), true},
		{"只有终点且已过", rangeSchedule("", "2026-11-13T00:00:00Z"), at(2026, 11, 13, 9, 0), false},
		{"脏数据按不限", rangeSchedule("not-a-time", "also-bad"), at(2026, 11, 13, 9, 0), true},

		// daily：北京时间，起点含、终点不含
		{"白班内", dailySchedule(9*60, 18*60, providerWeekdaysAll), at(2026, 11, 13, 10, 0), true},
		{"白班前", dailySchedule(9*60, 18*60, providerWeekdaysAll), at(2026, 11, 13, 8, 59), false},
		{"白班终点", dailySchedule(9*60, 18*60, providerWeekdaysAll), at(2026, 11, 13, 18, 0), false},
		{"跨零点深夜", dailySchedule(22*60, 8*60, providerWeekdaysAll), at(2026, 11, 13, 23, 30), true},
		{"跨零点凌晨", dailySchedule(22*60, 8*60, providerWeekdaysAll), at(2026, 11, 14, 2, 0), true},
		{"跨零点终点", dailySchedule(22*60, 8*60, providerWeekdaysAll), at(2026, 11, 14, 8, 0), false},
		{"跨零点白天", dailySchedule(22*60, 8*60, providerWeekdaysAll), at(2026, 11, 14, 12, 0), false},
		{"起止相同=全天", dailySchedule(0, 0, providerWeekdaysAll), at(2026, 11, 14, 15, 0), true},

		// 星期：2026-11-13 是周五，2026-11-16 是周一
		{"只选周一且当天是周五", dailySchedule(0, 0, 1<<0), at(2026, 11, 13, 12, 0), false},
		{"只选周五且当天是周五", dailySchedule(0, 0, 1<<4), at(2026, 11, 13, 12, 0), true},
		// 跨零点时段归属「时段开始那天」：周一 22:00-08:00 覆盖周二凌晨
		{"周一夜间覆盖周二凌晨", dailySchedule(22*60, 8*60, 1<<0), at(2026, 11, 17, 2, 0), true},
		{"周二夜里不算周一", dailySchedule(22*60, 8*60, 1<<0), at(2026, 11, 17, 23, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.schedule.ActiveAt(tc.now); got != tc.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v（schedule=%+v）", tc.now.Format(time.RFC3339), got, tc.want, tc.schedule)
			}
		})
	}
}

func TestNormalizeProviderSchedule(t *testing.T) {
	t.Run("空值等于不限", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{})
		if err != nil || schedule.Mode != ProviderScheduleAlways {
			t.Fatalf("schedule=%+v err=%v", schedule, err)
		}
	})

	t.Run("本地时间按北京时间解释", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "range", Start: "2026-11-13T20:00", End: "2026-11-14T08:00"})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.StartUTC != "2026-11-13T12:00:00Z" || schedule.EndUTC != "2026-11-14T00:00:00Z" {
			t.Fatalf("start=%s end=%s", schedule.StartUTC, schedule.EndUTC)
		}
	})

	t.Run("结束必须晚于开始", func(t *testing.T) {
		_, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "range", Start: "2026-11-14T08:00", End: "2026-11-13T20:00"})
		if err == nil || !strings.Contains(err.Error(), "晚于开始") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("格式错误要给出可照做的提示", func(t *testing.T) {
		_, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "range", Start: "13号晚上"})
		if err == nil || !strings.Contains(err.Error(), "开始时间格式不对") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("每天时段接受不补零的写法", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "8:00", DailyEnd: "22:30"})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.StartMinute != 480 || schedule.EndMinute != 22*60+30 || schedule.Weekdays != providerWeekdaysAll {
			t.Fatalf("schedule=%+v", schedule)
		}
	})

	t.Run("拒绝越界时间", func(t *testing.T) {
		if _, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "25:00"}); err == nil {
			t.Fatal("25:00 应该被拒")
		}
	})

	t.Run("星期掩码", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "22:00", DailyEnd: "08:00", Weekdays: []int{1, 3, 5}})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Weekdays != 1<<0|1<<2|1<<4 {
			t.Fatalf("weekdays=%07b", schedule.Weekdays)
		}
		if _, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", Weekdays: []int{0}}); err == nil {
			t.Fatal("星期 0 应该被拒")
		}
	})

	t.Run("全选星期等于每天", func(t *testing.T) {
		// 用非全天时段：00:00-00:00 会被归一成 always（见下一个子测试），
		// 那样看不到星期掩码。
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "09:00", DailyEnd: "10:00", Weekdays: []int{1, 2, 3, 4, 5, 6, 7}})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Weekdays != providerWeekdaysAll {
			t.Fatalf("weekdays=%d", schedule.Weekdays)
		}
	})

	// v0.15.1：全天段不再作为 daily 存下来。「起止相同 + 每天」整条约束等于空转，
	// 与「不限」等价；两种表达并存会让列表标签在「不限」和「全天」之间摇摆，
	// 排查时还要先反应过来是一回事。
	t.Run("全天段归一成不限", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily"})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Mode != ProviderScheduleAlways || len(schedule.Windows) != 0 {
			t.Fatalf("daily 全天应降级为 always，得到 %+v", schedule)
		}
		// 多段里只要有一段覆盖全天，其余段全是冗余，同样降级。
		schedule, err = NormalizeProviderSchedule(ProviderScheduleInput{
			Mode: "daily",
			Windows: []ScheduleWindowInput{
				{Start: "22:00", End: "08:00"},
				{Start: "00:00", End: "00:00"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Mode != ProviderScheduleAlways {
			t.Fatalf("含全天段的 daily 应降级，得到 %+v", schedule)
		}
		// 只覆盖某一天的全天段对其它日子仍有约束，不能降级。
		schedule, err = NormalizeProviderSchedule(ProviderScheduleInput{
			Mode:    "daily",
			Windows: []ScheduleWindowInput{{Start: "00:00", End: "00:00", Weekdays: []int{1}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Mode != ProviderScheduleDaily || len(schedule.Windows) != 1 {
			t.Fatalf("只覆盖周一的全天段应保持 daily，得到 %+v", schedule)
		}
		// 排除段与生效方式正交：降级成 always 时不能把排除段丢掉。
		schedule, err = NormalizeProviderSchedule(ProviderScheduleInput{
			Mode:           "daily",
			ExcludeWindows: []ScheduleWindowInput{{Start: "02:00", End: "04:00"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Mode != ProviderScheduleAlways || len(schedule.ExcludeWindows) != 1 {
			t.Fatalf("降级后仍应保留排除段，得到 %+v", schedule)
		}
	})
}

func TestProviderScheduleCRUD(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "22:00", DailyEnd: "08:00", Weekdays: []int{1, 2, 3, 4, 5}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := repo.CreateProvider(ctx, NewProvider{Name: "night", BaseURL: "https://night.example", APIKey: "secret", Schedule: schedule})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScheduleMode != ProviderScheduleDaily || created.ScheduleStartMinute != 22*60 || created.ScheduleEndMinute != 8*60 {
		t.Fatalf("created=%+v", created)
	}

	stored, err := repo.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ScheduleMode != ProviderScheduleDaily || stored.ScheduleWeekdays != 0x1f {
		t.Fatalf("stored=%+v", stored)
	}

	// 只改名字不能顺手把时段清掉：ScheduleMode 为 nil 表示「保持原样」
	renamed, err := repo.UpdateProvider(ctx, created.ID, UpdateProvider{Name: "night-renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "night-renamed" || renamed.ScheduleMode != ProviderScheduleDaily || renamed.ScheduleStartMinute != 22*60 {
		t.Fatalf("renamed=%+v", renamed)
	}

	// 整段替换成固定区间
	mode := "range"
	start, end := "2026-11-13T20:00", "2026-11-14T08:00"
	updated, err := repo.UpdateProvider(ctx, created.ID, UpdateProvider{ScheduleMode: &mode, ScheduleStart: &start, ScheduleEnd: &end})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ScheduleMode != ProviderScheduleRange || updated.ScheduleEndUTC != "2026-11-14T00:00:00Z" {
		t.Fatalf("updated=%+v", updated)
	}

	// 非法时段必须报错且不落库
	badMode := "range"
	badStart, badEnd := "2026-11-14T08:00", "2026-11-13T20:00"
	if _, err := repo.UpdateProvider(ctx, created.ID, UpdateProvider{ScheduleMode: &badMode, ScheduleStart: &badStart, ScheduleEnd: &badEnd}); err == nil {
		t.Fatal("结束早于开始应该被拒")
	}
	unchanged, err := repo.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ScheduleMode != ProviderScheduleRange || unchanged.ScheduleStartUTC != "2026-11-13T12:00:00Z" {
		t.Fatalf("被拒的更新不应落库：%+v", unchanged)
	}
}

func TestUsableModelsForKeySkipsOutOfWindowProvider(t *testing.T) {
	repo, key, model, _ := newReservationFixture(t, 1_000_000, 1_000_000)
	ctx := context.Background()

	usable, err := repo.UsableModelsForKey(ctx, key.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 1 {
		t.Fatalf("默认时段应放行，got %d 个模型", len(usable))
	}

	mode := "range"
	start := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	end := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := repo.UpdateProvider(ctx, model.ProviderID, UpdateProvider{ScheduleMode: &mode, ScheduleStart: &start, ScheduleEnd: &end}); err != nil {
		t.Fatal(err)
	}
	usable, err = repo.UsableModelsForKey(ctx, key.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 0 {
		t.Fatalf("时段已过应过滤掉该渠道的模型，got %d", len(usable))
	}
	// 授权关系本身不随时段变化：管理台仍要能看到「本来授权了哪些模型」
	authorized, err := repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 1 {
		t.Fatalf("AuthorizedModelsForKey 不应按时段过滤，got %d", len(authorized))
	}

	// 换到覆盖当前时刻的区间后立刻恢复
	mode = "range"
	start = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	end = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := repo.UpdateProvider(ctx, model.ProviderID, UpdateProvider{ScheduleMode: &mode, ScheduleStart: &start, ScheduleEnd: &end}); err != nil {
		t.Fatal(err)
	}
	usable, err = repo.UsableModelsForKey(ctx, key.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 1 {
		t.Fatalf("进入时段后应恢复，got %d", len(usable))
	}
}

// 转发口径自己再挡一层：停用 Key 即使调用方忘了鉴权，也拿不到「此刻可用」的模型。
// AuthorizedModelsForKey 刻意不校验 enabled（管理台要展示配置口径），两者不能混。
func TestUsableModelsForKeyRejectsDisabledKey(t *testing.T) {
	repo, key, _, _ := newReservationFixture(t, 1_000_000, 1_000_000)
	ctx := context.Background()
	disabled := false
	if _, err := repo.UpdateClientKey(ctx, key.ID, UpdateClientKey{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	usable, err := repo.UsableModelsForKey(ctx, key.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 0 {
		t.Fatalf("停用 Key 不应拿到可用模型，got %d", len(usable))
	}
	authorized, err := repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 1 {
		t.Fatalf("AuthorizedModelsForKey 仍应展示授权关系（停用不影响配置口径），got %d", len(authorized))
	}
}

// v21 之前建的渠道没有时段列，升级后必须一律是「不限」，不能因为默认值把渠道
// 悄悄停掉——这是这个迁移唯一可能造成事故的地方。
func TestMigrationV21DefaultsToAlways(t *testing.T) {
	db := openLegacyAt(t, 20)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO providers(id,name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (1,'legacy','https://legacy.example',x'00',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	applyMigration(db, t, 21)

	var mode, start, end string
	var startMinute, endMinute, weekdays int
	if err := db.QueryRow(`SELECT schedule_mode,schedule_start_utc,schedule_end_utc,schedule_start_minute,schedule_end_minute,schedule_weekdays FROM providers WHERE id=1`).
		Scan(&mode, &start, &end, &startMinute, &endMinute, &weekdays); err != nil {
		t.Fatal(err)
	}
	if mode != ProviderScheduleAlways || start != "" || end != "" || startMinute != 0 || endMinute != 0 || weekdays != 0 {
		t.Fatalf("旧渠道升级后时段不为不限：mode=%q start=%q end=%q startMinute=%d endMinute=%d weekdays=%d", mode, start, end, startMinute, endMinute, weekdays)
	}
	if !(ProviderSchedule{Mode: mode}).ActiveAt(time.Now()) {
		t.Fatal("旧渠道升级后应全天可用")
	}
}

// openLegacyAt 在临时文件上按顺序执行 v1..version 的迁移语句，构造「升级前」的库。
func openLegacyAt(t *testing.T, version int) *sql.DB {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.TempDir(), `\`, "/") + "/legacy.db?_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > version {
			break
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply legacy migration v%d: %v", m.version, err)
			}
		}
	}
	return db
}

// multiWindowSchedule 给每段套同一份星期——只有在「所有段星期相同」时，
// 才等价于一个全局星期。
// sw 是测试里的时段简写。ScheduleWindow 有第三字段（每段自己的星期），
// 位置式字面量读起来太吵；默认「每天」，与归一化后未指定星期的结果一致。
func sw(startMinute, endMinute int) ScheduleWindow {
	return ScheduleWindow{StartMinute: startMinute, EndMinute: endMinute, Weekdays: providerWeekdaysAll}
}

func multiWindowSchedule(weekdays int, windows ...ScheduleWindow) ProviderSchedule {
	for i := range windows {
		windows[i].Weekdays = weekdays
	}
	return ProviderSchedule{Mode: ProviderScheduleDaily, Weekdays: weekdays, Windows: windows}
}

// v25：一天里可以有多个窗口，「夜间 + 午休」不用再拆成两条渠道。
func TestProviderScheduleMultipleWindowsActiveAt(t *testing.T) {
	all := providerWeekdaysAll
	monday := 1 << 0
	cases := []struct {
		name     string
		schedule ProviderSchedule
		now      time.Time
		want     bool
	}{
		{
			"两段都命中",
			multiWindowSchedule(all, sw(8*60, 12*60), sw(20*60, 22*60)),
			at(2026, 11, 13, 9, 0), true,
		},
		{
			"两段之间不算",
			multiWindowSchedule(all, sw(8*60, 12*60), sw(20*60, 22*60)),
			at(2026, 11, 13, 13, 0), false,
		},
		{
			"晚段命中",
			multiWindowSchedule(all, sw(8*60, 12*60), sw(20*60, 22*60)),
			at(2026, 11, 13, 21, 0), true,
		},
		{
			"晚段终点不含",
			multiWindowSchedule(all, sw(8*60, 12*60), sw(20*60, 22*60)),
			at(2026, 11, 13, 22, 0), false,
		},
		{
			"跨零点段与普通段混排：深夜",
			multiWindowSchedule(all, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 13, 23, 30), true,
		},
		{
			"跨零点段与普通段混排：凌晨",
			multiWindowSchedule(all, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 14, 2, 0), true,
		},
		{
			"跨零点段与普通段混排：午间",
			multiWindowSchedule(all, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 14, 12, 30), true,
		},
		{
			"跨零点段与普通段混排：上午空档",
			multiWindowSchedule(all, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 14, 9, 0), false,
		},
		{
			"星期是全局的：周一晚段",
			multiWindowSchedule(monday, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 16, 23, 0), true,
		},
		{
			"星期是全局的：跨零点归周一",
			multiWindowSchedule(monday, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 17, 2, 0), true,
		},
		{
			"星期是全局的：周二午间不生效",
			multiWindowSchedule(monday, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 17, 12, 30), false,
		},
		{
			"星期是全局的：周一午间生效",
			multiWindowSchedule(monday, sw(22*60, 8*60), sw(12*60, 13*60)),
			at(2026, 11, 16, 12, 30), true,
		},
		{
			"多段里的全天段",
			multiWindowSchedule(all, sw(0, 0)),
			at(2026, 11, 14, 3, 17), true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.schedule.ActiveAt(tc.now); got != tc.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v（schedule=%+v）", tc.now.Format(time.RFC3339), got, tc.want, tc.schedule)
			}
		})
	}
}

// 旧数据（v25 之前）没有多段列：Windows 为空时必须继续按单段列判定，
// 不能在升级瞬间把「夜间渠道」读成「全天」。
func TestProviderScheduleFallsBackToSingleWindow(t *testing.T) {
	schedule := dailySchedule(22*60, 8*60, providerWeekdaysAll)
	if !schedule.ActiveAt(at(2026, 11, 13, 23, 0)) {
		t.Fatal("深夜应生效")
	}
	if schedule.ActiveAt(at(2026, 11, 13, 12, 0)) {
		t.Fatal("白天不应生效")
	}
}

func TestNormalizeProviderScheduleWindows(t *testing.T) {
	t.Run("多段按开始时间排序并回写第一段", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{
			Mode:     "daily",
			Weekdays: []int{1, 2, 3, 4, 5},
			Windows: []ScheduleWindowInput{
				{Start: "20:00", End: "22:00"},
				{Start: "8:00", End: "12:00"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(schedule.Windows) != 2 {
			t.Fatalf("windows = %+v", schedule.Windows)
		}
		if schedule.Windows[0] != (sw(8*60, 12*60)) || schedule.Windows[1] != (sw(20*60, 22*60)) {
			t.Fatalf("windows 未按钟点排序：%+v", schedule.Windows)
		}
		// 单段列写第一段：旧版本回滚后仍有一个能用的时段，不会变成全天。
		if schedule.StartMinute != 8*60 || schedule.EndMinute != 12*60 {
			t.Fatalf("单段列 = %d-%d, want 480-720", schedule.StartMinute, schedule.EndMinute)
		}
	})

	t.Run("重复段去重", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{
			Mode: "daily",
			Windows: []ScheduleWindowInput{
				{Start: "08:00", End: "12:00"},
				{Start: "8:00", End: "12:00"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(schedule.Windows) != 1 {
			t.Fatalf("windows = %+v", schedule.Windows)
		}
	})

	t.Run("空数组要报错而不是静默变全天", func(t *testing.T) {
		_, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", Windows: []ScheduleWindowInput{}})
		if err == nil || !strings.Contains(err.Error(), "至少") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("段落错误要指出第几段", func(t *testing.T) {
		_, err := NormalizeProviderSchedule(ProviderScheduleInput{
			Mode: "daily",
			Windows: []ScheduleWindowInput{
				{Start: "08:00", End: "12:00"},
				{Start: "25:00", End: "26:00"},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "第 2 段") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("段数上限", func(t *testing.T) {
		windows := make([]ScheduleWindowInput, 0, maxScheduleWindows+1)
		for i := 0; i <= maxScheduleWindows; i++ {
			windows = append(windows, ScheduleWindowInput{Start: "00:00", End: "00:01"})
		}
		if _, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", Windows: windows}); err == nil {
			t.Fatal("超过上限应被拒")
		}
	})

	t.Run("没传多段时仍认单段写法", func(t *testing.T) {
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", DailyStart: "22:00", DailyEnd: "08:00"})
		if err != nil {
			t.Fatal(err)
		}
		if len(schedule.Windows) != 1 || schedule.Windows[0] != (sw(22*60, 8*60)) {
			t.Fatalf("windows = %+v", schedule.Windows)
		}
	})
}

func TestScheduleWindowsCodec(t *testing.T) {
	windows := []ScheduleWindow{sw(8*60, 12*60), sw(22*60, 8*60)}
	raw := encodeScheduleWindows(windows)
	decoded := decodeScheduleWindows(raw)
	if len(decoded) != 2 || decoded[0] != windows[0] || decoded[1] != windows[1] {
		t.Fatalf("往返失败：raw=%q decoded=%+v", raw, decoded)
	}
	if encodeScheduleWindows(nil) != "" {
		t.Fatal("空切片应存空串")
	}
	if got := decodeScheduleWindows(""); got != nil {
		t.Fatalf("空串应为 nil，得到 %+v", got)
	}
	if got := decodeScheduleWindows("{not json"); got != nil {
		t.Fatalf("坏 JSON 应为 nil，得到 %+v", got)
	}
	// 越界分钟（手工改库写坏）要丢掉，而不是让判定算出个永远不生效的时段。
	sanitized := decodeScheduleWindows(`[{"StartMinute":-1,"EndMinute":10},{"StartMinute":600,"EndMinute":700}]`)
	if len(sanitized) != 1 || sanitized[0] != (sw(600, 700)) {
		t.Fatalf("越界项未被丢弃：%+v", sanitized)
	}
}

// 多段要能完整落库并读回来：判定虽然只用内存结构，但管理台回显与重启后恢复
// 都依赖这一列。
func TestProviderScheduleWindowsPersist(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "night", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	mode := "daily"
	// 多段时星期跟着每段走（v25）；顶层的 ScheduleWeekdays 只服务于单段老写法。
	windows := []ScheduleWindowInput{
		{Start: "22:00", End: "08:00", Weekdays: []int{1, 2, 3, 4, 5}},
		{Start: "12:00", End: "13:00", Weekdays: []int{1, 2, 3, 4, 5}},
	}
	if _, err := repo.UpdateProvider(ctx, provider.ID, UpdateProvider{
		ScheduleMode: &mode, ScheduleWindows: &windows,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ScheduleWindows) != 2 {
		t.Fatalf("落库后的 windows = %+v", stored.ScheduleWindows)
	}
	// 排序后第一段是 12:00-13:00（720-780），星期跟着这一段走。
	if stored.ScheduleWindows[0] != (ScheduleWindow{StartMinute: 12 * 60, EndMinute: 13 * 60, Weekdays: 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4}) {
		t.Fatalf("windows[0] = %+v", stored.ScheduleWindows[0])
	}
	if stored.ScheduleStartMinute != 12*60 || stored.ScheduleEndMinute != 13*60 {
		t.Fatalf("单段列 = %d-%d", stored.ScheduleStartMinute, stored.ScheduleEndMinute)
	}
	if stored.ScheduleWeekdays != 1<<0|1<<1|1<<2|1<<3|1<<4 {
		t.Fatalf("weekdays = %07b", stored.ScheduleWeekdays)
	}
	// 只改名字不能顺手把多段清掉。
	renamed, err := repo.UpdateProvider(ctx, provider.ID, UpdateProvider{Name: "night-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.ScheduleWindows) != 2 {
		t.Fatalf("改名后 windows = %+v", renamed.ScheduleWindows)
	}
}

// 每段自己的星期：最常见的组合是「工作日只在夜里、周末全天」。
// 全局星期做不到这一点，只能拆成两条渠道——而 v24 之后一个渠道只属于一个组，
// 那就意味着两个上游账号。
func TestProviderSchedulePerWindowWeekdays(t *testing.T) {
	workday := 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4
	weekend := 1<<5 | 1<<6
	schedule := ProviderSchedule{Mode: ProviderScheduleDaily, Windows: []ScheduleWindow{
		{StartMinute: 22 * 60, EndMinute: 8 * 60, Weekdays: workday},
		{StartMinute: 0, EndMinute: 0, Weekdays: weekend},
	}}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		// 2026-11-13 周五，14 周六，15 周日，16 周一
		{"周五夜间", at(2026, 11, 13, 23, 0), true},
		{"周五白天不生效", at(2026, 11, 13, 12, 0), false},
		{"周六凌晨归周五夜间", at(2026, 11, 14, 2, 0), true},
		{"周六全天段", at(2026, 11, 14, 12, 0), true},
		{"周日全天段", at(2026, 11, 15, 20, 0), true},
		{"周一凌晨不再是周末", at(2026, 11, 16, 3, 0), false},
		{"周一白天不生效", at(2026, 11, 16, 12, 0), false},
		{"周一夜间恢复", at(2026, 11, 16, 23, 0), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := schedule.ActiveAt(tc.now); got != tc.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// 归一化要把每段的星期各自解析出来，空列表 = 这段每天生效。
// 全天段只放在周一：每天的全天段会被降级成 always（见 TestNormalizeProviderSchedule），
// 那样就没有多段可看了。
func TestNormalizeSchedulePerWindowWeekdays(t *testing.T) {
	schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{
		Mode: "daily",
		Windows: []ScheduleWindowInput{
			{Start: "22:00", End: "08:00", Weekdays: []int{1, 2, 3, 4, 5}},
			{Start: "00:00", End: "00:00", Weekdays: []int{1}},
			{Start: "12:00", End: "13:00"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Windows) != 3 {
		t.Fatalf("windows = %+v", schedule.Windows)
	}
	// 按开始分钟排序：00:00（周一全天）、12:00（每天）、22:00（工作日夜间）。
	if schedule.Windows[0].Weekdays != 1<<0 {
		t.Fatalf("周一全天段的掩码 = %07b", schedule.Windows[0].Weekdays)
	}
	if schedule.Windows[1].Weekdays != providerWeekdaysAll {
		t.Fatalf("未指定星期的段应等于每天：%+v", schedule.Windows[1])
	}
	if schedule.Windows[2].Weekdays != 1<<0|1<<1|1<<2|1<<3|1<<4 {
		t.Fatalf("工作日掩码 = %07b", schedule.Windows[2].Weekdays)
	}
	// 单段列回写第一段（含它的星期），回滚到旧版本后行为仍然一致。
	if schedule.StartMinute != 0 || schedule.EndMinute != 0 || schedule.Weekdays != 1<<0 {
		t.Fatalf("单段列 = %d-%d/%07b", schedule.StartMinute, schedule.EndMinute, schedule.Weekdays)
	}

	if _, err := NormalizeProviderSchedule(ProviderScheduleInput{
		Mode:    "daily",
		Windows: []ScheduleWindowInput{{Start: "08:00", End: "12:00", Weekdays: []int{0}}},
	}); err == nil {
		t.Fatal("星期 0 应被拒")
	}
}

// 排除时段（v28）与三种生效方式正交：它先判，命中即不可用。
func TestProviderScheduleExcludeWindowsActiveAt(t *testing.T) {
	// 2026-11-13 周五，11-14 周六，11-18 周三
	always := ProviderSchedule{Mode: ProviderScheduleAlways}
	always.ExcludeWindows = []ScheduleWindow{sw(2*60, 4*60)}
	daily := ProviderSchedule{Mode: ProviderScheduleDaily, Windows: []ScheduleWindow{sw(22*60, 8*60)}}
	daily.ExcludeWindows = []ScheduleWindow{sw(3*60, 4*60)}
	rangeSched := ProviderSchedule{Mode: ProviderScheduleRange, StartUTC: "2026-11-01T00:00:00Z", EndUTC: "2026-12-01T00:00:00Z"}
	rangeSched.ExcludeWindows = []ScheduleWindow{sw(2*60, 4*60)}
	// 只排除周三 10:00-11:00
	weekdayExclude := ProviderSchedule{Mode: ProviderScheduleAlways}
	weekdayExclude.ExcludeWindows = []ScheduleWindow{{StartMinute: 10 * 60, EndMinute: 11 * 60, Weekdays: 1 << 2}}
	// 排除周五 23:00-01:00（跨零点，凌晨算作周五）
	crossMidnight := ProviderSchedule{Mode: ProviderScheduleAlways}
	crossMidnight.ExcludeWindows = []ScheduleWindow{{StartMinute: 23 * 60, EndMinute: 1 * 60, Weekdays: 1 << 4}}

	cases := []struct {
		name     string
		schedule ProviderSchedule
		now      time.Time
		want     bool
	}{
		{"不限 + 排除：排除段内", always, at(2026, 11, 13, 3, 0), false},
		{"不限 + 排除：排除段起点含", always, at(2026, 11, 13, 2, 0), false},
		{"不限 + 排除：排除段终点不含", always, at(2026, 11, 13, 4, 0), true},
		{"不限 + 排除：段外", always, at(2026, 11, 13, 1, 59), true},

		{"夜间窗口 + 排除：窗口内被排除", daily, at(2026, 11, 13, 3, 30), false},
		{"夜间窗口 + 排除：窗口内未被排除", daily, at(2026, 11, 13, 23, 0), true},
		{"夜间窗口 + 排除：本来就不可用", daily, at(2026, 11, 13, 12, 0), false},

		{"固定区间 + 排除：区间内被排除", rangeSched, at(2026, 11, 13, 3, 0), false},
		{"固定区间 + 排除：区间内未被排除", rangeSched, at(2026, 11, 13, 5, 0), true},

		{"排除段带星期：周三命中", weekdayExclude, at(2026, 11, 18, 10, 30), false},
		{"排除段带星期：周四不命中", weekdayExclude, at(2026, 11, 19, 10, 30), true},

		{"排除段跨零点：当日深夜", crossMidnight, at(2026, 11, 13, 23, 30), false},
		{"排除段跨零点：次日凌晨归周五", crossMidnight, at(2026, 11, 14, 0, 30), false},
		{"排除段跨零点：次日凌晨过后恢复", crossMidnight, at(2026, 11, 14, 1, 0), true},
		{"排除段跨零点：周六深夜不受周五段影响", crossMidnight, at(2026, 11, 14, 23, 30), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.schedule.ActiveAt(tc.now); got != tc.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// 排除段的归一化：允许为空；起止相同（全天排除）必须拒绝，否则一次手滑就把渠道
// 永久停掉，而排查时管理台只会显示「未到时段」。
func TestNormalizeProviderExcludeWindows(t *testing.T) {
	schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "always"})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ExcludeWindows != nil {
		t.Fatalf("没传排除段应为 nil，得到 %+v", schedule.ExcludeWindows)
	}

	schedule, err = NormalizeProviderSchedule(ProviderScheduleInput{
		Mode: "always",
		ExcludeWindows: []ScheduleWindowInput{
			{Start: "04:00", End: "05:00"},
			{Start: "02:00", End: "03:00", Weekdays: []int{1, 2, 3, 4, 5}},
			{Start: "04:00", End: "05:00"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.ExcludeWindows) != 2 {
		t.Fatalf("去重/排序后应为 2 段：%+v", schedule.ExcludeWindows)
	}
	if schedule.ExcludeWindows[0] != (ScheduleWindow{StartMinute: 2 * 60, EndMinute: 3 * 60, Weekdays: 0x1f}) {
		t.Fatalf("第一段 = %+v", schedule.ExcludeWindows[0])
	}
	if schedule.ExcludeWindows[1] != (ScheduleWindow{StartMinute: 4 * 60, EndMinute: 5 * 60, Weekdays: providerWeekdaysAll}) {
		t.Fatalf("未指定星期的排除段应等于每天：%+v", schedule.ExcludeWindows[1])
	}

	// 排除段跟着「不限」一起保存：daily/range 之外也要生效（与 Mode 无关）。
	if schedule.Mode != ProviderScheduleAlways {
		t.Fatalf("mode = %q", schedule.Mode)
	}

	if _, err := NormalizeProviderSchedule(ProviderScheduleInput{
		Mode:           "always",
		ExcludeWindows: []ScheduleWindowInput{{Start: "02:00", End: "02:00"}},
	}); err == nil || !strings.Contains(err.Error(), "全天排除") {
		t.Fatalf("起止相同的排除段应被拒，得到 %v", err)
	}
}

// 排除段要能落库、读回，并且改名不会顺手清掉；整段替换（给了 ScheduleMode）才清空。
func TestProviderScheduleExcludeWindowsPersist(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "maint", BaseURL: "http://localhost:1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	mode := "always"
	if _, err := repo.UpdateProvider(ctx, provider.ID, UpdateProvider{
		ScheduleMode: &mode,
		ScheduleExcludeWindows: []ScheduleWindowInput{
			{Start: "02:00", End: "04:00"},
			{Start: "23:00", End: "01:00", Weekdays: []int{6, 7}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ScheduleMode != ProviderScheduleAlways || len(stored.ScheduleExcludeWindows) != 2 {
		t.Fatalf("落库后 = %+v", stored.ScheduleExcludeWindows)
	}
	// 排序后 02:00-04:00 在前。
	if stored.ScheduleExcludeWindows[0] != (ScheduleWindow{StartMinute: 2 * 60, EndMinute: 4 * 60, Weekdays: providerWeekdaysAll}) {
		t.Fatalf("exclude[0] = %+v", stored.ScheduleExcludeWindows[0])
	}
	if stored.ScheduleExcludeWindows[1].Weekdays != 1<<5|1<<6 {
		t.Fatalf("exclude[1] = %+v", stored.ScheduleExcludeWindows[1])
	}

	// 只改名字：排除段必须原样保留。
	renamed, err := repo.UpdateProvider(ctx, provider.ID, UpdateProvider{Name: "maint-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.ScheduleExcludeWindows) != 2 {
		t.Fatalf("改名后 = %+v", renamed.ScheduleExcludeWindows)
	}

	// 整段替换成 daily 且不带排除段：清空。
	daily := "daily"
	windows := []ScheduleWindowInput{{Start: "22:00", End: "08:00"}}
	replaced, err := repo.UpdateProvider(ctx, provider.ID, UpdateProvider{ScheduleMode: &daily, ScheduleWindows: &windows})
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.ScheduleExcludeWindows) != 0 {
		t.Fatalf("整段替换后应清空排除段：%+v", replaced.ScheduleExcludeWindows)
	}
}

// v28 给存量库补列，默认空串 = 不排除，旧渠道行为完全不变。
func TestMigrationV28AddsExcludeWindows(t *testing.T) {
	db := openLegacyAt(t, 27)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO providers(id,name,base_url,api_key_ciphertext,enabled,created_at_utc,updated_at_utc) VALUES (1,'legacy','https://legacy.example',x'00',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	applyMigration(db, t, 28)

	var raw string
	if err := db.QueryRow(`SELECT schedule_exclude_windows FROM providers WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		t.Fatalf("默认值应为空串，得到 %q", raw)
	}
	if !(ProviderSchedule{Mode: ProviderScheduleAlways, ExcludeWindows: decodeScheduleWindows(raw)}).ActiveAt(time.Now()) {
		t.Fatal("升级后的旧渠道应仍然全天可用")
	}
}

// 排除段必须真的影响「客户端可见/可路由」：把当前这一小时排除掉，模型的可用集合
// 立刻变空；清空排除段后恢复。这里走的是 UsableModelsForKey（/v1/models 与转发的
// 同一份判定），而不是只验 ActiveAt。
func TestUsableModelsForKeySkipsExcludeWindow(t *testing.T) {
	repo, key, model, _ := newReservationFixture(t, 1_000_000, 1_000_000)
	ctx := context.Background()

	now := time.Now().In(beijingZone)
	start := now.Format("15:00") // 当前小时整点
	end := now.Add(time.Hour).Format("15:00")

	mode := "always"
	if _, err := repo.UpdateProvider(ctx, model.ProviderID, UpdateProvider{
		ScheduleMode:           &mode,
		ScheduleExcludeWindows: []ScheduleWindowInput{{Start: start, End: end}},
	}); err != nil {
		t.Fatal(err)
	}
	usable, err := repo.UsableModelsForKey(ctx, key.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 0 {
		t.Fatalf("排除段命中时应没有任何可用模型，got %d", len(usable))
	}
	// 授权关系本身不随排除段变化：管理台仍要能看到「本来授权了哪些模型」。
	authorized, err := repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 1 {
		t.Fatalf("AuthorizedModelsForKey 不应按排除段过滤，got %d", len(authorized))
	}

	// 清空排除段 → 立刻恢复（同样走 UpdateProvider 的整段替换）。
	if _, err := repo.UpdateProvider(ctx, model.ProviderID, UpdateProvider{
		ScheduleMode:           &mode,
		ScheduleExcludeWindows: []ScheduleWindowInput{},
	}); err != nil {
		t.Fatal(err)
	}
	usable, err = repo.UsableModelsForKey(ctx, key.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 1 {
		t.Fatalf("清空排除段后应恢复，got %d", len(usable))
	}
}
