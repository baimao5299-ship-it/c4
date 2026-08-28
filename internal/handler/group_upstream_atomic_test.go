// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// atomicGroupTestStore adds the repository's atomic group-routing capability
// to the existing upstream test store without widening the shared fake.
type atomicGroupTestStore struct {
	*upstreamTestStore
	members map[int64][]*domain.GroupUpstream
}

func (s *atomicGroupTestStore) ListGroupUpstreams(_ context.Context, groupID int64) ([]*domain.GroupUpstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAtomicGroupMembers(s.members[groupID]), nil
}

func (s *atomicGroupTestStore) SetGroupUpstreams(_ context.Context, groupID int64, members []*domain.GroupUpstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[groupID] = cloneAtomicGroupMembers(members)
	return nil
}

func (s *atomicGroupTestStore) CreateGroupWithUpstreams(_ context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.groupNameConflictLocked(0, group.Name); err != nil {
		return nil, err
	}
	created := *group
	created.ID = s.nextID
	s.nextID++
	now := time.Now()
	created.CreatedAt = now
	created.UpdatedAt = now
	created.AllowedModels = append([]string(nil), group.AllowedModels...)
	created.ProtocolConverts = append([]domain.ProtocolConvert(nil), group.ProtocolConverts...)
	s.groups[created.ID] = &created
	rows := cloneAtomicGroupMembers(members)
	for _, row := range rows {
		row.ID = s.nextID
		s.nextID++
		row.GroupID = created.ID
		row.CreatedAt = now
		row.UpdatedAt = now
	}
	s.members[created.ID] = rows
	out := created
	return &out, nil
}

func (s *atomicGroupTestStore) UpdateGroupWithUpstreams(_ context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[group.ID]; !ok {
		return nil, fmt.Errorf("%w: group missing", repository.ErrNotFound)
	}
	if err := s.groupNameConflictLocked(group.ID, group.Name); err != nil {
		return nil, err
	}
	updated := *group
	updated.UpdatedAt = time.Now()
	updated.AllowedModels = append([]string(nil), group.AllowedModels...)
	updated.ProtocolConverts = append([]domain.ProtocolConvert(nil), group.ProtocolConverts...)
	s.groups[group.ID] = &updated
	rows := cloneAtomicGroupMembers(members)
	for _, row := range rows {
		row.ID = s.nextID
		s.nextID++
		row.GroupID = group.ID
		row.CreatedAt = updated.UpdatedAt
		row.UpdatedAt = updated.UpdatedAt
	}
	s.members[group.ID] = rows
	out := updated
	return &out, nil
}

func cloneAtomicGroupMembers(rows []*domain.GroupUpstream) []*domain.GroupUpstream {
	out := make([]*domain.GroupUpstream, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			out = append(out, nil)
			continue
		}
		cloned := *row
		cloned.Upstream = nil
		out = append(out, &cloned)
	}
	return out
}

func newAtomicGroupHandler(t *testing.T) (*AdminAPI, *atomicGroupTestStore, int64, int64) {
	t.Helper()
	base := newUpstreamTestStore()
	first, err := base.CreateUpstream(context.Background(), &domain.Upstream{
		Name: "primary", BaseURL: "https://primary.example.com", Enabled: true,
		Models: []string{"gpt-5.6", "gpt-5.5"}, MultiplierBP: 600,
	})
	require.NoError(t, err)
	second, err := base.CreateUpstream(context.Background(), &domain.Upstream{
		Name: "secondary", BaseURL: "https://secondary.example.com", Enabled: true,
		Models: []string{"gpt-5.6", "gpt-5.5"}, MultiplierBP: 700,
	})
	require.NoError(t, err)
	store := &atomicGroupTestStore{upstreamTestStore: base, members: make(map[int64][]*domain.GroupUpstream)}
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	return New(svc), store, first.ID, second.ID
}

func postAtomicGroup(t *testing.T, h *AdminAPI, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.PostGroups(rec, req)
	return rec
}

func requireAtomicGroupCount(t *testing.T, store *atomicGroupTestStore, want int) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.groups, want)
	if want == 0 {
		require.Empty(t, store.members, "失败请求不得留下成员关系")
	}
}

