package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"civic-ai-relay/internal/store"
)

type providerScheduleResponse struct {
	Data struct {
		ID                  int64  `json:"ID"`
		ScheduleMode        string `json:"ScheduleMode"`
		ScheduleStartUTC    string `json:"ScheduleStartUTC"`
		ScheduleEndUTC      string `json:"ScheduleEndUTC"`
		ScheduleStartMinute int    `json:"ScheduleStartMinute"`
		ScheduleEndMinute   int    `json:"ScheduleEndMinute"`
		ScheduleWeekdays    int    `json:"ScheduleWeekdays"`
		ScheduleActive      bool   `json:"ScheduleActive"`
	} `json:"data"`
}

// 创建时带时段（snake_case，与其它创建接口一致），列表与更新要能原样读回。
func TestAdminProviderScheduleCreateAndUpdate(t *testing.T) {
	handler, adminKey := adminFixture(t)

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey, map[string]any{
		"name": "night", "base_url": "https://night.example", "api_key": "k",
		"schedule_mode": "daily", "schedule_daily_start": "22:00", "schedule_daily_end": "08:00",
		"schedule_weekdays": []int{1, 2, 3, 4, 5},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var payload providerScheduleResponse
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.ScheduleMode != "daily" || payload.Data.ScheduleStartMinute != 22*60 || payload.Data.ScheduleEndMinute != 8*60 || payload.Data.ScheduleWeekdays != 0x1f {
		t.Fatalf("创建后的时段不对: %+v", payload.Data)
	}
	// ScheduleActive 是「此刻」的状态，不能写死期望值（否则 22:00 之后跑测试就挂）；
	// 用同一份配置独立算一遍，既能验证字段真的接通了，也不依赖运行时刻。
	wantActive := store.ProviderSchedule{Mode: "daily", StartMinute: 22 * 60, EndMinute: 8 * 60, Weekdays: 0x1f}.ActiveAt(time.Now())
	if payload.Data.ScheduleActive != wantActive {
		t.Fatalf("ScheduleActive = %v，按当前时刻应为 %v", payload.Data.ScheduleActive, wantActive)
	}

	// PUT 走 PascalCase（providerItem 直接解码 store.UpdateProvider）
	updated := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, map[string]any{
		"ScheduleMode": "range", "ScheduleStart": "2026-11-13T20:00", "ScheduleEnd": "2026-11-14T08:00",
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", updated.Code, updated.Body.String())
	}
	var after providerScheduleResponse
	if err := json.Unmarshal(updated.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Data.ScheduleMode != "range" || after.Data.ScheduleStartUTC != "2026-11-13T12:00:00Z" || after.Data.ScheduleEndUTC != "2026-11-14T00:00:00Z" {
		t.Fatalf("时段没有按北京时间归一化成 UTC: %+v", after.Data)
	}

	// 只改名字不能把时段顺手清空
	renamed := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, map[string]any{"Name": "night-2"})
	if renamed.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", renamed.Code, renamed.Body.String())
	}
	var renamedPayload providerScheduleResponse
	if err := json.Unmarshal(renamed.Body.Bytes(), &renamedPayload); err != nil {
		t.Fatal(err)
	}
	if renamedPayload.Data.ScheduleMode != "range" || renamedPayload.Data.ScheduleEndUTC != "2026-11-14T00:00:00Z" {
		t.Fatalf("只改名字却清掉了时段: %+v", renamedPayload.Data)
	}

	// 回到不限
	cleared := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, map[string]any{"ScheduleMode": "always"})
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", cleared.Code, cleared.Body.String())
	}
	var clearedPayload providerScheduleResponse
	if err := json.Unmarshal(cleared.Body.Bytes(), &clearedPayload); err != nil {
		t.Fatal(err)
	}
	if clearedPayload.Data.ScheduleMode != "always" || !clearedPayload.Data.ScheduleActive {
		t.Fatalf("不限之后应立刻生效: %+v", clearedPayload.Data)
	}
}

