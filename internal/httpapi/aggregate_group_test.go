package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

type groupPayload struct {
	Data []struct {
		ID        int64   `json:"ID"`
		Name      string  `json:"Name"`
		Kind      string  `json:"Kind"`
		MemberIDs []int64 `json:"MemberIDs"`
		Members   []struct {
			ID   int64  `json:"ID"`
			Name string `json:"Name"`
		} `json:"Members"`
	} `json:"data"`
}

// 创建聚合组时用 snake_case（与其它创建接口一致），成员关系要能原样读回；
// 列表接口必须带上 Kind 与 Members，否则前端无法区分两类组。
func TestAdminAggregateGroupCreateAndList(t *testing.T) {
	handler, adminKey := adminFixture(t)

	member := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{"name": "成员组"})
	if member.Code != http.StatusCreated {
		t.Fatalf("member create = %d: %s", member.Code, member.Body.String())
	}
	var memberBody struct {
		Data struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(member.Body.Bytes(), &memberBody); err != nil {
		t.Fatal(err)
	}

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "聚合组", "kind": "aggregate", "rate": 1.5, "group_ids": []int64{memberBody.Data.ID},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("aggregate create = %d: %s", created.Code, created.Body.String())
	}
	var createdBody struct {
		Data struct {
			ID   int64  `json:"ID"`
			Kind string `json:"Kind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}
	if createdBody.Data.Kind != "aggregate" {
		t.Fatalf("kind = %q", createdBody.Data.Kind)
	}

	listed := adminRequest(t, handler, http.MethodGet, "/admin/api/groups", adminKey, nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", listed.Code, listed.Body.String())
	}
	var list groupPayload
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, g := range list.Data {
		if g.ID != createdBody.Data.ID {
			continue
		}
		found = true
		if g.Kind != "aggregate" || len(g.Members) != 1 || g.Members[0].ID != memberBody.Data.ID {
			t.Fatalf("聚合组列表字段不对：%+v", g)
		}
	}
	if !found {
		t.Fatal("列表里找不到刚建的聚合组")
	}

	// 普通组带成员必须被拒，且不能留下半成品。
	bad := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "错误组", "group_ids": []int64{memberBody.Data.ID},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("普通组带成员 = %d，期望 400: %s", bad.Code, bad.Body.String())
	}
}

// 成员关系可以单独覆盖写；把聚合组塞进去必须 400（只允许一层）。
func TestAdminAggregateGroupMembersPut(t *testing.T) {
	handler, adminKey := adminFixture(t)

	newGroup := func(name, kind string) int64 {
		body := map[string]any{"name": name}
		if kind != "" {
			body["kind"] = kind
		}
		resp := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, body)
		if resp.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", name, resp.Code, resp.Body.String())
		}
		var out struct {
			Data struct {
				ID int64 `json:"ID"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data.ID
	}
	plain := newGroup("普通", "")
	inner := newGroup("内层聚合", "aggregate")
	outer := newGroup("外层聚合", "aggregate")

	resp := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(outer)+"/members", adminKey, map[string]any{"group_ids": []int64{plain}})
	if resp.Code != http.StatusOK {
		t.Fatalf("put members = %d: %s", resp.Code, resp.Body.String())
	}
	read := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/"+itoa(outer)+"/members", adminKey, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("get members = %d: %s", read.Code, read.Body.String())
	}
	var membersBody struct {
		Data []struct {
			ID int64 `json:"ID"`
		} `json:"data"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &membersBody); err != nil {
		t.Fatal(err)
	}
	if membersBody.Kind != "aggregate" || len(membersBody.Data) != 1 || membersBody.Data[0].ID != plain {
		t.Fatalf("成员读回不对：%+v", membersBody)
	}

	nested := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+itoa(outer)+"/members", adminKey, map[string]any{"group_ids": []int64{inner}})
	if nested.Code != http.StatusBadRequest {
		t.Fatalf("嵌套聚合 = %d，期望 400: %s", nested.Code, nested.Body.String())
	}
	// 失败的写入不能破坏已有成员关系。
	again := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/"+itoa(outer)+"/members", adminKey, nil)
	var after struct {
		Data []struct {
			ID int64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(again.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Data) != 1 || after.Data[0].ID != plain {
		t.Fatalf("失败写入后成员关系被破坏：%+v", after.Data)
	}
}
