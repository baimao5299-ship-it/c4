// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo 账号类型化鉴权扩展（account_ext 1:1 边缘表；codex 专用——
// credential_type ∈ {codex-oauth, codex-pat}，codex 列组：installation_id/
// session_id/thread_id/window_id（身份四元组，service 导入时自动生成、持久
// 复用）+ oauth 组 + pat 组；列组类型约束由 service 校验）。未来 claude oauth
// 等新类型 → 新增 ext_claude_repo.go 同构。
type AccountExtRepo struct{ client *ent.Client }

// UpsertAccountExt 幂等写入（同 TemplateExtRepo.UpsertTemplateExt 语义：
// 冲突 UPDATE 显式 ClearX 清 NULL）。installation_id 必存（非空由 service
// 校验）；session/thread/window 由 service 维护（导入时生成、持久复用——
// nil → 清空，正常路径恒非 nil）。
func (r *AccountExtRepo) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	_, err := r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetInstallationID(e.InstallationID).
		SetNillableSessionID(e.SessionID).
		SetNillableThreadID(e.ThreadID).
		SetNillableWindowID(e.WindowID).
		SetNillableOauthToken(e.OAuthToken).
		SetNillableOauthRefreshToken(e.OAuthRefreshToken).
		SetNillableOauthExpiresAt(e.OAuthExpiresAt).
		SetNillablePatKey(e.PATKey).
		SetNillableEmail(e.Email).
		OnConflictColumns(accountext.FieldAccountID).
		Update(func(u *ent.AccountExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			u.SetInstallationID(e.InstallationID)
			if e.SessionID != nil {
				u.SetSessionID(*e.SessionID)
			} else {
				u.ClearSessionID()
			}
			if e.ThreadID != nil {
				u.SetThreadID(*e.ThreadID)
			} else {
				u.ClearThreadID()
			}
			if e.WindowID != nil {
				u.SetWindowID(*e.WindowID)
			} else {
				u.ClearWindowID()
			}
			if e.OAuthToken != nil {
				u.SetOauthToken(*e.OAuthToken)
			} else {
				u.ClearOauthToken()
			}
			if e.OAuthRefreshToken != nil {
				u.SetOauthRefreshToken(*e.OAuthRefreshToken)
			} else {
				u.ClearOauthRefreshToken()
			}
			if e.OAuthExpiresAt != nil {
				u.SetOauthExpiresAt(*e.OAuthExpiresAt)
			} else {
				u.ClearOauthExpiresAt()
			}
			if e.PATKey != nil {
				u.SetPatKey(*e.PATKey)
			} else {
				u.ClearPatKey()
			}
			if e.Email != nil {
				u.SetEmail(*e.Email)
			} else {
				u.ClearEmail()
			}
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetAccountExt(ctx, e.AccountID)
}

// TryInsertAccountExt 先写者胜的空插入（首写原子性——并发双导入同一账号时
// 保持先写身份：ON CONFLICT (account_id) DO NOTHING，冲突行跳过不覆盖不报错）。
// 返回是否实际插入：true = 本请求首写胜出（行已含本次全量字段）；
// false = 冲突（已有行），调用方应沿用存量身份后走 UpsertAccountExt 写令牌。
// 账号缺 id（FK）→ error。
func (r *AccountExtRepo) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	err := r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetInstallationID(e.InstallationID).
		SetNillableSessionID(e.SessionID).
		SetNillableThreadID(e.ThreadID).
		SetNillableWindowID(e.WindowID).
		SetNillableOauthToken(e.OAuthToken).
		SetNillableOauthRefreshToken(e.OAuthRefreshToken).
		SetNillableOauthExpiresAt(e.OAuthExpiresAt).
		SetNillablePatKey(e.PATKey).
		SetNillableEmail(e.Email).
		OnConflictColumns(accountext.FieldAccountID).
		DoNothing().
		Exec(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // 冲突：先写者已落行，本次跳过（不覆盖）
	}
	return false, err
}

// GetAccountExt 按账号 id 取扩展行；无行 → ErrNotFound。
func (r *AccountExtRepo) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	row, err := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: account_id=%d missing", ErrNotFound, accountID)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}
