package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

func TestNewServerUsesProvidedAddress(t *testing.T) {
	server := newServer("127.0.0.1:8000", nil)
	if server.Addr != "127.0.0.1:8000" {
		t.Fatalf("server address = %q, want %q", server.Addr, "127.0.0.1:8000")
	}
}

func TestTrackedHandlerRejectsNewPublicRequestWithSettlementCodeWhileDraining(t *testing.T) {
	handler := newTrackedHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("draining handler forwarded a new request")
	}))
	handler.drain()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("draining response is not JSON: %v", err)
	}
	if body.Error.Code != "settlement_unavailable" {
		t.Fatalf("error code = %q, want settlement_unavailable", body.Error.Code)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.env")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	content := "ADMIN_API_KEY=adm_test\nRELAY_ENCRYPTION_KEY=" + key + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHealthcheckTargetFollowsRelayEnv 确认探针端口跟随 relay.env——端口改了
// 而探针还盯着 8000 会让容器永久处于 unhealthy。
func TestHealthcheckTargetFollowsRelayEnv(t *testing.T) {
	path := writeConfig(t, "HOST=0.0.0.0\nPORT=9123\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)
	t.Setenv("PORT", "1234") // 服务端不读环境变量，环境变量不得覆盖文件

	host, port := healthcheckTarget()
	if port != "9123" {
		t.Fatalf("port = %q, want 9123 from relay.env", port)
	}
	if host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1 for a wildcard listen address", host)
	}
}

func TestHealthcheckTargetKeepsExplicitListenAddress(t *testing.T) {
	path := writeConfig(t, "HOST=10.0.0.5\nPORT=8001\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)

	host, port := healthcheckTarget()
	if host != "10.0.0.5" || port != "8001" {
		t.Fatalf("target = %s:%s, want 10.0.0.5:8001", host, port)
	}
}

func TestHealthcheckTargetFallsBackWhenConfigMissing(t *testing.T) {
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("PORT", "9999")

	host, port := healthcheckTarget()
	if host != "127.0.0.1" || port != "9999" {
		t.Fatalf("target = %s:%s, want 127.0.0.1:9999", host, port)
	}
	// 探针必须只读：配置缺失时不能顺手把它创建出来。
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("CIVIC_RELAY_CONFIG_FILE")), "relay.env")); err == nil {
		t.Fatal("healthcheck must not create the configuration file")
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	// 取一个必然无人监听的端口，确保探针在有界时间内失败而不是永久挂起。
	path := writeConfig(t, "HOST=127.0.0.1\nPORT=1\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)
	if code := runHealthcheck(); code != 1 {
		t.Fatalf("exit code = %d, want 1 when the server is not listening", code)
	}
}

func TestCloseCancelsOverdueHandlerBeforeClosingStore(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	started := make(chan struct{})
	settled := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		settled <- repo.Ping(context.Background())
	})
	server := newServer("127.0.0.1:0", handler)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	client := &http.Client{Timeout: 3 * time.Second}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, getErr := client.Get("http://" + listener.Addr().String() + "/v1/chat/completions")
		if getErr == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	app := &Application{Server: server, Store: repo}
	closeErr := app.Close(ctx)
	_ = server.Close()
	select {
	case err := <-settled:
		if err != nil {
			t.Fatalf("store closed before canceled handler finished: %v (close: %v)", err, closeErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not exit after shutdown deadline")
	}
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client request did not end")
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve error = %v, want ErrServerClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestBuildApplicationRecoversCrashReservationBeforeServing(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(databasePath, box)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	provider, err := repo.CreateProvider(ctx, store.NewProvider{Name: "fixture", BaseURL: "https://fixture.example", APIKey: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	model, err := repo.CreateModel(ctx, store.NewModel{ProviderID: provider.ID, PublicName: "m", UpstreamName: "m", Enabled: true, InputPriceMicroyuan: &price, OutputPriceMicroyuan: &price})
	if err != nil {
		t.Fatal(err)
	}
	group, err := repo.CreateModelGroup(ctx, store.NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	clientKey, err := repo.CreateClientKey(ctx, store.NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, clientKey.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	// A previous process can leave a timestamp in the startup millisecond (or
	// slightly ahead after clock correction); recovery must still clear it.
	_, err = repo.ReserveRequest(ctx, store.ReserveInput{RequestID: "crashed", KeyID: clientKey.ID, ModelID: model.ID, ProviderID: provider.ID, StartedAt: time.Now().UTC().Add(time.Second), InputText: "hi", OutputTokenCeiling: 64, MaxOutputTokens: 64, InputPriceMicroyuan: price, OutputPriceMicroyuan: price, Limits: store.QuotaLimits{RPMLimit: 30}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	settings := config.Settings{DBPath: databasePath, EncryptionKey: key, AdminAPIKey: "adm_test", GlobalConcurrencyLimit: 2, MaxOutputTokens: 64}
	app, _, err := buildApplication(settings, filepath.Join(t.TempDir(), "relay.env"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	rows, err := app.Store.RecentRequests(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" {
		t.Fatalf("request after startup = %v, want crash-recovered aborted", rows)
	}
}

func TestServeApplicationClosesResourcesWhenListenFails(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "relay.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	app := &Application{Store: repo, Server: newServer("127.0.0.1:-1", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))}
	settings := config.Settings{MaxStreamDuration: time.Minute, RetentionDays: 7}
	done := make(chan error, 1)
	go func() {
		done <- serveApplication(context.Background(), app, func() config.Settings { return settings })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected listen failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveApplication did not return after listen failure")
	}
	if err := repo.Ping(context.Background()); err == nil {
		t.Fatal("database remained open after listen failure")
	}
	select {
	case <-app.maintenanceDone:
	default:
		t.Fatal("maintenance loop remained active after listen failure")
	}
}
