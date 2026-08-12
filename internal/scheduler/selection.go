// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// Select 按预生成调度路径（格式硬过滤 + 模型偏好 + 加权轮询序列）选号，并占用并发槽。
// 路径在快照重建时生成（buildRoutes），本函数热路径只做 O(1) 桶查找 + 序列游标取用
// + 动态状态检查（冷却/禁用/并发满，atomic 读）+ CAS 抢占。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
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
	rt, ok := gs.routes[routeKey{format, model}]
	if !ok {
		// 未知模型：回落默认桶（默认格式 tier2 语义）
		rt, ok = gs.routes[routeKey{format, ""}]
	}
	if !ok {
		return nil, ErrFormatUnavailable
	}
	now := s.timeNow()
	if rt.tier1 != nil {
		if sel, ok := s.pickFrom(rt.tier1, format, model, now); ok {
			return sel, nil
		}
	}
	if rt.tier2 != nil {
		if sel, ok := s.pickFrom(rt.tier2, format, model, now); ok {
			return sel, nil
		}
	}
	return nil, ErrNoAvailable
}

// pickFrom 沿预生成序列扫描候选：游标取模 + 动态状态检查 + CAS 抢占。
// 扫描上限 = 序列一轮（每候选检查一次）；全不可用/全竞争失败返回 false。
func (s *Scheduler) pickFrom(ws *weightedSeq, format domain.RequestFormat, model string, now time.Time) (*Selection, bool) {
	n := len(ws.seq)
	if n == 0 {
		return nil, false
	}
	for i := 0; i < n; i++ {
		a := ws.seq[int(ws.cursor.Add(1))%n]
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue
		}
		cur := a.concurrency.Load()
		if cur >= int64(a.acc.MaxConcurrency) {
			continue
		}
		if a.concurrency.CompareAndSwap(cur, cur+1) {
			mapped := model
			if m, ok := a.tpl.ModelMapping[model]; ok {
				mapped = m
			}
			used := s.timeNow()
			st2 := *st
			st2.lastUsedAt = &used
			a.state.Store(&st2)
			return &Selection{
				AccountID: a.acc.ID, TemplateID: a.tpl.ID,
				BaseURL: a.tpl.BaseURL, Format: format,
				UpstreamKey: a.acc.UpstreamKey, CredentialType: a.tpl.CredentialType, Model: mapped,
				StripImageTools: a.tpl.StripImageTools, // W4：模板快照布尔复制（热路径零 DB）
			}, true
		}
	}
	return nil, false
}
