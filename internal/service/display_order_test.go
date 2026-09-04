// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type displayOrderFake struct {
	*fakeStore
	groupIDs []int64
}

func (f *displayOrderFake) ReorderGroups(_ context.Context, ids []int64) error {
	f.groupIDs = append([]int64(nil), ids...)
	return nil
}

type upstreamOrderFake struct {
	*upstreamServiceStub
	ids []int64
}

func (f *upstreamOrderFake) ReorderUpstreams(_ context.Context, ids []int64) error {
	f.ids = append([]int64(nil), ids...)
	return nil
}

func TestDisplayOrderServicesValidateAndForwardIDs(t *testing.T) {
	store := &displayOrderFake{fakeStore: newFakeStore()}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	groupIDs := []int64{3, 2, 1}
	require.NoError(t, svc.ReorderGroups(context.Background(), groupIDs))
	require.Equal(t, []int64{3, 2, 1}, store.groupIDs)
	groupIDs[0] = 99
	require.Equal(t, int64(3), store.groupIDs[0], "service must isolate the store from caller mutation")

	require.ErrorIs(t, svc.ReorderGroups(context.Background(), []int64{1}), ErrInvalidInput)
	require.ErrorIs(t, svc.ReorderGroups(context.Background(), []int64{1, 1}), ErrInvalidInput)

	upstreamStore := &upstreamOrderFake{upstreamServiceStub: &upstreamServiceStub{}}
	upstreamService := &Service{upstreams: upstreamStore}
	upstreamIDs := []int64{8, 7}
	require.NoError(t, upstreamService.ReorderUpstreams(context.Background(), upstreamIDs))
	require.Equal(t, []int64{8, 7}, upstreamStore.ids)
	require.ErrorIs(t, upstreamService.ReorderUpstreams(context.Background(), []int64{0, 2}), ErrInvalidInput)
}
