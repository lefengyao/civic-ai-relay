package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// v24：一个渠道只能属于一个组。管理端两条写入路径（建组即带渠道 / 单独改渠道）
// 都必须返回可读的 provider_in_use，而不是笼统的 group_invalid——后者会被当成
// 「名字或额度没填对」，运维会一直改错地方。
func TestAdminGroupRejectsProviderAlreadyGrouped(t *testing.T) {
	handler, adminKey := adminFixture(t)

	prov := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey, map[string]any{
		"name": "up1", "base_url": "https://up1.example", "api_key": "sk-test",
	})
	if prov.Code != http.StatusCreated {
		t.Fatalf("create provider = %d: %s", prov.Code, prov.Body.String())
	}
	var provBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(prov.Body.Bytes(), &provBody); err != nil {
		t.Fatal(err)
	}
	providerID := provBody.Data.ID
	if providerID == 0 {
		t.Fatalf("provider id = 0: %s", prov.Body.String())
	}

	first := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "夜间池", "kind": "normal", "provider_ids": []int64{providerID},
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create first group = %d: %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}

	// 建组时直接带这条已归属的渠道：拒绝，并把「已属于哪个组」写清楚。
	second := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "白班池", "kind": "normal", "provider_ids": []int64{providerID},
	})
	if second.Code != http.StatusBadRequest || !strings.Contains(second.Body.String(), "provider_in_use") {
		t.Fatalf("second group = %d: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "夜间池") {
		t.Fatalf("冲突文案没有指出原分组：%s", second.Body.String())
	}
	// 失败必须等于什么都没发生：不能让「白班池」留在列表里（它的渠道是空的）。
	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/groups", adminKey, nil)
	if strings.Contains(listed.Body.String(), "白班池") {
		t.Fatalf("冲突的组被建出来了：%s", listed.Body.String())
	}

	// 另一条路径：先建空组，再单独改渠道（管理台编辑弹窗走的就是这条）。
	empty := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{"name": "白班池", "kind": "normal"})
	if empty.Code != http.StatusCreated {
		t.Fatalf("create empty group = %d: %s", empty.Code, empty.Body.String())
	}
	var emptyBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &emptyBody); err != nil {
		t.Fatal(err)
	}
	put := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(emptyBody.Data.ID)+"/providers", adminKey, map[string]any{
		"provider_ids": []int64{providerID},
	})
	if put.Code != http.StatusBadRequest || !strings.Contains(put.Body.String(), "provider_in_use") {
		t.Fatalf("put providers = %d: %s", put.Code, put.Body.String())
	}
	if own := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/"+itoa(emptyBody.Data.ID)+"/providers", adminKey, nil); !strings.Contains(own.Body.String(), "[]") {
		t.Fatalf("被拒之后空组仍拿到了渠道：%s", own.Body.String())
	}

	// 同一组重复保存是幂等的（替换写先删后插），不能被自己的旧归属挡下来。
	same := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(firstBody.Data.ID)+"/providers", adminKey, map[string]any{
		"provider_ids": []int64{providerID},
	})
	if same.Code != http.StatusOK {
		t.Fatalf("同组重复保存 = %d: %s", same.Code, same.Body.String())
	}
}

// 渠道列表要带出归属组：管理台靠它显示「归哪个组」、并在组编辑里把别组的渠道灰掉。
func TestAdminProvidersListCarriesGroupName(t *testing.T) {
	handler, adminKey := adminFixture(t)
	prov := adminRequest(t, handler, http.MethodPost, "/admin/api/providers", adminKey, map[string]any{
		"name": "up1", "base_url": "https://up1.example", "api_key": "sk-test",
	})
	if prov.Code != http.StatusCreated {
		t.Fatalf("create provider = %d: %s", prov.Code, prov.Body.String())
	}
	var provBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(prov.Body.Bytes(), &provBody); err != nil {
		t.Fatal(err)
	}
	if created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "夜间池", "kind": "normal", "provider_ids": []int64{provBody.Data.ID},
	}); created.Code != http.StatusCreated {
		t.Fatalf("create group = %d: %s", created.Code, created.Body.String())
	}
	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/providers", adminKey, nil)
	if !strings.Contains(listed.Body.String(), `"GroupName":"夜间池"`) {
		t.Fatalf("渠道列表缺少归属组：%s", listed.Body.String())
	}
}

// 对不存在的组改渠道必须是 404，而不是 400 + "group_invalid: sql: no rows in
// result set"。后者既没说清是哪个组，也让调用方以为是自己传的参数有问题。
func TestAdminGroupProvidersOnMissingGroupReturns404(t *testing.T) {
	handler, adminKey := adminFixture(t)
	cases := []struct {
		method string
		body   map[string]any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"provider_ids": []int64{}}},
	}
	for _, c := range cases {
		resp := adminRequest(t, handler, c.method, "/admin/api/groups/9999/providers", adminKey, c.body)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s 不存在的组 = %d: %s", c.method, resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), "no rows in result set") {
			t.Fatalf("%s 把 SQLite 原文抛给了运维：%s", c.method, resp.Body.String())
		}
	}
}
