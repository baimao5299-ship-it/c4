// go-proxy-mini 入口：配置 → DB/ent → 各模块装配 → 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"

	"go-proxy-mini/internal/config"
	"go-proxy-mini/internal/handler"
	"go-proxy-mini/internal/proxy"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/server"
	"go-proxy-mini/internal/service"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/internal/worker"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/httpx"
	"go-proxy-mini/pkg/logx"
)

func main() {
	cfgPath := flag.String("config", "config.toml", "path to TOML config")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatalf("config: %v", err)
	}
	log, err := logx.New(cfg.Log.Level, cfg.Log.Output)
	if err != nil {
		fatalf("logger: %v", err)
	}
	if cfg.Admin.Token == "" || cfg.DB.DSN == "" {
		fatalf("admin.token and db.dsn are required (config or GPM_ADMIN_TOKEN/GPM_DB_DSN)")
	}

	pool, err := repository.OpenPG(context.Background(), cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		fatalf("db: %v", err)
	}
	defer pool.Close()
	// ent v0.14.6 的 entsql.OpenDB 只接受 *sql.DB：pgxpool 经 pgx/stdlib 桥接（用户决策 2026-08-05）
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB(dialect.Postgres, db)
	repos, err := repository.New(drv, true)
	if err != nil {
		fatalf("migrate: %v", err)
	}

	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: cfg.Scheduler.DefaultMaxConcurrency,
		Cooldown429:           cfg.Scheduler.Cooldown429,
		BackoffBase:           cfg.Scheduler.BackoffBase,
		BackoffMax:            cfg.Scheduler.BackoffMax,
		SyncInterval:          cfg.Scheduler.SyncInterval,
	}, repos.Groups, log)
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		LogRetentionDays:   cfg.Usage.LogRetentionDays,
		StatsFlushInterval: cfg.Usage.StatsFlushInterval,
	}, repos.Logs, repos.Stats, log)

	auth := proxy.NewAuth(repos.Groups, log)
	hc := httpx.NewClient(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
	})
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
	})
	px := proxy.New(proxy.Config{
		MaxBodySize:           cfg.Proxy.MaxBodySize,
		MaxInflight:           cfg.Proxy.MaxInflight,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
		FailoverAttempts:      cfg.Proxy.FailoverAttempts,
		GroupKeyRPM:           cfg.Limit.GroupKeyRPM,
		UsageCapture:          cfg.Proxy.UsageCapture,
	}, sched, rec, clients, auth, log)

	// 管理端变更统一经 invalidate 回调生效：调度器重载快照（选号/状态）+ aiclient
	// 工厂丢弃 SDK 客户端（base_url 变化下次使用时按新地址重建；评审发现：此前
	// Factory.InvalidateAll 无人调用，模板 base_url 更新后流量仍打旧上游直至重启）。
	invalidate := func() {
		sched.InvalidateAll()
		clients.InvalidateAll()
	}
	svc := service.New(repos, sched, invalidate, auth, log)
	h := handler.New(svc)
	aiRouter := proxy.AIRouter(px)

	srv := server.NewServer(server.Options{
		AdminToken:        cfg.Admin.Token,
		MaxInflight:       cfg.Proxy.MaxInflight,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		AdminHandler:      h.RoutesMux(),
		AIHandler:         aiRouter,
		Logger:            log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 统一 worker 管理：顺序启动（scheduler 先、usage 后）、反向排空（usage 先排明细）。
	wm := worker.New(log)
	wm.Register(sched, rec)
	if err := wm.StartAll(ctx); err != nil {
		fatalf("worker start: %v", err)
	}
	// 调度器初始加载必须先于服务流量（Select 在 nil 快照上 panic，Task 4 评审结论）。
	if err := sched.InvalidateAllSync(); err != nil {
		fatalf("scheduler initial load: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}
	wm.Go(ctx, "http-server", func(ctx context.Context) {
		log.Info("server listening", logx.String("addr", cfg.Server.Addr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("server: %v", err)
		}
	})

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = wm.Shutdown(shutdownCtx) // 反向：usage 先排空明细，scheduler 后排空回写
	log.Info("shutdown complete")
	_ = log.Sync()
}

func fatalf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("fatal: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}
