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
	require.Equal(t, []string{"gpt-5"}, models, "空白名单只公开所有成员共同确认的模型")

	_, err := s.Select(31, domain.FormatOpenAIChat, "o3")
	require.ErrorIs(t, err, ErrFormatUnavailable, "不在共同能力集中的模型不得创建路由")
}
