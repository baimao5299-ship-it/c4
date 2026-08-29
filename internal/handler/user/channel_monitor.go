// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

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
		out.Rows = append(out.Rows, UserChannelMetric{
			GroupID: metric.Group.ID, Name: metric.Group.Name, AllowedModels: models,
			PriceMultiplier: float64(metric.Group.PriceMultiplier) / 10000,
			RequestCount:    metric.RequestCount, ErrorCount: metric.ErrorCount,
			AverageLatencyMS: metric.AverageLatencyMS, SuccessRate: metric.SuccessRate,
			LastCalledAt: metric.LastCalledAt, Status: status,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
