package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

// SetGroupAssignments 替换语义设置组的授予用户（PUT /admin/groups/{id}/
// assignments）：完整列表 = 授予结果（未列出即撤销，空数组 = 清空）。
// 幂等（Grant/Revoke 本身幂等）；用户/组必须存在（缺失 → 404）。
// 授予变化影响 key 创建的可选组校验（DB 直读，无需 invalidate——Auth 快照
// 不含授予信息）。
func (s *Service) SetGroupAssignments(ctx context.Context, groupID int64, userIDs []int64) ([]int64, error) {
	if _, err := s.store.GetGroup(ctx, groupID); err != nil {
		return nil, mapRepoErr(err)
	}
	if len(userIDs) > 100 {
		return nil, ErrInvalidInput
	}
	want := make(map[int64]bool, len(userIDs))
	for _, uid := range userIDs {
		if uid <= 0 || want[uid] {
			return nil, ErrInvalidInput // 非法 id / 重复
		}
		want[uid] = true
		if _, err := s.store.GetUser(ctx, uid); err != nil {
			return nil, mapRepoErr(err)
		}
	}
	cur, err := s.store.ListAssignmentsByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	have := make(map[int64]bool, len(cur))
	for _, a := range cur {
		have[a.UserID] = true
	}
	for uid := range want {
		if !have[uid] {
			if err := s.store.GrantGroup(ctx, groupID, uid); err != nil {
				return nil, err
			}
		}
	}
	for _, a := range cur {
		if !want[a.UserID] {
			if err := s.store.RevokeGroup(ctx, groupID, a.UserID); err != nil {
				return nil, err
			}
		}
	}
	if s.log != nil {
		s.log.Info("group assignments set", logx.Int64("group_id", groupID), logx.Int64("count", int64(len(userIDs))))
	}
	return userIDs, nil
}

// ListGroupsForUser 用户可选组列表（public 全部 + 已授予 private；/user/groups
// 只读，key 创建时选组）。
func (s *Service) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return s.store.ListGroupsForUser(ctx, userID)
}
