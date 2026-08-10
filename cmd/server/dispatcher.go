package main

import (
	"context"

	"go-proxy-mini/internal/invalidate"
	"go-proxy-mini/internal/notify"
)

// schedGroupPub 把 notify.Publisher 适配为 scheduler.GroupChangePublisher
//（scheduler 不 import notify——发布面接口化（与 service.Publisher 同模式），
// 装配侧粘合）。账号状态回写成功后发组级 NOTIFY（Change.Groups），接收端
// Dispatcher.Apply → Accounts(gids, false) → 组级定向重载。
type schedGroupPub struct{ p *notify.Publisher }

func (a schedGroupPub) PublishGroups(ctx context.Context, gids []int64) {
	_ = a.p.Publish(ctx, notify.Change{Groups: gids}) // 失败 Publisher 内部已 Warn，60s 兜底收敛
}

// schedFullReloader 同步全量重载（*scheduler.Scheduler 实现；FullRefresh 需要
// 同步 + 带错误 + 响应 ctx 取消——invalidate.SchedReloader 的 InvalidateAll 是
// 异步 fail-safe，不适用；评审 M-2：断线重连的全量刷新不得耗尽停机预算）。
type schedFullReloader interface {
	InvalidateAllSyncCtx(ctx context.Context) error
}

// dispatcher 实现 notify.Dispatcher（#14 T3a 装配侧）：把 NOTIFY Change 转发
// 给 invalidate 去抖器的 Mark 方法（本地/远端变更共享同一去抖窗口，天然合并
// 去重——设计文档 §2.3）；FullRefresh 直接调各快照全量重载（启动/断线重连
// 兜底，R8）。
//
// 放装配侧（cmd/server）而非 notify 包：notify 不 import invalidate/service
// 是 T1 设计约束（避免依赖环），适配只能在依赖两者的最外层做。
type dispatcher struct {
	inv      *invalidate.Debouncer
	auth     invalidate.AuthReloader
	balances invalidate.BalancesReloader // billing.enabled=false → nil 接口（防 typed-nil，与 main 同纪律）
	sched    schedFullReloader
	svc      invalidate.SettingsReloader // *service.Service
	rules    invalidate.RulesReloader    // *rule.RuleEngine
}

// Apply 处理一条 NOTIFY 变更：按映射表转去抖器 Mark（设计文档 §2.2/§2.3）。
// 映射表：
//   - Users → Users()：auth + 余额快照全量
//   - Templates → Templates()：sched 全量 + clients 失效
//   - Groups（±Clients）→ Accounts(gids, keyChanged)：sched 组级定向；账号
//     upstream_key 变更带 Clients → 同批 clients 失效
//   - Clients（独立）→ Clients()：仅客户端工厂失效（服务端恒与 Templates/
//     Groups 并排，防御性兜底）
//   - Multipliers → Multipliers()：余额倍率快照定向刷新
//   - Keys → Keys()：auth 快照全量（key CRUD 缺口）
//   - Settings → Settings()：settings 快照重载
//   - Rules → Rules()：规则表全量重载（重载清窗口计数，全实例同步语义）
//
// 合并语义：Templates + Groups 同窗（载荷守卫降级 full）→ 去抖器 merge 后
// 组级被全量包含跳过，语义仍正确。Mark 路径零锁零 DB，恒返回 nil。
func (d *dispatcher) Apply(ctx context.Context, ch notify.Change) error {
	if ch.Users {
		d.inv.Users()
	}
	if ch.Templates {
		d.inv.Templates()
	}
	switch {
	case ch.Clients && len(ch.Groups) > 0:
		d.inv.Accounts(ch.Groups, true) // 账号 upstream_key 变更：组级重载 + clients 失效（一次 mark 合并）
	case ch.Clients:
		d.inv.Clients()
	case len(ch.Groups) > 0:
		d.inv.Accounts(ch.Groups, false)
	}
	if ch.Multipliers {
		d.inv.Multipliers()
	}
	if ch.Keys {
		d.inv.Keys()
	}
	if ch.Settings {
		d.inv.Settings()
	}
	if ch.Rules {
		d.inv.Rules()
	}
	return nil
}

// FullRefresh 启动首连 / 断线重连全量本地刷新（设计文档 §2.3 / R8）：Auth +
// 余额 + sched 全量 + settings + 规则表，覆盖断连期间 NOTIFY 丢失。各步独立
// 尽力执行，返回首个错误（listener 侧 Warn）。与 main 启动序（ruleEngine.Reload
// + sched.InvalidateAllSync）并存——幂等重复无害（仅一次启动开销），重连路径
// 是本方法唯一入口。
func (d *dispatcher) FullRefresh(ctx context.Context) error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	record(d.auth.Reload(ctx))
	if d.balances != nil {
		record(d.balances.Reload(ctx))
	}
	record(d.sched.InvalidateAllSyncCtx(ctx))
	record(d.svc.ReloadSettings(ctx))
	record(d.rules.ReloadRules(ctx))
	return firstErr
}
