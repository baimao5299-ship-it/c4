// Package domain 定义网关的核心领域类型；业务层（scheduler/proxy/service）只依赖本包。
package domain

import (
	"slices"
	"time"
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
	Err5xx       ErrorType = "5xx"
	ErrNetwork   ErrorType = "network"
	ErrAuth      ErrorType = "auth"
	ErrNoAccount ErrorType = "no_account"
	ErrAbort     ErrorType = "abort"
)

type Template struct {
	ID            int64
	Name          string
	BaseURL       string
	DefaultFormat RequestFormat
	Models        []string
	ModelFormats  map[string]RequestFormat
	ModelMapping  map[string]string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FormatFor 返回模型 m 在该模板下的请求格式：model_formats 覆盖优先，否则默认格式。
func (t *Template) FormatFor(m string) RequestFormat {
	if f, ok := t.ModelFormats[m]; ok {
		return f
	}
	return t.DefaultFormat
}

// Serves 模型是否在可服务集合（models ∪ model_formats keys ∪ mapping keys）内。
func (t *Template) Serves(m string) bool {
	if slices.Contains(t.Models, m) {
		return true
	}
	if _, ok := t.ModelFormats[m]; ok {
		return true
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
	LatencyMS        int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CreatedAt        time.Time
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
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	TotalLatencyMS   int64
}
