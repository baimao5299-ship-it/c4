package service

import (
	"context"

	"go-proxy-mini/internal/domain"
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
	return s.store.GetGroup(ctx, id)
}

func (s *Service) ListGroups(ctx context.Context) ([]*domain.Group, error) {
	return s.store.ListGroups(ctx)
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
		return err
	}
	if s.keys != nil {
		s.keys.Delete(g.KeyHash)
	}
	if err := s.store.DeleteGroup(ctx, id); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func (s *Service) SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	if err := s.store.SetGroupAccounts(ctx, groupID, accountIDs); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// RotateGroupKey 轮换客户端 key：返回新 raw key（仅此一次明文）。
func (s *Service) RotateGroupKey(ctx context.Context, groupID int64) (string, error) {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return "", err
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
