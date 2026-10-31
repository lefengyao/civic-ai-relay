package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/httpapi"
	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/memory"
	"civic-ai-relay/internal/plugin"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
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
	Service  *relay.Service
	Registry *relay.Registry
	Monitor  *relay.Monitor
	Syncer   *relay.ModelSyncer
	Plugins  *plugin.Manager
	Server   *http.Server

	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
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
	// A fresh single-instance process has no active leases. Release every
	// reservation left by the previous process before accepting public traffic.
	startupCutoff := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	recovered, err := database.ReapOrphanReservations(context.Background(), startupCutoff, nil, nil)
	if err != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("recover crashed reservations: %w", err)
	}
	if recovered > 0 {
		logging.Infof("recovered %d reservation(s) left by a previous process", recovered)
	}
	registry := relay.NewRegistryWithOptions(database, upstream.Options{
		ConnectTimeout: settings.ConnectTimeout, HeaderTimeout: settings.HeaderTimeout,
		ReadTimeout: settings.ReadTimeout, WriteTimeout: settings.WriteTimeout,
		PoolTimeout: settings.PoolTimeout, IdleTimeout: settings.UpstreamIdleTimeout,
		UserAgent: settings.UpstreamUserAgent, UpstreamSessionHeader: settings.UpstreamSessionHeader,
	})
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
	plugins, err := plugin.NewManager(database, settings.PluginDir)
	if err != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("initialize plugin manager: %w", err)
	}
	admin.SetPluginManager(plugins)
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
	// source.Get 让 CORS 等逐请求读取的配置在管理台保存后立即生效
	handler := httpapi.NewServerWithAdmission(service, settings.MaxBodyBytes, guard.PublicAdmission, guard.StreamContinue, source.Get, admin)
	return &Application{Handler: handler, Store: database, Service: service, Registry: registry, Monitor: monitor, Syncer: syncer, Plugins: plugins}, source, nil
}

func (a *Application) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if a.Service != nil {
		a.Service.BeginDraining()
	}
	var closeErr error
	var handlersErr error
	if a.Server != nil {
		tracker, _ := a.Server.Handler.(*trackedHandler)
		if tracker != nil {
			tracker.drain()
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		shutdownErr := a.Server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			logging.Warnf("graceful shutdown expired: %v", shutdownErr)
			if a.Service != nil {
				a.Service.CancelActive()
			}
			if err := a.Server.Close(); err != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("force close HTTP server: %w", err))
			}
		}
		if tracker != nil {
			waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			handlersErr = tracker.wait(waitCtx)
			waitCancel()
		}
	} else if a.Service != nil {
		a.Service.CancelActive()
	}
	if handlersErr == nil && a.Service != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := a.drainSettlements(settleCtx); err != nil {
			logging.Warnf("settlement drain incomplete: %v", err)
			closeErr = errors.Join(closeErr, err)
		}
		cancel()
	}
	if a.maintenanceCancel != nil {
		a.maintenanceCancel()
	}
	if a.maintenanceDone != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		select {
		case <-a.maintenanceDone:
		case <-stopCtx.Done():
			closeErr = errors.Join(closeErr, fmt.Errorf("wait for maintenance: %w", stopCtx.Err()))
			cancel()
			return closeErr
		}
		cancel()
	}
	if handlersErr != nil {
		// An uncooperative handler may still need the database. Maintenance has
		// stopped, but closing Store here would race that handler's settlement.
		return errors.Join(closeErr, fmt.Errorf("wait for active handlers: %w", handlersErr))
	}
	if a.Store != nil {
		closeErr = errors.Join(closeErr, a.Store.Close())
	}
	return closeErr
}

func (a *Application) drainSettlements(ctx context.Context) error {
	if len(a.Service.PendingReservationIDs()) == 0 {
		return nil
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := a.Service.ReconcilePending(ctx); err != nil && ctx.Err() == nil {
			logging.Warnf("settlement reconciliation: %v", err)
		}
		if len(a.Service.PendingReservationIDs()) == 0 {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("pending settlements remain: %w", ctx.Err())
		}
	}
}

