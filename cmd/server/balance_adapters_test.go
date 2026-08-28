// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
)

type balanceAdapterRoundTripper func(*http.Request) (*http.Response, error)

func (f balanceAdapterRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestConfiguredBalanceAdapters(t *testing.T) {
	upstreamClient := &http.Client{}
	adapters, err := configuredBalanceAdapters(map[string]config.BalanceAdapterConfig{
		"7": {Provider: "relay-a", Endpoint: "/v1/balance", Auth: "bearer", BalancePath: "data.balance", CurrencyPath: "data.currency"},
	}, upstreamClient)
	require.NoError(t, err)
	require.Len(t, adapters, 1)
	adapter, ok := adapters[7]
	require.True(t, ok)
	require.Equal(t, "relay-a", adapter.Provider())
	require.True(t, adapter.EndpointConfigured())
	jsonAdapter, ok := adapter.(billing.HTTPJSONAdapter)
	require.True(t, ok)
	require.NotNil(t, jsonAdapter.Client)
	require.NotSame(t, upstreamClient, jsonAdapter.Client)
	require.ErrorIs(t, jsonAdapter.Client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
}

func TestConfiguredBalanceAdaptersRejectsInvalidTemplateID(t *testing.T) {
	_, err := configuredBalanceAdapters(map[string]config.BalanceAdapterConfig{"nope": {Provider: "relay"}}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "positive template id")
}

func TestConfiguredBalanceAdaptersRejectsInvalidAuth(t *testing.T) {
	_, err := configuredBalanceAdapters(map[string]config.BalanceAdapterConfig{
		"7": {Provider: "relay", Endpoint: "https://relay.example/balance", Auth: "cookie"},
	}, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "unsupported")
}

func TestConfiguredBalanceAdaptersUsesSharedUpstreamTransport(t *testing.T) {
	called := false
	upstreamClient := &http.Client{Transport: balanceAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		require.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"balance":"12.50"}`)),
			Request:    r,
		}, nil
	})}
	adapters, err := configuredBalanceAdapters(map[string]config.BalanceAdapterConfig{
		"7": {Provider: "relay", Endpoint: "https://provider.invalid/balance", Auth: "bearer", BalancePath: "balance"},
	}, upstreamClient)
	require.NoError(t, err)
	adapter := adapters[7].(billing.HTTPJSONAdapter)
	got, err := adapter.Fetch(context.Background(), billing.BalanceAccount{UpstreamKey: "fixture-key"})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "12.50", got.Amount)
}
