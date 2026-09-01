// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later or commercial license; see LICENSE files.

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrustedProxyCIDRsLoadAndValidate(t *testing.T) {
	setenvRequired(t)
	c, err := Load(writeConfig(t, "[proxy]\ntrusted_proxy_cidrs = [\"127.0.0.1/32\", \"::1/128\"]\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"127.0.0.1/32", "::1/128"}, c.Proxy.TrustedProxyCIDRs)
}

func TestProductionConfigTrustsOnlyLocalReverseProxy(t *testing.T) {
	setenvRequired(t)
	c, err := Load("../../deploy/config.toml")
	require.NoError(t, err)
	require.True(t, c.Proxy.BehindCDN)
	require.Equal(t, []string{"127.0.0.1/32", "::1/128", "172.18.0.1/32"}, c.Proxy.TrustedProxyCIDRs)
}

func TestTrustedProxyCIDRsRejectInvalidValues(t *testing.T) {
	for name, value := range map[string]string{
		"empty":    "[\"\"]",
		"not-cidr": "[\"127.0.0.1\"]",
		"garbage":  "[\"not-a-network\"]",
	} {
		t.Run(name, func(t *testing.T) {
			setenvRequired(t)
			_, err := Load(writeConfig(t, "[proxy]\ntrusted_proxy_cidrs = "+value+"\n"))
			require.Error(t, err)
			require.ErrorContains(t, err, "proxy.trusted_proxy_cidrs")
		})
	}
}
