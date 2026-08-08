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
// BillingTier、不处理 service_tier 转发策略、不做余额预检）。
// T3 填满（中间态清理终点）：Balances/Flusher 为真实类型直接接线，无 nil
// 容忍分支——装配方（main）保证 bill 非 nil 时四字段齐备（计费开关 =
// config.Billing.Enabled，与 hooks 装配同一判定）。
type BillingHooks struct {
	Prices   PriceLookup
	Balances *billing.Balances // 余额只读快照（预检 + 扣费后定向刷新）
	Flusher  *billing.Flusher  // 批量扣费落库（billed 路由终点）
	// TierPolicy 读取 service_tier 转发策略（nil = 恒透传）：priority/flex
	// 分别按 settings service_tier_policy_priority / service_tier_policy_flex
	// 快照取值（装配方注入闭包，零 DB）。
	TierPolicy func(tier billing.Tier) billing.TierPolicyMode
}

var (
	// errNoPrice 402：模型缺价（计费启用后未设价/未同步；空价格表 = 全模型 402）。
	errNoPrice = &formatError{status: http.StatusPaymentRequired, msg: "no price configured for this model"}
	// errInsufficientBalance 402：余额预检拒绝（快照缺失或 ≤0；免费放行路径
	// T3.5 价格倍率扩展）。
	errInsufficientBalance = &formatError{status: http.StatusPaymentRequired, msg: "insufficient balance"}
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
