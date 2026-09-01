// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestToAPIUsageLogPreservesCallBillingSnapshot(t *testing.T) {
	callCount := int64(3)
	pricePerCall := int64(5400)
	row := toAPIUsageLog(&domain.UsageLog{
		ID:                 7,
		RequestID:          "call-contract",
		Model:              "image-model",
		Format:             domain.FormatOpenAIImages,
		CallCount:          callCount,
		PricePerCallMillis: &pricePerCall,
	})
	require.NotNil(t, row.CallCount)
	require.Equal(t, callCount, *row.CallCount)
	require.NotNil(t, row.PricePerCallMillis)
	require.Equal(t, pricePerCall, *row.PricePerCallMillis)

	payload, err := json.Marshal(row)
	require.NoError(t, err)
	var encoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &encoded))
	require.Equal(t, float64(callCount), encoded["CallCount"])
	require.Equal(t, float64(pricePerCall), encoded["PricePerCallMillis"])
}
