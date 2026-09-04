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

// officialPrice converts the persisted catalogue unit (milli-USD on the
// 1e-5 USD grid) without applying a group/assignment multiplier. Keeping this
// separate from scaledPrice prevents clients from having to reverse a free or
// discounted group price to recover the official catalogue value.
func officialPrice(millis *int64) *float64 {
	return scaledPrice(millis, 1)
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
		status := GroupPublicStatus(metric.Group.PublicStatus)
		if status == "" {
			status = Available
		}
		models := append([]string(nil), metric.Group.AllowedModels...)
		if models == nil {
			models = []string{}
		}
		multiplier := float64(metric.PriceMultiplier) / 10000
		modelPrices := make([]UserChannelModelPrice, 0, len(metric.ModelPrices))
		for _, price := range metric.ModelPrices {
			row := UserChannelModelPrice{
				Model:                  price.Model,
				OfficialInputPerM:      officialPrice(price.OfficialInputPerM),
				OfficialImgInTokPerM:   officialPrice(price.OfficialImgInTokPerM),
				OfficialImgOutTokPerM:  officialPrice(price.OfficialImgOutTokPerM),
				OfficialOutputPerM:     officialPrice(price.OfficialOutputPerM),
				OfficialCacheReadPerM:  officialPrice(price.OfficialCacheReadPerM),
				OfficialCacheWritePerM: officialPrice(price.OfficialCacheWritePerM),
				OfficialPricePerCall:   officialPrice(price.OfficialPricePerCall),
				OfficialPricePerImage:  officialPrice(price.OfficialPricePerImage),
				InputPerM:              scaledPrice(price.InputPerM, multiplier),
				ImgInTokPerM:           scaledPrice(price.ImgInTokPerM, multiplier),
				ImgOutTokPerM:          scaledPrice(price.ImgOutTokPerM, multiplier),
				OutputPerM:             scaledPrice(price.OutputPerM, multiplier),
				CacheReadPerM:          scaledPrice(price.CacheReadPerM, multiplier),
				CacheWritePerM:         scaledPrice(price.CacheWritePerM, multiplier),
				PricePerCall:           scaledPrice(price.PricePerCall, multiplier),
				PricePerImage:          scaledPrice(price.PricePerImage, multiplier),
			}
			if price.Mode != "" {
				mode := UserChannelModelPriceMode(price.Mode)
				row.Mode = &mode
			}
			modelPrices = append(modelPrices, row)
		}
		out.Rows = append(out.Rows, UserChannelMetric{
			GroupID: metric.Group.ID, Name: metric.Group.Name, Remark: metric.Group.Remark, Category: metric.Group.Category, AllowedModels: models, ModelPrices: modelPrices,
			PriceMultiplier: multiplier, Status: status,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
