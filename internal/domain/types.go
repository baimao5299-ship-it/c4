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
	StatusActive   AccountStatus = "active"
	StatusUnhealthy AccountStatus = "unhealthy"
	Status429      AccountStatus = "429"
	StatusDisabled AccountStatus = "disabled"
)

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
)

type Template struct {
	ID               int64
	Name             string
	BaseURL          string
	SupportedFormats []RequestFormat            // 模板支持的格式（非空、去重）
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
	CredentialType credential.Type // 默认 api_key（创建时 DB 默认；本轮不可改）
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
	ID        int64
	Name      string
	KeyHash   string
	KeyPrefix string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UsageLog struct {
	ID               int64
	RequestID        string
	GroupID          int64 // 0 = 无
	AccountID        int64 // 0 = 无
	TemplateID       int64 // 0 = 无
	Model            string
	MappedModel      string // 空 = 未映射
	Format           RequestFormat
	StatusCode       int
	ErrorType        ErrorType
	LatencyMS           int64
	PromptTokens        int64
	CompletionTokens    int64
	TotalTokens         int64
	CacheReadTokens     int64 // 缓存读取 token（跨协议归一化，sub2api 计费语义）
	CacheCreationTokens int64 // 缓存写入 token（OpenAI ephemeral 5m/1h 聚合）
	CreatedAt           time.Time
}

type StatBucket struct {
	BucketTime       time.Time // 对齐到小时（UTC）
	GroupID          int64     // 0 = 无
	AccountID        int64     // 0 = 无
	TemplateID       int64     // 0 = 无
	Model            string
	IsError          bool
	RequestCount     int64
	ErrorCount       int64
	PromptTokens        int64
	CompletionTokens    int64
	TotalTokens         int64
	CacheReadTokens     int64 // 缓存读取 token
	CacheCreationTokens int64 // 缓存写入 token
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