type trackedHandler struct {
	next     http.Handler
	mu       sync.Mutex
	active   int
	idle     chan struct{}
	draining bool
}

func newTrackedHandler(next http.Handler) *trackedHandler {
	if next == nil {
		next = http.DefaultServeMux
	}
	idle := make(chan struct{})
	close(idle)
	return &trackedHandler{next: next, idle: idle}
}

func (h *trackedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.draining {
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"message": "settlement_unavailable: service shutting down",
			"type":    "relay_error",
			"code":    "settlement_unavailable",
		}})
		return
	}
	if h.active == 0 {
		h.idle = make(chan struct{})
	}
	h.active++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.active--
		if h.active == 0 {
			close(h.idle)
		}
		h.mu.Unlock()
	}()
	h.next.ServeHTTP(w, r)
}

func (h *trackedHandler) drain() {
	h.mu.Lock()
	h.draining = true
	h.mu.Unlock()
}

func (h *trackedHandler) wait(ctx context.Context) error {
	h.mu.Lock()
	idle := h.idle
	h.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startMaintenance runs the background loops:
//   - reaper: settles reservations from crashed requests so they stop holding
//     concurrency slots and inflating the 5-hour token window;
//   - prune: deletes ledger rows older than RETENTION_DAYS so the database
//     (and with it every window-based counter) stays bounded;
//   - monitor: checks whether each model group can still serve traffic;
//   - sync: imports newly published upstream models (disabled + unpriced).
//
// 接收 settings 读取函数而非快照：这几个循环都要在管理台改配置后立即跟随，
// 例如把 GROUP_MONITOR_INTERVAL 改成 0 应当立刻停掉监测。
func (a *Application) startMaintenance(current func() config.Settings) {
	ctx, cancel := context.WithCancel(context.Background())
	a.maintenanceCancel = cancel
	a.maintenanceDone = make(chan struct{})
	reap := func() {
		maxDuration := current().MaxStreamDuration
		if maxDuration <= 0 {
			// Unlimited streams cannot be distinguished from a slow but healthy
			// request by age alone. Startup recovery handles crash leftovers;
			// runtime reaping stays disabled until a finite limit is configured.
			return
		}
		maxAge := maxDuration + 2*time.Minute
		var activeIDs, pendingIDs []int64
		if a.Service != nil {
			activeIDs = a.Service.ActiveReservationIDs()
			pendingIDs = a.Service.PendingReservationIDs()
		}
		reaped, err := a.Store.ReapOrphanReservations(ctx, time.Now().UTC().Add(-maxAge), activeIDs, pendingIDs)
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
		defer close(a.maintenanceDone)
		defer reaperTicker.Stop()
		defer pruneTicker.Stop()
		defer monitorTicker.Stop()
		defer syncTicker.Stop()
		reconcileTicker := time.NewTicker(5 * time.Second)
		defer reconcileTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcileTicker.C:
				if a.Service != nil {
					if _, err := a.Service.ReconcilePending(ctx); err != nil && ctx.Err() == nil {
						logging.Warnf("settlement reconciliation: %v", err)
					}
				}
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
	logging.Infof("listening on %s", server.Addr)
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := serveApplication(signalCtx, app, source.Get); err != nil {
		log.Fatal(err)
	}
	logging.Infof("stopped")
}

func serveApplication(signalCtx context.Context, app *Application, current func() config.Settings) error {
	app.startMaintenance(current)
	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Server.ListenAndServe() }()
	select {
	case <-signalCtx.Done():
		logging.Infof("shutting down: draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		closeErr := app.Close(shutdownCtx)
		cancel()
		serveErr := <-serveDone
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return errors.Join(serveErr, closeErr)
		}
		return closeErr
	case serveErr := <-serveDone:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		closeErr := app.Close(shutdownCtx)
		cancel()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return errors.Join(serveErr, closeErr)
		}
		return closeErr
	}
}

// newServer keeps WriteTimeout at zero on purpose: SSE streams may stay open
// for MAX_STREAM_DURATION. ReadHeaderTimeout bounds slow-loris style header
// stalls; IdleTimeout recycles keep-alive connections.
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           newTrackedHandler(handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
