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
		schedule, err := NormalizeProviderSchedule(ProviderScheduleInput{Mode: "daily", Weekdays: []int{1, 2, 3, 4, 5, 6, 7}})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Weekdays != providerWeekdaysAll {
			t.Fatalf("weekdays=%d", schedule.Weekdays)
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
