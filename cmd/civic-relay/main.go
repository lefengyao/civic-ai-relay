package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/httpapi"
	"civic-ai-relay/internal/memory"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

// Build metadata. The container build injects real values with
// -ldflags "-X main.version=... -X main.commit=... -X main.buildDate=...";
// the defaults keep a plain `go run ./cmd/civic-relay` self-describing.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type Application struct {
	Handler  http.Handler
	Store    *store.Store
	Registry *relay.Registry
	Server   *http.Server
}

func buildApplication(settings config.Settings, configFilePath string) (*Application, *config.Source, error) {
	box, err := secret.New(settings.EncryptionKey)
	if err != nil {
		return nil, nil, err
	}
	database, err := store.Open(settings.DBPath, box)
	if err != nil {
		return nil, nil, err
	}
	registry := relay.NewRegistry(database, settings.ConnectTimeout, settings.ReadTimeout, settings.WriteTimeout, settings.PoolTimeout)
	// 渠道密钥轮换或地址修改后驱逐缓存的 upstream client，避免旧凭据继续生效
	database.SetProviderCacheEvictor(registry.EvictProvider)
	// 设置热源：管理端保存后立即生效，无需重启
	source := config.NewSource(settings)
	service := relay.NewService(database, registry, source.Get)
	admin := httpapi.NewAdminHandler(database, service, settings, settings.AdminAPIKey)
	admin.SetConfigPersistence(configFilePath)
	admin.SetSettingsListener(source.Set)
	// 内存软保护：RSS 超过 MEMORY_LIMIT_MB 时拒绝新公共请求并中止进行中的流
	guard := memory.NewGuard(settings.MemoryLimitMB, memory.RSS)
	handler := httpapi.NewServerWithAdmission(service, settings.MaxBodyBytes, guard.PublicAdmission, guard.StreamContinue, admin)
	return &Application{Handler: handler, Store: database, Registry: registry}, source, nil
}

func (a *Application) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if a.Server != nil {
		_ = a.Server.Shutdown(ctx)
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

// startMaintenance runs the two background loops:
//   - reaper: settles reservations from crashed requests so they stop holding
//     concurrency slots and inflating the 5-hour token window;
//   - prune: deletes ledger rows older than RETENTION_DAYS so the database
//     (and with it every window-based counter) stays bounded.
func (a *Application) startMaintenance(settings config.Settings) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	// 略长于最长流式请求，避免误杀仍在进行的正常请求
	maxAge := settings.MaxStreamDuration + 2*time.Minute
	reap := func() {
		reaped, err := a.Store.ReapStaleReservations(ctx, maxAge)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("stale reservation reaper: %v", err)
			}
			return
		}
		if reaped > 0 {
			log.Printf("reaped %d stale reservation(s) from crashed requests", reaped)
		}
	}
	reap()
	reaperTicker := time.NewTicker(5 * time.Minute)
	prune := func() {
		cutoff := time.Now().UTC().AddDate(0, 0, -settings.RetentionDays)
		removed, err := a.Store.Prune(ctx, cutoff)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("retention prune: %v", err)
			}
			return
		}
		if removed > 0 {
			log.Printf("pruned %d request(s) older than %d day(s)", removed, settings.RetentionDays)
		}
	}
	pruneTicker := time.NewTicker(24 * time.Hour)
	go func() {
		defer reaperTicker.Stop()
		defer pruneTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reaperTicker.C:
				reap()
			case <-pruneTicker.C:
				prune()
			}
		}
	}()
	return cancel
}

func main() {
	// 容器镜像基于 distroless（无 shell、无 curl），编排层的 HEALTHCHECK
	// 直接复用本二进制：`civic-relay -healthcheck`。
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck())
	}
	httpapi.SetVersion(version)
	log.Printf("civic-relay %s (commit %s, built %s)", version, commit, buildDate)

	path, err := config.DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	settings, bootstrapKey, created, err := config.Ensure(path)
	if err != nil {
		log.Fatal(err)
	}
	if created {
		// The credential itself is written only to the adjacent one-time file and
		// is intentionally never logged or included in process metadata.
		_ = bootstrapKey
		log.Printf("first start created bootstrap administrator key file: %s", filepath.Join(filepath.Dir(path), "bootstrap-admin-key.txt"))
	}
	app, _, err := buildApplication(settings, path)
	if err != nil {
		log.Fatal(err)
	}
	server := newServer(net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), app.Handler)
	app.Server = server
	stopMaintenance := app.startMaintenance(settings)

	// 优雅停机：SIGINT/SIGTERM 先排空在途请求（含 SSE 流），再关闭数据库
	shutdownDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Print("shutting down: draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = app.Close(ctx)
		close(shutdownDone)
	}()

	err = server.ListenAndServe()
	stopMaintenance()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
	log.Print("stopped")
}

// newServer keeps WriteTimeout at zero on purpose: SSE streams may stay open
// for MAX_STREAM_DURATION. ReadHeaderTimeout bounds slow-loris style header
// stalls; IdleTimeout recycles keep-alive connections.
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
