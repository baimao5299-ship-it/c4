// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// Select 按预生成调度路径（格式硬过滤 + 模型硬白名单 + 全模型账号 tier2 兜底
// + 加权轮询序列）选号，并占用并发槽。
// 路径在快照重建时生成（buildRoutes），本函数热路径只做 O(1) 桶查找 + 序列游标取用
// + 动态状态检查（冷却/禁用/并发满，atomic 读）+ CAS 抢占。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	return s.SelectExcluding(groupID, format, model, nil)
}

// SelectExcluding is Select with a per-request exclusion list. It is used by
// failover to avoid sending the same logical request to an account that has
// already failed while its rule event is still queued. The list is intentionally
// a slice: failover attempts are small, and this keeps the hot path allocation
// free for the normal (non-failover) Select call.
func (s *Scheduler) SelectExcluding(groupID int64, format domain.RequestFormat, model string, excluded []int64) (*Selection, error) {
	// 快照未加载（首刷失败 / DB 故障启动：注册表 ReloadAll 失败仅 Warn——评审
	// R3 M-1）：断言 ok 分支优雅失败（404 group not found），不 panic——旧启动
	// 序在此失败 fatalf（进程退出，无流量），Warn-and-serve 语义下客户端应见
	// 4xx 而非断连。auth/余额/pricing 空快照均安全拒绝（401/402），唯 scheduler
	// 需此守卫。热路径零成本：断言本身既有，ok 分支仅在未加载时进入。
	groups, ok := s.store.groups.Load().(map[int64]*groupSnapshot)
	if !ok {
		return nil, ErrGroupNotFound
	}
	gs, ok := groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	if gs.routingMode == domain.GroupRoutingModeUpstreams {
		return s.selectUpstream(gs, groupID, format, model, excluded)
	}
	rt, ok := gs.routes[routeKey{format, model}]
	if !ok {
		// 未知模型：回落默认桶（仅含全模型账号的默认格式 tier2）
		rt, ok = gs.routes[routeKey{format, ""}]
	}
	if !ok {
		return nil, ErrFormatUnavailable
	}
	now := s.timeNow()
	if rt.tier1 != nil {
		if sel, ok := s.pickFrom(rt.tier1, groupID, format, model, now, excluded); ok {
			return sel, nil
		}
	}
	if rt.tier2 != nil {
		if sel, ok := s.pickFrom(rt.tier2, groupID, format, model, now, excluded); ok {
			return sel, nil
		}
	}
	return nil, ErrNoAvailable
}

// pickFrom 沿预生成序列扫描候选：游标取模 + 动态状态检查 + CAS 抢占。
// 扫描上限 = 序列一轮（每候选检查一次）；全不可用/全竞争失败返回 false。
func (s *Scheduler) pickFrom(ws *weightedSeq, groupID int64, format domain.RequestFormat, model string, now time.Time, excluded []int64) (*Selection, bool) {
	n := len(ws.seq)
	if n == 0 {
		return nil, false
	}
	// 单代纪律（spec §1.1）：除数与视图在入口各取一次、整轮扫描共用——不是微优化，
	// 是语义要求：worker 可能在扫描中途换入新一代视图，逐候选现读会让同一请求的
	// 不同候选用不同代视图判定（决策不连贯）。Select 的 tier1/tier2 各自调用本
	// 方法（跨层允许换代，层级间本就独立决策）。
	cn := s.instancesN()
	view := s.concView.Load()
	for i := 0; i < n; i++ {
		a := ws.seq[int(ws.cursor.Add(1))%n]
		// 静态字段视图一次 Load（评审 Critical 修复）：重建/权重动作以原子指针
		// 整体替换视图，本热路径读与低频写零锁并发安全，同量级开销。
		av := a.static.Load()
		st := a.statePtr()
		if accountExcluded(av.acc.ID, excluded) {
			continue
		}
		if !av.upstreamEnabled {
			continue
		}
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue
		}
		cur := a.concurrency.Load()
		limit := int64(av.acc.MaxConcurrency) // buildSnapshots 已归一化 ≤0→defaultMax，恒 >0
		if cur >= int64(concShare(int(limit), cn)) {
			if cur >= limit || !concAllows(view, av.acc.ID, limit, cur+1) {
				continue // 视图满 / 本地已达真上限 → 换下一候选（借用拒绝=换号，非拒流）
			}
			// 借用放行：落入下方既有 CAS(cur, cur+1)；CAS 天然封顶竞态
			// （双借同时过 limit−1 时第二个 CAS 必败），无需新锁
		}
		if a.concurrency.CompareAndSwap(cur, cur+1) {
			mapped := model
			if m, ok := av.tpl.ModelMapping[model]; ok {
				mapped = m
			}
			used := s.timeNow()
			// Publish lastUsedAt with CAS so a rule worker update that races
			// with selection (notably a 429 cooldown) is never overwritten by
			// the stale state snapshot read above.
			markLastUsed(a, used)
			// 优先级账号级 > 模板级：仅 nil 检查 + 字符串比较（零分配——
			// 用户裁决 2026-08-14 热路径约束）；baseURL 局部变量为值拷贝
			//（与现 BaseURL: av.tpl.BaseURL 同成本）。
			baseURL := av.tpl.BaseURL
			if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
				baseURL = *av.acc.BaseURL
			}
			return &Selection{
				TargetKind: TargetKindAccount, TargetID: av.acc.ID,
				AccountID: av.acc.ID, GroupID: groupID, TemplateID: av.tpl.ID,
				BaseURL: baseURL, Format: format,
				UpstreamKey: av.acc.UpstreamKey, CredentialType: av.tpl.CredentialType, Model: mapped,
				StripImageTools: av.tpl.StripImageTools, // W4：模板快照布尔复制（热路径零 DB）
				Ext:             av.acc.Ext,             // T2：账号扩展快照（codex 路由派生 AccountCredential；指针复制零拷贝）
				accountRef:      a,
			}, true
		}
	}
	return nil, false
}

// markLastUsed updates only the selection timestamp while preserving every
// state field published concurrently by the rule worker. The CAS loop is
// intentionally small and allocation-free on the uncontended path.
func markLastUsed(a *accountSnapshot, used time.Time) {
	for {
		current := a.statePtr()
		next := *current
		next.lastUsedAt = &used
		if a.state.CompareAndSwap(current, &next) {
			return
		}
	}
}

func accountExcluded(accountID int64, excluded []int64) bool {
	for _, id := range excluded {
		if id == accountID {
			return true
		}
	}
	return false
}
