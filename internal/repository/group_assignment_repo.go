package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/group"
	"go-proxy-mini/internal/ent/groupassignment"
)

// GroupAssignmentRepo private 组授予（用户 ↔ 组多对多）。
type GroupAssignmentRepo struct{ client *ent.Client }

// Grant 授予；联合唯一 (group_id, user_id) 兜底幂等（重复授予不报错）。
func (r *GroupAssignmentRepo) Grant(ctx context.Context, groupID, userID int64) error {
	_, err := r.client.GroupAssignment.Create().
		SetGroupID(groupID).
		SetUserID(userID).
		OnConflictColumns("group_id", "user_id").
		Ignore().
		ID(ctx)
	return err
}

// Revoke 撤销（不存在时幂等成功）。
func (r *GroupAssignmentRepo) Revoke(ctx context.Context, groupID, userID int64) error {
	_, err := r.client.GroupAssignment.Delete().
		Where(groupassignment.GroupIDEQ(groupID), groupassignment.UserIDEQ(userID)).
		Exec(ctx)
	return err
}

func (r *GroupAssignmentRepo) ListByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.UserIDEQ(userID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.GroupAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroupAssignment(row))
	}
	return out, nil
}

func (r *GroupAssignmentRepo) ListByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.GroupIDEQ(groupID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.GroupAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroupAssignment(row))
	}
	return out, nil
}

// ListGroupsForUser 用户可选组：public 全部 + 已授予的 private（/user/groups
// 只读列表）。
func (r *GroupAssignmentRepo) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	rows, err := r.client.Group.Query().
		Where(group.Or(
			group.VisibilityEQ(group.VisibilityPublic),
			group.HasAssignmentsWith(groupassignment.UserIDEQ(userID)),
		)).
		Order(ent.Asc(group.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroup(row))
	}
	return out, nil
}
