package service

import (
	"context"
	"fmt"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

// ErrGroupNotEligible private 组未授予（key 创建组可选性校验；包装
// ErrInvalidInput → 400）。
var ErrGroupNotEligible = fmt.Errorf("%w: group is private and not granted to user", ErrInvalidInput)

// CreateKey 用户自建 key（/user/keys POST）：
// 组可选性校验（public 或已授予 private）→ cryptox 生成 raw/hash/prefix →
// 落库 → Auth 增量 Upsert（KeyMeta 含用户门禁字段，管理路径 1 次 GetUser）。
// raw 明文仅本次返回。
func (s *Service) CreateKey(ctx context.Context, userID int64, name string, groupID int64, maxConcurrency int, quota int64) (*domain.Key, string, error) {
	if name == "" || groupID <= 0 || maxConcurrency < 0 || quota < 0 {
		return nil, "", ErrInvalidInput
	}
	if err := s.checkGroupEligible(ctx, userID, groupID); err != nil {
		return nil, "", err
	}
	raw, hash, prefix := cryptox.NewGroupKey()
	created, err := s.store.CreateKey(ctx, &domain.Key{
		UserID: userID, GroupID: groupID, Name: name,
		KeyHash: hash, KeyPrefix: prefix,
		Status: domain.KeyStatusActive, MaxConcurrency: maxConcurrency,
		Quota: quota, QuotaUsed: 0,
	})
	if err != nil {
		return nil, "", err
	}
	if err := s.upsertKeyMeta(ctx, created); err != nil {
		return nil, "", err
	}
	if s.log != nil {
		s.log.Info("key created", logx.Int64("id", created.ID), logx.Int64("user_id", userID), logx.String("name", name))
	}
	return created, raw, nil
}

// checkGroupEligible 组可选性：组必须存在（缺失 → 404）；private 组须有
// 授予记录（未授予 → 400，防越权使用专属容量池）。
func (s *Service) checkGroupEligible(ctx context.Context, userID, groupID int64) error {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return mapRepoErr(err)
	}
	if g.Visibility == domain.GroupVisibilityPublic {
		return nil
	}
	assignments, err := s.store.ListAssignmentsByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if a.GroupID == groupID {
			return nil
		}
	}
	return ErrGroupNotEligible
}

// GetKey 用户取自己的 key 详情（/user/keys/{id} GET）。
func (s *Service) GetKey(ctx context.Context, userID, keyID int64) (*domain.Key, error) {
	return s.ownedKey(ctx, userID, keyID)
}

// ListKeys 用户自己的 key 列表（/user/keys GET）。
func (s *Service) ListKeys(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.Key, int64, error) {
	if err := validateListQuery(q, listSortFields["keys"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListKeysByUser(ctx, userID, q)
}

// UpdateKey 更新自己的 key（name/status/max_concurrency/quota；nil 字段不变）。
// 变更后 Auth 增量 Upsert（禁用/额度调整即时生效——评审 I-2 的 key 级路径）。
func (s *Service) UpdateKey(ctx context.Context, userID, keyID int64, name *string, status *domain.KeyStatus, maxConcurrency *int, quota *int64) (*domain.Key, error) {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return nil, err
	}
	if name != nil {
		if *name == "" {
			return nil, ErrInvalidInput
		}
		cur.Name = *name
	}
	if status != nil {
		if !status.Valid() {
			return nil, ErrInvalidInput
		}
		cur.Status = *status
	}
	if maxConcurrency != nil {
		if *maxConcurrency < 0 {
			return nil, ErrInvalidInput
		}
		cur.MaxConcurrency = *maxConcurrency
	}
	if quota != nil {
		if *quota < 0 {
			return nil, ErrInvalidInput
		}
		cur.Quota = *quota
	}
	updated, err := s.store.UpdateKey(ctx, cur)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if err := s.upsertKeyMeta(ctx, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

// RotateKey 轮换自己的 key（/user/keys/{id}/rotate）：新 raw 仅返回一次；
// 旧 hash 增量移除（立即失效）、新 hash 增量注册。
func (s *Service) RotateKey(ctx context.Context, userID, keyID int64) (string, *domain.Key, error) {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return "", nil, err
	}
	raw, hash, prefix := cryptox.NewGroupKey()
	updated, err := s.store.RotateKey(ctx, keyID, hash, prefix)
	if err != nil {
		return "", nil, mapRepoErr(err)
	}
	if s.keys != nil {
		s.keys.Delete(cur.KeyHash)
	}
	if err := s.upsertKeyMeta(ctx, updated); err != nil {
		return "", nil, err
	}
	if s.log != nil {
		s.log.Info("key rotated", logx.Int64("id", keyID), logx.Int64("user_id", userID))
	}
	return raw, updated, nil
}

// DeleteKey 删除自己的 key（/user/keys/{id} DELETE；Auth 增量移除——旧 hash
// 立即失效）。
func (s *Service) DeleteKey(ctx context.Context, userID, keyID int64) error {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteKey(ctx, keyID); err != nil {
		return mapRepoErr(err)
	}
	if s.keys != nil {
		s.keys.Delete(cur.KeyHash)
	}
	return nil
}

// ownedKey 取 key 并校验归属：非本人 key 一律按不存在处理（404，防越权探测
// 他人 key 存在性）。
func (s *Service) ownedKey(ctx context.Context, userID, keyID int64) (*domain.Key, error) {
	k, err := s.store.GetKey(ctx, keyID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if k.UserID != userID {
		return nil, ErrNotFound
	}
	return k, nil
}

// upsertKeyMeta 构造 KeyMeta 并增量注册到 Auth 鉴权快照（key 创建/更新/轮换
// 后调用）。用户门禁字段需 GetUser——管理路径 1 次查询，热路径零影响。
func (s *Service) upsertKeyMeta(ctx context.Context, k *domain.Key) error {
	if s.keys == nil {
		return nil
	}
	meta := domain.KeyMeta{
		KeyID: k.ID, UserID: k.UserID, GroupID: k.GroupID,
		KeyStatus: k.Status, KeyMaxConc: k.MaxConcurrency,
		HasQuota: k.HasQuota(), Quota: k.Quota, QuotaUsed: k.QuotaUsed,
	}
	u, err := s.store.GetUser(ctx, k.UserID)
	if err != nil {
		return mapRepoErr(err)
	}
	meta.UserStatus = u.Status
	meta.UserMaxConc = u.MaxConcurrency
	s.keys.Upsert(k.KeyHash, meta)
	return nil
}
