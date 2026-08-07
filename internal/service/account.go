package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
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
	s.invalidate()
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
	updated, err := s.store.UpdateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	if a.GroupIDs != nil {
		// nil = 不变；非 nil = 替换（含空数组 = 清空）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, a.ID, *a.GroupIDs)); err != nil {
			return nil, err
		}
	}
	s.invalidate()
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
	if err := mapRepoErr(s.store.DeleteAccount(ctx, id)); err != nil {
		return err // 404 缺 id（与批量语义对齐）
	}
	s.invalidate()
	return nil
}

func (s *Service) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := mapRepoErr(s.store.DeleteAccountsBatch(ctx, ids)); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func (s *Service) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := validateAccountPatch(p); err != nil {
		return err
	}
	if err := mapRepoErr(s.store.UpdateAccountsBatch(ctx, ids, p)); err != nil {
		return err
	}
	s.invalidate()
	return nil
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
