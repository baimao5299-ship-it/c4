// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
)

type AccountRepo struct{ client *ent.Client }

func (r *AccountRepo) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.Create().
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Account.Query().Where(account.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(account.NameContainsFold(q.Name))
	}
	if len(q.StatusList) > 0 {
		sts, err := toAccountStatusList(q.StatusList)
		if err != nil {
			return nil, 0, err
		}
		pred = pred.Where(account.StatusIn(sts...))
	}
	if q.TemplateID > 0 {
		pred = pred.Where(account.TemplateIDEQ(q.TemplateID))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(accountSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.WithTemplate().Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAccount(row))
	}
	return out, int64(total), nil
}

// toAccountStatusList 校验并转换多值 status 筛选。ent 生成的枚举没有 Valid() 方法
// （只有 StatusValidator），对照枚举常量校验；非法值返回 error（handler 已校验，repo 兜底）。
func toAccountStatusList(list []string) ([]account.Status, error) {
	out := make([]account.Status, 0, len(list))
	for _, s := range list {
		st := account.Status(s)
		switch st {
		case account.StatusActive, account.StatusUnhealthy, account.Status429, account.StatusDisabled:
		default:
			return nil, fmt.Errorf("invalid account status %q", s)
		}
		out = append(out, st)
	}
	return out, nil
}

func (r *AccountRepo) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.UpdateOneID(a.ID).
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		SetStatus(account.Status(a.Status)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

// DeleteAccount 软删除：deleted_at 置值（行保留留审计；调度器快照按
// deleted_at IS NULL 过滤，GET 单个仍可查已删项）。bulk Update（无 re-SELECT）
// 单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *AccountRepo) DeleteAccount(ctx context.Context, id int64) error {
	n, err := r.client.Account.Update().Where(account.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// SetAccountGroups 替换账号的全部分组（替换语义：给定集合 = 账号全部分组；
// 空数组 = 清空）。组 id 先做存在性校验（缺失 → ErrNotFound 含 id）；
// 账号缺 id → ErrNotFound（errMissingID）。
func (r *AccountRepo) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	if len(groupIDs) > 0 {
		if err := checkGroupExist(ctx, r.client.Group.Query, groupIDs); err != nil {
			return err
		}
	}
	_, err := r.client.Account.UpdateOneID(accountID).
		ClearGroups().
		AddGroupIDs(groupIDs...).
		Save(ctx)
	return errMissingID(err, accountID)
}

// GetAccountGroups 读取账号的全部分组 id（编辑回显专用端点数据源；
// 不 eager-load，GetAccount/ListAccounts 读路径不加 groups edge）。
// 账号是否存在由调用方（service.GetAccountGroups 先 GetAccount）负责——
// 本方法对不存在账号返回空集而非错误。
func (r *AccountRepo) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	return r.client.Account.Query().
		Where(account.ID(accountID)).
		QueryGroups().
		IDs(ctx)
}

// UpdateAccountStatus 满足 scheduler.Loader：状态/冷却/错误信息回写；weight 非 nil
// 时一并更新（规则引擎权重动作，nil = 不动 weight）。
func (r *AccountRepo) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string, weight *int) error {
	u := r.client.Account.UpdateOneID(id).SetStatus(account.Status(status))
	if cooldownUntil != nil {
		u = u.SetCooldownUntil(*cooldownUntil)
	} else {
		u = u.ClearCooldownUntil()
	}
	if lastError != nil {
		u = u.SetLastError(*lastError)
	} else {
		u = u.ClearLastError()
	}
	if weight != nil {
		u = u.SetWeight(*weight)
	}
	_, err := u.Save(ctx)
	return err
}

// SetAccountFailed 幂等写账号失效（SDK 接入 T1——统一失效回调处理链第一步）：
// failed_at + last_error（失效原因文本，复用既有 last_error——用户裁决
// 2026-08-13：两原因字段并存会漂移；失效后账号摘除不再被调度 → caller.go
// 的普通失败写点不会覆盖失效原因，复用安全）首写生效——failed_at 已置（首次
// 上报）→ 0 行不覆盖（保持首次失效时刻与原因；重复上报不重复写；T5 恢复由
// 管理面清 failed_at + last_error）。**空 reason 不清旧值**（P3-2 评审：
// 空原因上报不触碰既有 last_error——保持"最近错误"审计语义；调度回写携带的
// 快照旧值与 DB 一致，不互相覆盖）。不触碰 status（与 disabled 语义分离：
// disabled = 管理面手动禁用；调度摘除走 scheduler.FailAccount 经 loader 落库）。
// 账号不存在 → 0 行不报错（审计性写入，无对象可写；调度摘除亦会因快照外账号 no-op）。
func (r *AccountRepo) SetAccountFailed(ctx context.Context, id int64, failedAt time.Time, reason string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.FailedAtIsNil()).
		SetFailedAt(failedAt)
	if reason != "" {
		u = u.SetLastError(reason)
	}
	_, err := u.Save(ctx)
	return err
}
