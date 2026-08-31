// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPricingPrecisionHTTPContract locks the public admin pricing contract to
// the fixed-point storage precision. In particular, a positive 0.001 value
// must remain billable rather than becoming an explicit free value after the
// API conversion and read-back.
func TestPricingPrecisionHTTPContract(t *testing.T) {
	doAdmin, _, _ := newSharedRouters(t)

	t.Run("group multiplier 0.001 create and update", func(t *testing.T) {
		rec := doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"precision-group","price_multiplier":0.001}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
		var group Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &group))
		require.NotNil(t, group.ID)
		require.NotNil(t, group.PriceMultiplier)
		require.InDelta(t, 0.001, *group.PriceMultiplier, 1e-12, "create must not become free")

		rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*group.ID), `{"name":"precision-group","price_multiplier":0.001}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "update: %s", rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &group))
		require.NotNil(t, group.PriceMultiplier)
		require.InDelta(t, 0.001, *group.PriceMultiplier, 1e-12, "update must not become free")

		rec = doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"sub-precision-group","price_multiplier":1e-13}`, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "a positive multiplier below storage precision must be rejected")
	})

	t.Run("token prices", func(t *testing.T) {
		const model = "precision-token"
		put := func(body string) *PriceEntry {
			rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, body, "")
			require.Equal(t, http.StatusOK, rec.Code, "put %s: %s", body, rec.Body.String())
			var entry PriceEntry
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))
			return &entry
		}

		entry := put(`{"mode":"token","input_per_m":0.001,"output_per_m":0.001}`)
		require.Equal(t, PriceEntryModeToken, entry.Mode)
		require.InDelta(t, 0.001, *entry.InputPerM, 1e-12)
		require.InDelta(t, 0.001, *entry.OutputPerM, 1e-12)

		// The smallest representable positive price is accepted and round-trips.
		entry = put(`{"mode":"token","input_per_m":0.00001,"output_per_m":0.00001}`)
		require.InDelta(t, 0.00001, *entry.InputPerM, 1e-12)
		require.InDelta(t, 0.00001, *entry.OutputPerM, 1e-12)

		for name, body := range map[string]string{
			"sub-precision": `{"mode":"token","input_per_m":0.000001,"output_per_m":0.001}`,
			"negative":      `{"mode":"token","input_per_m":-0.001,"output_per_m":0.001}`,
		} {
			t.Run(name, func(t *testing.T) {
				rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, body, "")
				require.Equal(t, http.StatusBadRequest, rec.Code, "invalid token price must be rejected: %s", rec.Body.String())
			})
		}

		// A failed update must leave the last valid minimum value intact.
		rec := doAdmin(http.MethodGet, "/api/admin/prices/entry?model="+model, "", "")
		require.Equal(t, http.StatusOK, rec.Code, "get: %s", rec.Body.String())
		var got PriceEntry
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.InDelta(t, 0.00001, *got.InputPerM, 1e-12)
	})

	t.Run("per-call prices", func(t *testing.T) {
		const model = "precision-call"
		valid := func(value string) *PriceEntry {
			rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, `{"mode":"call","price_per_call":`+value+`}`, "")
			require.Equal(t, http.StatusOK, rec.Code, "put %s: %s", value, rec.Body.String())
			var entry PriceEntry
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))
			return &entry
		}

		entry := valid("0.001")
		require.InDelta(t, 0.001, *entry.PricePerCall, 1e-12)
		entry = valid("0.00001")
		require.InDelta(t, 0.00001, *entry.PricePerCall, 1e-12)

		for name, value := range map[string]string{"sub-precision": "0.000001", "negative": "-0.001"} {
			t.Run(name, func(t *testing.T) {
				rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, `{"mode":"call","price_per_call":`+value+`}`, "")
				require.Equal(t, http.StatusBadRequest, rec.Code, "invalid call price must be rejected: %s", rec.Body.String())
			})
		}
	})

	t.Run("image prices", func(t *testing.T) {
		const model = "precision-image"
		valid := func(value string) *PriceEntry {
			rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, `{"mode":"image","price_per_image":`+value+`}`, "")
			require.Equal(t, http.StatusOK, rec.Code, "put %s: %s", value, rec.Body.String())
			var entry PriceEntry
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entry))
			return &entry
		}

		entry := valid("0.001")
		require.InDelta(t, 0.001, *entry.PricePerImage, 1e-12)
		entry = valid("0.00001")
		require.InDelta(t, 0.00001, *entry.PricePerImage, 1e-12)

		for name, value := range map[string]string{"sub-precision": "0.000001", "negative": "-0.001"} {
			t.Run(name, func(t *testing.T) {
				rec := doAdmin(http.MethodPut, "/api/admin/prices/entry?model="+model, `{"mode":"image","price_per_image":`+value+`}`, "")
				require.Equal(t, http.StatusBadRequest, rec.Code, "invalid image price must be rejected: %s", rec.Body.String())
			})
		}
	})
}
