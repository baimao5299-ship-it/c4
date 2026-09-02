// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package serviceerr 定义 service 层错误哨兵（单一真相）。
//
// 叶子包（零内部依赖）：不 import internal/service 或任何上层包，否则形成
// service → 本包 → service 的 import 环。service 包以别名 re-export
// （var ErrNotFound = serviceerr.ErrNotFound 等）保持既有引用
// （errors.Is(err, service.ErrXxx)）同一哨兵实例语义——80+ 调用点零改动。
package serviceerr

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound           = errors.New("service: not found")
	ErrInvalidInput       = errors.New("service: invalid input")
	ErrConflict           = errors.New("service: conflict")
	ErrInvalidCredentials = errors.New("service: invalid email or password")
	// ErrUserDisabled is returned only after the supplied password is valid. It
	// lets the HTTP boundary explain an administrative ban without exposing
	// account state to invalid-credential probes.
	ErrUserDisabled      = errors.New("service: user disabled")
	ErrSignupDisabled    = errors.New("service: signup disabled")
	ErrTooManyRequests   = errors.New("service: too many requests")
	ErrMailNotConfigured = errors.New("service: mail not configured")
	ErrMailQueueFull     = errors.New("service: mail queue full")
)

// ConflictCode is an optional machine-readable reason for a 409 response.
// Keeping the HTTP status and ErrConflict wrapping unchanged preserves the
// existing contract while allowing clients to distinguish stale edits from
// ordinary uniqueness conflicts.
type ConflictCode string

const (
	ConflictCodeRevision      ConflictCode = "revision_conflict"
	ConflictCodeDuplicateName ConflictCode = "duplicate_name"
)

// ConflictError carries a stable code while still matching ErrConflict via
// errors.Is. Detail remains the existing human-readable context (for example
// `name="relay"` or `id=7 changed`).
type ConflictError struct {
	Code   ConflictCode
	Detail string
}

func (e *ConflictError) Error() string {
	if e == nil || e.Detail == "" {
		return ErrConflict.Error()
	}
	return fmt.Sprintf("%s: %s", ErrConflict, e.Detail)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }
