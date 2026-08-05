package scheduler

import (
	"math/rand/v2"

	"go-proxy-mini/internal/domain"
)

// Select 按硬过滤（格式）+ 模型偏好 + 加权随机选号，并占用并发槽。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	gs, ok := groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	now := s.timeNow()
	var (
		// found 标记是否有账号通过格式硬过滤（可能正处冷却/并发满/禁用）。
		// 空池时区分：格式不匹配 → ErrFormatUnavailable；候选存在但暂时不可用 → ErrNoAvailable。
		found        bool
		tier1, tier2 []*accountSnapshot
	)
	for _, a := range gs.accounts {
		if a.tpl == nil {
			continue
		}
		// 硬过滤：format_for(model) == 请求格式（未声明模型回落到默认格式）
		if a.tpl.FormatFor(model) != format {
			continue
		}
		found = true
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue // 冷却未过期
		}
		if a.concurrency.Load() >= int64(a.acc.MaxConcurrency) {
			continue // 并发满
		}
		if a.tpl.Serves(model) {
			tier1 = append(tier1, a)
		} else {
			tier2 = append(tier2, a)
		}
	}
	pool := tier1
	if len(pool) == 0 {
		pool = tier2
	}
	if len(pool) == 0 {
		if found {
			return nil, ErrNoAvailable
		}
		return nil, ErrFormatUnavailable
	}
	for _, a := range s.weightedOrder(pool) {
		// CAS 抢占并发槽
		for {
			cur := a.concurrency.Load()
			if cur >= int64(a.acc.MaxConcurrency) {
				break
			}
			if a.concurrency.CompareAndSwap(cur, cur+1) {
				st := a.statePtr()
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
					UpstreamKey: a.acc.UpstreamKey, Model: mapped,
				}, nil
			}
		}
	}
	return nil, ErrNoAvailable
}

// weightedOrder 按 score = weight × (1 − errRate) 做加权随机排列（不放回）。
// 随机源用 math/rand/v2 顶层函数（并发安全，Go 1.26 现代特性），无全局 RNG 锁。
func (s *Scheduler) weightedOrder(pool []*accountSnapshot) []*accountSnapshot {
	remaining := make([]*accountSnapshot, len(pool))
	copy(remaining, pool)
	out := make([]*accountSnapshot, 0, len(pool))
	for len(remaining) > 0 {
		total := 0.0
		for _, a := range remaining {
			total += a.score()
		}
		pick := rand.Float64() * total
		acc := 0.0
		idx := 0
		for i, a := range remaining {
			acc += a.score()
			if acc >= pick || i == len(remaining)-1 {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}
