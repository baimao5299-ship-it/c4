package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Setenv("GPM_ADMIN_TOKEN", "tok")
	t.Setenv("GPM_DB_DSN", "postgres://x")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Log.Level != "warn" {
		t.Fatalf("default level: %s", c.Log.Level)
	}
	if c.Proxy.MaxInflight != 50000 {
		t.Fatalf("default inflight: %d", c.Proxy.MaxInflight)
	}
	if c.Usage.BatchSize != 500 {
		t.Fatalf("default batch: %d", c.Usage.BatchSize)
	}
	if c.Admin.Token != "tok" {
		t.Fatalf("env token: %s", c.Admin.Token)
	}
	if c.Scheduler.SyncInterval != 30*time.Second {
		t.Fatalf("default sync: %s", c.Scheduler.SyncInterval)
	}
}
