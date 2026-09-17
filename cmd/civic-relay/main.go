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
	"civic-ai-relay/internal/logging"
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
	Monitor  *relay.Monitor
	Syncer   *relay.ModelSyncer
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
	// 分组可用性监测：定时对每个分组的启用渠道打一次上游 /v1/models。
	// 注册到管理端是为了支持「立即检测」，两者共用同一个 Monitor。
	monitor := relay.NewMonitor(database, registry)
	// 自动同步上游模型（MODEL_AUTO_SYNC + MODEL_SYNC_INTERVAL）。导入的模型一律
	// 停用 + 未定价，需要运维定价并启用后才会授权给客户端。
	syncer := relay.NewModelSyncer(database, registry)
	admin := httpapi.NewAdminHandler(database, service, settings, settings.AdminAPIKey)
	admin.SetConfigPersistence(configFilePath)
	// 顺带把日志级别热应用到运行中的进程：把 LOG_LEVEL 改成 DEBUG 后不必重启
	// 就能拿到访问日志。
	admin.SetSettingsListener(func(next config.Settings) {
		source.Set(next)
		logging.Configure(next.LogLevel)
	})
	admin.SetGroupMonitor(monitor)
	// 内存软保护：RSS 超过 MEMORY_LIMIT_MB 时拒绝新公共请求并中止进行中的流
	guard := memory.NewGuard(settings.MemoryLimitMB, memory.RSS)
	handler := httpapi.NewServerWithAdmission(service, settings.MaxBodyBytes, guard.PublicAdmission, guard.StreamContinue, admin)
	return &Application{Handler: handler, Store: database, Registry: registry, Monitor: monitor, Syncer: syncer}, source, nil
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

// startMaintenance runs the four background loops:
//   - reaper: settles reservations from crashed requests so they stop holding
//     concurrency slots and inflating the 5-hour token window;
//   - prune: deletes ledger rows older than RETENTION_DAYS so the database
//     (and with it every window-based counter) stays bounded;
//   - monitor: checks whether each model group can still serve traffic;
//   - sync: imports newly published upstream models (disabled + unpriced).
//
// 接收 settings 读取函数而非快照：这几个循环都要在管理台改配置后立即跟随，
// 例如把 GROUP_MONITOR_INTERVAL 改成 0 应当立刻停掉监测。
func (a *Application) startMaintenance(current func() config.Settings) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	// 略长于最长流式请求，避免误杀仍在进行的正常请求
	reap := func() {
		maxAge := current().MaxStreamDuration + 2*time.Minute
		reaped, err := a.Store.ReapStaleReservations(ctx, maxAge)
		if err != nil {
			if ctx.Err() == nil {
				logging.Warnf("stale reservation reaper: %v", err)
			}
			return
		}
		if reaped > 0 {
			logging.Infof("reaped %d stale reservation(s) from crashed requests", reaped)
		}
	}
	reap()
	reaperTicker := time.NewTicker(5 * time.Minute)
	prune := func() {
		days := current().RetentionDays
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		removed, err := a.Store.Prune(ctx, cutoff)
		if err != nil {
			if ctx.Err() == nil {
				logging.Warnf("retention prune: %v", err)
			}
			return
		}
		if removed > 0 {
			logging.Infof("pruned %d request(s) older than %d day(s)", removed, days)
		}
		if n, err := a.Store.PruneGroupMonitorResults(ctx, cutoff); err != nil {
			if ctx.Err() == nil {
				logging.Warnf("monitor prune: %v", err)
			}
		} else if n > 0 {
			logging.Infof("pruned %d group monitor result(s)", n)
		}
	}
	pruneTicker := time.NewTicker(24 * time.Hour)

	// 监测循环用一个 30 秒的轻量 ticker 判断"是否到了下一轮"，而不是直接按
	// 间隔建 ticker：这样管理台把间隔从 30m 改成 1m（或改成 0 关闭）能立即生效，
	// 不需要重建定时器。
	monitorTicker := time.NewTicker(30 * time.Second)
	var lastMonitor time.Time
	runMonitor := func() {
		if a.Monitor == nil {
			return
		}
		interval := current().GroupMonitorInterval
		if interval <= 0 {
			return
		}
		if !lastMonitor.IsZero() && time.Since(lastMonitor) < interval {
			return
		}
		lastMonitor = time.Now()
		started := time.Now()
		count, err := a.Monitor.RunOnce(ctx, "auto")
		if err != nil {
			if ctx.Err() == nil {
				logging.Warnf("group monitor: %v", err)
			}
			return
		}
		logging.Infof("group monitor: 已检测 %d 个分组，用时 %s", count, time.Since(started).Round(time.Millisecond))
	}
	// 启动就跑一轮：容器重启后管理台立刻有新鲜结果，而不是等一个间隔。
	runMonitor()

	// 自动同步上游模型。开关与间隔都可能被管理台热改，所以同样用「轻 ticker +
	// 到点判断」而不是直接按间隔建 ticker。
	syncTicker := time.NewTicker(30 * time.Second)
	var lastSync time.Time
	runSync := func() {
		if a.Syncer == nil {
			return
		}
		settings := current()
		if !settings.ModelAutoSync || settings.ModelSyncInterval <= 0 {
			return
		}
		if !lastSync.IsZero() && time.Since(lastSync) < settings.ModelSyncInterval {
			return
		}
		lastSync = time.Now()
		summary, err := a.Syncer.RunOnce(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logging.Warnf("model sync: %v", err)
			}
			return
		}
		logging.Infof("model sync: 已检查 %d 个启用渠道，新增 %d 个模型，失败 %d 个",
			summary.Providers, summary.Imported, summary.Failed)
	}

	go func() {
		defer reaperTicker.Stop()
		defer pruneTicker.Stop()
		defer monitorTicker.Stop()
		defer syncTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reaperTicker.C:
				reap()
			case <-pruneTicker.C:
				prune()
			case <-monitorTicker.C:
				runMonitor()
			case <-syncTicker.C:
				runSync()
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

	path, err := config.DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	settings, bootstrapKey, created, err := config.Ensure(path)
	if err != nil {
		log.Fatal(err)
	}
	// 配置已通过校验（含 LOG_LEVEL 的级别名），此后日志按该级别过滤。
	level := logging.Configure(settings.LogLevel)
	logging.Infof("civic-relay %s (commit %s, built %s)", version, commit, buildDate)
	logging.Infof("日志级别 %s（在管理台把 LOG_LEVEL 改成 DEBUG 可打开逐请求访问日志，无需重启）", level.Tag())
	if created {
		// The credential itself is written only to the adjacent one-time file and
		// is intentionally never logged or included in process metadata.
		_ = bootstrapKey
		logging.Infof("first start created bootstrap administrator key file: %s", filepath.Join(filepath.Dir(path), "bootstrap-admin-key.txt"))
	}
	app, source, err := buildApplication(settings, path)
	if err != nil {
		log.Fatal(err)
	}
	server := newServer(net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), app.Handler)
	app.Server = server
	stopMaintenance := app.startMaintenance(source.Get)
	logging.Infof("listening on %s", server.Addr)

	// 优雅停机：SIGINT/SIGTERM 先排空在途请求（含 SSE 流），再关闭数据库
	shutdownDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		logging.Infof("shutting down: draining in-flight requests")
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
	logging.Infof("stopped")
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
