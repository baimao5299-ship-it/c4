package proxy

import (
	"encoding/json"
	"net/http"

	"go-proxy-mini/internal/billing"
	"go-proxy-mini/internal/domain"
)

// PriceLookup 价格快照读取（service.Service 实现，零 DB 快照读）。任何错误
// = 该模型无可用价格（计费方拒绝计费而非按 0 计价）。
type PriceLookup interface {
	GetPrice(model string) (*domain.Pricing, error)
}

// BillingHooks 计费钩子（proxy.New 参数；nil = 计费全关：不查价、不记
// BillingTier、不处理 service_tier 转发策略）。
// 中间态（T2）：只含 Prices + TierPolicy；Balances/Flusher 在 T3 扩展
// （类型届时定义，本任务不留 nil 容忍分支）。
type BillingHooks struct {
	Prices PriceLookup
	// TierPolicy 读取 service_tier 转发策略（nil = 恒透传）：priority/flex
	// 分别按 settings service_tier_policy_priority / service_tier_policy_flex
	// 快照取值（装配方注入闭包，零 DB）。
	TierPolicy func(tier billing.Tier) billing.TierPolicyMode
}

var (
	// errNoPrice 402：模型缺价（计费启用后未设价/未同步；空价格表 = 全模型 402）。
	errNoPrice = &formatError{status: http.StatusPaymentRequired, msg: "no price configured for this model"}
	// errServiceTierRejected 400：service_tier 策略 reject（不转发，记 ErrBilling）。
	errServiceTierRejected = &formatError{status: http.StatusBadRequest, msg: "service_tier rejected by gateway policy"}
)

// stripServiceTier 删除请求体 service_tier 字段（strip 策略）：map 重写构造
// 新 body（与 setStreamFlag/setModel 同构）。
func stripServiceTier(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, "service_tier")
	return json.Marshal(m)
}
