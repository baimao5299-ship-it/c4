// Package domain 定义网关的核心领域类型；业务层（scheduler/proxy/service）只依赖本包。
package domain

import (
	"slices"
	"time"

	"go-proxy-mini/internal/credential"
)

type RequestFormat string

const (
	FormatOpenAIChat      RequestFormat = "openai-chat"
	FormatOpenAIResponses RequestFormat = "openai-responses"
	FormatAnthropic       RequestFormat = "anthropic"
)

func (f RequestFormat) Valid() bool {
	switch f {
	case FormatOpenAIChat, FormatOpenAIResponses, FormatAnthropic:
		return true
	}
	return false
}

type AccountStatus string

const (
	StatusActive    AccountStatus = "active"
	StatusUnhealthy AccountStatus = "unhealthy"
	Status429       AccountStatus = "429"
	StatusDisabled  AccountStatus = "disabled"
)

// Role 用户角色（两级：platform_admin | user）。
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleUser          Role = "user"
)

func (r Role) Valid() bool {
	switch r {
	case RolePlatformAdmin, RoleUser:
		return true
	}
	return false
}

// UserStatus 用户状态。
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

func (s UserStatus) Valid() bool {
	switch s {
	case UserStatusActive, UserStatusDisabled:
		return true
	}
	return false
}

// KeyStatus 客户端 key 状态。
type KeyStatus string

const (
	KeyStatusActive   KeyStatus = "active"
	KeyStatusDisabled KeyStatus = "disabled"
)

func (s KeyStatus) Valid() bool {
	switch s {
	case KeyStatusActive, KeyStatusDisabled:
		return true
	}
	return false
}

// GroupVisibility 组可见性：public 全部用户可选；private 仅授予用户。
type GroupVisibility string

const (
	GroupVisibilityPublic  GroupVisibility = "public"
	GroupVisibilityPrivate GroupVisibility = "private"
)

func (v GroupVisibility) Valid() bool {
	switch v {
	case GroupVisibilityPublic, GroupVisibilityPrivate:
		return true
	}
	return false
}

// SettingType settings 值类型。
type SettingType string

const (
	SettingTypeSwitch SettingType = "switch"
	SettingTypeNumber SettingType = "number"
	SettingTypeString SettingType = "string"
)

func (t SettingType) Valid() bool {
	switch t {
	case SettingTypeSwitch, SettingTypeNumber, SettingTypeString:
		return true
	}
	return false
}

type ErrorType string

const (
	ErrNone      ErrorType = "none"
	Err429       ErrorType = "429"
	Err4xx       ErrorType = "4xx"
	Err5xx       ErrorType = "5xx"
	ErrNetwork   ErrorType = "network"
	ErrAuth      ErrorType = "auth"
	ErrNoAccount ErrorType = "no_account"
	ErrAbort     ErrorType = "abort"
	ErrBilling   ErrorType = "billing" // 计费拒绝（缺价/余额不足 402）
)

// ErrMsgMaxLen 错误文本域内截断上限（usagelog.error_message varchar(500)
// 与 accounts.last_error 共用；部署故障修复：错误文本留痕但列长度有界）。
const ErrMsgMaxLen = 500

// TruncateErrMsg 把错误文本截断到 ErrMsgMaxLen 字符（按 rune 截断，不拆断
// 多字节 UTF-8）。仅错误分支调用（成功路径不经过），热路径零成本——短文本
// （≤500 字符，含全部 ASCII 错误文案）直接返回不分配。
func TruncateErrMsg(s string) string {
	if len(s) <= ErrMsgMaxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= ErrMsgMaxLen {
		return s
	}
	return string(r[:ErrMsgMaxLen])
}

