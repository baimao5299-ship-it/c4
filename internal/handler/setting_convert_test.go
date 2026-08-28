// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestToAPISettingRedactsSecrets(t *testing.T) {
	proxy := domain.Setting{Key: "upstream_proxy_url", Type: domain.SettingTypeString, Value: "socks5h://user:pass@127.0.0.1:7897", UpdatedAt: time.Now()}
	got := toAPISetting(&proxy)
	require.Equal(t, "socks5h://127.0.0.1:7897", *got.Value)

	password := domain.Setting{Key: "mail.smtp_password", Type: domain.SettingTypeString, Value: "secret", UpdatedAt: time.Now()}
	got = toAPISetting(&password)
	require.Equal(t, "********", *got.Value)

	malformed := domain.Setting{Key: "upstream_proxy_url", Type: domain.SettingTypeString, Value: "socks5h://user:pass@", UpdatedAt: time.Now()}
	got = toAPISetting(&malformed)
	require.Equal(t, "invalid", *got.Value)
	require.NotContains(t, *got.Value, "pass")
}
