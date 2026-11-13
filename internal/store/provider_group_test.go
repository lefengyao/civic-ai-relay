package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// v24：一个渠道只能属于一个组。
//
// 这条规则的意义不只是「好看」：渠道是「上游地址 + 凭据」的实体，挂到两个组就
// 意味着同一个上游账号受两份额度约束，而一笔请求的账目只能记到一个组上，于是
// 必然有一边永远显示 0 用量——最隐蔽的那种「某个组看起来从没被用过」。
func TestProviderBelongsToOnlyOneGroup(t *testing.T) {
	ctx := context.Background()
	repo, providerID, _ := setupGroupScope(t)
	a, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, a.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	// 同一组再次保存是幂等的：替换写是先删后插，不能被自己的旧行挡下来。
	if err := repo.ReplaceGroupProviders(ctx, a.ID, []int64{providerID}); err != nil {
		t.Fatalf("同组重复保存应幂等：%v", err)
	}

	err = repo.ReplaceGroupProviders(ctx, b.ID, []int64{providerID})
	var conflict *ProviderGroupConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *ProviderGroupConflict", err)
	}
	if !errors.Is(err, ErrProviderGrouped) {
		t.Fatalf("errors.Is(ErrProviderGrouped) = false: %v", err)
	}
	// 文案要能直接给运维看：哪条渠道、已经在哪个组。
	if conflict.GroupID != a.ID || conflict.GroupName != "a" || conflict.ProviderName == "" {
		t.Fatalf("conflict = %#v", conflict)
	}
	if !strings.Contains(err.Error(), "只能属于一个组") || !strings.Contains(err.Error(), "「a」") {
		t.Fatalf("message = %q", err.Error())
	}

	// 被拒之后不能留下半成品：b 仍然是 0 个渠道，a 仍然持有它。
	if own, err := repo.GroupProviderIDs(ctx, b.ID); err != nil || len(own) != 0 {
		t.Fatalf("b 组渠道 = %v, %v; want 空", own, err)
	}
	if own, err := repo.GroupProviderIDs(ctx, a.ID); err != nil || len(own) != 1 || own[0] != providerID {
		t.Fatalf("a 组渠道 = %v, %v; want [%d]", own, err, providerID)
	}

	// 应用层之外也要挡住：唯一索引是最后一道墙（手工 SQL、旧脚本都绕不过）。
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO group_providers(group_id,provider_id) VALUES (?,?)`, b.ID, providerID); err == nil {
		t.Fatal("唯一索引未生效：直接插入重复归属成功了")
	}
}

// ListProviders 要带出归属组：管理台靠它显示「已归哪个组」，组编辑的勾选列表
// 靠它把已经在别组的渠道灰掉。字段空着会把所有渠道显示成「未加入分组」。
func TestListProvidersCarriesGroupOwnership(t *testing.T) {
	ctx := context.Background()
	repo, providerID, _ := setupGroupScope(t)
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}

	before, err := repo.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].GroupID != 0 || before[0].GroupName != "" {
		t.Fatalf("入组前 = %#v, want 未归属", before)
	}

	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{providerID}); err != nil {
		t.Fatal(err)
	}
	after, err := repo.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].GroupID != group.ID || after[0].GroupName != "g" {
		t.Fatalf("入组后 = %#v, want #%d", after, group.ID)
	}

	// 移出组后归属要跟着清空，否则界面会一直显示一个已经不成立的归属。
	if err := repo.ReplaceGroupProviders(ctx, group.ID, nil); err != nil {
		t.Fatal(err)
	}
	cleared, err := repo.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0].GroupID != 0 {
		t.Fatalf("移出后 = %#v, want 未归属", cleared)
	}
}