type Template struct {
	ID               int64
	Name             string
	BaseURL          string
	CredentialType   credential.Type        // 模板级：默认 api_key（DB 默认；号池生态类型后续）
	SupportedFormats []RequestFormat        // 模板支持的格式（非空、去重）
	Models           []string                   // 可服务模型集合
	FormatModels     map[RequestFormat][]string // 格式 → 该格式支持的模型列表；未配置 = 全部 Models
	ModelMapping     map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// FormatsFor 模板支持的格式列表。
func (t *Template) FormatsFor() []RequestFormat { return t.SupportedFormats }

// FormatSupports 格式 f 是否支持模型 m（模型级限制）：
// f 不在 SupportedFormats → false；FormatModels[f] 配置了 → m ∈ 列表；未配置 → true。
func (t *Template) FormatSupports(f RequestFormat, m string) bool {
	if !slices.Contains(t.SupportedFormats, f) {
		return false
	}
	if list, ok := t.FormatModels[f]; ok {
		return slices.Contains(list, m)
	}
	return true
}

// Serves 模型是否在可服务集合（models ∪ format_models 全部列表 ∪ mapping keys）内。
func (t *Template) Serves(m string) bool {
	if slices.Contains(t.Models, m) {
		return true
	}
	for _, list := range t.FormatModels {
		if slices.Contains(list, m) {
			return true
		}
	}
	if _, ok := t.ModelMapping[m]; ok {
		return true
	}
	return false
}

type Account struct {
	ID             int64
	Name           string
	TemplateID     int64
	Template       *Template
	UpstreamKey    string
	Status         AccountStatus
	CooldownUntil  *time.Time
	Weight         int
	MaxConcurrency int
	LastError      *string
	LastUsedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// GroupIDs 写路径（创建/更新）专用：nil = 不设置/不变；非 nil = 替换账号
	// 全部分组（含空数组 = 清空）。读路径忽略——编辑回显走 GetAccountGroups
	// 独立查询（toDomainAccount 不填充该字段）。
	GroupIDs *[]int64
}

type Group struct {
	ID         int64
	Name       string
	Visibility GroupVisibility
	// PriceMultiplier 万分数（T3.5 价格倍率）：组默认 10000 = ×1；0 = 免费。
	// 写路径语义：Create 缺省（nil，service 归一为 10000）恒写入；Update 恒写入
	// （PUT 全量替换）。API 边界（handler/convert.go）与正常值 float64 换算
	// （1.5 ↔ 15000）。
	PriceMultiplier int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// User 用户（顶层实体，无租户）。标识 = 邮箱；PasswordHash 为 bcrypt
// DefaultCost(10)（与 sub2api 同参数，存量 hash 可迁移验证）。
// Balance 最小单位（毫分；1 USD = 100,000 毫分，Phase 5 计费统一单位，
// 管理面 API 展示/输入换算 USD）。
// 价格倍率按组（T3.5 修正）：挂在 group_assignment 上（GroupAssignment.
// PriceMultiplier），用户不同组可有不同倍率——User 无倍率字段。
type User struct {
	ID             int64
	Email          string
	PasswordHash   string
	Role           Role
	Status         UserStatus
	MaxConcurrency int
	Balance        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Key 客户端 API key（独立表，重建 group 内嵌 key 语义）。
type Key struct {
	ID             int64
	UserID         int64
	GroupID        int64
	Name           string
	KeyHash        string
	KeyPrefix      string
	Status         KeyStatus
	MaxConcurrency int
	Quota          int64 // 累计 token 上限；0 = 不限
	QuotaUsed      int64 // 已消耗（后扣；无额度 key 恒 0）
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// HasQuota 是否有额度上限（quota > 0）。热路径门禁/扣减短路标志。
func (k *Key) HasQuota() bool { return k.Quota > 0 }

// GroupAssignment private 组的授予记录（用户 ↔ 组多对多）。
// PriceMultiplier 该用户在该组的专属价格倍率（万分数，T3.5 修正：按组——
// 用户在不同组可有不同倍率）；nil = 未设置 → 用组倍率；0 = 免费。
type GroupAssignment struct {
	ID              int64
	GroupID         int64
	UserID          int64
	PriceMultiplier *int
	CreatedAt       time.Time
}

// Setting 类型化配置（key/type/value；signup_enabled 注册开关等）。
type Setting struct {
	ID        int64
	Key       string
	Type      SettingType
	Value     string
	UpdatedAt time.Time
}

// KeyMeta 鉴权快照条目：key + 归属用户的关键门禁字段（Auth 内存表元素，
// repository.LoadKeys 构建；热路径零 DB 读取）。
type KeyMeta struct {
	KeyID          int64
	UserID         int64
	GroupID        int64
	KeyStatus      KeyStatus
	KeyMaxConc     int
	UserStatus     UserStatus
	UserMaxConc    int
	HasQuota       bool
	Quota          int64
	QuotaUsed      int64 // 快照值（reload 时从 DB 读）；在途扣减走内存计数
}

// UsageLog 用量日志：user_id/key_id 为鉴权归属（context 传递，0 = 无）。
// 计费列（Phase 5）：Cost 毫分（1 USD = 100,000 毫分）；BillingTier 请求
// service_tier 归一化值（priority/flex/fast/auto，空 = 未计费路径）；AboveHit
// 任一分量超 above 阈值命中分段；Overdraft 本次扣费透支（负余额）。
// ErrorMessage 错误文本（部署故障修复）：连接级 err.Error() / 4xx+ 上游 body，
// 域内截断 500 字符（TruncateErrMsg）；nil = 无错误文本（成功路径恒空）。
type UsageLog struct {
	ID               int64
	RequestID        string
	GroupID          int64 // 0 = 无
	AccountID        int64 // 0 = 无
	TemplateID       int64 // 0 = 无
	UserID           int64 // 0 = 无（鉴权失败/无 key）
	KeyID            int64 // 0 = 无
	Model            string
	MappedModel      string // 空 = 未映射
	Format           RequestFormat
	StatusCode       int
	ErrorType        ErrorType
	ErrorMessage     *string // nil = 无错误文本（NULL 落库）
	LatencyMS           int64
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	CacheReadTokens     int64 // 缓存读取 token（跨协议归一化，sub2api 计费语义）
	CacheCreationTokens int64 // 缓存写入 token（OpenAI ephemeral 5m/1h 聚合）
	Cost                int64 // 毫分；错误请求（402/4xx）为 0
	BillingTier         string // priority/flex/fast/auto；空 = 未计费路径
	AboveHit            bool
	Overdraft           bool
	CreatedAt           time.Time
}

type StatBucket struct {
	BucketTime       time.Time // 对齐到小时（UTC）
	GroupID          int64     // 0 = 无
	AccountID        int64     // 0 = 无
	TemplateID       int64     // 0 = 无
	UserID           int64     // 0 = 无（鉴权失败/无 key）；/user/stats 按此过滤
	Model            string
	IsError          bool
	RequestCount     int64
	ErrorCount       int64
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	CacheReadTokens     int64 // 缓存读取 token
	CacheCreationTokens int64 // 缓存写入 token
	Cost                int64 // 毫分（计费预聚合，花费统计不扫明细）
	TotalLatencyMS      int64
}

// —— 规则引擎（可编排状态管理） ——
type RuleWhen struct {
	Kind                 *string  `json:"kind,omitempty"`
	HTTPStatus           *int     `json:"http_status,omitempty"`
	ErrorMessageContains *string  `json:"error_message_contains,omitempty"`
	AccountID            *int64   `json:"account_id,omitempty"`
	TemplateID           *int64   `json:"template_id,omitempty"`
	GroupID              *int64   `json:"group_id,omitempty"`
	Model                *string  `json:"model,omitempty"`
	WindowSeconds        *int     `json:"window_seconds,omitempty"`
	Count429GE           *int     `json:"count_429_ge,omitempty"`
	CountErrorGE         *int     `json:"count_error_ge,omitempty"`
	CountOKGE            *int     `json:"count_ok_ge,omitempty"`
	CountTotalGE         *int     `json:"count_total_ge,omitempty"`
	Ratio429GE           *float64 `json:"ratio_429_ge,omitempty"`
	RatioErrorGE         *float64 `json:"ratio_error_ge,omitempty"`
}

type RuleThen struct {
	Status   *AccountStatus `json:"status,omitempty"`
	Cooldown *string        `json:"cooldown,omitempty"` // time.ParseDuration 可解析的时长，如 "30s"、"5h"
	Weight   *int           `json:"weight,omitempty"`   // 0-100
}

type Rule struct {
	ID        int64
	Name      string
	Enabled   bool
	Priority  int
	When      RuleWhen
	Then      RuleThen
	CreatedAt time.Time
	UpdatedAt time.Time
}
