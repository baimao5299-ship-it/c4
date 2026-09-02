// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// GroupModels 单元测试（GET /v1/models 端点数据源）：快照未加载 → false；
// 组缺失 → false；跨格式去重 + 字典序稳定排序；空组 → 空列表 ok。
func TestGroupModels(t *testing.T) {
	// 快照未加载（构造后未 reload——atomic.Value 零值断言失败）→ false
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), newMemLoader(nil), re, nil)
	_, ok := s.GroupModels(10)
	require.False(t, ok, "快照未加载 → false（同 Select 的 ErrGroupNotFound 守卫）")

	// 已加载：gpt-4o 跨三格式（chat/responses/images）重复必须去重；空组 20 在
	// 快照内（无账号组保留空条目）→ 空列表 ok。
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	resp := tpl(2, domain.FormatOpenAIResponses, []string{"gpt-4o", "o3"})
	img := tpl(3, domain.FormatOpenAIImages, []string{"gpt-4o", "dall-e-3"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, chat, 4), acc(2, resp, 4), acc(3, img, 4)},
		20: nil,
	})
	s = newSched(t, m)

	models, ok := s.GroupModels(10)
	require.True(t, ok)
	require.Equal(t, []string{"dall-e-3", "gpt-4o", "o3"}, models, "跨格式去重 + 字典序稳定排序")

	empty, ok := s.GroupModels(20)
	require.True(t, ok, "空组存在 → ok（空 data 数组 200 语义）")
	require.Empty(t, empty)

	_, ok = s.GroupModels(999)
	require.False(t, ok, "组不存在 → false")
}

func TestGroupModelsUpstreamPool(t *testing.T) {
	member := testGroupUpstream(1, 101, 1, 0, 4)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		30: testUpstreamGroup(30, member),
	})

	models, ok := s.GroupModels(30)
	require.True(t, ok, "上游池组存在时应返回模型列表")
	require.Equal(t, []string{"gpt-5"}, models, "上游池模型来自 upstreamRoutes，而不是账号池 routes")
}

func TestUpstreamModelCapabilitiesFilterRoutes(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 0, 4)
	b := testGroupUpstream(2, 102, 1, 0, 4)
	a.Upstream.Models = []string{"gpt-5", "o3"}
	b.Upstream.Models = []string{"gpt-5"}

	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		31: {ID: 31, Name: "capabilities", RoutingMode: domain.GroupRoutingModeUpstreams, UpstreamMembers: []*domain.GroupUpstream{a, b}},
	})

	models, ok := s.GroupModels(31)
	require.True(t, ok)
	require.Equal(t, []string{"gpt-5", "o3"}, models, "空白名单公开所有已确认快照的模型并集")

	sel, err := s.Select(31, domain.FormatOpenAIChat, "o3")
	require.NoError(t, err, "上游独有但已确认的模型应创建独立路由")
	require.Equal(t, a.ID, sel.TargetID, "模型路由只能选择实际声明该模型的上游")
	s.ReleaseSelection(sel)

	_, err = s.Select(31, domain.FormatOpenAIChat, "gpt-4")
	require.ErrorIs(t, err, ErrFormatUnavailable, "不在已确认模型并集中的模型不得创建路由")
}

func TestUpstreamModelCapabilitiesEmptySnapshotDoesNotCreateAutomaticRoutes(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 0, 4)
	b := testGroupUpstream(2, 102, 1, 0, 4)
	a.Upstream.Models = nil
	b.Upstream.Models = nil

	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		32: {ID: 32, Name: "empty-capabilities", RoutingMode: domain.GroupRoutingModeUpstreams, UpstreamMembers: []*domain.GroupUpstream{a, b}},
	})

	models, ok := s.GroupModels(32)
	require.True(t, ok)
	require.Empty(t, models, "已检查但为空的快照不能伪造模型路由")
	_, err := s.Select(32, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrFormatUnavailable)
}

func TestUpstreamModelCapabilitiesUnknownMemberRemainsCandidateForKnownModel(t *testing.T) {
	checked := testGroupUpstream(1, 101, 1, 0, 4)
	unknown := testGroupUpstream(2, 102, 1, 0, 4)
	unknown.Upstream.ModelsCheckedAt = nil
	unknown.Upstream.Models = nil

	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		33: {ID: 33, Name: "mixed-capabilities", RoutingMode: domain.GroupRoutingModeUpstreams, UpstreamMembers: []*domain.GroupUpstream{checked, unknown}},
	})

	models, ok := s.GroupModels(33)
	require.True(t, ok)
	require.Equal(t, []string{"gpt-5"}, models, "未知成员不能凭空增加模型，但不能隐藏已确认模型")

	// Both members are valid candidates for the known model: the checked member
	// advertises it, while the unchecked member is retained as an unknown
	// capability fallback. Excluding the confirmed member must therefore select
	// the unchecked member instead of returning a false unavailable error.
	sel, err := s.SelectExcluding(33, domain.FormatOpenAIChat, "gpt-5", []int64{checked.ID})
	require.NoError(t, err)
	require.Equal(t, unknown.ID, sel.TargetID)
	s.ReleaseSelection(sel)
}

func TestUpstreamModelCapabilitiesNormalizesWhitespace(t *testing.T) {
	member := testGroupUpstream(1, 101, 1, 0, 4)
	member.Upstream.Models = []string{" gpt-5 ", "gpt-5"}

	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		34: {ID: 34, Name: "trim-capabilities", RoutingMode: domain.GroupRoutingModeUpstreams, UpstreamMembers: []*domain.GroupUpstream{member}},
	})

	models, ok := s.GroupModels(34)
	require.True(t, ok)
	require.Equal(t, []string{"gpt-5"}, models, "模型快照中的空白不应生成重复或不可请求的模型名")
	sel, err := s.Select(34, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	s.ReleaseSelection(sel)
}

func TestUpstreamSelectionNormalizesRequestedModelWhitespace(t *testing.T) {
	member := testGroupUpstream(1, 101, 1, 0, 4)
	member.Upstream.Models = []string{"gpt-5"}

	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{
		35: {ID: 35, Name: "trim-request", RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"}, UpstreamMembers: []*domain.GroupUpstream{member}},
	})

	sel, err := s.Select(35, domain.FormatOpenAIChat, "  gpt-5  ")
	require.NoError(t, err, "surrounding request whitespace must not hide a routable model")
	require.Equal(t, "gpt-5", sel.Model)
	s.ReleaseSelection(sel)
}
