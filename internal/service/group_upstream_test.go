// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// groupUpstreamStoreStub keeps these tests independent of PostgreSQL while
// exercising the same optional-store discovery used by production Repository.
type groupUpstreamStoreStub struct {
	*fakeStore
	upstreams    map[int64]*domain.Upstream
	members      map[int64][]*domain.GroupUpstream
	routingCalls int
	routingErr   error
	updateCalls  int
	updateErr    error
}

func newGroupUpstreamStoreStub() *groupUpstreamStoreStub {
	return &groupUpstreamStoreStub{fakeStore: newFakeStore(), upstreams: map[int64]*domain.Upstream{}, members: map[int64][]*domain.GroupUpstream{}}
}

func (s *groupUpstreamStoreStub) GetUpstream(_ context.Context, id int64) (*domain.Upstream, error) {
	u, ok := s.upstreams[id]
	if !ok || u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	cp := *u
	return &cp, nil
}
func (s *groupUpstreamStoreStub) CreateUpstream(context.Context, *domain.Upstream) (*domain.Upstream, error) {
	return nil, fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) ListUpstreams(context.Context, repository.ListQuery) ([]*domain.Upstream, int64, error) {
	return nil, 0, fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) UpdateUpstream(context.Context, *domain.Upstream) (*domain.Upstream, error) {
	return nil, fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) SetUpstreamEnabled(context.Context, int64, bool) (*domain.Upstream, error) {
	return nil, fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) DeleteUpstream(context.Context, int64) error {
	return fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) RecordUpstreamProbe(context.Context, *domain.Upstream, bool, int64, *string) (*domain.Upstream, error) {
	return nil, fmt.Errorf("not used")
}
func (s *groupUpstreamStoreStub) RecordUpstreamBalance(context.Context, *domain.Upstream, *string, *string, string, *time.Time) (*domain.Upstream, error) {
	return nil, fmt.Errorf("not used")
}

func (s *groupUpstreamStoreStub) ListGroupUpstreams(_ context.Context, id int64) ([]*domain.GroupUpstream, error) {
	rows := s.members[id]
	out := make([]*domain.GroupUpstream, 0, len(rows))
	for _, row := range rows {
		cp := *row
		if u := s.upstreams[row.UpstreamID]; u != nil {
			ucp := *u
			cp.Upstream = &ucp
		}
		out = append(out, &cp)
	}
	return out, nil
}
func (s *groupUpstreamStoreStub) SetGroupUpstreams(_ context.Context, id int64, members []*domain.GroupUpstream) error {
	rows := make([]*domain.GroupUpstream, 0, len(members))
	for i, row := range members {
		cp := *row
		cp.ID = int64(i + 1)
		rows = append(rows, &cp)
	}
	s.members[id] = rows
	return nil
}

func (s *groupUpstreamStoreStub) CreateGroupWithUpstreams(_ context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	s.routingCalls++
	if s.routingErr != nil {
		return nil, s.routingErr
	}
	if group == nil || len(members) == 0 {
		return nil, fmt.Errorf("invalid routing payload")
	}
	created := *group
	created.ID = int64(len(s.groups) + 1)
	s.groups[created.ID] = &created
	rows := make([]*domain.GroupUpstream, 0, len(members))
	for i, member := range members {
		cp := *member
		cp.ID = int64(i + 1)
		cp.GroupID = created.ID
		rows = append(rows, &cp)
	}
	s.members[created.ID] = rows
	return &created, nil
}

func (s *groupUpstreamStoreStub) UpdateGroupWithUpstreams(_ context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	s.updateCalls++
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	if group == nil || group.ID <= 0 {
		return nil, fmt.Errorf("invalid routing payload")
	}
	updated := *group
	s.groups[group.ID] = &updated
	rows := make([]*domain.GroupUpstream, 0, len(members))
	for i, member := range members {
		cp := *member
		cp.ID = int64(i + 1)
		cp.GroupID = group.ID
		rows = append(rows, &cp)
	}
	s.members[group.ID] = rows
	return &updated, nil
}

var _ Store = (*groupUpstreamStoreStub)(nil)
var _ UpstreamStore = (*groupUpstreamStoreStub)(nil)
var _ GroupUpstreamStore = (*groupUpstreamStoreStub)(nil)
var _ GroupRoutingStore = (*groupUpstreamStoreStub)(nil)

