package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

func (s *Service) CreateGroup(ctx context.Context, name string) (*domain.Group, string, error) {
	if name == "" {
		return nil, "", ErrInvalidInput
	}
	raw, hash, prefix := cryptox.NewGroupKey()
	g := &domain.Group{Name: name, KeyHash: hash, KeyPrefix: prefix}
	created, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return nil, "", err
	}
	if s.keys != nil {
		s.keys.Upsert(hash, created.ID)
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("group created", logx.Int64("id", created.ID), logx.String("name", name))
	}
	return created, raw, nil
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

func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return mapRepoErr(err)
	}
	if s.keys != nil {
		s.keys.Delete(g.KeyHash)
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
		g, err := s.store.GetGroup(ctx, id)
		if err != nil {
			return mapRepoErr(err) // 404 缺 id
		}
		if s.keys != nil {
			s.keys.Delete(g.KeyHash)
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
	if err := mapRepoErr(s.store.UpdateGroupsBatch(ctx, ids, p)); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// RotateGroupKey 轮换客户端 key：返回新 raw key（仅此一次明文）。
func (s *Service) RotateGroupKey(ctx context.Context, groupID int64) (string, error) {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return "", mapRepoErr(err) // 缺 id → 404
	}
	oldHash := g.KeyHash
	raw, hash, prefix := cryptox.NewGroupKey()
	g.KeyHash = hash
	g.KeyPrefix = prefix
	if _, err := s.store.UpdateGroup(ctx, g); err != nil {
		return "", err
	}
	if s.keys != nil {
		s.keys.Delete(oldHash)
		s.keys.Upsert(hash, groupID)
	}
	s.invalidate()
	return raw, nil
}
