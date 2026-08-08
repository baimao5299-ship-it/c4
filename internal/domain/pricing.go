package domain

import "time"

// PricingSource 价格行来源：litellm = 自动拉取；manual = 管理端手动设置。
// 行级互斥优先级 manual > litellm：拉取 upsert（DO UPDATE 带
// WHERE source != 'manual'）永不覆盖手动价；删除手动行后下轮拉取补回。
type PricingSource string

const (
	PricingSourceLitellm PricingSource = "litellm"
	PricingSourceManual  PricingSource = "manual"
)

func (s PricingSource) Valid() bool {
	switch s {
	case PricingSourceLitellm, PricingSourceManual:
		return true
	}
	return false
}

// Pricing 模型价格行（Phase 5 计费价格来源；一行 = 最终生效价，计费读快照
// 不关心来源）。价格单位：毫分 / 1M tokens（1 USD = 100,000 毫分 = 10⁻⁵ USD
// 精度；litellm per-token USD 换算 ×1e11 四舍五入取整）。
type Pricing struct {
	ID                        int64
	Model                     string
	PromptPricePerMillion     int64
	CompletionPricePerMillion int64
	MaxInputTokens            *int64 // litellm 自带上下文窗口；nil = 未知
	MaxOutputTokens           *int64
	Source                    PricingSource
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}
