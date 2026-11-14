package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"civic-ai-relay/internal/store"
)

// 池是模型级成员（v27）：组带 pool_ids 创建后只能查到池，渠道列表必须为空。
// 这条路径要是悄悄写进了 group_providers，就等于又把整条渠道授权出去了。
func TestAdminGroupPoolsCRUD(t *testing.T) {
	handler, repo, adminKey := poolAdminFixture(t)
	modelID := poolTestModel(t, repo)
	createdPool := adminRequest(t, handler, http.MethodPost, "/admin/api/pools", adminKey, map[string]any{
		"name": "pool-x", "members": []map[string]any{{"model_id": modelID, "enabled": true}},
	})
	if createdPool.Code != http.StatusCreated {
		t.Fatalf("create pool status=%d body=%s", createdPool.Code, createdPool.Body.String())
	}
	poolID := poolIDFromResponse(t, createdPool.Body.String())

	created := adminRequest(t, handler, http.MethodPost, "/admin/api/groups", adminKey, map[string]any{
		"name": "agg", "kind": "aggregate", "pool_ids": []int64{poolID},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group status=%d body=%s", created.Code, created.Body.String())
	}
	var payload struct {
		Data struct {
			ID float64 `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	groupID := int64(payload.Data.ID)
	path := "/admin/api/groups/" + strconv.FormatInt(groupID, 10)

	pools := adminRequest(t, handler, http.MethodGet, path+"/pools", adminKey, nil)
	if pools.Code != http.StatusOK || !strings.Contains(pools.Body.String(), strconv.FormatInt(poolID, 10)) {
		t.Fatalf("get pools status=%d body=%s", pools.Code, pools.Body.String())
	}
	providers := adminRequest(t, handler, http.MethodGet, path+"/providers", adminKey, nil)
	if providers.Code != http.StatusOK || strings.TrimSpace(providers.Body.String()) != `{"data":[]}` {
		t.Fatalf("池写入不该产生渠道成员，providers=%s", providers.Body.String())
	}
	list := adminRequest(t, handler, http.MethodGet, "/admin/api/groups", adminKey, nil)
	if !strings.Contains(list.Body.String(), `"PoolIDs":[`+strconv.FormatInt(poolID, 10)+`]`) {
		t.Fatalf("组列表没有带回 PoolIDs：%s", list.Body.String())
	}

	// 清空：允许组退化成只靠渠道/成员组，但组本身还在。
	cleared := adminRequest(t, handler, http.MethodPut, path+"/pools", adminKey, map[string]any{"pool_ids": []int64{}})
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear pools status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	again := adminRequest(t, handler, http.MethodGet, path+"/pools", adminKey, nil)
	if strings.TrimSpace(again.Body.String()) != `{"data":[]}` {
		t.Fatalf("清空后仍能读到池：%s", again.Body.String())
	}

	// 成员组不能挂池：它不是对外入口，挂上去没有任何意义。
	member, err := repo.CreateModelGroup(context.Background(), store.NewModelGroup{Name: "member", Kind: store.GroupKindNormal})
	if err != nil {
		t.Fatal(err)
	}
	bad := adminRequest(t, handler, http.MethodPut, "/admin/api/groups/"+strconv.FormatInt(member.ID, 10)+"/pools", adminKey,
		map[string]any{"pool_ids": []int64{poolID}})
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "pool_invalid") {
		t.Fatalf("成员组挂池 status=%d body=%s", bad.Code, bad.Body.String())
	}

	// 不存在的组 → 404，而不是 SQL 原文。
	missing := adminRequest(t, handler, http.MethodGet, "/admin/api/groups/99999/pools", adminKey, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing group status=%d body=%s", missing.Code, missing.Body.String())
	}
}
