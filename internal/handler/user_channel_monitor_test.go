// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service"
)

type channelMonitorStore struct {
	*fakeStore
	stats map[int64]*domain.PublicChannelStat
}

func (s *channelMonitorStore) ScanPublicChannelStats(context.Context, []int64, time.Time, time.Time) (map[int64]*domain.PublicChannelStat, error) {
	return s.stats, nil
}

func TestUserChannelMonitorReturnsEffectiveModelPrices(t *testing.T) {
	store := &channelMonitorStore{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	inPrice, outPrice := int64(100000), int64(250000)
	store.groups[1] = &domain.Group{ID: 1, Name: "public", Remark: "手机用户专用", Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-priced"}, PriceMultiplier: 800}
	store.priceEntries["gpt-priced"] = &domain.PriceEntry{Model: "gpt-priced", Mode: domain.PriceModeToken, InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceManual}

	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	iss := auth.NewIssuer("test-secret")
	ur := userapi.Router(svc, iss, fakeUserStatus{store: store.fakeStore}, nil)
	doUser := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ur.ServeHTTP(rec, req)
		return rec
	}
	token, _ := registerAndGet(t, doUser, "prices@example.com")
	viewer, err := store.GetUserByEmail(context.Background(), "prices@example.com")
	require.NoError(t, err)
	store.assign[1] = []int64{viewer.ID}
	effective := 10
	store.assignMult[[2]int64{1, viewer.ID}] = &effective
	rec := doUser(http.MethodGet, "/api/user/channel-monitor", "", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// Assert the wire contract as well as the decoded value. This catches a
	// regression where the remark is populated in the Go DTO but omitted or
	// renamed during JSON serialization, which would leave the mobile UI blank.
	require.Contains(t, rec.Body.String(), `"Remark":"手机用户专用"`)
	var body userapi.UserChannelMonitorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, "手机用户专用", body.Rows[0].Remark)
	require.Len(t, body.Rows[0].ModelPrices, 1)
	require.InDelta(t, 0.001, body.Rows[0].PriceMultiplier, 1e-12)
	require.InDelta(t, 0.001, *body.Rows[0].ModelPrices[0].InputPerM, 1e-12)
	require.InDelta(t, 0.0025, *body.Rows[0].ModelPrices[0].OutputPerM, 1e-12)
	require.InDelta(t, 1.0, *body.Rows[0].ModelPrices[0].OfficialInputPerM, 1e-12)
	require.InDelta(t, 2.5, *body.Rows[0].ModelPrices[0].OfficialOutputPerM, 1e-12)

	// The same public group projection powers key creation. Keep the remark
	// visible there too, so users do not have to leave the key flow to discover
	// the administrator's note.
	rec = doUser(http.MethodGet, "/api/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var groups []userapi.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 1)
	require.NotNil(t, groups[0].Remark)
	require.Equal(t, "手机用户专用", *groups[0].Remark)
}
