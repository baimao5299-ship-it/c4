// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// scaledPrice converts the pricing snapshot unit (milli-USD) to USD and
// applies the public group's multiplier. Keeping this conversion at the API
// boundary means all clients see the same price semantics and tiny values are
// not rounded down to zero.
func scaledPrice(millis *int64, multiplier float64) *float64 {
	if millis == nil {
		return nil
	}
	value := float64(*millis) / 1e5 * multiplier
	return &value
}

// GetUserChannelMonitor returns the privacy-safe public-channel aggregate.
// Missing bounds mean the rolling 24-hour window; explicit bounds are limited
// by service to keep this endpoint suitable for mobile polling.
func (h *UserAPI) GetUserChannelMonitor(w http.ResponseWriter, r *http.Request, params GetUserChannelMonitorParams) {
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	if params.To != nil {
		to = params.To.UTC()
	}
	if params.From != nil {
		from = params.From.UTC()
	}
	metrics, err := h.svc.UserChannelMetrics(r.Context(), currentUserID(r), from, to)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := UserChannelMonitorResponse{WindowFrom: from, WindowTo: to, Rows: make([]UserChannelMetric, 0, len(metrics))}
	for _, metric := range metrics {
		if metric == nil || metric.Group == nil {
			continue
		}
		status := UserChannelMetricStatus(metric.Status)
		models := append([]string(nil), metric.Group.AllowedModels...)
		if models == nil {
			models = []string{}
		}
		multiplier := float64(metric.PriceMultiplier) / 10000
		modelPrices := make([]UserChannelModelPrice, 0, len(metric.ModelPrices))
		for _, price := range metric.ModelPrices {
			row := UserChannelModelPrice{
				Model:          price.Model,
				InputPerM:      scaledPrice(price.InputPerM, multiplier),
				OutputPerM:     scaledPrice(price.OutputPerM, multiplier),
				CacheReadPerM:  scaledPrice(price.CacheReadPerM, multiplier),
				CacheWritePerM: scaledPrice(price.CacheWritePerM, multiplier),
				PricePerCall:   scaledPrice(price.PricePerCall, multiplier),
				PricePerImage:  scaledPrice(price.PricePerImage, multiplier),
			}
			if price.Mode != "" {
				mode := UserChannelModelPriceMode(price.Mode)
				row.Mode = &mode
			}
			modelPrices = append(modelPrices, row)
		}
		out.Rows = append(out.Rows, UserChannelMetric{
			GroupID: metric.Group.ID, Name: metric.Group.Name, AllowedModels: models, ModelPrices: modelPrices,
			PriceMultiplier: multiplier,
			RequestCount:    metric.RequestCount, ErrorCount: metric.ErrorCount,
			AverageLatencyMS: metric.AverageLatencyMS, SuccessRate: metric.SuccessRate,
			LastCalledAt: metric.LastCalledAt, Status: status,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
