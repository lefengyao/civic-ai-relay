package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

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
	m, err := repo.CreateModel(context.Background(), store.NewModel{ProviderID: p.ID, PublicName: "public-model", UpstreamName: "upstream-model", InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	g, err := repo.CreateModelGroup(context.Background(), store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupModels(context.Background(), g.ID, []int64{m.ID}); err != nil {
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
