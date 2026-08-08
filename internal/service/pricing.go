package service

import (
	"context"
	"fmt"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

// pricingReloadPage 快照全量加载的分页大小（litellm 官方表 ~2k 模型 → 3 页内取完）。
const pricingReloadPage = 1000

// ReloadPricing 全量重载价格快照（公开）：litellm 同步拉取成功后 + 管理端改价
// 后调用，读路径（GetPrice）即时生效。失败 fail-safe（Warn + 保留旧快照）。
func (s *Service) ReloadPricing() {
	s.reloadPricing(context.Background())
}

// reloadPricing 从 DB 全量加载价格快照（New 初始化 + ReloadPricing 调用）。
// 失败 fail-safe（评审 M-1 同款）：仅 Warn + 保留旧快照/空快照，不阻断服务
// 启动——读快照缺失按 ErrNotFound（Phase 5 计费拒绝计费而非按 0 计价）。
func (s *Service) reloadPricing(ctx context.Context) {
	var all []*domain.Pricing
	for offset := 0; ; offset += pricingReloadPage {
		rows, _, err := s.store.ListPricing(ctx, repository.ListQuery{
			Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc",
		}, nil, "")
		if err != nil {
			if s.log != nil {
				s.log.Warn("pricing snapshot reload failed", logx.Error(err))
			}
			return
		}
		all = append(all, rows...)
		if len(rows) < pricingReloadPage {
			break
		}
	}
	m := buildPricingSnapshot(all)
	s.pricing.Store(&m)
}

// buildPricingSnapshot 快照构建：model → 价格行。同一 model 出现多行（防御——
// 仓库 unique(model) 约束下不应出现）按 source 优先级收敛：manual 覆盖 litellm
// （与表内"一行 = 最终生效价"、拉取 upsert 的行级互斥语义一致）。
func buildPricingSnapshot(rows []*domain.Pricing) map[string]*domain.Pricing {
	m := make(map[string]*domain.Pricing, len(rows))
	for _, p := range rows {
		if prev, ok := m[p.Model]; ok && prev.Source == domain.PricingSourceManual {
			continue // 已收敛为 manual：后续行（含 manual/litellm）一律不覆盖
		}
		m[p.Model] = p
	}
	return m
}

// GetPrice 快照读价（Phase 5 计费热路径，零 DB）：命中返回价格行；缺失 →
// ErrNotFound（计费方应拒绝计费而非按 0 计价——M-1 语义，本任务注明）。
func (s *Service) GetPrice(model string) (*domain.Pricing, error) {
	if m := s.pricing.Load(); m != nil {
		if p, ok := (*m)[model]; ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: model=%q（无价格数据：请管理端设价或等待 litellm 同步）", ErrNotFound, model)
}

// UpsertManualPricing 手动设价（管理端 /admin/pricing POST）：校验 + 落库
// （upsert 强制 source=manual，可接管 litellm 行）+ 成功后重载快照（读路径
// 即时生效）。
func (s *Service) UpsertManualPricing(ctx context.Context, model string, promptP, completionP int64) (*domain.Pricing, error) {
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if promptP < 0 || completionP < 0 {
		return nil, fmt.Errorf("%w: prices must be >= 0", ErrInvalidInput)
	}
	p, err := s.store.UpsertManual(ctx, model, promptP, completionP)
	if err != nil {
		return nil, err
	}
	s.reloadPricing(ctx)
	return p, nil
}

// DeleteManualPricing 删除手动价（管理端 DELETE /admin/pricing/{model}）：仅
// source=manual 行可删（litellm 行 → ErrConflict；仓库语义）；成功后重载快照
// ——该 model 从快照消失（缺失窗口内 GetPrice → ErrNotFound，下轮拉取补回）。
func (s *Service) DeleteManualPricing(ctx context.Context, model string) error {
	if err := s.store.DeleteManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}

// PriceSourceURL 价格拉取源 URL（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }

// PriceSyncCron 价格同步 cron 表达式（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSyncCron() string { return s.settingValue("price_sync_cron") }
