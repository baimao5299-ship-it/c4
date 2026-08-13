// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// WriteOAuthRotation 轮转回写（SDK 接入 T5 §1——SDK OnTokenRotated 回调落库
// 面）：account_ext **部分更新**（幂等收敛语义与 upsert 等价——重复回调重复
// UPDATE 收敛）仅 oauth_token / oauth_refresh_token / oauth_expires_at 三列
// ——其余列不动（避免 UpsertAccountExt 全量 upsert 的 ClearX 清空：nil 字段
// 会把 session/thread/window/pat/email 等列清 NULL，ext_codex_repo.go:43-85）。
//
// expiresAt 为调用方携带的旧过期时刻（SDK 回调无 expiry、refreshResponse 无
// expires_in——保旧语义：恒写携带值，不引入新值；nil = 写 NULL，与"未知过
// 期"语义一致）。at/rt 由 SDK 保证非空（响应缺 refresh_token 时 SDK 保留内
// 存旧 rt 后回调——盲写不落空）。
//
// 行缺失（配置损坏——codex 账号必有 ext 行，选号前提）→ 0 行报错：错误 →
// SDK D4 回调重试 → 连续达阈值 CallbackDeliveryError fatal（fail-closed：
// 令牌无法持久化 = 账号失效信号，管理员重新导入后恢复）。不做 INSERT 路径
// ——ent 要求必填身份列（installation_id/credential_type），回调无身份材料
//（缺行场景 = 配置损坏，应报错而非以空身份静默建行）。
func (r *AccountExtRepo) WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error {
	u := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(accountID)).
		SetOauthToken(at).
		SetOauthRefreshToken(rt)
	if expiresAt != nil {
		u = u.SetOauthExpiresAt(*expiresAt)
	} else {
		u = u.ClearOauthExpiresAt() // SetNillable(nil) 是 no-op——保旧 nil 需显式清
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: account_id=%d ext row missing (codex account must have account_ext)", ErrNotFound, accountID)
	}
	return nil
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
