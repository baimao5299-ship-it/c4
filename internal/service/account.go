// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

func (s *Service) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	if _, err := s.store.GetTemplate(ctx, a.TemplateID); err != nil {
		return nil, mapRepoErr(err) // 模板缺 id → 404
	}
	if a.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *a.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	created, err := s.store.CreateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	if a.GroupIDs != nil {
		// 创建才有 id；替换语义（含空数组 = 清空，对新建账号即无分组）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, created.ID, *a.GroupIDs)); err != nil {
			return nil, err
		}
	}
	// O2 组级定向：新账号进其分组快照（无分组账号不入任何快照 → 空集 no-op）。
	s.inv.Accounts(groupsOf(a), false)
	s.publish(ctx, notify.Change{Groups: groupsOf(a)})
	return created, nil
}

func (s *Service) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	a, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return a, nil
}

func (s *Service) ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error) {
	if err := validateListQuery(q, listSortFields["accounts"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListAccounts(ctx, q)
}

func (s *Service) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	if a.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *a.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	// O2 组级定向：变更前取旧组（账号移组 A→B 时 A、B 两组快照都要重载——
	// 旧组移除账号、新组加入账号）+ 旧 upstream_key 比较（变更 → clients
	// 失效）。查询失败 → 空集 + Warn（调度器 ≤30s 同步兜底）。
	oldGroups, gErr := s.store.GetAccountGroups(ctx, a.ID)
	keyChanged := false
	recovered := false // T5 失效恢复审计：此前已失效（failed_at 置位）→ status→active 恢复
	if cur, err := s.store.GetAccount(ctx, a.ID); err == nil {
		keyChanged = cur.UpstreamKey != a.UpstreamKey
		recovered = cur.FailedAt != nil && a.Status == domain.StatusActive
	}
	updated, err := s.store.UpdateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	if recovered && s.log != nil {
		// T5 §4 恢复操作审计（日志面）：status→active 隐含清 failed_at +
		// last_error（repo 层执行），此处留痕恢复动作。
		s.log.Info("account failure cleared (status->active)", logx.Int64("account_id", a.ID))
	}
	if a.GroupIDs != nil {
		// nil = 不变；非 nil = 替换（含空数组 = 清空）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, a.ID, *a.GroupIDs)); err != nil {
			return nil, err
		}
	}
	gids := oldGroups
	if a.GroupIDs != nil {
		gids = append(gids, (*a.GroupIDs)...)
	}
	if gErr != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", a.ID), logx.Error(gErr))
	}
	s.inv.Accounts(gids, keyChanged)
	s.publish(ctx, notify.Change{Groups: gids, Clients: keyChanged}) // upstream_key 变更 → clients 失效
	return updated, nil
}

// GetAccountGroups 账号的分组 id 列表（编辑回显）。账号缺 id → 404。
func (s *Service) GetAccountGroups(ctx context.Context, id int64) ([]int64, error) {
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		return nil, mapRepoErr(err)
	}
	return s.store.GetAccountGroups(ctx, id)
}

// checkGroupsExist 校验分组全部存在（缺失 → service.ErrNotFound 含 id）。
func (s *Service) checkGroupsExist(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := s.store.GetGroup(ctx, id); err != nil {
			return mapRepoErr(err)
		}
	}
	return nil
}

func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	// O2：删除前查旧组（删除后快照须移除该账号）。
	gids, err := s.store.GetAccountGroups(ctx, id)
	if err != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
	}
	if err := mapRepoErr(s.store.DeleteAccount(ctx, id)); err != nil {
		return err // 404 缺 id（与批量语义对齐）
	}
	s.inv.Accounts(gids, false)
	s.publish(ctx, notify.Change{Groups: gids})
	return nil
}

func (s *Service) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	// O2：删除前逐个查旧组（组级定向并集）。
	var gids []int64
	for _, id := range ids {
		gs, err := s.store.GetAccountGroups(ctx, id)
		if err != nil {
			if s.log != nil {
				s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
			}
			continue
		}
		gids = append(gids, gs...)
	}
	if err := mapRepoErr(s.store.DeleteAccountsBatch(ctx, ids)); err != nil {
		return err
	}
	s.inv.Accounts(gids, false)
	s.publish(ctx, notify.Change{Groups: gids})
	return nil
}

func (s *Service) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := validateAccountPatch(p); err != nil {
		return err
	}
	// O2：变更前逐个查旧组 + 替换目标组并集（upstream_key 批量变更 →
	// clients 失效）。
	var gids []int64
	for _, id := range ids {
		gs, err := s.store.GetAccountGroups(ctx, id)
		if err != nil {
			if s.log != nil {
				s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
			}
			continue
		}
		gids = append(gids, gs...)
	}
	if p.GroupIDs != nil {
		gids = append(gids, (*p.GroupIDs)...)
	}
	if err := mapRepoErr(s.store.UpdateAccountsBatch(ctx, ids, p)); err != nil {
		return err
	}
	if p.Status != nil && *p.Status == domain.StatusActive && s.log != nil {
		// T5 §4 恢复操作审计（批量）：status→active 隐含清 failed_at +
		// last_error（repo 层执行——批量路径不做逐账号旧值比较，操作级留痕）。
		s.log.Info("account failure cleared (batch status->active)", logx.Int("count", len(ids)))
	}
	// 评审 I-3：nil = 未提供；空串 = 清除 upstream_key（同为变更语义）。
	// 批量路径不做逐账号旧值比较（需 N 次 GetAccount），只要提供了
	// UpstreamKey 就保守标记 clients 失效——clients 失效成本远低于旧 key
	// 滞留风险（宁可多失效一次）。
	s.inv.Accounts(gids, p.UpstreamKey != nil)
	s.publish(ctx, notify.Change{Groups: gids, Clients: p.UpstreamKey != nil})
	return nil
}

// groupsOf 账号分组 id 列表（nil = 无分组）。
func groupsOf(a *domain.Account) []int64 {
	if a.GroupIDs == nil {
		return nil
	}
	return *a.GroupIDs
}

// AccountView 是账号的管理端视图（含调度器运行时信息）。
type AccountView struct {
	*domain.Account
	Concurrency int64   `json:"concurrency"`
	ErrRate     float64 `json:"err_rate"`
	ErrCount    int     `json:"err_count"`
}

// ListAccountViews 账号管理端视图（含调度器运行时信息）。handler 列表入口，
// 与 ListAccounts 一致做 sort/order 校验（非法 → ErrInvalidInput → 400）。
func (s *Service) ListAccountViews(ctx context.Context, q repository.ListQuery) ([]*AccountView, int64, error) {
	if err := validateListQuery(q, listSortFields["accounts"]); err != nil {
		return nil, 0, err
	}
	accs, total, err := s.store.ListAccounts(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*AccountView, 0, len(accs))
	for _, a := range accs {
		v := &AccountView{Account: a}
		if s.sched != nil {
			if ri, ok := s.sched.Runtime(a.ID); ok {
				v.Concurrency, v.ErrRate, v.ErrCount = ri.Concurrency, ri.ErrRate, ri.ErrCount
			}
		}
		out = append(out, v)
	}
	return out, total, nil
}