func TestPostGroupsUpstreamsAtomic(t *testing.T) {
	t.Run("complete payload commits group and members", func(t *testing.T) {
		h, store, firstID, secondID := newAtomicGroupHandler(t)
		body := `{
			"name":"premium-route",
			"visibility":"private",
			"routing_mode":"upstreams",
			"allowed_models":["gpt-5.6","gpt-5.5"],
			"price_multiplier":1.25,
			"protocol_convert":["chat_to_resp"],
			"upstream_members":[
				{"upstream_id":` + itoa(firstID) + `,"weight":60,"priority":0,"max_concurrency":12,"enabled":true},
				{"upstream_id":` + itoa(secondID) + `,"weight":40,"priority":1,"max_concurrency":6,"enabled":false}
			]
		}`
		rec := postAtomicGroup(t, h, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.NotNil(t, got.ID)
		require.Equal(t, "premium-route", *got.Name)
		require.Equal(t, GroupVisibility(domain.GroupVisibilityPrivate), *got.Visibility)
		require.Equal(t, GroupRoutingMode(domain.GroupRoutingModeUpstreams), *got.RoutingMode)
		require.Equal(t, 1.25, *got.PriceMultiplier)
		require.ElementsMatch(t, []string{"gpt-5.6", "gpt-5.5"}, *got.AllowedModels)

		store.mu.Lock()
		rows := cloneAtomicGroupMembers(store.members[*got.ID])
		stored := *store.groups[*got.ID]
		store.mu.Unlock()
		require.Equal(t, domain.GroupRoutingModeUpstreams, stored.RoutingMode)
		require.Equal(t, 12500, stored.PriceMultiplier)
		require.Len(t, rows, 2)
		require.Equal(t, firstID, rows[0].UpstreamID)
		require.Equal(t, 60, rows[0].Weight)
		require.Equal(t, 12, rows[0].MaxConcurrency)
		require.True(t, rows[0].Enabled)
		require.Equal(t, secondID, rows[1].UpstreamID)
		require.False(t, rows[1].Enabled)
	})

	t.Run("unknown member leaves no group", func(t *testing.T) {
		h, store, _, _ := newAtomicGroupHandler(t)
		rec := postAtomicGroup(t, h, `{
			"name":"bad-member","routing_mode":"upstreams",
			"allowed_models":["gpt-5.6"],
			"upstream_members":[{"upstream_id":999999}]
		}`)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		requireAtomicGroupCount(t, store, 0)
	})

	t.Run("invalid model leaves no group", func(t *testing.T) {
		h, store, firstID, _ := newAtomicGroupHandler(t)
		rec := postAtomicGroup(t, h, `{
			"name":"bad-model","routing_mode":"upstreams",
			"allowed_models":["   "],
			"upstream_members":[{"upstream_id":`+itoa(firstID)+`}]
		}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		requireAtomicGroupCount(t, store, 0)
	})

	t.Run("duplicate upstream leaves no group", func(t *testing.T) {
		h, store, firstID, _ := newAtomicGroupHandler(t)
		rec := postAtomicGroup(t, h, `{
			"name":"duplicate-member","routing_mode":"upstreams",
			"allowed_models":["gpt-5.6"],
			"upstream_members":[
				{"upstream_id":`+itoa(firstID)+`},
				{"upstream_id":`+itoa(firstID)+`}
			]
		}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "duplicate upstream id")
		requireAtomicGroupCount(t, store, 0)
	})
}

func TestPutGroupsUpstreamsAtomic(t *testing.T) {
	h, store, firstID, secondID := newAtomicGroupHandler(t)
	created := postAtomicGroup(t, h, `{
		"name":"editable-route","routing_mode":"upstreams",
		"allowed_models":["gpt-5.6"],
		"upstream_members":[{"upstream_id":`+itoa(firstID)+`}]
	}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var group Group
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &group))
	require.NotNil(t, group.ID)

	updateBody := `{
		"name":"edited-route","visibility":"private","routing_mode":"upstreams",
		"allowed_models":["gpt-5.5"],"price_multiplier":1.4,
		"upstream_members":[{"upstream_id":` + itoa(secondID) + `,"weight":75,"max_concurrency":9}]
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/groups/"+itoa(*group.ID), strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.PutGroupsId(rec, req, *group.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	store.mu.Lock()
	stored := *store.groups[*group.ID]
	members := cloneAtomicGroupMembers(store.members[*group.ID])
	store.mu.Unlock()
	require.Equal(t, "edited-route", stored.Name)
	require.Equal(t, domain.GroupVisibilityPrivate, stored.Visibility)
	require.Equal(t, 14000, stored.PriceMultiplier)
	require.Equal(t, []string{"gpt-5.5"}, stored.AllowedModels)
	require.Len(t, members, 1)
	require.Equal(t, secondID, members[0].UpstreamID)
	require.Equal(t, 75, members[0].Weight)
	require.Equal(t, 9, members[0].MaxConcurrency)

	badReq := httptest.NewRequest(http.MethodPut, "/api/admin/groups/"+itoa(*group.ID), strings.NewReader(`{
		"name":"must-not-stick","routing_mode":"upstreams","allowed_models":["gpt-5.6"],
		"upstream_members":[{"upstream_id":999999}]
	}`))
	badReq.Header.Set("Content-Type", "application/json")
	badRec := httptest.NewRecorder()
	h.PutGroupsId(badRec, badReq, *group.ID)
	require.Equal(t, http.StatusNotFound, badRec.Code, badRec.Body.String())

	store.mu.Lock()
	require.Equal(t, "edited-route", store.groups[*group.ID].Name)
	require.Equal(t, secondID, store.members[*group.ID][0].UpstreamID)
	store.mu.Unlock()
}

func TestPutGroupsSwitchToAccountsClearsOmittedMembers(t *testing.T) {
	h, store, firstID, _ := newAtomicGroupHandler(t)
	created := postAtomicGroup(t, h, `{
		"name":"switchable-route","routing_mode":"upstreams",
		"allowed_models":["gpt-5.6"],
		"upstream_members":[{"upstream_id":`+itoa(firstID)+`}]
	}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var group Group
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &group))
	require.NotNil(t, group.ID)

	// Omitting upstream_members while explicitly switching modes must still
	// clear the old relation set; otherwise a later switch back revives it.
	req := httptest.NewRequest(http.MethodPut, "/api/admin/groups/"+itoa(*group.ID), strings.NewReader(`{
		"name":"switchable-route","routing_mode":"accounts"
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.PutGroupsId(rec, req, *group.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	store.mu.Lock()
	rows := cloneAtomicGroupMembers(store.members[*group.ID])
	stored := *store.groups[*group.ID]
	store.mu.Unlock()
	require.Equal(t, domain.GroupRoutingModeAccounts, stored.RoutingMode)
	require.Empty(t, rows)
}
