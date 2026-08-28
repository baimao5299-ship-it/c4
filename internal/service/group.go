// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// CreateGroup 创建分组（平台容量池）。priceMultiplier 万分数：nil = 未指定
// （归一 10000 = ×1，恒写入——API 边界 nullable 可表达显式 0 = 免费组）；
// 0~100000 显式写入；超界 → 400。protocolConverts：转换方向集合（缺省 nil =
// 不转换）——off 元素归一剔除（空/仅 off → 空数组）；非法方向/重复方向/
// 同客户端格式多方向 → 400。创建后 Multipliers()：新组倍率须即刻进余额倍率
// 快照（缺失 = ×1 计费窗口，评审 M-1 组倍率矩阵——组创建即倍率设定）。
func (s *Service) CreateGroup(ctx context.Context, name string, visibility domain.GroupVisibility, priceMultiplier *int, protocolConverts []domain.ProtocolConvert) (*domain.Group, error) {
	return s.CreateGroupWithRouting(ctx, name, visibility, priceMultiplier, protocolConverts, domain.GroupRoutingModeAccounts, nil)
}

// CreateGroupWithRouting creates a group and persists the selected routing
// source. The legacy CreateGroup entry point delegates here with accounts mode
// so existing callers keep their behavior.
func (s *Service) CreateGroupWithRouting(ctx context.Context, name string, visibility domain.GroupVisibility, priceMultiplier *int, protocolConverts []domain.ProtocolConvert, routingMode domain.GroupRoutingMode, allowedModels []string) (*domain.Group, error) {
	g, err := normalizeGroupInput(name, visibility, priceMultiplier, protocolConverts, routingMode, allowedModels)
	if err != nil {
		return nil, err
	}
	// A group created through this endpoint has no member payload. Do not let
	// an upstream-routed group become live with an empty pool; the UI uses an
	// account-mode compatibility create, writes members, then switches mode.
	if g.RoutingMode == domain.GroupRoutingModeUpstreams {
		return nil, fmt.Errorf("%w: upstream groups require at least one member", ErrInvalidInput)
	}
	created, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return nil, mapRepoErr(err) // name 唯一冲突 → ErrConflict（409）
	}
	s.inv.Multipliers()
	s.invalidateGroups(created.ID)
	s.publish(ctx, notify.Change{Multipliers: true})
	if s.log != nil {
		s.log.Info("group created", logx.Int64("id", created.ID), logx.String("name", name))
	}
	return created, nil
}

func normalizeGroupInput(name string, visibility domain.GroupVisibility, priceMultiplier *int, protocolConverts []domain.ProtocolConvert, routingMode domain.GroupRoutingMode, allowedModels []string) (*domain.Group, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidInput
	}
	if !visibility.Valid() {
		return nil, ErrInvalidInput
	}
	converts, err := normalizeProtocolConverts(protocolConverts)
	if err != nil {
		return nil, err
	}
	mult := 10000
	if priceMultiplier != nil {
		if *priceMultiplier < 0 || *priceMultiplier > 100000 {
			return nil, ErrInvalidInput
		}
		mult = *priceMultiplier
	}
	if routingMode == "" {
		routingMode = domain.GroupRoutingModeAccounts
	}
	if !routingMode.Valid() {
		return nil, ErrInvalidInput
	}
	models, err := normalizeAllowedModels(allowedModels)
	if err != nil {
		return nil, err
	}
	return &domain.Group{Name: strings.TrimSpace(name), Visibility: visibility, RoutingMode: routingMode, AllowedModels: models, PriceMultiplier: mult, ProtocolConverts: converts}, nil
}

func (s *Service) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return g, nil
}

