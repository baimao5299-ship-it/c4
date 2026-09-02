// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestCreateUpstreamGroupRejectsModelsMissingFromConfirmedCatalogues(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay", BaseURL: "https://relay.example", Enabled: true,
		Models: []string{"gpt-4o"}, ModelsCheckedAt: &checkedAt,
	}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	_, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{
		Name: "bad-model", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"},
	}, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), "gpt-5")
	require.Zero(t, store.routingCalls, "validation must run before the atomic repository write")
}

func TestCreateUpstreamGroupAllowsUnknownCatalogueAndUsesAnySupportingMember(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "checked", BaseURL: "https://checked.example", Enabled: true,
		Models: []string{"gpt-4o"}, ModelsCheckedAt: &checkedAt,
	}
	store.upstreams[12] = &domain.Upstream{
		ID: 12, Name: "unchecked", BaseURL: "https://unchecked.example", Enabled: true,
	}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	created, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{
		Name: "compatible", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"},
	}, []*domain.GroupUpstream{{UpstreamID: 11}, {UpstreamID: 12}})
	require.NoError(t, err)
	require.NotZero(t, created.ID)
	require.Equal(t, 1, store.routingCalls)
}

func TestCreateUpstreamGroupMatchesWhitespaceNormalizedModelCatalogue(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay", BaseURL: "https://relay.example", Enabled: true,
		Models: []string{"  claude-opus-4-6  "}, ModelsCheckedAt: &checkedAt,
	}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	created, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{
		Name: "normalized", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"claude-opus-4-6"},
	}, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.NoError(t, err, "surrounding whitespace in a legacy catalogue must not reject a usable model")
	require.NotZero(t, created.ID)
}

func TestCreateUpstreamGroupAcceptsModelRecordedByManualProtocolProbe(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay", BaseURL: "https://relay.example", Enabled: true,
		ModelsCheckedAt: &checkedAt,
		// Explicit model tests are allowed to record tenant aliases that the
		// provider omits from /models. Group validation must honor that evidence.
		ModelFormats: map[string][]domain.RequestFormat{
			"tenant-k3": {domain.FormatOpenAIResponses},
		},
	}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	created, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{
		Name: "manual-model", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"tenant-k3"},
	}, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.NoError(t, err)
	require.NotZero(t, created.ID)
}

func TestCreateUpstreamGroupRejectsModelWithEmptyVerifiedProtocolSet(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay", BaseURL: "https://relay.example", Enabled: true,
		Models: []string{"unavailable-model"}, ModelsCheckedAt: &checkedAt,
		ModelFormats: map[string][]domain.RequestFormat{"unavailable-model": {}},
	}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	_, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{
		Name: "unavailable-model", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"unavailable-model"},
	}, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestSetGroupUpstreamsRejectsReplacementThatDropsAllowedModel(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	checkedAt := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	store.groups[1] = &domain.Group{
		ID: 1, Name: "relay", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"},
	}
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay", BaseURL: "https://relay.example", Enabled: true,
		Models: []string{"gpt-4o"}, ModelsCheckedAt: &checkedAt,
	}
	store.members[1] = []*domain.GroupUpstream{{ID: 21, GroupID: 1, UpstreamID: 11, Enabled: true}}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	_, err := svc.SetGroupUpstreams(context.Background(), 1, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Len(t, store.members[1], 1, "invalid replacement leaves the old member set intact")
}
