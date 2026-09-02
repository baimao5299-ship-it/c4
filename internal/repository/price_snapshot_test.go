package repository

import (
	"context"
	"fmt"
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

func TestSuspiciousPriceSnapshotContent(t *testing.T) {
	current := make([]string, 100)
	for i := range current {
		current[i] = fmt.Sprintf("current-%03d", i)
	}
	candidate := make([]string, 100)
	for i := range candidate {
		candidate[i] = fmt.Sprintf("replacement-%03d", i)
	}
	require.True(t, suspiciousPriceSnapshotContent(current, candidate), "a same-sized disjoint feed is suspicious")
	candidate[0] = current[0]
	candidate[1] = current[1]
	candidate[2] = current[2]
	candidate[3] = current[3]
	candidate[4] = current[4]
	require.False(t, suspiciousPriceSnapshotContent(current, candidate), "five percent overlap is the acceptance boundary")
	require.False(t, suspiciousPriceSnapshotContent(current[:99], candidate), "small catalogues retain compatibility")
}

func TestApplyLiteLLMSnapshotRejectsInconsistentInputBeforeDatabase(t *testing.T) {
	r := &Repository{}
	_, _, err := r.ApplyLiteLLMSnapshot(context.Background(), nil, nil, []string{"priced"})
	require.ErrorContains(t, err, "does not match price rows")

	entry := &domain.PriceEntry{Model: "priced", Mode: domain.PriceModeToken}
	_, _, err = r.ApplyLiteLLMSnapshot(context.Background(), []*domain.PriceEntry{entry}, []*domain.PriceVariant{{Model: "other", Seq: 1}}, []string{"priced"})
	require.ErrorContains(t, err, "absent from the candidate snapshot")
}