// getGroupLive 取未软删的组（F3 单点：建 key/授 assignment 三调用点共用——
// repo GetGroup 不过滤 deleted_at，软删组不可用的过滤在 service 层做，管理面
// GET 详情/GetGroupAssignments 仍可查已删项）。
func (s *Service) getGroupLive(ctx context.Context, id int64) (*domain.Group, error) {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if g.DeletedAt != nil {
		return nil, ErrNotFound
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
	if g == nil || g.Name == "" {
		return nil, ErrInvalidInput
	}
	current, err := s.store.GetGroup(ctx, g.ID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if current == nil || current.DeletedAt != nil {
		return nil, ErrNotFound
	}
	// Keep the service-level partial-update contract used by existing callers:
	// an omitted visibility inherits the persisted value. Explicit unknown
	// values remain rejected below.
	if g.Visibility == "" {
		g.Visibility = current.Visibility
	}
	if !g.Visibility.Valid() {
		return nil, ErrInvalidInput
	}
	if g.PriceMultiplier < 0 || g.PriceMultiplier > 100000 {
		return nil, ErrInvalidInput
	}
	converts, err := normalizeProtocolConverts(g.ProtocolConverts)
	if err != nil {
		return nil, err
	}
	// 副本写入：归一结果落在副本上，不原地改调用方入参（当前无实际影响，防未来踩坑）
	cp := *g
	cp.ProtocolConverts = converts
	cp.RoutingMode = g.EffectiveRoutingMode()
	if !cp.RoutingMode.Valid() {
		return nil, ErrInvalidInput
	}
	if cp.RoutingMode == domain.GroupRoutingModeUpstreams {
		store, err := s.groupUpstreamStore()
		if err != nil {
			return nil, err
		}
		members, err := store.ListGroupUpstreams(ctx, cp.ID)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("%w: upstream groups require at least one member", ErrInvalidInput)
		}
	}
	cp.AllowedModels, err = normalizeAllowedModels(g.AllowedModels)
	if err != nil {
		return nil, err
	}
	if cp.RoutingMode == domain.GroupRoutingModeUpstreams && len(cp.AllowedModels) == 0 {
		return nil, fmt.Errorf("%w: upstream groups require at least one allowed model", ErrInvalidInput)
	}
	updated, err := s.store.UpdateGroup(ctx, &cp)
	if err != nil {
		return nil, mapRepoErr(err) // 改名撞已有 name → ErrConflict（409）
	}
	// O2 组倍率矩阵：倍率变更 → 余额倍率快照定向刷新（名字/可见性变更不触发
	// 任何快照，此处保守一并标记——去抖窗口内一次小表单查，可忽略）。
	// Keys：组更新（含 protocol_convert 变更）→ 旧 key 的 auth 快照全量 Reload
	// 即时收敛（A-2 姊妹路径；CreateGroup 不加——组创建时无 key，Keys reload
	// 空转，组创建后建 key 的即时性由 A-2 增量注册保证）。
	s.inv.Multipliers()
	s.invalidateGroups(updated.ID)
	s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: []int64{updated.ID}})
	return updated, nil
}

func normalizeAllowedModels(models []string) ([]string, error) {
	if len(models) > 200 {
		return nil, ErrInvalidInput
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		model := strings.TrimSpace(raw)
		if model == "" || len(model) > 200 {
			return nil, ErrInvalidInput
		}
		if _, ok := seen[model]; ok {
			return nil, ErrInvalidInput
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	// Stable ordering keeps snapshots and audit responses deterministic.
	slices.Sort(out)
	return out, nil
}

// DeleteGroup 删除组：删组前校验组内账号（含账号 → 409 "group has accounts"，
// F1 契约修正——软删 UPDATE 无 FK 约束，不再依赖仓库错误兜底）、前置清理组内
// 全部 key（key.group_id 外键约束；Auth 增量清理），再删组。key 清理与组删除
// 非同一事务——组删除失败时 key 已删，重试删除即可（key 被删组未删的中间态
// 不提供服务——Auth 快照已移除）。
func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return mapRepoErr(err)
	}
	if g == nil || g.DeletedAt != nil {
		return ErrNotFound
	}
	if err := s.checkGroupEmpty(ctx, id); err != nil {
		return err
	}
	// Production repositories expose a combined transaction for a single group,
	// so a concurrent account/key change cannot leave keys deleted while the
	// parent group remains live. Lightweight stores keep the legacy path below.
	if atomic, ok := s.store.(interface {
		DeleteGroupWithKeys(context.Context, int64) ([]string, error)
	}); ok {
		raws, err := atomic.DeleteGroupWithKeys(ctx, id)
		if err != nil {
			return mapRepoErr(err)
		}
		if s.keys != nil {
			for _, raw := range raws {
				s.keys.Delete(raw)
			}
		}
		s.inv.Multipliers()
		s.invalidateGroups(id)
		s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: []int64{id}})
		return nil
	}
	raws, err := s.store.DeleteKeysByGroup(ctx, id)
	if err != nil {
		return err
	}
	if s.keys != nil {
		for _, raw := range raws {
			s.keys.Delete(raw)
		}
	}
	if err := s.store.DeleteGroup(ctx, id); err != nil {
		return mapRepoErr(err) // 竞态窗口缺 id → 404（前置 Get 已拦截常见路径）
	}
	// O2：组删除后倍率快照清理（陈旧条目无害；保守标记——组变更统一走倍率
	// 定向刷新）。组内账号删除前已显式校验（含账号 → 409，整批/单删同语义）
	// → 调度器快照不受组删除影响。
	// Keys：组删除经 Auth.Delete 移除组内全部 key——其余实例快照需全量覆盖
	// （key CRUD 缺口同语义），与 Multipliers 合并同一条 NOTIFY。
	s.inv.Multipliers()
	s.invalidateGroups(id)
	s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: []int64{id}})
	return nil
}

