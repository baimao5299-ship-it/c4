// Package credential 是账号凭据抽象层：类型注册表 + Provider 分发，为后续
// 多种号池类型（codex oauth、codex personal_access_token、claude code 等）
// 打地基。本轮只实现接口 + api_key 默认实现 + 接线（行为零变化）。
//
// 正交原则：Provider 只返回凭据值（key/token），不感知请求格式——鉴权头名由
// 格式侧决定（openai → Authorization: Bearer、anthropic → x-api-key，
// aiclient 现状已按格式分头）。
package credential

import (
	"context"
	"errors"
	"fmt"
)

// Type 账号凭据类型：按生态命名（api_key = 静态 Key + 模板 BaseURL，现状语义）。
type Type string

const (
	// TypeAPIKey 默认类型：静态 Key（调度 Selection 携带的 UpstreamKey）。
	TypeAPIKey Type = "api_key"
	// 号池生态类型（后续任务实现，当前只留注释占位）：
	// TypeCodexOAuth = "codex_oauth"
	// TypeCodexPAT   = "codex_personal_access_token"
	// TypeClaudeCode = "claude_code"
)

// Valid 类型是否已注册可用的凭据类型。目前仅 api_key；未知类型（含空串）
// → false（未来生态类型注册时扩展）。
func (t Type) Valid() bool {
	switch t {
	case TypeAPIKey:
		return true
	}
	return false
}

// CredentialInput 凭据取用输入：api_key 类型用 APIKey（调度 Selection 携带
// 的静态 Key）；oauth 等类型后续按 AccountID 从凭据扩展表加载（Phase 2）。
type CredentialInput struct {
	AccountID int64
	Type      Type
	APIKey    string // api_key 类型的静态 Key
}

// Provider 凭据提供者：Credential 返回当前可用的凭据值（api_key → key；
// oauth → access_token）。鉴权头名由转发器按请求格式决定，provider 不感知
// 格式（正交）。
type Provider interface {
	Type() Type
	Credential(ctx context.Context, in CredentialInput) (string, error)
}

// ErrUnsupported 未知/未注册的凭据类型。
var ErrUnsupported = errors.New("credential: unsupported credential type")

// apiKeyProvider 默认实现：直接返回 CredentialInput.APIKey（空 Key 也原样
// 返回——行为与现状一致，Key 非空校验在别处）。类型不匹配 → 错误（防御性：
// For 的 fallback 路径正常情况下不可达）。
type apiKeyProvider struct{}

func (apiKeyProvider) Type() Type { return TypeAPIKey }

func (apiKeyProvider) Credential(_ context.Context, in CredentialInput) (string, error) {
	if in.Type != TypeAPIKey {
		return "", fmt.Errorf("%w: %q", ErrUnsupported, in.Type)
	}
	return in.APIKey, nil
}

// Registry 类型 → Provider 分发表。
type Registry struct{ m map[Type]Provider }

// New 构造注册表并默认注册 api_key provider。
func New() *Registry {
	r := &Registry{m: make(map[Type]Provider)}
	r.Register(apiKeyProvider{})
	return r
}

// Register 注册/覆盖指定类型的 provider。
func (r *Registry) Register(p Provider) { r.m[p.Type()] = p }

// For 取类型的 provider；未注册 → apiKeyProvider（正常情况下不可达：
// 调用方先做 Type.Valid() 校验，未注册类型在 Valid 即被拒绝——评审 M1：
// 不得静默 fallback）。
func (r *Registry) For(t Type) Provider {
	if p, ok := r.m[t]; ok {
		return p
	}
	return apiKeyProvider{}
}
