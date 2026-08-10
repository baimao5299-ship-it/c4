package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/notify"
	"go-proxy-mini/pkg/logx"
)

// SetGroupAssignments 替换语义设置组的授予用户（PUT /admin/groups/{id}/
// assignments）：完整列表 = 授予结果（未列出即撤销，空数组 = 清空）。
// 幂等（Grant/Revoke 本身幂等）；用户/组必须存在（缺失 → 404）。
// mults 可选：user_id → 该用户在该组的专属价格倍率（万分数，T3.5 修正：按
// 组——用户在不同组可有不同倍率；nil 值 = 清除为未设置 → 回退组倍率；0 =
// 免费）。mults 的 key 必须 ⊆ userIDs（未列出的用户不改动既有倍率；未知用户
// → 400）。返回生效的 user_ids 列表 + 该组各授予用户的 post-state 倍率
// （user_ids 全量，nil = 未设置；response 回显用）。
// 授予/倍率变更影响计费倍率快照 → Multipliers() 定向刷新（assignment 倍率
// 变更不依赖全量 Reload）。
func (s *Service) SetGroupAssignments(ctx context.Context, groupID int64, userIDs []int64, mults map[int64]*int) ([]int64, map[int64]*int, error) {
	if _, err := s.store.GetGroup(ctx, groupID); err != nil {
		return nil, nil, mapRepoErr(err)
	}
	if len(userIDs) > 100 {
		return nil, nil, ErrInvalidInput
	}
	want := make(map[int64]bool, len(userIDs))
	for _, uid := range userIDs {
		if uid <= 0 || want[uid] {
			return nil, nil, ErrInvalidInput // 非法 id / 重复
		}
		want[uid] = true
		if _, err := s.store.GetUser(ctx, uid); err != nil {
			return nil, nil, mapRepoErr(err)
		}
	}
	for uid, m := range mults {
		if !want[uid] {
			return nil, nil, ErrInvalidInput // multipliers key 必须 ∈ user_ids
		}
		if m != nil && (*m < 0 || *m > 100000) {
			return nil, nil, ErrInvalidInput // 万分数 0~100000（API 边界正常值 0~10 换算）
		}
	}
	cur, err := s.store.ListAssignmentsByGroup(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	have := make(map[int64]bool, len(cur))
	oldMult := make(map[int64]*int, len(cur))
	for _, a := range cur {
		have[a.UserID] = true
		oldMult[a.UserID] = a.PriceMultiplier
	}
	for uid := range want {
		if !have[uid] {
			if err := s.store.GrantGroup(ctx, groupID, uid); err != nil {
				return nil, nil, err
			}
		}
	}
	for uid, m := range mults {
		if err := s.store.SetAssignmentMultiplier(ctx, groupID, uid, m); err != nil {
			return nil, nil, err
		}
	}
	for _, a := range cur {
		if !want[a.UserID] {
			if err := s.store.RevokeGroup(ctx, groupID, a.UserID); err != nil {
				return nil, nil, err
			}
		}
	}
	// post-state 倍率：user_ids 全量（未在 mults 的用户沿用旧值；新授予未设 → nil）
	post := make(map[int64]*int, len(userIDs))
	for _, uid := range userIDs {
		if m, ok := mults[uid]; ok {
			post[uid] = m
			continue
		}
		post[uid] = oldMult[uid]
	}
	s.inv.Multipliers()
	s.publish(ctx, notify.Change{Multipliers: true})
	if s.log != nil {
		s.log.Info("group assignments set", logx.Int64("group_id", groupID), logx.Int64("count", int64(len(userIDs))))
	}
	return userIDs, post, nil
}

// ListGroupsForUser 用户可选组列表（public 全部 + 已授予 private；/user/groups
// 只读，key 创建时选组）。
func (s *Service) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return s.store.ListGroupsForUser(ctx, userID)
}
