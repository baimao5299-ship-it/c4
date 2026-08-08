package domain

import "time"

// RedemptionType 兑换码类型：资源发放的通用载体（Phase 5 计费前基础设施）。
// balance = users.balance += value；concurrency = users.max_concurrency（0 = 不限特判）；
// temp_balance = 插入 temp_balances 行（resource_expires_at 必填）。
// 类型 → applier 注册表：新类型 = 新增 applier，兑换流程零改动。
type RedemptionType string

const (
	RedemptionTypeBalance     RedemptionType = "balance"
	RedemptionTypeConcurrency RedemptionType = "concurrency"
	RedemptionTypeTempBalance RedemptionType = "temp_balance"
)

func (t RedemptionType) Valid() bool {
	switch t {
	case RedemptionTypeBalance, RedemptionTypeConcurrency, RedemptionTypeTempBalance:
		return true
	}
	return false
}

// RedemptionStatus 兑换码状态。
type RedemptionStatus string

const (
	RedemptionStatusActive   RedemptionStatus = "active"
	RedemptionStatusDisabled RedemptionStatus = "disabled"
)

func (s RedemptionStatus) Valid() bool {
	switch s {
	case RedemptionStatusActive, RedemptionStatusDisabled:
		return true
	}
	return false
}

// RedemptionCode 兑换码（不可编辑：生成后仅可失效）。
type RedemptionCode struct {
	ID                int64
	Code              string
	Type              RedemptionType
	Value             int64            // 最小单位（分/并发数）
	Remark            *string          // 运营备注，可选
	ExpiresAt         *time.Time       // 码未兑换即过期；nil = 永久
	ResourceExpiresAt *time.Time       // 兑换后资源到期；temp_balance 必填（service 校验）
	MaxUses           int              // 1 = 单次码；>1 = 多人码
	UsedCount         int              // 已兑换次数（条件递增，防并发超卖）
	Status            RedemptionStatus
	CreatedBy         int64 // 0 = 系统（静态 admin token）；>0 = platform_admin user_id
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RedemptionUse 兑换审计记录（全量留痕；UNIQUE(code_id, user_id) = DB 兜底幂等）。
type RedemptionUse struct {
	ID                int64
	CodeID            int64
	UserID            int64
	Value             int64 // 兑换时的值快照
	ResourceExpiresAt *time.Time
	CreatedAt         time.Time
}

// RedemptionApply 兑换成功回执（/user/redemptions POST 响应体）。
// ResourceExpiresAt = 兑换后资源到期（temp_balance 必有；balance/concurrency 恒 nil）。
type RedemptionApply struct {
	Type              RedemptionType
	Value             int64
	ResourceExpiresAt *time.Time
}
