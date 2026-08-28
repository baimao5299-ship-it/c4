// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
)

// configuredBalanceAdapters turns the config-file map into immutable adapters.
// Keeping this conversion at the composition root means the billing package has
// no dependency on config or environment conventions.
func configuredBalanceAdapters(entries map[string]config.BalanceAdapterConfig, upstreamClient *http.Client) (map[int64]billing.BalanceAdapter, error) {
	out := make(map[int64]billing.BalanceAdapter, len(entries))
	// Reuse the stable switchable transport while keeping redirects disabled so
	// credentials are never replayed to a location the operator did not set.
	balanceClient := cloneBalanceClient(upstreamClient)
	for rawID, entry := range entries {
		id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("billing.balance_adapters.%q must be a positive template id", rawID)
		}
		if _, exists := out[id]; exists {
			return nil, fmt.Errorf("billing.balance_adapters contains duplicate template id %d", id)
		}
		if strings.TrimSpace(entry.Provider) == "" && strings.TrimSpace(entry.Endpoint) == "" {
			// An empty placeholder table is equivalent to no adapter. It is useful
			// when a shared config is templated across machines.
			continue
		}
		adapter := billing.HTTPJSONAdapter{
			NameValue:       entry.Provider,
			Endpoint:        entry.Endpoint,
			Method:          entry.Method,
			Auth:            billing.HTTPAuthStyle(strings.ToLower(strings.TrimSpace(entry.Auth))),
			BalancePath:     entry.BalancePath,
			CurrencyPath:    entry.CurrencyPath,
			Client:          balanceClient,
			Timeout:         entry.Timeout,
			MaxResponseSize: entry.MaxResponseSize,
		}
		if err := adapter.Validate(); err != nil {
			return nil, fmt.Errorf("billing.balance_adapters.%d: %w", id, err)
		}
		out[id] = adapter
	}
	return out, nil
}

func cloneBalanceClient(upstreamClient *http.Client) *http.Client {
	if upstreamClient == nil {
		return nil
	}
	client := *upstreamClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}
