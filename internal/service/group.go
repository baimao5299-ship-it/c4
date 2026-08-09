package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

// CreateGroup 创建分组（平台容量池）。priceMultiplier 万分数：0 = 未指定
// （repo 落库组默认 10000 = ×1）；1~100000 显式写入（免费组 0 经 UpdateGroup
// 设置）；超界 → 400。创建后 Multipliers()：新组倍率须即刻进余额倍率快照
// （缺失 = ×1 计费窗口，评审 M-1 组倍率矩阵——组创建即倍率设定）。
func (s *Service) CreateGroup(ctx context.Context, name string, visibility domain.GroupVisibility, priceMultiplier int) (*domain.Group, error) {
	if name == "" {
		return nil, ErrInvalidInput
	}
	if !visibility.Valid() {
		visibility = domain.GroupVisibilityPublic
	}
	if priceMultiplier < 0 || priceMultiplier > 100000 {
		return nil, ErrInvalidInput
	}
	g := &domain.Group{Name: name, Visibility: visibility, PriceMultiplier: priceMultiplier}
	created, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return nil, mapRepoErr(err) // name 唯一冲突 → ErrConflict（409）
	}
	s.inv.Multipliers()
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
	if g.PriceMultiplier < 0 || g.PriceMultiplier > 100000 {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateGroup(ctx, g)
	if err != nil {
		return nil, mapRepoErr(err) // 改名撞已有 name → ErrConflict（409）
	}
	// O2 组倍率矩阵：倍率变更 → 余额倍率快照定向刷新（名字/可见性变更不触发
	// 任何快照，此处保守一并标记——去抖窗口内一次小表单查，可忽略）。
	s.inv.Multipliers()
	return updated, nil
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
	// O2：组删除后倍率快照清理（陈旧条目无害；保守标记——组变更统一走倍率
	// 定向刷新）。组内账号由 FK 约束保证为空（ent 默认无级联，删除含账号的
	// 组 → 仓库错误）→ 调度器快照不受组删除影响。
	s.inv.Multipliers()
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
	s.inv.Multipliers()
	return nil
}

// UpdateGroupsBatch 批量更新组（仅 name/visibility——GroupPatch 无倍率字段，
// 不触发任何快照重载；倍率批量变更走单组 UpdateGroup）。
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
	return mapRepoErr(s.store.UpdateGroupsBatch(ctx, ids, p))
}
