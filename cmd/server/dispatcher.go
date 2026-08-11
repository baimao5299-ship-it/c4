package main

import (
	"context"

	"go-proxy-mini/internal/invalidate"
	"go-proxy-mini/internal/notify"
	"go-proxy-mini/internal/snapshot"
	"go-proxy-mini/pkg/logx"
)

// schedGroupPub 把 notify.Publisher 适配为 scheduler.GroupChangePublisher
// （scheduler 不 import notify——发布面接口化（与 service.Publisher 同模式），
// 装配侧粘合）。账号状态回写成功后发组级 NOTIFY（Change.Groups），接收端
// Dispatcher.Apply → Accounts(gids, false) → 组级定向重载。
type schedGroupPub struct{ p *notify.Publisher }

func (a schedGroupPub) PublishGroups(ctx context.Context, gids []int64) {
	_ = a.p.Publish(ctx, notify.Change{Groups: gids}) // 失败 Publisher 内部已 Warn，60s 兜底收敛
}

// dispatcher 实现 notify.Dispatcher（#14 T3a 装配侧）：把 NOTIFY Change 转发
// 给 invalidate 去抖器的 Mark 方法（本地/远端变更共享同一去抖窗口，天然合并
// 去重——设计文档 §2.3）；settings 变更例外——同步 ReloadSettings 后再经快照
// 注册表按 scope 精确重载（#36：N 变更/auth 预算即时生效，时序见 Apply）；
// FullRefresh 经注册表全量刷新（启动/断线重连兜底，R8）。
//
// 放装配侧（cmd/server）而非 notify 包：notify 不 import invalidate/service
// 是 T1 设计约束（避免依赖环），适配只能在依赖两者的最外层做。
type dispatcher struct {
	inv       *invalidate.Debouncer
	svc       invalidate.SettingsReloader // *service.Service（ReloadSettings：Apply settings 分支同步刷新 + FullRefresh）
	snapshots *snapshot.Registry          // 五路快照注册表（NOTIFY scope 分发 + 断线重连全量刷新）
	log       *logx.Logger                // nil = 静默（测试）
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
//   - Settings → 同步 ReloadSettings（快照先刷新——#36 时序，去抖 Mark 由
//     同步重载取代）+ 注册表按 ScopeSettings 精确重载声明方（当前 = auth：
//     gate 预算按新 N 重算，#36）
//   - Rules → Rules()：规则表全量重载（重载清窗口计数，全实例同步语义）
//
// 合并语义：Templates + Groups 同窗（载荷守卫降级 full）→ 去抖器 merge 后
// 组级被全量包含跳过，语义仍正确。除 settings 分支（同步 ReloadSettings 一
// 次 DB 读——低频路径，时序见上）外 Mark 路径零锁零 DB，恒返回 nil；注册表
// scope 重载错误内部 Warn（Apply 仍返回 nil——NOTIFY 是事件提示）。
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
		// #36 即时重算时序（R2 M-1）：先同步刷新 settings 快照（ReloadSettings，
		// N 立即入快照），再按 scope 精确重载声明方（auth Reload → gate.reload →
		// allocBudget 现读 N 即时重分配预算）——顺序保证预算读到新 N。修复前
		// d.inv.Settings() 仅 Mark（200ms 去抖后才 flush ReloadSettings），
		// reloadScopes 同步 auth.Reload 读到旧 N = 白重算；新 N 落地后再无
		// gate.reload 触发。settings 为低频路径，同步 DB 读可接受；去抖 Mark 由
		// 本次同步重载取代（免 200ms 后重复 ReloadSettings），失败 Warn + 模块
		// 周期 ticker / 下次变更兜底收敛。
		if err := d.svc.ReloadSettings(ctx); err != nil && d.log != nil {
			d.log.Warn("settings reload failed", logx.Error(err))
		}
		d.reloadScopes(ctx, snapshot.ScopeSettings)
	}
	if ch.Rules {
		d.inv.Rules()
	}
	return nil
}

// reloadScopes 注册表按 scope 精确重载（nil 注册表 = 未装配，no-op）。错误
// 独立 Warn——NOTIFY 是事件提示，失败由各模块周期 ticker / 60s 兜底收敛。
func (d *dispatcher) reloadScopes(ctx context.Context, scopes ...string) {
	if d.snapshots == nil {
		return
	}
	for name, err := range d.snapshots.Reload(ctx, scopes...) {
		if d.log != nil {
			d.log.Warn("snapshot scope reload failed",
				logx.String("snapshot", name), logx.Error(err))
		}
	}
}

// FullRefresh 启动首连 / 断线重连全量本地刷新（设计文档 §2.3 / R8）：注册表
// ReloadAll（auth + scheduler + rules + pricing + balances）覆盖断连期间
// NOTIFY 丢失，另重载 settings 快照（svc——不在注册表内，保持既有语义）。
// 各步独立尽力执行，返回首个错误（listener 侧 Warn）。与 main 启动序
// （registry.ReloadAll）幂等重复无害（重连路径是本方法唯一入口）。
func (d *dispatcher) FullRefresh(ctx context.Context) error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.snapshots != nil {
		for _, err := range d.snapshots.ReloadAll(ctx) {
			record(err)
		}
	}
	record(d.svc.ReloadSettings(ctx))
	return firstErr
}
