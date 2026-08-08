package service

import (
	"context"
	"errors"
	"fmt"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/pricing"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

// ErrPriceFetch 价格拉取上游失败（管理端手动 sync 映射 502 Bad Gateway；
// 与 fetch 内部错误包装，保留详情）。
var ErrPriceFetch = errors.New("service: price fetch failed")

// PricingSyncStats 一次手动同步的拉取统计（POST /admin/pricing/sync 响应）。
type PricingSyncStats struct {
	Rows    int // 拉取到的有效模型行数
	Skipped int // 解析时跳过的非法行数
	Updated int // upsert 落库数（manual 行不计）
}

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

// UpsertManualPricing 手动设价（管理端 PUT /admin/pricing/{model}）：校验 + 落库
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

// ListPricing 管理端价格列表（GET /admin/pricing）：分页 + source/model 筛选 +
// sort 白名单校验（非法 → ErrInvalidInput 400）。source 枚举非法 → 400（handler
// 显式校验已做，service 兜底双保险）。
func (s *Service) ListPricing(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, model string) ([]*domain.Pricing, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["pricing"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPricing(ctx, q, source, model)
}

// SetPriceFetcher 注入价格拉取器（管理端手动 sync 用；与 SyncWorker 共享同一
// fetcher 实例——main 装配）。重复调用覆盖；nil 注入会被下次 SyncPricingNow
// 以错误拒绝。
func (s *Service) SetPriceFetcher(f pricing.Fetcher) { s.priceFetcher = f }

// SyncPricingNow 手动触发一次价格同步（管理端 POST /admin/pricing/sync）：与
// SyncWorker.Sync 同路径语义——fetch（price_source_url settings 快照，零 DB）
// → UpsertFromLiteLLM（manual 行级互斥，永不覆盖手动价）→ 快照重载。与 cron
// 并发安全（幂等 upsert，最坏浪费一次 fetch——M-3 语义）。错误映射：
// 拉取失败 → ErrPriceFetch（502）；url 未配置 → ErrInvalidInput（400）；
// upsert 失败 → 原样（500）。返回拉取统计。
func (s *Service) SyncPricingNow(ctx context.Context) (*PricingSyncStats, error) {
	if s.priceFetcher == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := s.settingValue("price_source_url")
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", ErrInvalidInput)
	}
	res, err := s.priceFetcher.Fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPriceFetch, err)
	}
	n, err := s.store.UpsertFromLiteLLM(ctx, res.Rows)
	// upsert 部分成功后仍刷新快照（已落库的批立即生效）；fetch 失败则数据未变，
	// 保留旧快照（上方因错误直接返回）。
	s.reloadPricing(ctx)
	if err != nil {
		return nil, err
	}
	return &PricingSyncStats{Rows: len(res.Rows), Skipped: res.Skipped, Updated: n}, nil
}

// PriceSourceURL 价格拉取源 URL（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }

// PriceSyncCron 价格同步 cron 表达式（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSyncCron() string { return s.settingValue("price_sync_cron") }
