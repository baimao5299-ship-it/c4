package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

func (s *Service) CreateGroup(ctx context.Context, name string, visibility domain.GroupVisibility) (*domain.Group, error) {
	if name == "" {
		return nil, ErrInvalidInput
	}
	if !visibility.Valid() {
		visibility = domain.GroupVisibilityPublic
	}
	g := &domain.Group{Name: name, Visibility: visibility}
	created, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("group created", logx.Int64("id", created.ID), logx.String("name", name))
	}
	return created, nil
}

func (s *Service) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return g, nil
}

func (s *Service) ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error) {
	if err := validateListQuery(q, listSortFields["groups"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListGroups(ctx, q)
}

func (s *Service) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	if g.Name == "" {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateGroup(ctx, g)
	if err == nil {
		s.invalidate()
	}
	return updated, err
}

// DeleteGroup 删除组：前置清理组内全部 key（key.group_id 外键约束；Auth 增量
// 清理），再删组。key 清理与组删除非同一事务——组删除失败时 key 已删，
// 重试删除即可（key 被删组未删的中间态不提供服务——Auth 快照已移除）。
func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	if _, err := s.store.GetGroup(ctx, id); err != nil {
		return mapRepoErr(err)
	}
	hashes, err := s.store.DeleteKeysByGroup(ctx, id)
	if err != nil {
		return err
	}
	if s.keys != nil {
		for _, h := range hashes {
			s.keys.Delete(h)
		}
	}
	if err := s.store.DeleteGroup(ctx, id); err != nil {
		return mapRepoErr(err) // 竞态窗口缺 id → 404（前置 Get 已拦截常见路径）
	}
	s.invalidate()
	return nil
}

func (s *Service) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.store.GetGroup(ctx, id); err != nil {
			return mapRepoErr(err) // 404 缺 id
		}
		hashes, err := s.store.DeleteKeysByGroup(ctx, id)
		if err != nil {
			return err
		}
		if s.keys != nil {
			for _, h := range hashes {
				s.keys.Delete(h)
			}
		}
	}
	if err := mapRepoErr(s.store.DeleteGroupsBatch(ctx, ids)); err != nil {
		return err // 事务回滚；key 已删但 DB 未删——与单删同性质（失败自愈：DB 仍在则 key 下次重载恢复）
	}
	s.invalidate()
	return nil
}

func (s *Service) UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.Visibility != nil && !p.Visibility.Valid() {
		return ErrInvalidInput
	}
	if err := mapRepoErr(s.store.UpdateGroupsBatch(ctx, ids, p)); err != nil {
		return err
	}
	s.invalidate()
	return nil
}
