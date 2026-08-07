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

// AuthConfig JWT 密钥：强制（GPM_AUTH_JWT_SECRET），缺失启动失败——
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
}

func defaults() *Config {
	return &Config{
		Server:    ServerConfig{Addr: ":8080", ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20},
		Log:       LogConfig{Level: "warn", Output: "stdout"},
		DB:        DBConfig{MaxConns: 10},
		Proxy:     ProxyConfig{MaxBodySize: 4 << 20, MaxInflight: 50000, UpstreamTimeout: 120 * time.Second, UpstreamStreamTimeout: 30 * time.Minute, FailoverAttempts: 3, UsageCapture: true},
		Upstream:  UpstreamConfig{MaxIdleConns: 8192, MaxIdleConnsPerHost: 2048, IdleConnTimeout: 90 * time.Second, DialTimeout: 10 * time.Second, ForceHTTP2: true},
		Scheduler: SchedulerConfig{DefaultMaxConcurrency: 8, Cooldown429: 30 * time.Second, BackoffBase: 5 * time.Second, BackoffMax: 5 * time.Minute, SyncInterval: 30 * time.Second},
		Usage:     UsageConfig{BatchSize: 500, FlushInterval: 500 * time.Millisecond, LogRetentionDays: 30, StatsFlushInterval: 10 * time.Second},
	}
}

// Load 先应用默认值，再叠加 TOML 文件，最后叠加 GPM_ 前缀 env。
func Load(path string) (*Config, error) {
	c := defaults()
	k := koanf.New(".")
	if path != "" {
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return nil, err
		}
	}
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: "GPM_",
		TransformFunc: func(k, v string) (string, any) {
			k = strings.ToLower(strings.TrimPrefix(k, "GPM_"))
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
		c.Admin.Token = os.Getenv("GPM_ADMIN_TOKEN")
	}
	if c.Auth.JWTSecret == "" {
		c.Auth.JWTSecret = os.Getenv("GPM_AUTH_JWT_SECRET")
	}
	if c.DB.DSN == "" {
		c.DB.DSN = os.Getenv("GPM_DB_DSN")
	}
	return c, nil
}
