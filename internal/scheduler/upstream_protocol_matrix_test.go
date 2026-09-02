// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestBuildUpstreamRoutesPublishesOnlyProbedConversationProtocols(t *testing.T) {
	checked := time.Now()
	key := "sk-upstream"
	upstream := &domain.Upstream{
		ID: 1, Name: "chat-only", BaseURL: "https://upstream.example",
		UpstreamKey: &key, Models: []string{"gpt-5"}, ModelsCheckedAt: &checked,
		ModelFormats: map[string][]domain.RequestFormat{
			"gpt-5": {domain.FormatOpenAIChat},
		},
		Enabled: true,
	}
	member := &domain.GroupUpstream{
		ID: 10, GroupID: 20, UpstreamID: upstream.ID, Upstream: upstream,
		Weight: 100, Priority: 0, MaxConcurrency: 4, Enabled: true,
	}
	routes := buildUpstreamRoutes([]*upstreamSnapshot{newUpstreamSnapshot(member, nil)}, nil)

	require.Contains(t, routes, routeKey{format: domain.FormatOpenAIChat, model: "gpt-5"})
	for _, format := range []domain.RequestFormat{
		domain.FormatOpenAIResponses,
		domain.FormatAnthropic,
		domain.FormatOpenAIResponsesWS,
		domain.FormatOpenAIImages,
		domain.FormatOpenAISearch,
	} {
		require.NotContains(t, routes, routeKey{format: format, model: "gpt-5"},
			"a chat probe must not advertise another wire protocol")
	}
}

func TestBuildUpstreamRoutesUsesPerMemberProtocolCapabilities(t *testing.T) {
	checked := time.Now()
	keyA, keyB := "sk-a", "sk-b"
	chat := &domain.Upstream{
		ID: 1, Name: "chat", BaseURL: "https://chat.example", UpstreamKey: &keyA,
		Models: []string{"gpt-5"}, ModelsCheckedAt: &checked, Enabled: true,
		ModelFormats: map[string][]domain.RequestFormat{"gpt-5": {domain.FormatOpenAIChat}},
	}
	responses := &domain.Upstream{
		ID: 2, Name: "responses", BaseURL: "https://responses.example", UpstreamKey: &keyB,
		Models: []string{"gpt-5"}, ModelsCheckedAt: &checked, Enabled: true,
		ModelFormats: map[string][]domain.RequestFormat{"gpt-5": {domain.FormatOpenAIResponses}},
	}
	members := []*upstreamSnapshot{
		newUpstreamSnapshot(&domain.GroupUpstream{ID: 10, GroupID: 20, UpstreamID: 1, Upstream: chat, Weight: 100, Enabled: true}, nil),
		newUpstreamSnapshot(&domain.GroupUpstream{ID: 11, GroupID: 20, UpstreamID: 2, Upstream: responses, Weight: 100, Enabled: true}, nil),
	}
	routes := buildUpstreamRoutes(members, nil)

	chatRoute := routes[routeKey{format: domain.FormatOpenAIChat, model: "gpt-5"}]
	require.NotNil(t, chatRoute)
	require.Equal(t, int64(1), chatRoute.seq.seq[0].upstream.ID)
	responsesRoute := routes[routeKey{format: domain.FormatOpenAIResponses, model: "gpt-5"}]
	require.NotNil(t, responsesRoute)
	require.Equal(t, int64(2), responsesRoute.seq.seq[0].upstream.ID)
	require.NotContains(t, routes, routeKey{format: domain.FormatAnthropic, model: "gpt-5"})
}

