// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestAccountBaseURLInvalidation 账号级 base_url 变更 → clients 失效（C2）：
// UpdateAccount 按值判定（M4——nil↔"" 同值不误报）并入既有 keyChanged（复用
// Accounts(gids, keyChanged) 参数面，零新增失效类型）；UpdateAccountsBatch
// 保守失效（提供 BaseURL 即失效，含 "" 清空态）。
func TestAccountBaseURLInvalidation(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	tpl, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := svc.CreateAccount(ctx, &domain.Account{
		Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1",
	})
	require.NoError(t, err)

	t.Run("UpdateAccount base_url 变更 → keyChanged", func(t *testing.T) {
		b := "https://acc.example.com"
		_, err := svc.UpdateAccount(ctx, &domain.Account{
			ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", BaseURL: &b,
		})
		require.NoError(t, err)
		require.True(t, rec.last().key, "账号级 base_url 变更 → clients 失效（keyChanged=true）")
	})

	t.Run("UpdateAccount base_url 不变 → 不失效", func(t *testing.T) {
		b := "https://acc.example.com"
		_, err := svc.UpdateAccount(ctx, &domain.Account{
			ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", BaseURL: &b,
		})
		require.NoError(t, err)
		require.False(t, rec.last().key, "base_url 不变 → keyChanged=false（按值判定不误报）")
	})

	t.Run("UpdateAccountsBatch 提供 BaseURL（非空与空串清空）→ keyChanged", func(t *testing.T) {
		b := "https://batch.example.com"
		require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{BaseURL: &b}))
		require.True(t, rec.last().key, "批量非空 base_url → 保守失效")
		empty := ""
		require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{BaseURL: &empty}))
		require.True(t, rec.last().key, "批量空串（清空态）→ 保守失效")
	})

	t.Run("UpdateAccountsBatch 未提供 BaseURL/UpstreamKey → 不失效", func(t *testing.T) {
		name := "a1"
		require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Name: &name}))
		require.False(t, rec.last().key, "未提供 base_url/upstream_key → keyChanged=false")
	})
}

// TestValidateAccountBaseURL 账号级 base_url 提供时复用 validateBaseURL（I1）：
// 非空非法（无 scheme/含 /v1）→ ErrInvalidInput；nil/空串跳过（create 路径
// 已归一，双保险）。
func TestValidateAccountBaseURL(t *testing.T) {
	a := &domain.Account{Name: "a", TemplateID: 1, BaseURL: strPtr("no-scheme")}
	require.ErrorIs(t, validateAccount(a), ErrInvalidInput, "无 scheme → 复用 validateBaseURL 拒绝")
	a.BaseURL = strPtr("https://u/v1")
	require.ErrorIs(t, validateAccount(a), ErrInvalidInput, "裸根约定：含 /v1 拒绝")
	a.BaseURL = strPtr("https://api.example.com")
	require.NoError(t, validateAccount(a))
	a.BaseURL = nil
	require.NoError(t, validateAccount(a), "nil = 继承模板，跳过校验")
	a.BaseURL = strPtr("")
	require.NoError(t, validateAccount(a), "空串跳过校验（create 已归一 nil）")
}

// TestValidateAccountPatchBaseURL 批量三态（C1）：空串放行（清空合法）；
// 非空时复用 validateBaseURL；nil = 不变不校验。
func TestValidateAccountPatchBaseURL(t *testing.T) {
	require.NoError(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("")}), "空串 = 清空，合法")
	require.NoError(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("https://api.example.com")}))
	require.ErrorIs(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("no-scheme")}), ErrInvalidInput)
	require.NoError(t, validateAccountPatch(repository.AccountPatch{BaseURL: nil}), "nil = 不变")
}