func TestSetGroupUpstreamsValidatesAndReplacesAtomically(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	group := &domain.Group{ID: 1, Name: "relay", Visibility: domain.GroupVisibilityPublic, RoutingMode: domain.GroupRoutingModeUpstreams}
	store.groups[group.ID] = group
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	store.upstreams[12] = &domain.Upstream{ID: 12, Name: "b", BaseURL: "https://b.example", Enabled: true}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	rows, err := svc.SetGroupUpstreams(context.Background(), group.ID, []*domain.GroupUpstream{{UpstreamID: 11, Enabled: true}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 100, rows[0].Weight, "default weight")
	require.Equal(t, 8, rows[0].MaxConcurrency, "default concurrency")

	_, err = svc.SetGroupUpstreams(context.Background(), group.ID, []*domain.GroupUpstream{{UpstreamID: 11}, {UpstreamID: 11}})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Len(t, store.members[group.ID], 1, "invalid replacement leaves old set intact")

	_, err = svc.SetGroupUpstreams(context.Background(), group.ID, []*domain.GroupUpstream{{UpstreamID: 404}})
	require.ErrorIs(t, err, ErrNotFound)
	require.Len(t, store.members[group.ID], 1, "missing upstream leaves old set intact")
}

func TestListGroupUpstreamsRejectsDeletedGroup(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	now := time.Now()
	store.groups[1] = &domain.Group{ID: 1, Name: "deleted", DeletedAt: &now}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	_, err := svc.ListGroupUpstreams(context.Background(), 1)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestUpstreamGroupCannotBePublishedWithoutModelAndMember(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	_, err := svc.CreateGroupWithRouting(context.Background(), "empty", domain.GroupVisibilityPublic, nil, nil, domain.GroupRoutingModeUpstreams, nil)
	require.ErrorIs(t, err, ErrInvalidInput)

	group := &domain.Group{ID: 2, Name: "live", Visibility: domain.GroupVisibilityPublic, RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"model-a"}}
	store.groups[group.ID] = group
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	_, err = svc.SetGroupUpstreams(context.Background(), group.ID, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestCreateUpstreamGroupUsesAtomicRepositoryOperation(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	pub := &pubRecorder{}
	svc := New(store, nil, NopInvalidator{}, pub, nil, nil, nil)

	group := &domain.Group{Name: "relay", Visibility: domain.GroupVisibilityPublic, RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"}}
	created, err := svc.CreateUpstreamGroup(context.Background(), group, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.NoError(t, err)
	require.Equal(t, 1, store.routingCalls)
	require.NotZero(t, created.ID)
	require.Len(t, store.members[created.ID], 1)
	require.Equal(t, 100, store.members[created.ID][0].Weight)
	require.Equal(t, 8, store.members[created.ID][0].MaxConcurrency)
	require.Equal(t, 1, pub.total())
	require.True(t, pub.last().Multipliers)
	require.Equal(t, []int64{created.ID}, pub.last().Groups)
}

func TestCreateUpstreamGroupRejectsIncompletePayloadBeforeRepository(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	_, err := svc.CreateUpstreamGroup(context.Background(), &domain.Group{Name: "empty", RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"}}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Zero(t, store.routingCalls)
	_, err = svc.CreateUpstreamGroup(context.Background(), &domain.Group{Name: "no-model", RoutingMode: domain.GroupRoutingModeUpstreams}, []*domain.GroupUpstream{{UpstreamID: 11}})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Zero(t, store.routingCalls)
}

func TestUpdateGroupWithUpstreamsCommitsPolicyAndMembersTogether(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	store.groups[1] = &domain.Group{ID: 1, Name: "old", Visibility: domain.GroupVisibilityPublic, RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-old"}, PriceMultiplier: 10000}
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	store.upstreams[12] = &domain.Upstream{ID: 12, Name: "b", BaseURL: "https://b.example", Enabled: true}
	store.members[1] = []*domain.GroupUpstream{{ID: 1, GroupID: 1, UpstreamID: 11, Weight: 100, MaxConcurrency: 8, Enabled: true}}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	updated, err := svc.UpdateGroupWithUpstreams(context.Background(), &domain.Group{
		ID: 1, Name: "new", Visibility: domain.GroupVisibilityPrivate,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-new"}, PriceMultiplier: 12500,
	}, []*domain.GroupUpstream{{UpstreamID: 12, Weight: 60, MaxConcurrency: 4, Enabled: true}})
	require.NoError(t, err)
	require.Equal(t, 1, store.updateCalls)
	require.Equal(t, "new", updated.Name)
	require.Equal(t, []string{"gpt-new"}, updated.AllowedModels)
	require.Len(t, store.members[1], 1)
	require.Equal(t, int64(12), store.members[1][0].UpstreamID)
	require.Equal(t, 60, store.members[1][0].Weight)
}

func TestUpdateGroupWithUpstreamsRejectsInvalidReplacementBeforeWrite(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	store.groups[1] = &domain.Group{ID: 1, Name: "old", Visibility: domain.GroupVisibilityPublic, RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-old"}, PriceMultiplier: 10000}
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "a", BaseURL: "https://a.example", Enabled: true}
	store.members[1] = []*domain.GroupUpstream{{ID: 1, GroupID: 1, UpstreamID: 11, Weight: 100, MaxConcurrency: 8, Enabled: true}}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	_, err := svc.UpdateGroupWithUpstreams(context.Background(), &domain.Group{
		ID: 1, Name: "bad", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-new"}, PriceMultiplier: 10000,
	}, []*domain.GroupUpstream{{UpstreamID: 404}})
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, store.updateCalls)
	require.Equal(t, "old", store.groups[1].Name)
	require.Equal(t, int64(11), store.members[1][0].UpstreamID)
}
