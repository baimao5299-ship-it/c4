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
	require.Equal(t, "tok", c.Admin.Token)
	require.Equal(t, 30*time.Second, c.Scheduler.SyncInterval)
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