// 非法时段必须 400 且带可照做的原因，不能让运维面对一句 provider_invalid。
func TestAdminProviderScheduleRejectsInvalidInput(t *testing.T) {
	handler, adminKey := adminFixture(t)
	created := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey, map[string]any{
		"name": "p", "base_url": "https://p.example", "api_key": "k",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var payload providerScheduleResponse
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"end-before-start", map[string]any{"ScheduleMode": "range", "ScheduleStart": "2026-11-14T08:00", "ScheduleEnd": "2026-11-13T20:00"}, "晚于开始"},
		{"bad-time", map[string]any{"ScheduleMode": "range", "ScheduleStart": "13号晚上"}, "格式不对"},
		{"bad-clock", map[string]any{"ScheduleMode": "daily", "ScheduleDailyStart": "25:00"}, "超出范围"},
		{"bad-mode", map[string]any{"ScheduleMode": "weekends"}, "不支持的生效方式"},
	}
	for _, tc := range cases {
		resp := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, tc.body)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d: %s", tc.name, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), tc.want) {
			t.Fatalf("%s: 响应缺少原因 %q: %s", tc.name, tc.want, resp.Body.String())
		}
	}

	// 被拒之后渠道上的时段必须还是「不限」
	after := adminRequest(t, handler, http.MethodGet, "/admin/api/providers", adminKey, nil)
	if !strings.Contains(after.Body.String(), `"ScheduleMode":"always"`) {
		t.Fatalf("被拒的更新污染了时段: %s", after.Body.String())
	}
}

// v25：多段时段的接口形态。[{start,end}] 提交、原样读回、清空要报错。
func TestAdminProviderScheduleMultipleWindows(t *testing.T) {
	handler, adminKey := adminFixture(t)

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey, map[string]any{
		"name": "night", "base_url": "https://night.example", "api_key": "k",
		"schedule_mode": "daily",
		"ScheduleWindows": []map[string]any{
			{"start": "22:00", "end": "08:00", "weekdays": []int{1, 2, 3, 4, 5}},
			{"start": "12:00", "end": "13:00"},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var payload struct {
		Data struct {
			ID              int64                  `json:"ID"`
			ScheduleMode    string                 `json:"ScheduleMode"`
			ScheduleWindows []store.ScheduleWindow `json:"ScheduleWindows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.ScheduleMode != "daily" || len(payload.Data.ScheduleWindows) != 2 {
		t.Fatalf("创建后的时段不对: %+v", payload.Data)
	}
	// 排序后第一段是 12:00-13:00。
	if payload.Data.ScheduleWindows[0] != (store.ScheduleWindow{StartMinute: 12 * 60, EndMinute: 13 * 60, Weekdays: 0x7f}) {
		t.Fatalf("windows[0] = %+v", payload.Data.ScheduleWindows[0])
	}
	// 每段的星期各自独立：第一段（12:00-13:00）没指定 = 每天，第二段是工作日。
	if payload.Data.ScheduleWindows[1].Weekdays != 1<<0|1<<1|1<<2|1<<3|1<<4 {
		t.Fatalf("windows[1] = %+v", payload.Data.ScheduleWindows[1])
	}

	// 整体替换成三段（前端保存走的正是这条 PUT）。
	updated := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, map[string]any{
		"ScheduleMode": "daily", "ScheduleWeekdays": []int{1, 2, 3, 4, 5, 6, 7},
		"ScheduleWindows": []map[string]any{
			{"start": "00:00", "end": "06:00", "weekdays": []int{1, 2, 3, 4, 5}},
			{"start": "13:00", "end": "14:00", "weekdays": []int{6, 7}},
			{"start": "20:00", "end": "23:30"},
		},
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", updated.Code, updated.Body.String())
	}
	var reread struct {
		Data struct {
			ScheduleWindows []store.ScheduleWindow `json:"ScheduleWindows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &reread); err != nil {
		t.Fatal(err)
	}
	if len(reread.Data.ScheduleWindows) != 3 {
		t.Fatalf("更新后 windows = %+v", reread.Data.ScheduleWindows)
	}
	if reread.Data.ScheduleWindows[2] != (store.ScheduleWindow{StartMinute: 20 * 60, EndMinute: 23*60 + 30, Weekdays: 0x7f}) {
		t.Fatalf("windows[2] = %+v", reread.Data.ScheduleWindows[2])
	}
	if reread.Data.ScheduleWindows[1].Weekdays != 1<<5|1<<6 {
		t.Fatalf("周六周日段 = %+v", reread.Data.ScheduleWindows[1])
	}

	// 清空所有段：拒绝，并提示改选「不限」——静默变成全天会让时段保护凭空消失。
	empty := adminRequest(t, handler, http.MethodPut, "/admin/api/providers/"+itoa(payload.Data.ID), adminKey, map[string]any{
		"ScheduleMode": "daily", "ScheduleWindows": []map[string]string{},
	})
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), "至少") {
		t.Fatalf("清空时段 = %d: %s", empty.Code, empty.Body.String())
	}
}
