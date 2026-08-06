package repository

import (
	"context"
	"errors"
	"fmt"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/account"
	"go-proxy-mini/internal/ent/group"
	"go-proxy-mini/internal/ent/template"
)

// ErrNotFound 批量操作中存在性检查失败（缺失 id）。
var ErrNotFound = errors.New("repository: not found")

// --- 批量更新字段子集（nil 字段 = 不更新） ---

type TemplatePatch struct {
	Name             *string
	BaseURL          *string
	SupportedFormats *[]domain.RequestFormat
	Models           *[]string
	FormatModels     *map[domain.RequestFormat][]string
	ModelMapping     *map[string]string
}

type AccountPatch struct {
	Name           *string
	TemplateID     *int64
	UpstreamKey    *string
	Status         *domain.AccountStatus
	Weight         *int
	MaxConcurrency *int
}

type GroupPatch struct {
	Name *string
}

// --- 批量删除（事务，全成或全败） ---

func (r *TemplateRepo) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	if err := checkTemplateExist(ctx, tx.Template.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Template.DeleteOneID(id).Exec(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *AccountRepo) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkAccountExist(ctx, tx.Account.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Account.DeleteOneID(id).Exec(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *GroupRepo) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkGroupExist(ctx, tx.Group.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Group.DeleteOneID(id).Exec(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

// --- 批量更新（事务，全成或全败；Set 链只按 patch 非 nil 字段） ---

func (r *TemplateRepo) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkTemplateExist(ctx, tx.Template.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Template.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.BaseURL != nil {
			u = u.SetBaseURL(*p.BaseURL)
		}
		if p.SupportedFormats != nil {
			u = u.SetSupportedFormats(formatsToStrings(*p.SupportedFormats))
		}
		if p.Models != nil {
			u = u.SetModels(*p.Models)
		}
		if p.FormatModels != nil {
			u = u.SetFormatModels(formatModelsToStrings(*p.FormatModels))
		}
		if p.ModelMapping != nil {
			u = u.SetModelMapping(*p.ModelMapping)
		}
		if _, err := u.Save(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *AccountRepo) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkAccountExist(ctx, tx.Account.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Account.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.TemplateID != nil {
			u = u.SetTemplateID(*p.TemplateID)
		}
		if p.UpstreamKey != nil {
			u = u.SetUpstreamKey(*p.UpstreamKey)
		}
		if p.Status != nil {
			u = u.SetStatus(account.Status(*p.Status))
		}
		if p.Weight != nil {
			u = u.SetWeight(*p.Weight)
		}
		if p.MaxConcurrency != nil {
			u = u.SetMaxConcurrency(*p.MaxConcurrency)
		}
		if _, err := u.Save(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *GroupRepo) UpdateGroupsBatch(ctx context.Context, ids []int64, p GroupPatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkGroupExist(ctx, tx.Group.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Group.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if _, err := u.Save(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

// errMissingID 把 per-id 执行错误映射为 ErrNotFound 包装（与 diffMissing 同格式）。
// 覆盖 check→exec 竞态窗口：存在性检查通过后、DeleteOne/UpdateOne 执行前若并发
// 请求已删同 id，ent 对 0 行删除/更新返回 NotFoundError（ent.IsNotFound）。
func errMissingID(err error, id int64) error {
	if ent.IsNotFound(err) {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return err
}

// --- 事务内存在性检查：ids 必须全部存在，否则 ErrNotFound（含缺失 id） ---

// checkTemplateExist 查询实际存在的 ids 与输入比对，拼出第一个缺失 id。
// （ent v0.14 生成的查询无 Ints/Ints64，用等价的 IDs —— 内部即
// q.Select(FieldID).Scan(&ids)。）
func checkTemplateExist(ctx context.Context, q *ent.TemplateQuery, ids []int64) error {
	existing, err := q.Where(template.IDIn(ids...)).IDs(ctx)
	if err != nil {
		return err
	}
	return diffMissing(existing, ids)
}

func checkAccountExist(ctx context.Context, q *ent.AccountQuery, ids []int64) error {
	existing, err := q.Where(account.IDIn(ids...)).IDs(ctx)
	if err != nil {
		return err
	}
	return diffMissing(existing, ids)
}

func checkGroupExist(ctx context.Context, q *ent.GroupQuery, ids []int64) error {
	existing, err := q.Where(group.IDIn(ids...)).IDs(ctx)
	if err != nil {
		return err
	}
	return diffMissing(existing, ids)
}

// diffMissing 返回 ids 中不在 existing 里的第一个缺失 id 错误。
func diffMissing(existing, ids []int64) error {
	seen := make(map[int64]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return nil
}

