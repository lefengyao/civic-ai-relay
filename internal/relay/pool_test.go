package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

type poolFakeClient struct {
	calls      int
	models     []string
	chatErr    error
	streamErr  error
	streamBody string
}

func (c *poolFakeClient) ChatJSON(_ context.Context, payload map[string]any) ([]byte, error) {
	c.calls++
	c.models = append(c.models, fmt.Sprint(payload["model"]))
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	return []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`), nil
}

func (c *poolFakeClient) Stream(_ context.Context, payload map[string]any) (*http.Response, error) {
	c.calls++
	c.models = append(c.models, fmt.Sprint(payload["model"]))
	if c.streamErr != nil {
		return nil, c.streamErr
	}
	body := c.streamBody
	if body == "" {
		body = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

type poolFactory struct{ clients map[int64]UpstreamClient }

func (f poolFactory) ForProvider(_ context.Context, id int64) (UpstreamClient, error) {
	client, ok := f.clients[id]
	if !ok {
		return nil, errors.New("unknown provider")
	}
	return client, nil
}

type poolIDs struct {
	providerA, providerB, modelA, modelB, groupID int64
}

func poolRelayFixture(t *testing.T, clients map[int64]UpstreamClient) (*Service, *store.Store, store.ClientKey, poolIDs) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 7)
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
	ctx := context.Background()
	ids := poolIDs{}
	for i, name := range []string{"provider-a", "provider-b"} {
		provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: name, BaseURL: "https://" + name + ".example", APIKey: "fake"})
		if err != nil {
			t.Fatal(err)
		}
		price := int64(1000 * (i + 1))
		model, err := repo.CreateModel(ctx, store.NewModel{
			ProviderID: provider.ID, PublicName: "model-" + string(rune('a'+i)),
			UpstreamName:        "upstream-" + string(rune('a'+i)),
			InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			ids.providerA, ids.modelA = provider.ID, model.ID
		} else {
			ids.providerB, ids.modelB = provider.ID, model.ID
		}
	}
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	ids.groupID = group.ID
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{ids.providerA, ids.providerB}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateModelPool(ctx, store.NewModelPool{Name: "pool-model", Members: []store.PoolMemberInput{
		{ModelID: ids.modelA, Enabled: true}, {ModelID: ids.modelB, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{GlobalConcurrencyLimit: 4, RPMLimit: 30, MaxOutputTokens: 64}
	service := NewService(repo, poolFactory{clients: clients}, func() config.Settings { return settings })
	return service, repo, key, ids
}

func TestOrderPoolCandidates(t *testing.T) {
	now := time.Now()
	models := []store.Model{{ID: 1}, {ID: 2}, {ID: 3}}
	ordered := orderPoolCandidates(models, map[int64]time.Time{1: now.Add(30 * time.Second)}, now)
	if ordered[0].ID != 2 || ordered[1].ID != 3 || ordered[2].ID != 1 {
		t.Fatalf("cooling member must be deferred, got %v", ordered)
	}
	allCooled := orderPoolCandidates(models, map[int64]time.Time{
		1: now.Add(time.Second), 2: now.Add(time.Second), 3: now.Add(time.Second),
	}, now)
	for i, m := range allCooled {
		if m.ID != int64(i+1) {
			t.Fatalf("all-cooled must keep configured order, got %v", allCooled)
		}
	}
	expired := orderPoolCandidates(models, map[int64]time.Time{1: now.Add(-time.Second)}, now)
	if expired[0].ID != 1 {
		t.Fatalf("expired cooldown must be ignored, got %v", expired)
	}
}

func TestRetryablePoolFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"503", &upstream.Error{Code: "upstream_unavailable", Status: 503}, true},
		{"429", &upstream.Error{Code: "upstream_rate_limited", Status: 429}, true},
		{"401", &upstream.Error{Code: "upstream_authentication_failed", Status: 401}, true},
		{"404", &upstream.Error{Code: "upstream_http_error", Status: 404}, true},
		{"connection", &upstream.Error{Code: "upstream_connection_failed"}, true},
		{"invalid body", &upstream.Error{Code: "upstream_response_invalid", Status: 200}, true},
		{"400", &upstream.Error{Code: "upstream_request_rejected", Status: 400}, false},
		{"422", &upstream.Error{Code: "upstream_request_rejected", Status: 422}, false},
		{"canceled", &upstream.Error{Code: "client_canceled"}, false},
		{"duration", &upstream.Error{Code: "request_duration_exceeded"}, false},
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"context", context.Canceled, false},
	}
	for _, tc := range cases {
		if got := retryablePoolFailure(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func latestRequest(t *testing.T, repo *store.Store) map[string]any {
	t.Helper()
	requests, err := repo.RecentRequests(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatal("no request recorded")
	}
	return requests[0]
}

func TestPoolFailoverSwitchesMemberAndBillsWinner(t *testing.T) {
	first := &poolFakeClient{chatErr: &upstream.Error{Code: "upstream_unavailable", Status: 503}}
	second := &poolFakeClient{}
	service, repo, key, ids := poolRelayFixture(t, nil)
	// clients 必须在 fixture 建好 provider ID 之后注入（ID 由数据库分配）。
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}

	data, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8})
	if err != nil {
		t.Fatalf("failover should succeed: %v", err)
	}
	if !strings.Contains(string(data), "usage") {
		t.Fatalf("unexpected response: %s", data)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("want one call each, got first=%d second=%d", first.calls, second.calls)
	}
	if len(second.models) != 1 || second.models[0] != "upstream-b" {
		t.Fatalf("winner must use its own upstream model name: %v", second.models)
	}
	row := latestRequest(t, repo)
	if row["model_id"] != ids.modelB || row["provider_id"] != ids.providerB {
		t.Fatalf("ledger must point at the successful member: %+v", row)
	}
	if row["status"] != "completed" {
		t.Fatalf("status = %v", row["status"])
	}
	// model-b 单价 2000 微元/百万 token：100 in + 50 out = 2 微元。
	// 若错按首选成员（1000）计价会得到 1 —— 这条断言钉住「按实际成员计费」。
	if row["amount_microyuan"] != int64(2) {
		t.Fatalf("amount = %v, want 2 (winner's price)", row["amount_microyuan"])
	}
	models, err := service.Models(context.Background(), key.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		if m.PublicName == "pool-model" && m.PoolID > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("/v1/models must advertise the pool name")
	}
}

func TestPoolAllMembersFail(t *testing.T) {
	first := &poolFakeClient{chatErr: &upstream.Error{Code: "upstream_unavailable", Status: 503}}
	second := &poolFakeClient{chatErr: &upstream.Error{Code: "upstream_unavailable", Status: 502}}
	service, repo, key, ids := poolRelayFixture(t, nil)
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}
	if _, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8}); err == nil {
		t.Fatal("all members failing must return an error")
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("each member should be tried once, got %d/%d", first.calls, second.calls)
	}
	row := latestRequest(t, repo)
	if row["status"] != "failed" || row["error_code"] != "upstream_unavailable" {
		t.Fatalf("unexpected failure record: %+v", row)
	}
	if row["model_id"] != ids.modelB {
		t.Fatalf("ledger should name the last attempted member, got %+v", row)
	}
}

func TestPoolCooldownPrefersHealthyMember(t *testing.T) {
	first := &poolFakeClient{chatErr: &upstream.Error{Code: "upstream_unavailable", Status: 503}}
	second := &poolFakeClient{}
	service, _, key, ids := poolRelayFixture(t, nil)
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}
	for i := 0; i < 2; i++ {
		if _, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if first.calls != 1 || second.calls != 2 {
		t.Fatalf("cooldown should skip the failed member on the next request, got %d/%d", first.calls, second.calls)
	}
}

func TestPoolDoesNotSwitchOnBadRequest(t *testing.T) {
	first := &poolFakeClient{chatErr: &upstream.Error{Code: "upstream_request_rejected", Status: 400}}
	second := &poolFakeClient{}
	service, _, key, ids := poolRelayFixture(t, nil)
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}
	if _, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8}); err == nil {
		t.Fatal("400 must fail")
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("400 must not trigger failover, got %d/%d", first.calls, second.calls)
	}
}

func TestPoolRespectsGroupScope(t *testing.T) {
	first, second := &poolFakeClient{}, &poolFakeClient{}
	service, repo, key, ids := poolRelayFixture(t, nil)
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}
	// 分组只留 providerA：池里的 modelB 不在授权集合里，绝不能被打到。
	if err := repo.ReplaceGroupProviders(context.Background(), ids.groupID, []int64{ids.providerA}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Chat(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8}); err != nil {
		t.Fatal(err)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("unauthorized member was called: %d/%d", first.calls, second.calls)
	}
}

func TestPoolStreamSwitchesBeforeHeader(t *testing.T) {
	first := &poolFakeClient{streamErr: &upstream.Error{Code: "upstream_unavailable", Status: 503}}
	second := &poolFakeClient{}
	service, _, key, ids := poolRelayFixture(t, nil)
	service.clients = poolFactory{clients: map[int64]UpstreamClient{ids.providerA: first, ids.providerB: second}}
	prepared, err := service.PrepareStream(context.Background(), Request{Token: key.Token, Model: "pool-model", MaxTokens: 8, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Open()
	if err != nil {
		t.Fatalf("stream failover should succeed: %v", err)
	}
	defer response.Body.Close()
	defer prepared.Cancel()
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("want one stream attempt each, got %d/%d", first.calls, second.calls)
	}
	if got := prepared.CurrentModel().ID; got != ids.modelB {
		t.Fatalf("CurrentModel must be the winning member, got %d want %d", got, ids.modelB)
	}
	if err := prepared.Lease.Close(context.Background(), Outcome{Status: "completed", HTTPStatus: http.StatusOK, UsageSource: "upstream", AmountMicroyuan: 2, InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Fatal(err)
	}
}
