package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

// TestRequestContextAppliesMaxStreamDuration 钉住 MAX_STREAM_DURATION 真的作用到上游请求上
// —— 它曾经只是个给后台回收循环看的数字，管理台里配了也不生效（线上长对话被上游客户端的
// 340 秒总超时掐断，而这项配的是 30 分钟）。同时守住「0 = 不限」的口径：
// 若对 0 也套 WithTimeout，会立刻超时，等于把所有请求打死。
func TestRequestContextAppliesMaxStreamDuration(t *testing.T) {
	service := &Service{settings: func() config.Settings { return config.Settings{MaxStreamDuration: 90 * time.Second} }}
	ctx, cancel := service.requestContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("MAX_STREAM_DURATION > 0 时应当给上游请求设截止时间")
	}
	if remaining := time.Until(deadline); remaining > 90*time.Second || remaining < 80*time.Second {
		t.Fatalf("截止时间与配置不符：剩余 %v", remaining)
	}

	unlimited := &Service{settings: func() config.Settings { return config.Settings{} }}
	openCtx, openCancel := unlimited.requestContext(context.Background())
	defer openCancel()
	if _, ok := openCtx.Deadline(); ok {
		t.Fatal("MAX_STREAM_DURATION = 0 表示不限，不应设置截止时间（否则会立即超时）")
	}
}

type fakeClient struct {
	chatErr error
	calls   int
}

func (f *fakeClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	f.calls++
	return []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), f.chatErr
}
func (f *fakeClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	f.calls++
	return nil, f.chatErr
}

type fakeFactory struct{ client *fakeClient }

func (f *fakeFactory) ForProvider(context.Context, int64) (UpstreamClient, error) {
	return f.client, nil
}

func serviceFixture(t *testing.T, client *fakeClient) (*Service, *store.Store, store.ClientKey) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	box, err := secret.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	p, err := repo.CreateProvider(context.Background(), store.NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000)
	if _, err := repo.CreateModel(context.Background(), store.NewModel{ProviderID: p.ID, PublicName: "public-model", UpstreamName: "upstream-model", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	g, err := repo.CreateModelGroup(context.Background(), store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(context.Background(), g.ID, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	k, err := repo.CreateClientKey(context.Background(), store.NewClientKey{Name: "k", ConcurrencyLimit: 1, TokenLimit: ptr(1000), AmountLimitMicroyuan: ptr(100000)})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(context.Background(), k.ID, []int64{g.ID}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{GlobalConcurrencyLimit: 2, RPMLimit: 30, TokenLimit5H: 100000, TokenLimitDaily: 20000, MaxOutputTokens: 32}
	service := NewService(repo, &fakeFactory{client: client}, func() config.Settings { return settings })
	return service, repo, k
}

func ptr(v int64) *int64 { return &v }

func TestServiceRejectsUnauthorizedModelBeforeUpstream(t *testing.T) {
	client := &fakeClient{}
	service, _, key := serviceFixture(t, client)
	_, _, err := service.Begin(context.Background(), Request{Token: key.Token, Model: "forbidden"})
	if !errors.Is(err, ErrModelNotAllowed) || client.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, client.calls)
	}
}

func TestServiceReleasesBothGatesWhenUpstreamFails(t *testing.T) {
	client := &fakeClient{chatErr: errors.New("upstream down")}
	service, _, key := serviceFixture(t, client)
	_, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "public-model"})
	if err == nil || service.ActiveGlobal() != 0 || service.ActiveForKey(key.Token) != 0 {
		t.Fatalf("err=%v global=%d key=%d", err, service.ActiveGlobal(), service.ActiveForKey(key.Token))
	}
}

func TestLeaseCloseIsIdempotent(t *testing.T) {
	client := &fakeClient{}
	service, _, key := serviceFixture(t, client)
	lease, _, err := service.Begin(context.Background(), Request{Token: key.Token, Model: "public-model", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background(), Outcome{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background(), Outcome{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	if service.ActiveGlobal() != 0 || service.ActiveForKey(key.Token) != 0 {
		t.Fatal("leases leaked")
	}
}

func TestBeginAppliesGroupRateMultiplier(t *testing.T) {
	client := &fakeClient{}
	service, repo, key := serviceFixture(t, client)
	ctx := context.Background()
	groups, err := repo.ListModelGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateModelGroup(ctx, groups[0].ID, store.UpdateModelGroup{RateMilli: &[]int64{2500}[0]}); err != nil {
		t.Fatal(err)
	}
	lease, model, err := service.Begin(ctx, Request{Token: key.Token, Model: "public-model", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close(ctx, Outcome{Status: "aborted"}) })
	// 模型单价 1000 microyuan/M，分组倍率 2.5x → 2500
	if model.InputPriceMicroyuan == nil || *model.InputPriceMicroyuan != 2500 {
		t.Fatalf("input price = %v", model.InputPriceMicroyuan)
	}
	if model.OutputPriceMicroyuan == nil || *model.OutputPriceMicroyuan != 2500 {
		t.Fatalf("output price = %v", model.OutputPriceMicroyuan)
	}
	// 数据库中的原始定价不被改写
	stored, err := repo.GetModel(ctx, model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *stored.InputPriceMicroyuan != 1000 {
		t.Fatalf("stored price mutated: %d", *stored.InputPriceMicroyuan)
	}
}
