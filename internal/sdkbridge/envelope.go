// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import "fmt"

// EnvelopeError 网关侧信封错误（P2-1 信封包装）：SDK 内部 HTTP 错误（*HTTPError
// 仅 StatusCode+Raw，无 RawJSON()）经适配层（T2 起）包装为网关侧信封错误——
// 实现 StatusCode() int + RawJSON() string 协议，网关 statusOf/upstreamBody/
// upstreamErrMsg 零改动复用（4xx 透传 + error_message 匹配）；**Unwrap 保留
// errors.As 链**（SDK 错误类别判断 / 双源去重 errors.As 命中穿透本信封）。
// Err 为 nil（无底层 SDK 错误）时 Unwrap 返回 nil——errors.As 链自然中断，
// 仅信封协议可用。
type EnvelopeError struct {
	Status int
	Body   string // 上游响应原始 body（RawJSON() 返回；空 = 无 body）
	Err    error  // 被包装的 SDK 错误；nil = 无
}

// NewEnvelopeError 构造信封错误（适配层包装入口；T2 起使用）。
func NewEnvelopeError(status int, body string, err error) *EnvelopeError {
	return &EnvelopeError{Status: status, Body: body, Err: err}
}

func (e *EnvelopeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("upstream error status=%d", e.Status)
}

func (e *EnvelopeError) StatusCode() int { return e.Status }

func (e *EnvelopeError) RawJSON() string { return e.Body }

func (e *EnvelopeError) Unwrap() error { return e.Err }
