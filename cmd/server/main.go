// go-proxy-mini 入口：配置 → DB/ent → 各模块装配 → 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"

	jwtauth "go-proxy-mini/internal/auth"
	"go-proxy-mini/internal/billing"
	"go-proxy-mini/internal/config"
	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/handler"
	userapi "go-proxy-mini/internal/handler/user"
	"go-proxy-mini/internal/pricing"
	"go-proxy-mini/internal/proxy"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/rule"
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
	pprofAddr := flag.String("pprof", "", "listen addr for /debug/pprof (heap/goroutine profile under load)")
	flag.Parse()
	if *pprofAddr != "" {
		go func() { _ = http.ListenAndServe(*pprofAddr, nil) }() // net/http/pprof 自动挂载
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatalf("config: %v", err)
	}
	log, err := logx.New(cfg.Log.Level, cfg.Log.Output)
	if err != nil {
		fatalf("logger: %v", err)
	}
	if cfg.Admin.Token == "" || cfg.Auth.JWTSecret == "" || cfg.DB.DSN == "" {
		fatalf("admin.token, auth.jwt_secret and db.dsn are required (config or GPM_ADMIN_TOKEN/GPM_AUTH_JWT_SECRET/GPM_DB_DSN)")
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

	// 规则引擎先行构造（不 Reload——New 只建结构）：scheduler 构造期注册 apply 回调。
	ruleEngine := rule.New(rule.Config{}, repos.Rules, log)
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: cfg.Scheduler.DefaultMaxConcurrency,
		SyncInterval:          cfg.Scheduler.SyncInterval,
	}, repos.Groups, ruleEngine, log)
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		LogRetentionDays:   cfg.Usage.LogRetentionDays,
		StatsFlushInterval: cfg.Usage.StatsFlushInterval,
	}, repos.Logs, repos.Stats, log)

	auth := proxy.NewAuth(repos.Keys, repos.Users, log)
	rec.SetQuotaWriter(repos.Keys) // 额度扣减批量回写（Recorder 节奏）
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
	// 管理端变更统一经 invalidate 回调生效：调度器重载快照（选号/状态）+
	// aiclient 工厂丢弃 SDK 客户端（base_url 变化下次使用时按新地址重建；
	// 评审发现：此前 Factory.InvalidateAll 无人调用，模板 base_url 更新后流量
	// 仍打旧上游直至重启）+ Auth 鉴权快照全量刷新（用户禁用/并发/额度调整
	// 即时生效——评审 I-2）+ 余额快照全量刷新（管理面改余额/Redeem 后计费
	// 预检即时生效，Phase 5 T3）。
	var billBalances *billing.Balances
	invalidate := func() {
		sched.InvalidateAll()
		clients.InvalidateAll()
		if err := auth.Reload(context.Background()); err != nil {
			log.Warn("auth reload failed", logx.Error(err))
		}
		if billBalances != nil {
			_ = billBalances.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		}
	}
	// ruleReload 独立于 invalidate：规则 CRUD 后全量重载（重载会重置窗口计数，
	// 不能随模板/账号/分组等任意资源变更触发）。
	svc := service.New(repos, sched, invalidate, ruleEngine, auth, log)
	// Phase 5 计费装配（T3：余额快照 + 批量 flusher；T2 中间态清理终点——hooks
	// 四字段齐备，无 nil 容忍分支）。Enabled 默认关（opt-in）：关 → billHooks
	// nil → proxy 计费全关（不查价/不预检/不路由）。
	var billHooks *proxy.BillingHooks
	var billFlusher *billing.Flusher
	if cfg.Billing.Enabled {
		billBalances = billing.NewBalances(repos.Users, log)
		_ = billBalances.Reload(context.Background()) // 启动同步，fail-safe（失败保留空快照 → 预检全 402 拒绝，安全侧）
		billFlusher = billing.NewFlusher(billing.FlushConfig{
			FlushInterval:          cfg.Billing.FlushInterval,
			BalanceRefreshInterval: cfg.Billing.BalanceRefreshInterval,
		}, repos, rec, billBalances, log)
		billHooks = &proxy.BillingHooks{
			Prices:     svc,
			Balances:   billBalances,
			Flusher:    billFlusher,
			TierPolicy: svc.ServiceTierPolicy,
		}
	}
	px := proxy.New(proxy.Config{
		MaxBodySize:           cfg.Proxy.MaxBodySize,
		MaxInflight:           cfg.Proxy.MaxInflight,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
		FailoverAttempts:      cfg.Proxy.FailoverAttempts,
		GroupKeyRPM:           cfg.Limit.GroupKeyRPM,
		UsageCapture:          cfg.Proxy.UsageCapture,
		BillingCapture:        cfg.Billing.Enabled,
	}, sched, credential.New(), rec, clients, auth, log, billHooks)
	// litellm 价格同步 worker：启动异步拉取一次（不阻塞启动）+ price_sync_cron
	// 定期循环；source_url/cron 每轮从 svc 的 settings 快照现读（变更下次循环
	// 生效，无热加载通道）；同步成功后刷新 svc 价格快照（Phase 5 计费读零 DB）。
	// fetcher 与 svc 共享同一实例（手动 sync 端点 /admin/pricing/sync 同路径）。
	priceFetcher := pricing.NewFetcher(hc)
	svc.SetPriceFetcher(priceFetcher)
	pricingSync := pricing.NewSyncWorker(pricing.SyncWorkerConfig{
		Fetcher:  priceFetcher,
		Repo:     repos,
		Settings: svc,
		Reload:   svc.ReloadPricing,
		Log:      log,
	})
	h := handler.New(svc)
	aiRouter := proxy.AIRouter(px)
	iss := jwtauth.NewIssuer(cfg.Auth.JWTSecret)
	userHandler := userapi.Router(svc, iss, auth)

	srv := server.NewServer(server.Options{
		AdminToken:        cfg.Admin.Token,
		JWTIssuer:         iss,
		UserStatus:        auth,
		MaxInflight:       cfg.Proxy.MaxInflight,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		AdminHandler:      h.RoutesMux(),
		UserHandler:       userHandler,
		AIHandler:         aiRouter,
		WebFS:             webUI(),
		Logger:            log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 规则表是状态管理唯一路径：启动前显式 Reload（空表写种子），失败即 fatalf。
	if err := ruleEngine.Reload(ctx); err != nil {
		fatalf("rule engine reload: %v", err)
	}
	// 统一 worker 管理：顺序启动（scheduler 先、usage 后）、反向排空
	// （usage 先排明细 → rule 先关排空事件（scheduler 已停投）→ scheduler 后排空回写）。
	wm := worker.New(log)
	wm.Register(sched, ruleEngine, rec, pricingSync)
	if billFlusher != nil {
		wm.Register(billFlusher) // 计费 flusher 最后注册 → 反向排空最先（扣费 + 计费日志全量落库）
	}
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
	// 优雅停机链（Phase 5 计费不丢窗口）：
	// 1) Shutdown(2s) 优雅窗口：快速请求收尾（长连接流式超时留给 Close 强断）
	// 2) Close 强制断长连接 → 客户端断开 → recordStreamAbort → finish（断前
	//    usage 帧照常计费）
	// 3) waitForInflight：等在途归零（100ms 轮询；超时 Warn 继续不阻塞退出）
	// 4) wm.Shutdown 反向排空：billingFlusher 最先（扣费 + 计费日志全量落库）
	//    → rec 排空明细 → rule → sched
	srvCtx, cancelSrv := context.WithTimeout(shutdownCtx, 2*time.Second)
	_ = httpSrv.Shutdown(srvCtx)
	cancelSrv()
	_ = httpSrv.Close()
	waitForInflight(px, shutdownCtx, log)
	_ = wm.Shutdown(shutdownCtx)
	log.Info("shutdown complete")
	_ = log.Sync()
}

// waitForInflight 等在途请求归零（优雅停机第 3 步）：100ms 轮询 px.Inflight()；
// 超出剩余预算 → Warn 继续（不阻塞退出——极限情况丢 ≤1 flush 窗口，可接受，
// 见计划风险节）。
func waitForInflight(px *proxy.Proxy, ctx context.Context, log *logx.Logger) {
	for px.Inflight() > 0 {
		select {
		case <-ctx.Done():
			log.Warn("shutdown: inflight requests not drained, continuing")
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func fatalf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("fatal: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}
