// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestSelectResolvesUniqueModelAlias(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"kimi-k3"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "k3")
	require.NoError(t, err)
	require.Equal(t, "kimi-k3", sel.Model, "the upstream request must use the canonical model ID")
	s.ReleaseSelection(sel)
}

func TestModelShortAliasesRecognizesClaudeVersionShorthands(t *testing.T) {
	aliases := modelShortAliases("claude-3-7-sonnet")
	require.Contains(t, aliases, "c3")
	require.Contains(t, aliases, "c37")
}

func TestSelectResolvesClaudeVersionAlias(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"claude-3-7-sonnet"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})

	for _, requested := range []string{"c3", "C37"} {
		sel, err := s.Select(10, domain.FormatOpenAIChat, requested)
		require.NoError(t, err, requested)
		require.Equal(t, "claude-3-7-sonnet", sel.Model, requested)
		s.ReleaseSelection(sel)
	}
}

func TestSelectKeepsAmbiguousModelAliasUnresolved(t *testing.T) {
	a := acc(1, tpl(1, domain.FormatOpenAIChat, []string{"kimi-k3"}), 4)
	b := acc(2, tpl(2, domain.FormatOpenAIChat, []string{"qwen-k3"}), 4)
	s := newTestScheduler(t, []*domain.Account{a, b})

	_, err := s.Select(10, domain.FormatOpenAIChat, "k3")
	require.ErrorIs(t, err, ErrFormatUnavailable, "an alias shared by two models must not guess")

	sel, err := s.Select(10, domain.FormatOpenAIChat, "kimi-k3")
	require.NoError(t, err, "exact model IDs still select normally")
	require.Equal(t, int64(1), sel.AccountID)
	s.ReleaseSelection(sel)
}

func TestSelectExactModelWinsOverAlias(t *testing.T) {
	a := acc(1, tpl(1, domain.FormatOpenAIChat, []string{"kimi-k3"}), 4)
	b := acc(2, tpl(2, domain.FormatOpenAIChat, []string{"qwen-k3", "k3"}), 4)
	s := newTestScheduler(t, []*domain.Account{a, b})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "k3")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "an explicitly advertised ID is authoritative over generated aliases")
	require.Equal(t, "k3", sel.Model)
	s.ReleaseSelection(sel)
}

func TestUpstreamSelectionResolvesUniqueModelAlias(t *testing.T) {
	checked := time.Now()
	key := "relay-key"
	u := &domain.Upstream{
		ID: 101, Name: "kimi-relay", BaseURL: "https://relay.example",
		UpstreamKey: &key, Models: []string{"kimi-k3"}, ModelsCheckedAt: &checked,
		Enabled: true,
	}
	member := &domain.GroupUpstream{
		ID: 1, GroupID: 10, UpstreamID: u.ID, Upstream: u,
		Weight: 100, Priority: 1, MaxConcurrency: 4, Enabled: true,
	}
	group := &domain.Group{ID: 10, Name: "relay-group", RoutingMode: domain.GroupRoutingModeUpstreams,
		AllowedModels: []string{"kimi-k3"}, UpstreamMembers: []*domain.GroupUpstream{member}}
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: group})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "k3")
	require.NoError(t, err)
	require.Equal(t, "kimi-k3", sel.Model)
	s.ReleaseSelection(sel)
}

func TestModelAliasCollisionIsScopedToRequestFormat(t *testing.T) {
	chat := acc(1, tpl(1, domain.FormatOpenAIChat, []string{"kimi-k3"}), 4)
	messages := acc(2, tpl(2, domain.FormatAnthropic, []string{"qwen-k3"}), 4)
	s := newTestScheduler(t, []*domain.Account{chat, messages})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "k3")
	require.NoError(t, err, "an Anthropic alias collision must not block the Chat route")
	require.Equal(t, "kimi-k3", sel.Model)
	s.ReleaseSelection(sel)
}

func TestUpstreamModelCosmeticVariantsShareRouteAndPreserveRawID(t *testing.T) {
	checked := time.Now()
	key := "relay-key"
	u := &domain.Upstream{
		ID: 201, Name: "variant-relay", BaseURL: "https://relay.example",
		UpstreamKey: &key, Models: []string{"Claude-Fable-5.1"}, ModelsCheckedAt: &checked,
		Enabled: true,
	}
	member := &domain.GroupUpstream{ID: 2, GroupID: 20, UpstreamID: u.ID, Upstream: u, Weight: 100, Enabled: true}
	group := &domain.Group{ID: 20, Name: "variant-group", RoutingMode: domain.GroupRoutingModeUpstreams,
		AllowedModels: []string{"claude-fable-5-1"}, UpstreamMembers: []*domain.GroupUpstream{member}}
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{20: group})

	models, ok := s.GroupModels(20)
	require.True(t, ok)
	require.Equal(t, []string{"claude-fable-5-1"}, models)

	sel, err := s.Select(20, domain.FormatOpenAIChat, "CLAUDE_FABLE_5.1")
	require.NoError(t, err)
	require.Equal(t, "Claude-Fable-5.1", sel.Model, "proxy must send the upstream's original model spelling")
	s.ReleaseSelection(sel)
}

func TestUpstreamSelectionPreservesProtocolSpecificRawModelID(t *testing.T) {
	checked := time.Now()
	key := "relay-key"
	u := &domain.Upstream{
		ID: 202, Name: "protocol-variants", BaseURL: "https://relay.example",
		UpstreamKey: &key, Models: []string{"Model.1", "MODEL-1"}, ModelsCheckedAt: &checked,
		ModelFormats: map[string][]domain.RequestFormat{
			"Model.1": {domain.FormatOpenAIChat},
			"MODEL-1": {domain.FormatOpenAIResponses},
		},
		Enabled: true,
	}
	member := &domain.GroupUpstream{ID: 3, GroupID: 21, UpstreamID: u.ID, Upstream: u, Weight: 100, Enabled: true}
	group := &domain.Group{ID: 21, Name: "protocol-group", RoutingMode: domain.GroupRoutingModeUpstreams,
		AllowedModels: []string{"model-1"}, UpstreamMembers: []*domain.GroupUpstream{member}}
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{21: group})

	chat, err := s.Select(21, domain.FormatOpenAIChat, "model-1")
	require.NoError(t, err)
	require.Equal(t, "Model.1", chat.Model)
	s.ReleaseSelection(chat)

	responses, err := s.Select(21, domain.FormatOpenAIResponses, "model.1")
	require.NoError(t, err)
	require.Equal(t, "MODEL-1", responses.Model)
	s.ReleaseSelection(responses)
}
