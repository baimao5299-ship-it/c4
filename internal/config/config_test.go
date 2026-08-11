package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	t.Setenv("GPM_ADMIN_TOKEN", "tok")
	t.Setenv("GPM_DB_DSN", "postgres://x")
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "warn", c.Log.Level)
	require.Equal(t, int64(50000), c.Proxy.MaxInflight)
	require.Equal(t, 500, c.Usage.BatchSize)
	require.Equal(t, 8, c.Usage.FlushWorkers, "usage flush 并行 worker 默认 8（O1 管道化）")
	require.Equal(t, "tok", c.Admin.Token)
	require.Equal(t, 30*time.Second, c.Scheduler.SyncInterval)
	require.False(t, c.Billing.Enabled, "计费默认关（opt-in，评审 C-1）")
	require.Equal(t, 1*time.Second, c.Billing.FlushInterval)
	require.Equal(t, 10*time.Second, c.Billing.BalanceRefreshInterval)
	require.Equal(t, 8, c.Billing.FlushWorkers, "flush 并行 worker 默认 8（O1）")
}

func TestEnvOverlay(t *testing.T) {
	t.Setenv("GPM_PROXY_MAX_INFLIGHT", "7")
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, int64(7), c.Proxy.MaxInflight)
}

func TestLoadFromTOML(t *testing.T) {
	c, err := Load("../../config.example.toml")
	require.NoError(t, err)
	require.Equal(t, "change-me", c.Admin.Token)
	require.Equal(t, 500*time.Millisecond, c.Usage.FlushInterval)
}
