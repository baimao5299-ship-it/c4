package domain

import (
	"encoding/json"
	"time"
)

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
	ID                           int64
	Model                        string
	PromptPricePerMillion        int64
	CompletionPricePerMillion    int64
	MaxInputTokens               *int64 // litellm 自带上下文窗口；nil = 未知
	MaxOutputTokens              *int64
	CacheReadPricePerMillion     *int64          // 缓存读取价（litellm cache_read_input_token_cost）；nil = 无缓存价
	CacheCreationPricePerMillion *int64          // 缓存写入价（litellm cache_creation_input_token_cost）；nil = 无缓存价
	Provider                     *string         // litellm_provider（litellm 行才有；manual 行 nil）
	Mode                         *string         // litellm mode（chat/completion/embedding 等）
	SupportsPromptCaching        *bool           // litellm supports_prompt_caching
	Raw                          json.RawMessage // litellm 原始条目完整镜像（含未映射字段）；nil = 无（manual 行）
	Source                       PricingSource
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}
