// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// github.com/is7qin/c3api 入口：配置 → DB/ent → 各模块装配 → 优雅退出。
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

	jwtauth "github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/handler"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/is7qin/c3api/pkg/logx"
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
		// 附 -config 路径与 CWD（p2-01 P3-8）：相对路径文件缺失/校验失败可归因；
		// env-only 部署（-config ""）报错时此处即线索。
		wd, _ := os.Getwd()
		fatalf("config: %v (path: %s, cwd: %s)", err, *cfgPath, wd)
	}
	log, err := logx.New(cfg.Log.Level, cfg.Log.Output)
	if err != nil {
		fatalf("logger: %v", err)
	}
	// 必填校验（admin.token/auth.jwt_secret/db.dsn）已内聚到 config.Load，此处只做错误处理。

	// 启动期 DB 操作统一 30s 预算（OpenPG + ent migrate + 三分区 bootstrap）：
	// 超时/失败经 fatalDB 明确文案（"db bootstrap timed out after 30s" 可归因）。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	pool, err := repository.OpenPG(startupCtx, cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		fatalDB("db", err)
	}
	defer pool.Close()
	// ent v0.14.6 的 entsql.OpenDB 只接受 *sql.DB：pgxpool 经 pgx/stdlib 桥接（用户决策 2026-08-05）
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB(dialect.Postgres, db)
	repos, err := repository.NewWithPG(startupCtx, drv, true, pool) // pool 供 Stats.Upsert COPY 两阶段批量写（#17）与 Billing.DeductAndLog COPY 路径（热点修复 A 扩）
	if err != nil {
		fatalDB("migrate", err)
	}
	// usage_logs/err_logs/usage_stats 分区 bootstrap（Phase 5 T4.5 + 分表设计 +
	// 用户裁决 2026-08-11 三表统一分区机制）：ent migrate 已跳过三表
	// （migrateHookExcludesPartitioned——atlas 对分区表 diff 规划期必失败，实测
	// 结论见 internal/repository/partition.go），此处独占建分区表 + 预建当日/明日
	// 分区 + 索引；幂等（已分区 → 仅补齐分区），失败即 fatal（明细/审计/统计表
	// 不可缺）。
	if err := repos.EnsureUsageLogPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("usagelog partition bootstrap", err)
	}
	if err := repos.EnsureErrLogPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("err_logs partition bootstrap", err)
	}
	if err := repos.EnsureUsageStatsPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("usage_stats partition bootstrap", err)
	}

	// #14 T3a：NOTIFY 发布器（多实例广播，设计文档 §2）。实例 ID = hostname-pid
	// （config 无实例字段，最小方案；评审 I-2：同主机多实例各进程 pid 不同，
	// Src 不碰撞，接收端凭此跳过自播）。发布在 DB 写成功后（与 inv.* 调用点
	// 并排）；计费路径永不发布。
	host, err := os.Hostname()
	if err != nil {
		fatalf("hostname: %v", err)
	}
	src := fmt.Sprintf("%s-%d", host, os.Getpid())
	pub := notify.NewPublisher(pool, src, log)

	// 规则引擎先行构造（不 Reload——New 只建结构）：scheduler 构造期注册 apply 回调。
	ruleEngine := rule.New(rule.Config{}, repos.Rules, log)
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: cfg.Scheduler.DefaultMaxConcurrency,
		SyncInterval:          cfg.Scheduler.SyncInterval,
		// 账号状态回写成功后广播受影响组（其余实例组级重载收敛分裂快照）。
		GroupPub: schedGroupPub{pub},
	}, repos.Groups, ruleEngine, log)
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		StatsFlushInterval: cfg.Usage.StatsFlushInterval,
		Workers:            cfg.Usage.FlushWorkers,
	}, repos.Usages, repos.Stats, log)
	// errlog worker（分表设计）：错误明细落盘通道——与计费 flusher 完全解耦
	// （独立有界队列 + 背压采样丢弃 + 独立排空）；落盘 err_logs（瘦表审计）。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize:     cfg.Usage.ErrLogQueueSize,
		BatchSize:     cfg.Usage.ErrLogBatchSize,
		FlushInterval: cfg.Usage.ErrLogFlushInterval,
	}, repos.ErrLogs, log)
	// retention worker：三表按日分区保留（T4.5 + 分表设计 + usage_stats 分区化，
	// 清理统一 DROP PARTITION O(1)——PG DELETE 不释放空间，用户裁决）；保留天数
	// 同源 config（usage_logs = usage.log_retention_days；err_logs =
	// usage.errlog_retention_days 默认 7 天短保留——错误审计；usage_stats =
	// usage.stats_retention_days 默认 180 天——聚合统计长保留）。
	retention := usage.NewRetention(usage.RetentionConfig{
		LogRetentionDays:    cfg.Usage.LogRetentionDays,
		ErrLogRetentionDays: cfg.Usage.ErrLogRetentionDays,
		StatsRetentionDays:  cfg.Usage.StatsRetentionDays,
	}, repos, log)

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
	// 管理端变更统一经 invalidate 去抖器生效（O2 接线矩阵，评审 M-1）：
	// - 用户 CRUD（含创建）/余额变更 → auth + 余额快照全量 Reload（去抖窗口
	//   内合并；新用户必须即刻进余额快照——评审 M-2，防 ≤10s 402 窗口）
	// - 模板（base_url/models/映射）→ sched 全量 + clients 失效（base_url
	//   变更需按新地址重建 SDK 客户端；评审发现：此前 Factory.InvalidateAll
	//   无人调用，模板 base_url 更新后流量仍打旧上游直至重启）
	// - 账号 → sched 组级定向 InvalidateGroup（full ⊇ 组级 ⊇ 无）；upstream_key
	//   变更 → clients 失效
	// - 组倍率 / 用户-组专属倍率（group_assignment，T3.5 按组）→ 余额倍率
	//   快照定向刷新（EffectiveMultiplier 陈旧 ≤10s 不可接受）
	// - key CRUD（#14 T2 扩展）→ auth 快照全量 Reload（本地仍走 auth 增量
	//   Upsert/Delete——单实例快路径；Keys() 分支覆盖远端实例的陈旧快照）
	// - settings 变更（UpdateSetting）→ settings 快照重载（svc 实现，SetSettings
	//   装配，见下）
	// - 规则 CRUD → 规则表全量重载（ruleEngine.ReloadRules，重载清窗口计数——
	//   全实例同步执行语义）
	// - pricing → 现状（内部 reloadPricing，不进 invalidate）
	// 去抖窗口 200ms：管理面变更生效延迟 ≤ 窗口 + 一次重载时长；后沿语义
	// （评审 C-6：完成后又脏立即再执行，不按固定间隔 throttle——不与长 reload
	// 重叠）。读端永不阻塞：Mark 路径零锁零 DB，重载单 goroutine 串行（消除
	// Phase 6 压测实证的 33,705 goroutine reloadMu 串行雪崩）。
	//
	// 计费装配提前到 svc 之前：去抖器装配需要余额快照引用；billHooks 仍需
	// svc，在 svc 之后组装。
	var billFlusher *billing.Flusher
	var billHooks *proxy.BillingHooks
	var billBalances *billing.Balances
	// invBalances 接口声明而非具体类型：billing 关闭时保持 nil 接口（非 typed
	// nil）。若用 *billing.Balances 声明，关闭时接口 = (*Balances)(nil)，类型部分
	// 非 nil → invalidate.reloadAll 的 `!= nil` 检查判 TRUE → Reload 调用 nil
	// receiver panic（2026-08-10 管理端建用户实证：Debouncer.loop panic）。
	var invBalances invalidate.BalancesReloader
	if cfg.Billing.Enabled {
		// loader = Repository 门面（BalanceLoader：余额 → Users，组倍率 +
		// assignment 专属倍率 → Groups，T3.5 修正按组）。
		billBalances = billing.NewBalances(repos, log)
		invBalances = billBalances
		// 首载不在此（fail-safe 语义由注册表 ReloadAll 承担：错误独立 Warn 保留
		// 空快照 → 预检全 402 拒绝，安全侧）——单一启动入口，消灭双重加载。
	}
	inv := invalidate.New(invalidate.Config{
		Window:   invalidate.DefaultWindow, // 200ms（生效延迟语义见 invalidate 包注释）
		Sched:    sched,
		Clients:  clients,
		Auth:     auth,
		Balances: invBalances, // billing.enabled=false → nil 接口（flush 跳过余额路径）
		Rules:    ruleEngine,  // 规则 CRUD → 全实例规则表重载（#14 T3a；ruleEngine 先于 invalidate 构造）
		Log:      log,
	})
	// ruleReload 独立于 invalidate：规则 CRUD 后全量重载（重载会重置窗口计数，
	// 不能随模板/账号/分组等任意资源变更触发）。
	svc := service.New(repos, sched, inv, pub, ruleEngine, auth, log)
	// 快照注册表装配（统一生命周期）：五路快照（auth/scheduler/rules/pricing/
	// balances——billing 关闭不注册）登记 scope 与 Reload。注册只登记元数据
	// （零 DB），首刷统一在构造链完成后执行（见下 ReloadAll——单一启动入口，
	// 各模块构造内不自行 reload）。scope 分发 = settings 变更 → ScopeSettings
	// 声明方（auth gate N 预算，#36）；其余变更类型仍走去抖器，注册表不重复
	// 接管（周期 ticker 亦各模块自管，边界见 internal/snapshot 包注释）。
	snapReg := snapshot.New()
	for _, s := range []snapshot.Snapshot{
		authSnapshot{auth},
		schedSnapshot{sched},
		ruleSnapshot{ruleEngine},
		pricingSnapshot{svc},
	} {
		if err := snapReg.Register(s); err != nil {
			fatalf("snapshot register: %v", err)
		}
	}
	if billBalances != nil { // 与 invBalances 同纪律：billing 关闭不注册
		if err := snapReg.Register(balanceSnapshot{billBalances}); err != nil {
			fatalf("snapshot register: %v", err)
		}
	}
	// #14 T3a：NOTIFY 监听装配。变更分发器放装配侧——notify 不 import
	// invalidate（T1 依赖环约束）。
	disp := &dispatcher{
		inv:       inv,
		svc:       svc,
		snapshots: snapReg,
		log:       log,
	}
	// #36 本地实例即时重算：settings 变更直连本地分发器（与远端 NOTIFY 同路径
	// Apply——自播 NOTIFY 被 Src 跳过，本地实例预算重算不能依赖 NOTIFY 回环）。
	// 装配序：dispatcher 需要 svc（SettingsReloader）、svc 需要 dispatcher（本地
	// 分发）——构造环，svc 构造完成后回填（与 inv.SetSettings 同模式）。
	svc.SetLocalDispatcher(disp)
	// invalidate 的 settings 分支延迟绑定：service.New 需要去抖器做 Invalidator、
	// 去抖器需要 svc 做 SettingsReloader（构造环）——svc 构造完成后、Start 前回填。
	inv.SetSettings(svc)
	// NOTIFY 监听 worker（Name="notify"）：独立 pgx 连接 LISTEN c3api_invalidate；
	// 断线指数退避重连 + 重连即全量刷新（R8）；Src 跳过自播（省重复 reload）。
	listener := notify.NewListener(notify.ListenerConfig{
		DSN:        cfg.DB.DSN,
		Src:        src,
		Dispatcher: disp,
		Log:        log,
	})
	// 60s 周期鉴权快照兜底（R1：NOTIFY 丢失/断连期间 key 与用户变更最长 60s
	// 收敛；现状 auth 无周期 reload，是兜底缺口——auth-sync worker 补位。
	// T3b 在其 Reload 内接入 N 与预算重分配，本 worker 侧无需再改）。
	authSync := newAuthSync(auth, 0, log)
	if cfg.Billing.Enabled {
		billFlusher = billing.NewFlusher(billing.FlushConfig{
			FlushInterval:          cfg.Billing.FlushInterval,
			BalanceRefreshInterval: cfg.Billing.BalanceRefreshInterval,
			Workers:                cfg.Billing.FlushWorkers,
		}, repos, rec, billBalances, log)
		billHooks = &proxy.BillingHooks{
			Prices:      svc,
			ImagePrices: svc, // Task B：images 端点预检查 image_price 快照（P1-1 预检按格式切换）
			Balances:    billBalances,
			Flusher:     billFlusher,
			TierPolicy:  svc.ServiceTierPolicy,
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
	}, sched, credential.New(), rec, clients, auth, log, billHooks, errlogW)
	// codex SDK 适配层装配（T2 §3——统一失效回调先落生图路径；T5 全量）：
	// 适配层构造注册 WithOnAuthFatal → 统一回调 → 失效处理链（写 failed_at +
	// 调度摘除 + 审计，T1 契约）。
	codexAdapter := sdkbridge.NewCodex(sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{
		Store:  repos.Accounts,
		Failer: sched,
		Log:    log,
	}))
	// SDK HTTPClient 上游 transport（resp 补压测修复——SDK 默认 transport
	// MaxIdleConnsPerHost=2 连接风暴，压测 profile ~12% CPU）：连接池参数与
	// 网关既有 client 同形态（同一 httpx 构造 helper + cfg.Upstream 同源），
	// MaxConnsPerHost 显式上界对齐 MaxIdleConnsPerHost（防单上游连接失控；
	// 网关既有 client 不设 = 不限，压测验证形态保持）。
	codexAdapter.SetTransport(httpx.NewTransport(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		MaxConnsPerHost:     cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
	}))
	// T5 §1 轮转回写面装配：WithOnTokenRotated → account_ext 部分更新 upsert
	//（oauth_token + oauth_refresh_token + oauth_expires_at 保旧）+ 失效调度器
	// AccountExt 内存快照条目（P3-3——下个会话重载新凭据；不重建 Auth 缓存）。
	codexAdapter.SetRotationDeps(sdkbridge.RotationDeps{
		Store:              repos.AccountExts,
		InvalidateSnapshot: sched.InvalidateAccount,
		Log:                log,
	})
	px.SetCodex(codexAdapter)
	// 多实例集群 N 注入（#14 T3b）：gate 预算 ceil(剩余/N) + limit RPM ceil(rpm/N)。
	// svc 构造后调用（svc.ClusterInstances 读 settings 快照）；settings NOTIFY
	// 变更 N 后再次调用即触发预算即时重算（设计 §3.4）。
	px.SetInstancesProvider(svc)
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
		// Task A 双线：文本价 + image 价快照一并刷新（image 拉取成功后同样
		// 重载 image 快照——spec §2.1 管线扩展）。
		Reload: func() {
			svc.ReloadPricing()
			svc.ReloadImagePricing()
		},
		Log: log,
	})
	h := handler.New(svc)
	aiRouter := proxy.AIRouter(px)
	iss := jwtauth.NewIssuer(cfg.Auth.JWTSecret)
	userHandler := userapi.Router(svc, iss, auth)

	// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改
	// worker.Worker——装配侧类型断言聚合（各模块已持具体引用，断言实现
	// server.StatsProvider 的入列；快照注册表状态单独经 Status 直出）。
	var opsWorkers []server.StatsProvider
	for _, w := range []worker.Worker{inv, sched, ruleEngine, rec, errlogW, pricingSync, retention, listener, authSync} {
		if s, ok := w.(server.StatsProvider); ok {
			opsWorkers = append(opsWorkers, s)
		}
	}
	if billFlusher != nil {
		opsWorkers = append(opsWorkers, billFlusher)
	}

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
		OpsWorkers:        opsWorkers,
		OpsSnapshots:      func() []server.SnapshotState { return snapshotStates(snapReg.Status()) },
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 统一启动就绪（快照注册表）：构造链完成后全量首刷（并行；各快照错误独立
	// Warn 不阻塞启动——DB 故障时快照保持空/旧值，模块周期 ticker / listener
	// FullRefresh 兜底收敛）。取代此前分散的 ruleEngine.Reload fatalf +
	// sched.InvalidateAllSync fatalf + auth/billBalances 构造时加载：scheduler
	// 首刷在此完成（Select 在 nil 快照上 panic 的窗口随单一入口消灭），规则
	// 空表种子亦在此写入（失败降级 Warn——注册表 Status 可观测）。
	for name, err := range snapReg.ReloadAll(ctx) {
		log.Warn("snapshot initial reload failed", logx.String("snapshot", name), logx.Error(err))
	}
	// 统一 worker 管理：顺序启动（scheduler 先、usage 后）、反向排空
	// （usage 先排明细 → rule 先关排空事件（scheduler 已停投）→ scheduler 后排空回写）。
	wm := worker.New(log)
	wm.Register(inv, sched, ruleEngine, rec, errlogW, pricingSync, retention) // invalidate 去抖器执行 goroutine（单 goroutine 串行）；errlog 错误明细排空在 rec 之后注册 → 反向排空先于 rec；retention 顺序无依赖（DROP/预建均幂等）
	if billFlusher != nil {
		// 计费 flusher 注册在 listener/auth-sync 之前（评审 I-1）：反向排空时它
		// 是最后一个产生计费流量的 worker（扣费 + 计费日志全量落库）；其后注册
		// 的 listener/auth-sync 是旁观者（不产生流量），先关无碍。
		wm.Register(billFlusher)
	}
	// listener/auth-sync 最后注册 → 反向排空最先关：停止接收/周期刷新后再排空
	// 业务 worker（scheduler 排空回写仍会发布 NOTIFY，自播跳过、其它实例接收，
	// 无依赖）。
	wm.Register(listener, authSync)
	if err := wm.StartAll(ctx); err != nil {
		fatalf("worker start: %v", err)
	}
	// 调度器初始加载已由上方注册表 ReloadAll 完成（先于 StartAll 与流量）——
	// 此处不再单独 InvalidateAllSync（单一启动入口）。

	// http.Server 超时（D-P2-4，现存最重 P2）：IdleTimeout 防 keep-alive 空闲连接
	// 与 goroutine 无限驻留（有效 key 吃满 50000 并发面数小时即修复面）；ReadTimeout
	// = proxy.upstream_timeout 同源单一事实源（120s）——只限请求头+体读取时长，不
	// 限制响应写出（net/http 语义），SSE 长流不受影响。slowloris 场景：1KB/s ×
	// 4MB ≈ 4096s → 120s 截断。
	// 不设 WriteTimeout：会切断 SSE 长流（03-streaming.md C-P2-1 依赖节）；写侧
	// 防线是 C 方向 C-P2-1 的 SetWriteDeadline。
	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Proxy.UpstreamTimeout,
		IdleTimeout:       60 * time.Second,
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

// fatalDB 启动期 DB 操作失败 fatal（D-P2-3）：30s 预算超时 → 明确可归因文案。
func fatalDB(step string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		fatalf("db bootstrap timed out after 30s (%s): %v", step, err)
	}
	fatalf("%s: %v", step, err)
}

func fatalf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("fatal: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}
