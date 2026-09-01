package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestSuspiciousPriceSnapshotShrink(t *testing.T) {
	require.False(t, suspiciousPriceSnapshotShrink(0, 0))
	require.False(t, suspiciousPriceSnapshotShrink(99, 1), "small custom catalogues stay supported")
	require.False(t, suspiciousPriceSnapshotShrink(100, 50), "an exact half remains the acceptance boundary")
	require.True(t, suspiciousPriceSnapshotShrink(100, 49))
	require.True(t, suspiciousPriceSnapshotShrink(3000, 1))
}

func TestApplyLiteLLMSnapshotRejectsInconsistentInputBeforeDatabase(t *testing.T) {
	r := &Repository{}
	_, _, err := r.ApplyLiteLLMSnapshot(context.Background(), nil, nil, []string{"priced"})
	require.ErrorContains(t, err, "does not match price rows")

	entry := &domain.PriceEntry{Model: "priced", Mode: domain.PriceModeToken}
	_, _, err = r.ApplyLiteLLMSnapshot(context.Background(), []*domain.PriceEntry{entry}, []*domain.PriceVariant{{Model: "other", Seq: 1}}, []string{"priced"})
	require.ErrorContains(t, err, "absent from the candidate snapshot")
}
