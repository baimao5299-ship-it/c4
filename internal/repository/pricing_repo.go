package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"entgo.io/ent/dialect"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/pricing"
)

// PricingRepo 模型价格持久化（Phase 5 计费价格来源）。
// source 行级互斥优先级 manual > litellm：拉取 upsert 带 WHERE 条件永不覆盖
// 手动价；手动设价可接管 litellm 行；删除手动行后下轮拉取补回。
type PricingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（UpsertFromLiteLLM 批量 upsert）用：普通 client 与
	// tx client（WithTx 内）均可用——ent v0.14 生成代码无 ExecContext，
	// raw SQL 经 dialect.Driver 统一执行（对齐 UpdateUserBalance 先例）。
	driver dialect.Driver
}

// pricingBatchSize litellm 官方表 ~2k 模型，单条 INSERT 过大（评审 M-2）：
// 按 500/批分块执行。
const pricingBatchSize = 500

// UpsertFromLiteLLM 批量 upsert 拉取价（评审 M-2）：
//   - 核心语义：ON CONFLICT (model) DO UPDATE ... WHERE pricing.source != 'manual'
//     —— 永不覆盖手动价（表内一行 = 最终生效价）；litellm 源行的换算价格、
//     max_tokens、cache 价、provider/mode/supports_prompt_caching、raw 与
//     source 均更新（source 恒为 litellm）
//   - 分批 500/批、每批独立事务：部分成功可接受——返回成功行数，失败的批记
//     Warn 日志（返回首个失败错误，worker 侧决定重试/告警）；不影响已成功批
//   - 返回 n = 实际插入/更新的行数（DO UPDATE 被 WHERE 过滤掉的手动行不计入；
//     PG 对未修改行不产生命令标签计数）
func (r *PricingRepo) UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now()
	total := 0
	var firstErr error
	for start := 0; start < len(rows); start += pricingBatchSize {
		end := start + pricingBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		n, err := r.upsertLitellmBatch(ctx, rows[start:end], now)
		if err != nil {
			log.Printf("pricing: litellm upsert batch [%d:%d) failed: %v", start, end, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		total += n
	}
	return total, firstErr
}

// upsertLitellmBatch 单批 upsert（独立事务，单语句原子）：raw SQL 手工拼
// 多行 VALUES + ON CONFLICT (model) DO UPDATE SET ... WHERE pricings.source
// != 'manual'——ent v0.14 的 upsert 构建器不支持 DO UPDATE 的 WHERE 条件，
// 故 raw SQL 经 driver 执行。created_at/updated_at 显式传值（无默认值依赖）。
// raw JSONB：pgx 对 []byte 参数按 bytea 编码，无法隐式转 jsonb → 转 string 传参
// 并 SQL 侧显式 ::jsonb 强转（nil → NULL）；其余列 nil → NULL。
func (r *PricingRepo) upsertLitellmBatch(ctx context.Context, batch []*domain.Pricing, now time.Time) (int, error) {
	const colsPerRow = 14
	var buf strings.Builder
	buf.WriteString("INSERT INTO " + pricing.Table + " (model, prompt_price_per_million, " +
		"completion_price_per_million, max_input_tokens, max_output_tokens, " +
		"cache_read_price_per_million, cache_creation_price_per_million, " +
		"provider, mode, supports_prompt_caching, raw, source, created_at, updated_at) VALUES ")
	args := make([]any, 0, len(batch)*colsPerRow)
	for i, p := range batch {
		if i > 0 {
			buf.WriteString(", ")
		}
		n := i * colsPerRow
		fmt.Fprintf(&buf, "($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d::jsonb, $%d, $%d, $%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10, n+11, n+12, n+13, n+14)
		var maxIn, maxOut, cacheRead, cacheCreate, provider, mode, spc, raw any
		if p.MaxInputTokens != nil {
			maxIn = *p.MaxInputTokens
		}
		if p.MaxOutputTokens != nil {
			maxOut = *p.MaxOutputTokens
		}
		if p.CacheReadPricePerMillion != nil {
			cacheRead = *p.CacheReadPricePerMillion
		}
		if p.CacheCreationPricePerMillion != nil {
			cacheCreate = *p.CacheCreationPricePerMillion
		}
		if p.Provider != nil {
			provider = *p.Provider
		}
		if p.Mode != nil {
			mode = *p.Mode
		}
		if p.SupportsPromptCaching != nil {
			spc = *p.SupportsPromptCaching
		}
		if len(p.Raw) > 0 {
			raw = string(p.Raw)
		}
		// source 恒为 litellm（本方法即 litellm 拉取路径，防止误传 manual 行破坏优先级）。
		args = append(args, p.Model, p.PromptPricePerMillion, p.CompletionPricePerMillion,
			maxIn, maxOut, cacheRead, cacheCreate, provider, mode, spc, raw,
			string(domain.PricingSourceLitellm), now, now)
	}
	buf.WriteString(" ON CONFLICT (" + pricing.FieldModel + ") DO UPDATE SET " +
		"prompt_price_per_million = EXCLUDED.prompt_price_per_million, " +
		"completion_price_per_million = EXCLUDED.completion_price_per_million, " +
		"max_input_tokens = EXCLUDED.max_input_tokens, " +
		"max_output_tokens = EXCLUDED.max_output_tokens, " +
		"cache_read_price_per_million = EXCLUDED.cache_read_price_per_million, " +
		"cache_creation_price_per_million = EXCLUDED.cache_creation_price_per_million, " +
		"provider = EXCLUDED.provider, " +
		"mode = EXCLUDED.mode, " +
		"supports_prompt_caching = EXCLUDED.supports_prompt_caching, " +
		"raw = EXCLUDED.raw, " +
		"source = EXCLUDED.source, " +
		"updated_at = EXCLUDED.updated_at " +
		"WHERE " + pricing.Table + ".source != '" + string(domain.PricingSourceManual) + "'")

	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	var res sql.Result
	if err := tx.Exec(ctx, buf.String(), args, &res); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// UpsertManual 手动设价（upsert 强制 source=manual，可接管已存在的 litellm
// 行——唯一冲突 → DO UPDATE 全字段 + source 改 manual）；model 非空。
// cacheRead/cacheCreation 可选：nil = 不设缓存价（落库 NULL）；非 nil = 显式
// 设价（可为 0——0 与非负主价一致，表示该缓存价明确为 0；接管的 litellm 行
// 的缓存价被手动值覆盖）。manual 行不写 provider/mode/raw（恒 NULL）。
// 返回落库后的完整行（GetPricing 重查：upsert 路径 RETURNING 仅 id，
// created_at 等列需读库才准确）。
func (r *PricingRepo) UpsertManual(ctx context.Context, model string, promptP, completionP int64, cacheRead, cacheCreation *int64) (*domain.Pricing, error) {
	if model == "" {
		return nil, fmt.Errorf("pricing: model is required")
	}
	_, err := r.client.Pricing.Create().
		SetModel(model).
		SetPromptPricePerMillion(promptP).
		SetCompletionPricePerMillion(completionP).
		SetNillableCacheReadPricePerMillion(cacheRead).
		SetNillableCacheCreationPricePerMillion(cacheCreation).
		SetSource(pricing.SourceManual).
		OnConflictColumns(pricing.FieldModel).
		Update(func(u *ent.PricingUpsert) {
			u.SetPromptPricePerMillion(promptP).
				SetCompletionPricePerMillion(completionP).
				SetSource(pricing.SourceManual).
				SetUpdatedAt(time.Now())
			// 冲突更新路径无 SetNillable（ent 生成）：nil → 显式 SetNull（清掉
			// 接管行原有的缓存价）；非 nil → Set 覆盖。
			if cacheRead != nil {
				u.SetCacheReadPricePerMillion(*cacheRead)
			} else {
				u.ClearCacheReadPricePerMillion()
			}
			if cacheCreation != nil {
				u.SetCacheCreationPricePerMillion(*cacheCreation)
			} else {
				u.ClearCacheCreationPricePerMillion()
			}
			// manual 行恒无 litellm 元数据/raw：接管 litellm 行时清掉（新行本来 NULL）。
			u.ClearProvider().
				ClearMode().
				ClearSupportsPromptCaching().
				ClearRaw()
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetPricing(ctx, model)
}

// DeleteManual 删除手动价行：仅 source=manual 可删（litellm 行 → ErrConflict——
// 语义：只允许删手动价，防误删拉取行；删除后下轮拉取会补回）；
// 行不存在 → ErrNotFound。
func (r *PricingRepo) DeleteManual(ctx context.Context, model string) error {
	n, err := r.client.Pricing.Delete().
		Where(pricing.ModelEQ(model), pricing.SourceEQ(pricing.SourceManual)).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		exists, err := r.client.Pricing.Query().Where(pricing.ModelEQ(model)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: model=%q source=litellm（只允许删手动价）", ErrConflict, model)
		}
		return fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	return nil
}

// ListPricing 价格列表：分页/筛选（source、model 模糊 ilike）/排序
// （sort 白名单：model/updated_at；非法值 → ErrInvalidSort，对齐 ListQuery）。
func (r *PricingRepo) ListPricing(ctx context.Context, q ListQuery, source *domain.PricingSource, model string) ([]*domain.Pricing, int64, error) {
	pred := r.client.Pricing.Query()
	if source != nil {
		pred = pred.Where(pricing.SourceEQ(pricing.Source(*source)))
	}
	if model != "" {
		pred = pred.Where(pricing.ModelContainsFold(model))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(pricingSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Pricing, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPricing(row))
	}
	return out, int64(total), nil
}

// GetPricing 按 model 取价格行（内部/后续 Phase 5 计费用）；缺失 → ErrNotFound。
func (r *PricingRepo) GetPricing(ctx context.Context, model string) (*domain.Pricing, error) {
	row, err := r.client.Pricing.Query().Where(pricing.ModelEQ(model)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
		}
		return nil, err
	}
	return toDomainPricing(row), nil
}

func toDomainPricing(p *ent.Pricing) *domain.Pricing {
	return &domain.Pricing{
		ID:                           p.ID,
		Model:                        p.Model,
		PromptPricePerMillion:        p.PromptPricePerMillion,
		CompletionPricePerMillion:    p.CompletionPricePerMillion,
		MaxInputTokens:               p.MaxInputTokens,
		MaxOutputTokens:              p.MaxOutputTokens,
		CacheReadPricePerMillion:     p.CacheReadPricePerMillion,
		CacheCreationPricePerMillion: p.CacheCreationPricePerMillion,
		Provider:                     p.Provider,
		Mode:                         p.Mode,
		SupportsPromptCaching:        p.SupportsPromptCaching,
		Raw:                          p.Raw,
		Source:                       domain.PricingSource(p.Source),
		CreatedAt:                    p.CreatedAt,
		UpdatedAt:                    p.UpdatedAt,
	}
}
