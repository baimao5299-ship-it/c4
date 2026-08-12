// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Server    ServerConfig    `koanf:"server"`
	Log       LogConfig       `koanf:"log"`
	Admin     AdminConfig     `koanf:"admin"`
	Auth      AuthConfig      `koanf:"auth"`
	DB        DBConfig        `koanf:"db"`
	Proxy     ProxyConfig     `koanf:"proxy"`
	Upstream  UpstreamConfig  `koanf:"upstream"`
	Limit     LimitConfig     `koanf:"limit"`
	Scheduler SchedulerConfig `koanf:"scheduler"`
	Usage     UsageConfig     `koanf:"usage"`
	Billing   BillingConfig   `koanf:"billing"`
}

type ServerConfig struct {
	Addr              string        `koanf:"addr"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	MaxHeaderBytes    int           `koanf:"max_header_bytes"`
}

type LogConfig struct {
	Level  string `koanf:"level"`
	Output string `koanf:"output"`
}

type AdminConfig struct {
	Token string `koanf:"token"`
}

// AuthConfig JWT 密钥：强制（C3API_AUTH_JWT_SECRET），缺失启动失败——
// 随机生成 = 重启全失效 + 多实例不一致（评审定夺①）。
type AuthConfig struct {
	JWTSecret string `koanf:"jwt_secret"`
}

type DBConfig struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

type ProxyConfig struct {
	MaxBodySize           int64         `koanf:"max_body_size"`
	MaxInflight           int64         `koanf:"max_inflight"`
	UpstreamTimeout       time.Duration `koanf:"upstream_timeout"`
	UpstreamStreamTimeout time.Duration `koanf:"upstream_stream_timeout"`
	FailoverAttempts      int           `koanf:"failover_attempts"`
	UsageCapture          bool          `koanf:"usage_capture"`
}

type UpstreamConfig struct {
	MaxIdleConns        int           `koanf:"max_idle_conns"`
	MaxIdleConnsPerHost int           `koanf:"max_idle_conns_per_host"`
	IdleConnTimeout     time.Duration `koanf:"idle_conn_timeout"`
	DialTimeout         time.Duration `koanf:"dial_timeout"`
	ForceHTTP2          bool          `koanf:"force_http2"`
}

type LimitConfig struct {
	GroupKeyRPM int `koanf:"group_key_rpm"`
}

type SchedulerConfig struct {
	DefaultMaxConcurrency int           `koanf:"default_max_concurrency"`
	Cooldown429           time.Duration `koanf:"cooldown_429"`
	BackoffBase           time.Duration `koanf:"backoff_base"`
	BackoffMax            time.Duration `koanf:"backoff_max"`
	SyncInterval          time.Duration `koanf:"sync_interval"`
}

type UsageConfig struct {
	BatchSize          int           `koanf:"batch_size"`
	FlushInterval      time.Duration `koanf:"flush_interval"`
	LogRetentionDays   int           `koanf:"log_retention_days"`
	StatsFlushInterval time.Duration `koanf:"stats_flush_interval"`
	FlushWorkers       int           `koanf:"flush_workers"` // flush 并行 worker 数（O1 管道化分片并行；明细/统计/额度共用）
	// err_logs 错误审计明细（分表设计）：有界队列 + 背压采样丢弃（风暴不淹没
	// DB 不爆内存；DB 写速率上界 = ErrLogBatchSize/ErrLogFlushInterval）。
	ErrLogQueueSize     int           `koanf:"errlog_queue_size"`     // 队列容量（默认 4096）
	ErrLogBatchSize     int           `koanf:"errlog_batch_size"`     // 每批落盘行数（默认 500）
	ErrLogFlushInterval time.Duration `koanf:"errlog_flush_interval"` // 批间隔（默认 500ms）
	ErrLogRetentionDays int           `koanf:"errlog_retention_days"` // err_logs 分区保留天数（默认 7 天短保留——错误审计；<= 0 = 不删除）
	// StatsRetentionDays usage_stats 分区保留天数（默认 180 天——聚合统计长保留；
	// 用户裁决 2026-08-11：usage_stats 也分区化，清理 DROP 分区 O(1)——PG DELETE
	// 不释放空间；<= 0 = 不删除）。
	StatsRetentionDays int `koanf:"stats_retention_days"`
}

// BillingConfig 计费（Phase 5 T3）：Enabled 默认关（评审 C-1 opt-in——启用前
// 需先同步价格：空价格表 = 全模型 402 契约语义；余额预检 + FEFO 条件扣费 +
// 优雅停机排空全链随之生效）。
type BillingConfig struct {
	Enabled                bool          `koanf:"enabled"`
	FlushInterval          time.Duration `koanf:"flush_interval"`           // 扣费落库周期
	BalanceRefreshInterval time.Duration `koanf:"balance_refresh_interval"` // 余额快照全量刷新周期
	FlushWorkers           int           `koanf:"flush_workers"`            // flush 并行 worker 数（O1 管道化分片并行）
}

func defaults() *Config {
	return &Config{
		Server:    ServerConfig{Addr: ":8080", ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20},
		Log:       LogConfig{Level: "warn", Output: "stdout"},
		DB:        DBConfig{MaxConns: 20}, // #17：10→20（billing 8 worker + stats 8 worker + 余量；统计 COPY 批量写已改毫秒级短事务）
		Proxy:     ProxyConfig{MaxBodySize: 4 << 20, MaxInflight: 50000, UpstreamTimeout: 120 * time.Second, UpstreamStreamTimeout: 30 * time.Minute, FailoverAttempts: 3, UsageCapture: true},
		Upstream:  UpstreamConfig{MaxIdleConns: 8192, MaxIdleConnsPerHost: 2048, IdleConnTimeout: 90 * time.Second, DialTimeout: 10 * time.Second, ForceHTTP2: true},
		Scheduler: SchedulerConfig{DefaultMaxConcurrency: 8, Cooldown429: 30 * time.Second, BackoffBase: 5 * time.Second, BackoffMax: 5 * time.Minute, SyncInterval: 30 * time.Second},
		Usage:     UsageConfig{BatchSize: 500, FlushInterval: 500 * time.Millisecond, LogRetentionDays: 30, StatsFlushInterval: 10 * time.Second, FlushWorkers: 8, ErrLogQueueSize: 4096, ErrLogBatchSize: 500, ErrLogFlushInterval: 500 * time.Millisecond, ErrLogRetentionDays: 7, StatsRetentionDays: 180},
		Billing:   BillingConfig{Enabled: false, FlushInterval: 1 * time.Second, BalanceRefreshInterval: 10 * time.Second, FlushWorkers: 8},
	}
}

// Load 先应用默认值，再叠加 TOML 文件，最后叠加 C3API_ 前缀 env。
func Load(path string) (*Config, error) {
	c := defaults()
	k := koanf.New(".")
	if path != "" {
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return nil, err
		}
	}
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: "C3API_",
		TransformFunc: func(k, v string) (string, any) {
			k = strings.ToLower(strings.TrimPrefix(k, "C3API_"))
			if i := strings.Index(k, "_"); i >= 0 {
				k = k[:i] + "." + k[i+1:]
			}
			return k, v
		},
	}), nil); err != nil {
		return nil, err
	}
	if err := k.Unmarshal("", c); err != nil {
		return nil, err
	}
	if c.Admin.Token == "" {
		c.Admin.Token = os.Getenv("C3API_ADMIN_TOKEN")
	}
	if c.Auth.JWTSecret == "" {
		c.Auth.JWTSecret = os.Getenv("C3API_AUTH_JWT_SECRET")
	}
	if c.DB.DSN == "" {
		c.DB.DSN = os.Getenv("C3API_DB_DSN")
	}
	return c, nil
}