// checkGroupEmpty 组内账号校验（F1）：LoadGroupAccounts 非空 → 409（含账号组
// 删除会让账号静默脱离路由——显式拒绝；已删账号不过滤进结果，不阻断）。
func (s *Service) checkGroupEmpty(ctx context.Context, groupID int64) error {
	accs, err := s.store.LoadGroupAccounts(ctx, groupID)
	if err != nil {
		return err
	}
	if len(accs) > 0 {
		return fmt.Errorf("%w: group has accounts", ErrConflict)
	}
	return nil
}

func (s *Service) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	// R1 预扫描：先全量校验（所有组先验完存在性 + 组内账号，任一含账号 →
	// 整批拒绝 409），后开始删 key——间插校验会在中途拒绝时制造"组存 key 亡"
	// 的不可恢复中间态。
	for _, id := range ids {
		if _, err := s.store.GetGroup(ctx, id); err != nil {
			return mapRepoErr(err) // 404 缺 id
		}
	}
	for _, id := range ids {
		if err := s.checkGroupEmpty(ctx, id); err != nil {
			return err
		}
	}
	// Production repositories expose a combined transaction that soft-deletes
	// keys and groups together. Keep the legacy sequence for lightweight stores
	// used by integrations/tests that do not implement the optional capability.
	if atomic, ok := s.store.(interface {
		DeleteGroupsBatchWithKeys(context.Context, []int64) ([]string, error)
	}); ok {
		raws, err := atomic.DeleteGroupsBatchWithKeys(ctx, ids)
		if err != nil {
			return mapRepoErr(err)
		}
		if s.keys != nil {
			for _, raw := range raws {
				s.keys.Delete(raw)
			}
		}
		s.inv.Multipliers()
		s.invalidateGroups(ids...)
		s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: ids})
		return nil
	}
	for _, id := range ids {
		raws, err := s.store.DeleteKeysByGroup(ctx, id)
		if err != nil {
			return err
		}
		if s.keys != nil {
			for _, raw := range raws {
				s.keys.Delete(raw)
			}
		}
	}
	if err := mapRepoErr(s.store.DeleteGroupsBatch(ctx, ids)); err != nil {
		return err // 事务回滚；key 已删但 DB 未删——与单删同性质（软删 key 不可重载复活，失败须重试收敛终态）
	}
	s.inv.Multipliers()
	s.invalidateGroups(ids...)
	s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: ids}) // 组删除同删组内 key（Auth.Delete）→ keys 覆盖
	return nil
}

// UpdateGroupsBatch 批量更新组（仅 name/visibility——GroupPatch 无倍率字段，
// 仍定向刷新受影响组，避免名称/可见性变更留下旧的可选组快照）。
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
	s.invalidateGroups(ids...)
	s.publish(ctx, notify.Change{Groups: ids})
	return nil
}

// normalizeProtocolConverts 校验并归一协议转换方向集合（Create/Update 共用）：
// off 元素归一剔除（空/仅 off/nil → 空数组 = 不转换）；非法方向/重复方向 →
// 400；同客户端格式多方向（chat_to_resp 与 chat_to_mess 并存——路由按客户端
// 格式命中，语义歧义）→ 400。返回去重后的集合（顺序 = 输入遍历序）。
func normalizeProtocolConverts(pcs []domain.ProtocolConvert) ([]domain.ProtocolConvert, error) {
	// auto is a mode, not another fallback direction. Keeping it exclusive makes
	// routing deterministic and prevents a manual list from being silently
	// ignored when an operator meant to opt into automatic negotiation.
	if slices.Contains(pcs, domain.ProtocolConvertAuto) {
		if len(pcs) != 1 {
			return nil, ErrInvalidInput
		}
		return []domain.ProtocolConvert{domain.ProtocolConvertAuto}, nil
	}
	seen := make(map[domain.ProtocolConvert]struct{}, len(pcs))
	out := make([]domain.ProtocolConvert, 0, len(pcs))
	for _, pc := range pcs {
		if pc == domain.ProtocolConvertOff {
			continue // off 不进数组（不转换 = 空数组表达）
		}
		if !pc.Valid() {
			return nil, ErrInvalidInput
		}
		if _, dup := seen[pc]; dup {
			return nil, ErrInvalidInput
		}
		seen[pc] = struct{}{}
		out = append(out, pc)
	}
	if _, hasChatToResp := seen[domain.ProtocolConvertChatToResp]; hasChatToResp {
		if _, clash := seen[domain.ProtocolConvertChatToMess]; clash {
			return nil, ErrInvalidInput // chat 客户端格式两方向并存 → 400
		}
	}
	return out, nil
}