func TestBuildLegacyUpstreamRoutesDoesNotInventNonConversationCapability(t *testing.T) {
	key := "sk-upstream"
	upstream := &domain.Upstream{
		ID: 1, Name: "legacy", BaseURL: "https://upstream.example",
		UpstreamKey: &key, Enabled: true,
	}
	member := &domain.GroupUpstream{
		ID: 10, GroupID: 20, UpstreamID: upstream.ID, Upstream: upstream,
		Weight: 100, Priority: 0, MaxConcurrency: 4, Enabled: true,
	}
	routes := buildUpstreamRoutes([]*upstreamSnapshot{newUpstreamSnapshot(member, nil)}, nil)

	require.Contains(t, routes, routeKey{format: domain.FormatOpenAIChat})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIResponses})
	require.NotContains(t, routes, routeKey{format: domain.FormatAnthropic})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIResponsesWS})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIImages})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAISearch})
}

func TestBuildUpstreamRoutesTreatsEmptyCapabilityMapAsLegacyChat(t *testing.T) {
	checked := time.Now()
	upstream := &domain.Upstream{
		ID: 1, Name: "mixed-snapshot", BaseURL: "https://upstream.example",
		Models: []string{"gpt-5", "gpt-4.1"}, ModelsCheckedAt: &checked,
		ModelFormats: map[string][]domain.RequestFormat{},
		Enabled:      true,
	}
	member := &domain.GroupUpstream{
		ID: 10, GroupID: 20, UpstreamID: upstream.ID, Upstream: upstream,
		Weight: 100, MaxConcurrency: 4, Enabled: true,
	}
	routes := buildUpstreamRoutes([]*upstreamSnapshot{newUpstreamSnapshot(member, nil)}, nil)

	// An empty capability map is the legacy representation written before
	// protocol-aware probing. Keep the historical Chat route for those rows.
	require.Contains(t, routes, routeKey{format: domain.FormatOpenAIChat, model: "gpt-5"})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIResponses, model: "gpt-5"})
	require.Contains(t, routes, routeKey{format: domain.FormatOpenAIChat, model: "gpt-4.1"})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIResponses, model: "gpt-4.1"})
}

func TestBuildUpstreamRoutesDoesNotWidenMissingCapabilityKey(t *testing.T) {
	checked := time.Now()
	key := "sk-upstream"
	upstream := &domain.Upstream{
		ID: 1, Name: "partial-snapshot", BaseURL: "https://upstream.example",
		UpstreamKey: &key, Models: []string{"gpt-5", "gpt-4.1"}, ModelsCheckedAt: &checked,
		ModelFormats: map[string][]domain.RequestFormat{
			"gpt-5": {domain.FormatOpenAIResponses},
		}, Enabled: true,
	}
	member := &domain.GroupUpstream{ID: 10, GroupID: 20, UpstreamID: 1, Upstream: upstream, Weight: 100, Enabled: true}
	routes := buildUpstreamRoutes([]*upstreamSnapshot{newUpstreamSnapshot(member, nil)}, nil)

	require.Contains(t, routes, routeKey{format: domain.FormatOpenAIResponses, model: "gpt-5"})
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIChat, model: "gpt-4.1"},
		"a model absent from a non-empty capability snapshot is not proven Chat-compatible")
	require.NotContains(t, routes, routeKey{format: domain.FormatOpenAIResponses, model: "gpt-4.1"})
}

func TestBuildUpstreamRoutesTreatsRecordedEmptyCapabilityAsUnavailable(t *testing.T) {
	checked := time.Now()
	upstream := &domain.Upstream{
		ID: 1, Name: "probed-unavailable", BaseURL: "https://upstream.example",
		Models: []string{"gpt-5"}, ModelsCheckedAt: &checked,
		ModelFormats: map[string][]domain.RequestFormat{
			"gpt-5": {},
		},
		Enabled: true,
	}
	member := &domain.GroupUpstream{
		ID: 10, GroupID: 20, UpstreamID: upstream.ID, Upstream: upstream,
		Weight: 100, MaxConcurrency: 4, Enabled: true,
	}
	routes := buildUpstreamRoutes([]*upstreamSnapshot{newUpstreamSnapshot(member, nil)}, nil)

	require.Empty(t, routes)
}
