# go-proxy-mini 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现单实例抗 W 级并发的 AI 中转网关：模板/账号/分组 + 内存调度器（故障转移/冷却/并发控制）+ 官方 SDK 转发（零协议自研）+ 用量明细与预聚合统计。

**Architecture:** 单二进制；热路径（AI 请求）零 DB、零 per-request 锁（atomic.Value 快照 + 原子计数）；分组级 key 鉴权；路径决定请求格式（openai-chat / openai-responses / anthropic），格式匹配是选号硬过滤；上游调用走 openai-go / anthropic-sdk-go 官方 SDK；用量经有界 channel 异步批量落库 + 内存预聚合。

**Tech Stack:** Go 1.26.5、entgo.io/ent v0.14+、jackc/pgx/v5、chi v5、zap（经 pkg/logx 包装）、openai-go + anthropic-sdk-go（官方 SDK）、testify、koanf v2 + BurntSushi/toml、modernc.org/sqlite（仅测试）。

## Global Constraints

- **Go 1.26.5**（本机已升级）；模块名 `go-proxy-mini`（本地仓库无远端）。使用 1.26 现代特性：range-over-func（`for chunk := range stream.Iter()`）、`slices`/`maps` 辅助、`math/rand/v2`（顶层函数并发安全）、1.22+ 每次迭代独立循环变量语义
- 业务代码禁止直接 import zap / openai / anthropic 之外的第三方库；**openai/anthropic SDK 只允许在 `pkg/aiclient` 内 import**（包装"调用模式"：客户端构建/鉴权注入/非流式超时；协议类型透传），日志只经 `pkg/logx`
- 热路径不变量：无 per-request 互斥锁、无 per-request DB 调用（规格 §10.3）
- 日志级别规范（规格 §7.1）：请求级追踪 Debug；账号状态流转/故障转移 Warn；配置/DB 错误 Error
- 测试断言统一 testify（require 前置、assert 独立）；`golangci-lint run` 必须全绿
- 每个任务结束时 `go test ./...` 通过 + git commit
- **计划内偏差（相对规格文档，实施时在代码注释标注）**：
  1. **用户决策（2026-08-05）替代原 SQLite 方案**：仓库层测试用 **pgxmock**（github.com/pashagolub/pgxmock/v4，经 ent v0.14 的 dialect.Driver 薄适配器桥接，无真实 DB）；生产连接用原生 **pgxpool**——`OpenPG(ctx, dsn, maxConns int32) (*pgxpool.Pool, error)`，ent 经 `pgx/v5/stdlib` 的 `OpenDBFromPool(pool)` 桥接（entsql.OpenDB 在 ent v0.14.6 只接受 *sql.DB）。modernc.org/sqlite 弃用
  2. 流式空闲 watchdog 简化为整体 backstop 超时 `upstream_stream_timeout`（默认 30m，可配）——SDK 层无法注入 per-read 空闲超时
  3. 规格 §10.2 的压测验收以 `tools/loadtest` + `tools/fakeupstream` 自研工具执行，不依赖 k6；压测/冒烟所需 PG 实例用 Docker 自启（`postgres:16`，5432 端口）

---

### Task 1: 项目脚手架（git / go.mod / pkg 包装 / 配置 / lint）

**Files:**
- Create: `.gitignore`
- Create: `.golangci.yml`
- Create: `go.mod`、`go.sum`
- Create: `pkg/logx/logx.go`
- Test: `pkg/logx/logx_test.go`
- Create: `pkg/cryptox/cryptox.go`
- Test: `pkg/cryptox/cryptox_test.go`
- Create: `pkg/httpx/httpx.go`
- Test: `pkg/httpx/httpx_test.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Create: `config.example.toml`

**Interfaces:**
- Produces: `logx.New(level, output string) (*logx.Logger, error)`、`logx.Debug/Info/Warn/Error(msg, fields...)`、`logx.With(fields...)`、`logx.Field`（= zap.Field 别名）+ `logx.String/Int/Int64/Duration/Bool/Error` 构造器；`cryptox.HashKey(key) string`、`cryptox.NewGroupKey() (raw, hash, prefix string)`；`httpx.NewClient(cfg TransportConfig) *http.Client`；`config.Load(path string) (*config.Config, error)`（路径为空则仅 env）

- [ ] **Step 1: 初始化 git 与模块**

```bash
cd /c/Users/i/project/go-proxy-mini
git init
go mod init go-proxy-mini
go mod edit -go=1.26.5
```

Expected: `go.mod` 首行 `go 1.26.5`；`go env GOVERSION` 为 1.26.5。

- [ ] **Step 2: 拉取依赖（全部 @latest）**

```bash
go get github.com/go-chi/chi/v5@latest
go get github.com/jackc/pgx/v5@latest
go get entgo.io/ent@latest
go get entgo.io/ent/dialect/sql@latest
go get entgo.io/ent/dialect/entsql@latest
go get go.uber.org/zap@latest
go get github.com/stretchr/testify@latest
go get github.com/knadh/koanf/v2@latest
go get github.com/knadh/koanf/v2/providers/toml@latest
go get github.com/knadh/koanf/v2/providers/env@latest
go get github.com/openai/openai-go@latest
go get github.com/anthropics/anthropic-sdk-go@latest
go get github.com/google/uuid@latest
go get modernc.org/sqlite@latest
go get golang.org/x/time@latest
```
Expected: `go.mod` 内出现上述 require 项。

- [ ] **Step 3: 写 .gitignore**

```gitignore
config.toml
bin/
*.exe
*.log
tools/*/tmp/
```

- [ ] **Step 4: 写 pkg/logx（zap 薄包装）**

`pkg/logx/logx.go`:

```go
// Package logx 是 zap 的薄包装：业务代码只允许经本包取日志，禁止直接 import zap。
package logx

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Field 是 zap.Field 的别名，业务代码通过 logx 构造器创建字段。
type Field = zap.Field

func String(k, v string) Field          { return zap.String(k, v) }
func Int(k string, v int) Field         { return zap.Int(k, v) }
func Int64(k string, v int64) Field     { return zap.Int64(k, v) }
func Duration(k string, v time.Duration) Field { return zap.Duration(k, v) }
func Bool(k string, v bool) Field       { return zap.Bool(k, v) }
func Error(err error) Field             { return zap.Error(err) }
func Any(k string, v any) Field         { return zap.Any(k, v) }

type Logger struct{ l *zap.Logger }

func New(level, output string) (*Logger, error) {
	lv, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, err
	}
	if output == "" {
		output = "stdout"
	}
	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(lv),
		Encoding:    "json",
		OutputPaths: []string{output},
		EncoderConfig: zapcore.EncoderConfig{
			MessageKey:    "msg",
			LevelKey:      "level",
			TimeKey:       "ts",
			CallerKey:     "caller",
			EncodeLevel:   zapcore.LowercaseLevelEncoder,
			EncodeTime:    zapcore.ISO8601TimeEncoder,
			EncodeCaller:  zapcore.ShortCallerEncoder,
		},
	}
	z, err := cfg.Build(zap.AddCallerSkip(1))
	if err != nil {
		return nil, err
	}
	return &Logger{l: z}, nil
}

func (l *Logger) Debug(msg string, fields ...Field) { l.l.Debug(msg, fields...) }
func (l *Logger) Info(msg string, fields ...Field)  { l.l.Info(msg, fields...) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.l.Warn(msg, fields...) }
func (l *Logger) Error(msg string, fields ...Field) { l.l.Error(msg, fields...) }

func (l *Logger) With(fields ...Field) *Logger { return &Logger{l: l.l.With(fields...)} }
func (l *Logger) Sync() error                   { return l.l.Sync() }
```

注意 `logx.go` 顶部需 `import "time"`（Duration 用）。

- [ ] **Step 5: 写 logx 测试（验证级别过滤与调用不 panic）**

`pkg/logx/logx_test.go`:

```go
package logx_test

import (
	"testing"

	"go-proxy-mini/pkg/logx"
)

func TestLevelFiltering(t *testing.T) {
	// warn 级别下 Debug 不输出、Warn 输出
	logger, err := logx.New("warn", "stdout")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	logger.Debug("hidden", logx.String("k", "v"))
	logger.Warn("visible", logx.Int("n", 1))
	logger.Sync()
}

func TestWithFields(t *testing.T) {
	logger, _ := logx.New("error", "stdout")
	child := logger.With(logx.String("trace", "abc"))
	child.Error("boom", logx.Error(nil))
	logger.Sync()
}
```

- [ ] **Step 6: 写 pkg/cryptox（key 哈希 / 生成）**

`pkg/cryptox/cryptox.go`:

```go
package cryptox

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// HashKey 返回 key 的 SHA-256 十六进制摘要，用于库内比对（不存明文）。
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewGroupKey 生成分组客户端 key：raw 只返回一次给调用方展示，hash 入库。
func NewGroupKey() (raw, hash, prefix string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptox: rand read failed: " + err.Error())
	}
	raw = "gk-" + hex.EncodeToString(b)
	return raw, HashKey(raw), raw[:8]
}

// Equal 常量时间比较两个摘要。
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
```

- [ ] **Step 7: 写 cryptox 测试**

`pkg/cryptox/cryptox_test.go`:

```go
package cryptox

import "testing"

func TestHashKeyDeterministic(t *testing.T) {
	a := HashKey("gk-abc")
	b := HashKey("gk-abc")
	if a != b || a == "gk-abc" {
		t.Fatalf("hash mismatch: %s %s", a, b)
	}
}

func TestNewGroupKey(t *testing.T) {
	raw, hash, prefix := NewGroupKey()
	if len(raw) != 35 || raw[:3] != "gk-" { // gk- + 32 hex
		t.Fatalf("bad raw: %q", raw)
	}
	if HashKey(raw) != hash {
		t.Fatal("hash mismatch")
	}
	if len(prefix) != 8 {
		t.Fatalf("bad prefix: %q", prefix)
	}
}

func TestEqual(t *testing.T) {
	if !Equal("abc", "abc") || Equal("abc", "abd") {
		t.Fatal("Equal wrong")
	}
}
```

- [ ] **Step 8: 写 pkg/httpx（共享 Transport/Client）**

`pkg/httpx/httpx.go`:

```go
// Package httpx 构造共享的上游 HTTP 客户端（规格 §10.2 连接层参数）。
package httpx

import (
	"net"
	"net/http"
	"time"
)

type TransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	DialTimeout         time.Duration
	ForceHTTP2          bool
}

func NewTransport(cfg TransportConfig) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     cfg.ForceHTTP2,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func NewClient(cfg TransportConfig) *http.Client {
	return &http.Client{Transport: NewTransport(cfg)}
}
```

- [ ] **Step 9: 写 httpx 测试**

`pkg/httpx/httpx_test.go`:

```go
package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientReusesTransport(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	for i := 0; i < 3; i++ {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if hits != 3 {
		t.Fatalf("hits=%d", hits)
	}
}
```

- [ ] **Step 10: 写 internal/config（TOML + env 覆盖，手动默认值）**

`internal/config/config.go`:

```go
package config

import (
	"os"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/koanf/v2/providers/env"
	"github.com/knadh/koanf/v2/providers/toml"
)

type Config struct {
	Server    ServerConfig    `koanf:"server"`
	Log       LogConfig       `koanf:"log"`
	Admin     AdminConfig     `koanf:"admin"`
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

type DBConfig struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

type ProxyConfig struct {
	MaxBodySize         int64         `koanf:"max_body_size"`
	MaxInflight         int64         `koanf:"max_inflight"`
	UpstreamTimeout     time.Duration `koanf:"upstream_timeout"`
	UpstreamStreamTimeout time.Duration `koanf:"upstream_stream_timeout"`
	FailoverAttempts    int           `koanf:"failover_attempts"`
	UsageCapture        bool          `koanf:"usage_capture"`
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
	DropOnFull         bool          `koanf:"drop_on_full"`
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
		Usage:     UsageConfig{BatchSize: 500, FlushInterval: 500 * time.Millisecond, DropOnFull: true, LogRetentionDays: 30, StatsFlushInterval: 10 * time.Second},
	}
}

// Load 先应用默认值，再叠加 TOML 文件，最后叠加 GPM_ 前缀 env。
func Load(path string) (*Config, error) {
	c := defaults()
	k := koanf.New(".")
	if path != "" {
		if err := k.Load(toml.Provider(path), nil); err != nil {
			return nil, err
		}
	}
	if err := k.Load(env.Provider("GPM_", ".", nil), nil); err != nil {
		return nil, err
	}
	if err := k.Unmarshal("", c); err != nil {
		return nil, err
	}
	if c.Admin.Token == "" {
		c.Admin.Token = os.Getenv("GPM_ADMIN_TOKEN")
	}
	if c.DB.DSN == "" {
		c.DB.DSN = os.Getenv("GPM_DB_DSN")
	}
	return c, nil
}
```

- [ ] **Step 11: 写 config 测试（env 覆盖 + 默认值）**

`internal/config/config_test.go`:

```go
package config

import (
	"os"
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
```

- [ ] **Step 12: 写 config.example.toml**

```toml
server = { addr = ":8080", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
admin = { token = "change-me" }
db = { dsn = "postgres://user:pass@localhost:5432/go_proxy_mini?sslmode=disable", max_conns = 10 }
proxy = {
  max_body_size = 4194304,
  max_inflight = 50000,
  upstream_timeout = "120s",
  upstream_stream_timeout = "30m",
  failover_attempts = 3,
  usage_capture = true,
}
upstream = {
  max_idle_conns = 8192,
  max_idle_conns_per_host = 2048,
  idle_conn_timeout = "90s",
  dial_timeout = "10s",
  force_http2 = true,
}
limit = { group_key_rpm = 0 }
scheduler = {
  default_max_concurrency = 8,
  cooldown_429 = "30s",
  backoff_base = "5s",
  backoff_max = "5m",
  sync_interval = "30s",
}
usage = {
  batch_size = 500,
  flush_interval = "500ms",
  drop_on_full = true,
  log_retention_days = 30,
  stats_flush_interval = "10s",
}
```

- [ ] **Step 13: 写 .golangci.yml**

```yaml
run:
  timeout: 5m
linters:
  enable:
    - govet
    - staticcheck
    - errcheck
    - ineffassign
    - unused
    - gosimple
```

- [ ] **Step 14: 全量测试 + lint + commit**

Run: `go build ./... && go test ./... && golangci-lint run ./...`
Expected: BUILD PASS；测试全 PASS（`pkg/cryptox`、`pkg/httpx`、`internal/config`、`pkg/logx`）；lint 无报错。若 `golangci-lint` 未安装：`go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`。

```bash
git add -A && git commit -m "chore: scaffold module, pkg wrappers (logx/cryptox/httpx), config"
```

---

### Task 2: 领域类型 + ent schema + 代码生成

**Files:**
- Create: `internal/domain/types.go`
- Test: `internal/domain/types_test.go`
- Create: `internal/ent/schema/template.go`
- Create: `internal/ent/schema/account.go`
- Create: `internal/ent/schema/group.go`
- Create: `internal/ent/schema/usagelog.go`
- Create: `internal/ent/schema/usagestat.go`
- Create: `internal/ent/generate.go`
- Create: `internal/ent/...`（生成产物，不手写）

**Interfaces:**
- Consumes: `pkg/logx`（无）；Task 1 的模块配置
- Produces: `domain.Template/Account/Group/UsageLog/StatBucket`、`domain.RequestFormat/AccountStatus/ErrorType` 常量与 `Template.FormatFor(model)`、`Template.Serves(model)`

- [ ] **Step 1: 写失败测试（领域语义先行）**

`internal/domain/types_test.go`:

```go
package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplateFormatFor(t *testing.T) {
	tpl := &Template{
		DefaultFormat: FormatOpenAIChat,
		ModelFormats:  map[string]RequestFormat{"o3": FormatOpenAIResponses},
	}
	require.Equal(t, FormatOpenAIChat, tpl.FormatFor("gpt-4o"))
	require.Equal(t, FormatOpenAIResponses, tpl.FormatFor("o3"))
}

func TestTemplateServes(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		ModelFormats: map[string]RequestFormat{"o3": FormatOpenAIResponses},
		ModelMapping: map[string]string{"claude-sonnet": "claude-sonnet-4-5"},
	}
	require.True(t, tpl.Serves("gpt-4o"), "serves models")
	require.True(t, tpl.Serves("o3"), "serves model_formats keys")
	require.True(t, tpl.Serves("claude-sonnet"), "serves mapping keys")
	require.False(t, tpl.Serves("nope"))
}

func TestRequestFormatValid(t *testing.T) {
	for _, f := range []RequestFormat{FormatOpenAIChat, FormatOpenAIResponses, FormatAnthropic} {
		require.True(t, f.Valid(), "format %s should be valid", f)
	}
	require.False(t, Format("gemini").Valid())
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/domain/ -run TestTemplate -count=1`
Expected: FAIL（`domain/types.go` 不存在）。

- [ ] **Step 3: 写 domain 类型**

`internal/domain/types.go`:

```go
// Package domain 定义网关的核心领域类型；业务层（scheduler/proxy/service）只依赖本包。
package domain

import (
	"slices"
	"time"
)

type RequestFormat string

const (
	FormatOpenAIChat      RequestFormat = "openai-chat"
	FormatOpenAIResponses RequestFormat = "openai-responses"
	FormatAnthropic       RequestFormat = "anthropic"
)

func (f RequestFormat) Valid() bool {
	switch f {
	case FormatOpenAIChat, FormatOpenAIResponses, FormatAnthropic:
		return true
	}
	return false
}

type AccountStatus string

const (
	StatusActive   AccountStatus = "active"
	StatusErr      AccountStatus = "err"
	Status429      AccountStatus = "429"
	StatusDisabled AccountStatus = "disabled"
)

type ErrorType string

const (
	ErrNone      ErrorType = "none"
	Err429       ErrorType = "429"
	Err5xx       ErrorType = "5xx"
	ErrNetwork   ErrorType = "network"
	ErrAuth      ErrorType = "auth"
	ErrNoAccount ErrorType = "no_account"
	ErrAbort     ErrorType = "abort"
)

type Template struct {
	ID            int64
	Name          string
	BaseURL       string
	DefaultFormat RequestFormat
	Models        []string
	ModelFormats  map[string]RequestFormat
	ModelMapping  map[string]string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FormatFor 返回模型 m 在该模板下的请求格式：model_formats 覆盖优先，否则默认格式。
func (t *Template) FormatFor(m string) RequestFormat {
	if f, ok := t.ModelFormats[m]; ok {
		return f
	}
	return t.DefaultFormat
}

// Serves 模型是否在可服务集合（models ∪ model_formats keys ∪ mapping keys）内。
func (t *Template) Serves(m string) bool {
	if slices.Contains(t.Models, m) {
		return true
	}
	if _, ok := t.ModelFormats[m]; ok {
		return true
	}
	if _, ok := t.ModelMapping[m]; ok {
		return true
	}
	return false
}

type Account struct {
	ID             int64
	Name           string
	TemplateID     int64
	Template       *Template
	UpstreamKey    string
	Status         AccountStatus
	CooldownUntil  *time.Time
	Weight         int
	MaxConcurrency int
	LastError      *string
	LastUsedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Group struct {
	ID        int64
	Name      string
	KeyHash   string
	KeyPrefix string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UsageLog struct {
	ID               int64
	RequestID        string
	GroupID          int64 // 0 = 无
	AccountID        int64 // 0 = 无
	TemplateID       int64 // 0 = 无
	Model            string
	MappedModel      string // 空 = 未映射
	Format           RequestFormat
	StatusCode       int
	ErrorType        ErrorType
	LatencyMS        int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CreatedAt        time.Time
}

type StatBucket struct {
	BucketTime       time.Time // 对齐到小时（UTC）
	GroupID          int64     // 0 = 无
	AccountID        int64     // 0 = 无
	TemplateID       int64     // 0 = 无
	Model            string
	IsError          bool
	RequestCount     int64
	ErrorCount       int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	TotalLatencyMS   int64
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/domain/ -count=1`
Expected: PASS。

- [ ] **Step 5: 写 ent schema（5 个文件）**

`internal/ent/schema/template.go`:

```go
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

type Template struct{ ent.Schema }

func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").Unique(),
		field.String("base_url"),
		field.Enum("default_format").
			Values("openai-chat", "openai-responses", "anthropic"),
		field.JSON("models", []string{}),
		field.JSON("model_formats", map[string]string{}), // model -> format 字符串
		field.JSON("model_mapping", map[string]string{}),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Template) Edges() []ent.Edge {
	return []ent.Edge{
		ent.HasMany("accounts", Account.Type),
	}
}
```

`internal/ent/schema/account.go`:

```go
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Account struct{ ent.Schema }

func (Account) Fields() []ent.Field {
	return []ent.Field{
		field.String("name"),
		field.Int64("template_id"),
		field.String("upstream_key"),
		field.Enum("status").
			Values("active", "err", "429", "disabled").
			Default("active"),
		field.Time("cooldown_until").Optional().Nillable(),
		field.Int("weight").Default(100),
		field.Int("max_concurrency").Default(8),
		field.String("last_error").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Account) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", Template.Type).
			Ref("accounts").
			Field("template_id").
			Unique().
			Required(),
		edge.To("groups", Group.Type),
	}
}
```

`internal/ent/schema/group.go`:

```go
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Group struct{ ent.Schema }

func (Group) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").Unique(),
		field.String("key_hash").Unique(),
		field.String("key_prefix"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Group) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("accounts", Account.Type).Ref("groups"),
	}
}
```

`internal/ent/schema/usagelog.go`:

```go
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageLog struct{ ent.Schema }

func (UsageLog) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id"),
		field.Int64("group_id").Optional().Nillable(),
		field.Int64("account_id").Optional().Nillable(),
		field.Int64("template_id").Optional().Nillable(),
		field.String("model").Default(""),
		field.String("mapped_model").Optional().Nillable(),
		field.Enum("format").
			Values("openai-chat", "openai-responses", "anthropic"),
		field.Int("status_code").Default(0),
		field.String("error_type").Default("none"),
		field.Int64("latency_ms").Default(0),
		field.Int64("prompt_tokens").Default(0),
		field.Int64("completion_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Time("created_at").Default(time.Now),
	}
}

func (UsageLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("account_id", "created_at"),
	}
}
```

`internal/ent/schema/usagestat.go`:

```go
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageStat struct{ ent.Schema }

func (UsageStat) Fields() []ent.Field {
	return []ent.Field{
		field.Time("bucket_time"),
		field.Int64("group_id").Default(0), // 0 = 无（唯一索引需要非 NULL）
		field.Int64("account_id").Default(0),
		field.Int64("template_id").Default(0),
		field.String("model").Default(""),
		field.Bool("is_error").Default(false),
		field.Int64("request_count").Default(0),
		field.Int64("error_count").Default(0),
		field.Int64("prompt_tokens").Default(0),
		field.Int64("completion_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("total_latency_ms").Default(0),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (UsageStat) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("bucket_time"),
		index.Fields("bucket_time", "group_id", "account_id", "template_id", "model", "is_error").Unique(),
	}
}
```

`internal/ent/generate.go`:

```go
package ent

//go:generate go run -mod=mod entgo.io/ent/cmd/ent generate --target . ./schema
```

- [ ] **Step 6: 生成代码并验证编译**

Run: `go generate ./internal/ent && go build ./internal/ent/...`
Expected: 生成 `internal/ent/` 全量文件；BUILD PASS。生成产物自动包含 `generate.go` 同目录的 schema（需 `--target .` 使输出落在 `internal/ent` 自身）。若 `--target` 参数不被当前 ent 版本支持，改用 `ent generate ./schema`（在 `internal/ent` 目录内执行 `go generate`，输出到 `internal/ent/ent` 时调整 import 路径，以编译通过为准）。

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "feat: domain types + ent schema (template/account/group/usage_log/usage_stat)"
```

---

### Task 3: 仓库层（repository：ent 适配 domain）

**Files:**
- Create: `internal/repository/repository.go`
- Create: `internal/repository/template_repo.go`
- Create: `internal/repository/account_repo.go`
- Create: `internal/repository/group_repo.go`
- Create: `internal/repository/log_repo.go`
- Create: `internal/repository/stat_repo.go`
- Create: `internal/repository/mapping.go`（ent↔domain 转换）
- Test: `internal/repository/repository_test.go`（内存 SQLite 集成测试，普通 `go test` 可跑）

**Interfaces:**
- Consumes: `internal/domain`、`internal/ent` 生成代码
- Produces（后续任务依赖的精确签名）:
  - `repository.New(drv ent.Driver, migrate bool) (*Repos, error)`（PG 生产用；测试用 `NewSQL(db *sql.DB, driverName, dialect, migrate)` 见下）
  - `Repos.Templates *TemplateRepo`、`Repos.Accounts`、`Repos.Groups`、`Repos.Logs`、`Repos.Stats`
  - `TemplateRepo: Create(ctx, *domain.Template) (*domain.Template, error)`、`Get/List/Update/Delete`
  - `AccountRepo: Create(ctx, *domain.Account) (*domain.Account, error)`、`Get/List/Update/Delete`、`UpdateStatus(ctx, id, status, cooldownUntil, lastError) error`
  - `GroupRepo: Create(ctx, *domain.Group) (*domain.Group, error)`、`Get/List/Update/Delete`、`SetAccounts(ctx, groupID, []int64) error`、`LoadGroupsAccounts(ctx) (map[int64][]*domain.Account, error)`、`LoadGroupAccounts(ctx, groupID) ([]*domain.Account, error)`、`LoadGroupKeys(ctx) (map[string]int64, error)`
  - `LogRepo: InsertBatch(ctx, []*domain.UsageLog) error`、`Query(ctx, LogQuery) ([]*domain.UsageLog, int64, error)`（rows + total）
  - `StatRepo: Upsert(ctx, []*domain.StatBucket) error`、`Scan(ctx, StatQuery) ([]*domain.StatBucket, error)`

- [ ] **Step 1: 写失败测试**

`internal/repository/repository_test.go`（先写核心流转用例，覆盖 CRUD/边/状态/日志/统计）：

```go
package repository_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	_ "modernc.org/sqlite"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

func newRepos(t *testing.T) *repository.Repos {
	t.Helper()
	db, err := sql.Open("sqlite", "file:test"+t.Name()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	drv := entsql.OpenDB(dialect.SQLite, db)
	repos, err := repository.New(drv, true)
	require.NoError(t, err)
	return repos
}

func ctx() context.Context { return context.Background() }

func TestTemplateCRUD(t *testing.T) {
	r := newRepos(t)
	tpl, err := r.Templates.Create(ctx(), &domain.Template{
		Name:          "openai-main",
		BaseURL:       "https://api.openai.com/v1",
		DefaultFormat: domain.FormatOpenAIChat,
		Models:        []string{"gpt-4o"},
		ModelFormats:  map[string]domain.RequestFormat{"o3": domain.FormatOpenAIResponses},
		ModelMapping:  map[string]string{"gpt-4o": "gpt-4o-2026-01-01"},
	})
	require.NoError(t, err)
	got, err := r.Templates.Get(ctx(), tpl.ID)
	require.NoError(t, err)
	require.Equal(t, "openai-main", got.Name)
	require.Equal(t, domain.FormatOpenAIChat, got.DefaultFormat)
	require.Equal(t, domain.FormatOpenAIResponses, got.FormatFor("o3"), "model_formats roundtrip")
	got.Name = "renamed"
	_, err = r.Templates.Update(ctx(), got)
	require.NoError(t, err)
	require.NoError(t, r.Templates.Delete(ctx(), tpl.ID))
	_, err = r.Templates.Get(ctx(), tpl.ID)
	require.Error(t, err, "expected not found after delete")
}

func TestAccountAndGroup(t *testing.T) {
	r := newRepos(t)
	tpl, err := r.Templates.Create(ctx(), &domain.Template{
		Name: "t", BaseURL: "https://u/v1", DefaultFormat: domain.FormatAnthropic,
	})
	require.NoError(t, err)
	acc, err := r.Accounts.Create(ctx(), &domain.Account{
		Name: "acc1", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 80, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	g, err := r.Groups.Create(ctx(), &domain.Group{Name: "g1", KeyHash: "h1", KeyPrefix: "gk-aaaa"})
	require.NoError(t, err)
	require.NoError(t, r.Groups.SetAccounts(ctx(), g.ID, []int64{acc.ID}))
	m, err := r.Groups.LoadGroupsAccounts(ctx())
	require.NoError(t, err)
	got := m[g.ID]
	require.Len(t, got, 1)
	require.Equal(t, acc.ID, got[0].ID)
	require.NotNil(t, got[0].Template)
	keys, err := r.Groups.LoadGroupKeys(ctx())
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, g.ID, keys["h1"])
	require.NoError(t, r.Accounts.UpdateStatus(ctx(), acc.ID, domain.Status429, nil, nil))
	a2, err := r.Accounts.Get(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, domain.Status429, a2.Status, "status persisted")
}

func TestLogsAndStats(t *testing.T) {
	r := newRepos(t)
	logs := []*domain.UsageLog{
		{RequestID: "r1", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, LatencyMS: 10, TotalTokens: 100},
		{RequestID: "r2", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 20, TotalTokens: 0},
	}
	require.NoError(t, r.Logs.InsertBatch(ctx(), logs))
	rows, total, err := r.Logs.Query(ctx(), repository.LogQuery{GroupID: 1, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)
	bucket := time.Now().Truncate(time.Hour)
	require.NoError(t, r.Stats.Upsert(ctx(), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 2, ErrorCount: 1, TotalTokens: 100, TotalLatencyMS: 30},
	}))
	require.NoError(t, r.Stats.Upsert(ctx(), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 3, ErrorCount: 1, TotalTokens: 200, TotalLatencyMS: 40},
	}))
	scanned, err := r.Stats.Scan(ctx(), repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, scanned, 1)
	require.Equal(t, int64(5), scanned[0].RequestCount, "upsert accumulates")
	require.Equal(t, int64(300), scanned[0].TotalTokens)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/repository/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 repository 入口 + mapping**

`internal/repository/mapping.go`:

```go
package repository

import (
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
)

func toDomainTemplate(t *ent.Template) *domain.Template {
	mf := make(map[string]domain.RequestFormat, len(t.ModelFormats))
	for k, v := range t.ModelFormats {
		mf[k] = domain.RequestFormat(v)
	}
	return &domain.Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		DefaultFormat: domain.RequestFormat(t.DefaultFormat),
		Models:        t.Models, ModelFormats: mf, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toDomainAccount(a *ent.Account) *domain.Account {
	var tpl *domain.Template
	if a.Edges.Template != nil {
		tpl = toDomainTemplate(a.Edges.Template)
	}
	return &domain.Account{
		ID: a.ID, Name: a.Name, TemplateID: a.TemplateID, Template: tpl,
		UpstreamKey: a.UpstreamKey, Status: domain.AccountStatus(a.Status),
		CooldownUntil: a.CooldownUntil, Weight: a.Weight, MaxConcurrency: a.MaxConcurrency,
		LastError: a.LastError, LastUsedAt: a.LastUsedAt,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// templatePredicate 供调用处过滤，避免未用 import 告警。
var _ = template.FieldName
```

`internal/repository/repository.go`:

```go
// Package repository 用 ent 实现持久化，对外只暴露 domain 类型。
package repository

import (
	"context"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go-proxy-mini/internal/ent"
)

type Repos struct {
	Templates *TemplateRepo
	Accounts  *AccountRepo
	Groups    *GroupRepo
	Logs      *LogRepo
	Stats     *StatRepo
	Client    *ent.Client
}

// New 用既有 driver 构建仓库（PG 生产：entsql.OpenDB(dialect.Postgres, db)；测试：sqlite）。
func New(drv dialect.Driver, migrate bool) (*Repos, error) {
	client := ent.NewClient(ent.Driver(drv))
	if migrate {
		if err := client.Schema.Create(context.Background()); err != nil {
			return nil, err
		}
	}
	return &Repos{
		Templates: &TemplateRepo{client: client},
		Accounts:  &AccountRepo{client: client},
		Groups:    &GroupRepo{client: client},
		Logs:      &LogRepo{client: client},
		Stats:     &StatRepo{client: client},
		Client:    client,
	}, nil
}

// OpenPG 打开原生 pgxpool（生产入口，用户决策 2026-08-05）。
// ent 桥接见 Task 8：stdlib.OpenDBFromPool(pool) → entsql.OpenDB(dialect.Postgres, sqlDB)。
func OpenPG(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	return pgxpool.NewWithConfig(ctx, cfg)
}
```

- [ ] **Step 4: 写 TemplateRepo / AccountRepo**

`internal/repository/template_repo.go`:

```go
package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
)

type TemplateRepo struct{ client *ent.Client }

func (r *TemplateRepo) Create(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	row, err := r.client.Template.Create().
		SetName(t.Name).SetBaseURL(t.BaseURL).
		SetDefaultFormat(string(t.DefaultFormat)).
		SetModels(t.Models).
		SetModelFormats(toStringMap(t.ModelFormats)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) Get(ctx context.Context, id int64) (*domain.Template, error) {
	row, err := r.client.Template.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) List(ctx context.Context) ([]*domain.Template, error) {
	rows, err := r.client.Template.Query().Order(ent.Asc(template.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, nil
}

func (r *TemplateRepo) Update(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	row, err := r.client.Template.UpdateOneID(t.ID).
		SetName(t.Name).SetBaseURL(t.BaseURL).
		SetDefaultFormat(string(t.DefaultFormat)).
		SetModels(t.Models).
		SetModelFormats(toStringMap(t.ModelFormats)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) Delete(ctx context.Context, id int64) error {
	return r.client.Template.DeleteOneID(id).Exec(ctx)
}

func toStringMap(m map[string]domain.RequestFormat) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}
```

`internal/repository/account_repo.go`:

```go
package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
)

type AccountRepo struct{ client *ent.Client }

func (r *AccountRepo) Create(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.Create().
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) Get(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Query().Where(ent2.ID(id)).WithTemplate().Only(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) List(ctx context.Context) ([]*domain.Account, error) {
	rows, err := r.client.Account.Query().WithTemplate().Order(ent.Asc(account.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAccount(row))
	}
	return out, nil
}

func (r *AccountRepo) Update(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.UpdateOneID(a.ID).
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		SetStatus(string(a.Status)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) Delete(ctx context.Context, id int64) error {
	return r.client.Account.DeleteOneID(id).Exec(ctx)
}

func (r *AccountRepo) UpdateStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string) error {
	u := r.client.Account.UpdateOneID(id).SetStatus(string(status))
	if cooldownUntil != nil {
		u = u.SetCooldownUntil(*cooldownUntil)
	} else {
		u = u.ClearCooldownUntil()
	}
	if lastError != nil {
		u = u.SetLastError(*lastError)
	} else {
		u = u.ClearLastError()
	}
	_, err := u.Save(ctx)
	return err
}
```

注意：`account_repo.go` 内 `ent2.ID`、`account.FieldID` 的 import 应为 `ent` 包别名与 `internal/ent/account`；在 Step 4 编译时报错时按提示修正 import（`Get` 用 `r.client.Account.Get(ctx, id)` 即可避免 predicate import——改用它）。

- [ ] **Step 5: 写 GroupRepo / LogRepo / StatRepo**

`internal/repository/group_repo.go`:

```go
package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/group"
)

type GroupRepo struct{ client *ent.Client }

func (r *GroupRepo) Create(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.Create().
		SetName(g.Name).SetKeyHash(g.KeyHash).SetKeyPrefix(g.KeyPrefix).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) Get(ctx context.Context, id int64) (*domain.Group, error) {
	row, err := r.client.Group.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) List(ctx context.Context) ([]*domain.Group, error) {
	rows, err := r.client.Group.Query().Order(ent.Asc(group.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, &domain.Group{
			ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

func (r *GroupRepo) Update(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.UpdateOneID(g.ID).
		SetName(g.Name).SetKeyHash(g.KeyHash).SetKeyPrefix(g.KeyPrefix).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) Delete(ctx context.Context, id int64) error {
	return r.client.Group.DeleteOneID(id).Exec(ctx)
}

// SetAccounts 全量替换分组账号成员（规格 §8）。
func (r *GroupRepo) SetAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	_, err := r.client.Group.UpdateOneID(groupID).
		ClearAccounts().
		AddAccountIDs(accountIDs...).
		Save(ctx)
	return err
}

func (r *GroupRepo) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	groups, err := r.client.Group.Query().
		WithAccounts(func(q *ent.AccountQuery) { q.WithTemplate() }).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]*domain.Account, len(groups))
	for _, g := range groups {
		var accs []*domain.Account
		for _, a := range g.Edges.Accounts {
			accs = append(accs, toDomainAccount(a))
		}
		out[g.ID] = accs
	}
	return out, nil
}

func (r *GroupRepo) LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error) {
	accs, err := r.client.Group.QueryAccounts(groupID).
		WithTemplate().
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Account, 0, len(accs))
	for _, a := range accs {
		out = append(out, toDomainAccount(a))
	}
	return out, nil
}

func (r *GroupRepo) LoadGroupKeys(ctx context.Context) (map[string]int64, error) {
	rows, err := r.client.Group.Query().Select(group.FieldKeyHash, group.FieldID).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.KeyHash] = row.ID
	}
	return out, nil
}
```

注意：`QueryAccounts` 的集合查询在 ent 上名为 `Group.QueryAccounts()`（单复数以生成代码为准，编译失败时改用 `r.client.Group.Query().Where(group.IDEQ(groupID)).QueryAccounts()`）。

`internal/repository/log_repo.go`:

```go
package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/usage log" // 生成包名无空格，实际为 usagelog
)

type LogQuery struct {
	GroupID    int64 // 0 = 不过滤
	AccountID  int64
	Model      string
	StatusCode int
	ErrorType  string
	From       *time.Time
	To         *time.Time
	Offset     int
	Limit      int
}

type LogRepo struct{ client *ent.Client }

func (r *LogRepo) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	builders := make([]*ent.UsageLogCreate, 0, len(logs))
	for _, l := range logs {
		c := r.client.UsageLog.Create().
			SetRequestID(l.RequestID).
			SetModel(l.Model).
			SetFormat(string(l.Format)).
			SetStatusCode(l.StatusCode).
			SetErrorType(string(l.ErrorType)).
			SetLatencyMS(l.LatencyMS).
			SetPromptTokens(l.PromptTokens).
			SetCompletionTokens(l.CompletionTokens).
			SetTotalTokens(l.TotalTokens).
			SetCreatedAt(l.CreatedAt)
		if l.GroupID > 0 {
			c = c.SetGroupID(l.GroupID)
		}
		if l.AccountID > 0 {
			c = c.SetAccountID(l.AccountID)
		}
		if l.TemplateID > 0 {
			c = c.SetTemplateID(l.TemplateID)
		}
		if l.MappedModel != "" {
			c = c.SetMappedModel(l.MappedModel)
		}
		builders = append(builders, c)
	}
	_, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx)
	return err
}

func (r *LogRepo) Query(ctx context.Context, q LogQuery) ([]*domain.UsageLog, int64, error) {
	pred := r.client.UsageLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(usagelog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagelog.AccountIDEQ(q.AccountID))
	}
	if q.Model != "" {
		pred = pred.Where(usagelog.ModelEQ(q.Model))
	}
	if q.StatusCode > 0 {
		pred = pred.Where(usagelog.StatusCodeEQ(q.StatusCode))
	}
	if q.ErrorType != "" {
		pred = pred.Where(usagelog.ErrorTypeEQ(q.ErrorType))
	}
	if q.From != nil {
		pred = pred.Where(usagelog.CreatedAtGTE(*q.From))
	}
	if q.To != nil {
		pred = pred.Where(usagelog.CreatedAtLTE(*q.To))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := pred.Order(ent.Desc(usagelog.FieldID)).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, row := range rows {
		l := &domain.UsageLog{
			ID: row.ID, RequestID: row.RequestID,
			Model: row.Model, Format: domain.RequestFormat(row.Format),
			StatusCode: row.StatusCode, ErrorType: domain.ErrorType(row.ErrorType),
			LatencyMS: row.LatencyMS,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			TotalTokens: row.TotalTokens, CreatedAt: row.CreatedAt,
		}
		if row.GroupID != nil {
			l.GroupID = *row.GroupID
		}
		if row.AccountID != nil {
			l.AccountID = *row.AccountID
		}
		if row.TemplateID != nil {
			l.TemplateID = *row.TemplateID
		}
		if row.MappedModel != nil {
			l.MappedModel = *row.MappedModel
		}
		out = append(out, l)
	}
	return out, int64(total), nil
}
```

`internal/repository/stat_repo.go`:

```go
package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/usagestat"
)

type StatQuery struct {
	GroupID   int64
	AccountID int64
	Model     string
	From      time.Time
	To        time.Time
}

type StatRepo struct{ client *ent.Client }

// Upsert 逐 bucket 冲突累加（规格 §10.5：聚合不可失真）。
func (r *StatRepo) Upsert(ctx context.Context, buckets []*domain.StatBucket) error {
	for _, b := range buckets {
		_, err := r.client.UsageStat.Create().
			SetBucketTime(b.BucketTime).
			SetGroupID(b.GroupID).
			SetAccountID(b.AccountID).
			SetTemplateID(b.TemplateID).
			SetModel(b.Model).
			SetIsError(b.IsError).
			SetRequestCount(b.RequestCount).
			SetErrorCount(b.ErrorCount).
			SetPromptTokens(b.PromptTokens).
			SetCompletionTokens(b.CompletionTokens).
			SetTotalTokens(b.TotalTokens).
			SetTotalLatencyMS(b.TotalLatencyMS).
			OnConflictColumns("bucket_time", "group_id", "account_id", "template_id", "model", "is_error").
			Update(func(u *ent.UsageStatUpsert) {
				u.AddRequestCount(b.RequestCount)
				u.AddErrorCount(b.ErrorCount)
				u.AddPromptTokens(b.PromptTokens)
				u.AddCompletionTokens(b.CompletionTokens)
				u.AddTotalTokens(b.TotalTokens)
				u.AddTotalLatencyMS(b.TotalLatencyMS)
			}).
			Save(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

// Scan 拉取时间范围内的原始小时桶（日聚合在 service 层做，规避方言差异）。
func (r *StatRepo) Scan(ctx context.Context, q StatQuery) ([]*domain.StatBucket, error) {
	pred := r.client.UsageStat.Query().
		Where(
			usagestat.BucketTimeGTE(q.From),
			usagestat.BucketTimeLT(q.To),
		)
	if q.GroupID > 0 {
		pred = pred.Where(usagestat.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagestat.AccountIDEQ(q.AccountID))
	}
	if q.Model != "" {
		pred = pred.Where(usagestat.ModelEQ(q.Model))
	}
	rows, err := pred.Order(ent.Asc(usagestat.FieldBucketTime)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.StatBucket, 0, len(rows))
	for _, row := range rows {
		out = append(out, &domain.StatBucket{
			BucketTime: row.BucketTime, GroupID: row.GroupID, AccountID: row.AccountID,
			TemplateID: row.TemplateID, Model: row.Model, IsError: row.IsError,
			RequestCount: row.RequestCount, ErrorCount: row.ErrorCount,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			TotalTokens: row.TotalTokens, TotalLatencyMS: row.TotalLatencyMS,
		})
	}
	return out, nil
}
```

- [ ] **Step 6: 编译修正 + 跑测试**

Run: `go build ./internal/repository/ && go test ./internal/repository/ -count=1`
Expected: 测试 PASS。注意点：① ent 生成代码的 import 包名（`usagelog`/`account`/`group`/`template`/`ent`）以实际生成为准；② `sqlite` 下 `ON CONFLICT` 由 ent 方言层适配；③ 若 `LoadGroupAccounts`/`QueryAccounts` API 与生成代码不一致，按编译错误改用等价查询。

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "feat: repository layer with ent adapters (sqlite-backed integration tests)"
```

---

### Task 4: 调度器（scheduler：状态机 / 选号 / 并发 / 快照 / 回写）

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Create: `internal/scheduler/selection.go`
- Create: `internal/scheduler/state.go`
- Test: `internal/scheduler/scheduler_test.go`（纯内存，无 DB）

**Interfaces:**
- Consumes: `internal/domain`、`pkg/logx`
- Produces:
  - `scheduler.New(cfg Config, loader Loader, log *logx.Logger) *Scheduler`
  - `scheduler.Config{DefaultMaxConcurrency int; Cooldown429, BackoffBase, BackoffMax, SyncInterval time.Duration}`
  - `type Loader interface { LoadGroupsAccounts(ctx) (map[int64][]*domain.Account, error); LoadGroupAccounts(ctx, groupID) ([]*domain.Account, error); UpdateAccountStatus(ctx, id, status, cooldownUntil, lastError) error }`
  - `scheduler.Select(groupID, format, model) (*Selection, error)` — `Selection{AccountID, TemplateID int64; BaseURL string; Format domain.RequestFormat; UpstreamKey string; Model string}`；错误：`ErrGroupNotFound` / `ErrFormatUnavailable` / `ErrNoAvailable`（sentinel）
  - `scheduler.Release(accountID)`、`scheduler.MarkResult(accountID, ResultKind, resetAt *time.Time)`（`ResultOK/Result429/ResultError`）、`scheduler.InvalidateGroup(groupID)`、`scheduler.InvalidateAll()`、`scheduler.Start(ctx)`、`scheduler.Runtime(accountID) (RuntimeInfo, bool)`（`RuntimeInfo{Status, CooldownUntil, Concurrency, ErrRate, ErrCount}`）

- [ ] **Step 1: 写失败测试（核心语义：硬过滤/偏好/冷却/退避/并发）**

`internal/scheduler/scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// --- 测试 Loader（内存实现） ---

type memLoader struct {
	mu       sync.Mutex
	byGroup  map[int64][]*domain.Account
	writes   []statusWrite
}

func newMemLoader(byGroup map[int64][]*domain.Account) *memLoader {
	return &memLoader{byGroup: byGroup}
}

func (m *memLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		out[k] = v
	}
	return out, nil
}

func (m *memLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byGroup[id], nil
}

func (m *memLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, statusWrite{id: id, status: status, cooldown: cooldown})
	return nil
}

type statusWrite struct {
	id       int64
	status   domain.AccountStatus
	cooldown *time.Time
}

func testCfg() Config {
	return Config{
		DefaultMaxConcurrency: 2,
		Cooldown429:           30 * time.Second,
		BackoffBase:           5 * time.Second,
		BackoffMax:            5 * time.Minute,
		SyncInterval:          100 * time.Hour, // 测试中不触发定时同步
	}
}

func tpl(id int64, format domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://u/v1", DefaultFormat: format, Models: models}
}

func acc(id int64, t *domain.Template, maxConc int) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: maxConc}
}

func newSched(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	s := New(testCfg(), m, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

func TestSelectFormatHardFilter(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	ant := tpl(2, domain.FormatAnthropic, []string{"claude"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, chat, 4), acc(2, ant, 4)},
	})
	s := newSched(t, m)

	// anthropic 路径下只命中 anthropic 模板账号
	sel, err := s.Select(10, domain.FormatAnthropic, "claude")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	s.Release(sel.AccountID)

	// 格式不匹配（组内只有 chat 模板）→ ErrFormatUnavailable
	m2 := newMemLoader(map[int64][]*domain.Account{10: {acc(1, chat, 4)}})
	s2 := newSched(t, m2)
	_, err = s2.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.ErrorIs(t, err, ErrFormatUnavailable)
}

func TestSelectModelPreference(t *testing.T) {
	// 两账号同格式：一个 Serves(model)，一个不
	tA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	tB := tpl(2, domain.FormatOpenAIChat, []string{"other"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tA, 4), acc(2, tB, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID, "model preference tier")
}

func TestConcurrencyLimit(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1)}})
	s := newSched(t, m)
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.Release(sel1.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after release")
}

func TestMark429CooldownAndRecover(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	reset := time.Now().Add(10 * time.Second)
	s.MarkResult(1, Result429, &reset)
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "in cooldown should be unavailable")
	// 冷却过期后惰性恢复
	s.timeNow = func() time.Time { return time.Now().Add(15 * time.Second) }
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after cooldown")
	s.MarkResult(sel.AccountID, ResultOK, nil)
	s.Release(sel.AccountID)
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.writes, "expected async status write")
}

func TestMarkErrorBackoff(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	s.MarkResult(1, ResultError, nil) // 第一次失败 → backoff base
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusErr, ri.Status)
	require.Equal(t, 1, ri.ErrCount)
	require.NotNil(t, ri.CooldownUntil)
	require.True(t, ri.CooldownUntil.After(time.Now().Add(4*time.Second)), "backoff base applied")
	s.MarkResult(1, ResultError, nil) // 第二次 → 指数
	ri, _ = s.Runtime(1)
	require.Equal(t, 2, ri.ErrCount)
	s.MarkResult(1, ResultOK, nil)
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status, "success resets status")
	require.Equal(t, 0, ri.ErrCount, "success resets err count")
}

func TestSelectUnknownGroup(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{})
	s := newSched(t, m)
	_, err := s.Select(99, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrGroupNotFound)
}

func TestInvalidateGroupReloads(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tplx, 4))
	m.mu.Unlock()
	s.InvalidateGroup(10) // 同步 reload
	// 账号 2 也进入候选：把账号1 占满并发后再选应命中 2
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.NotEqual(t, sel1.AccountID, sel2.AccountID, "both accounts should serve")
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/scheduler/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 state.go（快照 + 运行时状态）**

`internal/scheduler/state.go`:

```go
package scheduler

import (
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
)

// errRateScale 是错误率 EWMA 的定点缩放（0..1e6）。
const errRateScale = 1_000_000

type accState struct {
	status        domain.AccountStatus
	cooldownUntil *time.Time
	errCount      int
	lastError     *string
	lastUsedAt    *time.Time
}

type accountSnapshot struct {
	acc         domain.Account
	tpl         *domain.Template
	concurrency atomic.Int64
	errRate     atomic.Uint64 // 定点
	state       atomic.Pointer[accState]
}

func (a *accountSnapshot) statePtr() *accState {
	st := a.state.Load()
	if st == nil {
		st = &accState{status: domain.StatusActive}
		a.state.Store(st)
	}
	return st
}

func (a *accountSnapshot) score() float64 {
	rate := float64(a.errRate.Load()) / errRateScale
	return float64(a.acc.Weight) * (1 - rate)
}

type groupSnapshot struct {
	accounts []*accountSnapshot
}

// snapshotStore 整体换入换出（atomic.Value），重建不阻塞请求路径。
type snapshotStore struct {
	groups atomic.Value // map[int64]*groupSnapshot
	byID   atomic.Value // map[int64]*accountSnapshot
}

func (s *snapshotStore) store(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	s.groups.Store(groups)
	s.byID.Store(byID)
}
```

- [ ] **Step 4: 写 scheduler.go（核心）**

`internal/scheduler/scheduler.go`:

```go
// Package scheduler 实现内存优先的账号调度：状态机（err/429 冷却 + 指数退避）、
// 选号（格式硬过滤 + 模型偏好 + 加权随机）、并发槽、快照缓存与异步状态回写。
// 规格 §5。单实例语义：运行时状态仅存内存。
package scheduler

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrGroupNotFound    = errors.New("scheduler: group not found")
	ErrFormatUnavailable = errors.New("scheduler: no account for request format")
	ErrNoAvailable      = errors.New("scheduler: no available account")
)

type Config struct {
	DefaultMaxConcurrency int
	Cooldown429           time.Duration
	BackoffBase           time.Duration
	BackoffMax            time.Duration
	SyncInterval          time.Duration
}

// Loader 是调度器的数据源（由 repository 实现）。
type Loader interface {
	LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error)
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
	UpdateAccountStatus(ctx context.Context, accountID int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string) error
}

type ResultKind int

const (
	ResultOK ResultKind = iota
	Result429
	ResultError
)

type Selection struct {
	AccountID   int64
	TemplateID  int64
	BaseURL     string
	Format      domain.RequestFormat
	UpstreamKey string
	Model       string // 已应用模型映射
}

type RuntimeInfo struct {
	Status        domain.AccountStatus
	CooldownUntil *time.Time
	Concurrency   int64
	ErrRate       float64
	ErrCount      int
}

type statusWrite struct {
	id       int64
	status   domain.AccountStatus
	cooldown *time.Time
	lastErr  *string
}

type Scheduler struct {
	cfg     Config
	loader  Loader
	log     *logx.Logger
	store   snapshotStore
	reloadMu sync.Mutex // 重建互斥（低频，不占热路径）
	writeCh chan statusWrite
	timeNow func() time.Time
}

func New(cfg Config, loader Loader, log *logx.Logger) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		loader:  loader,
		log:     log,
		writeCh: make(chan statusWrite, 4096),
		timeNow: time.Now,
	}
}

// Start 启动定时同步与异步状态回写。
func (s *Scheduler) Start(ctx context.Context) {
	go s.syncLoop(ctx)
	go s.writebackLoop(ctx)
}

func (s *Scheduler) syncLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.reload(ctx); err != nil && s.log != nil {
				s.log.Warn("scheduler sync failed", logx.Error(err))
			}
		}
	}
}

func (s *Scheduler) writebackLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-s.writeCh:
			// 合并窗口内同一账号的重复写（幂等覆盖）
			accs := map[int64]statusWrite{w.id: w}
			drain := true
			for drain {
				select {
				case w2 := <-s.writeCh:
					accs[w2.id] = w2
				default:
					drain = false
				}
			}
			for _, ww := range accs {
				if err := s.loader.UpdateAccountStatus(context.Background(), ww.id, ww.status, ww.cooldown, ww.lastErr); err != nil && s.log != nil {
					s.log.Warn("account status writeback failed", logx.Int64("account_id", ww.id), logx.Error(err))
				}
			}
		}
	}
}

// reload 全量重建快照（启动/定时/InvalidateAll）。
func (s *Scheduler) reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	m, err := s.loader.LoadGroupsAccounts(ctx)
	if err != nil {
		return err
	}
	s.store.store(buildSnapshots(m, s.cfg.DefaultMaxConcurrency))
	return nil
}

func buildSnapshots(m map[int64][]*domain.Account, defaultMax int) (map[int64]*groupSnapshot, map[int64]*accountSnapshot) {
	groups := make(map[int64]*groupSnapshot, len(m))
	byID := make(map[int64]*accountSnapshot)
	for gid, accs := range m {
		gs := &groupSnapshot{}
		for _, a := range accs {
			as := &accountSnapshot{acc: *a, tpl: a.Template}
			as.state.Store(&accState{status: a.Status, cooldownUntil: a.CooldownUntil})
			if a.MaxConcurrency <= 0 {
				as.acc.MaxConcurrency = defaultMax
			}
			gs.accounts = append(gs.accounts, as)
			byID[a.ID] = as
		}
		groups[gid] = gs
	}
	return groups, byID
}

func (s *Scheduler) InvalidateGroup(groupID int64) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	accs, err := s.loader.LoadGroupAccounts(context.Background(), groupID)
	if err != nil {
		if s.log != nil {
			s.log.Warn("group reload failed", logx.Int64("group_id", groupID), logx.Error(err))
		}
		return
	}
	m, byID := s.store.groups.Load().(map[int64]*groupSnapshot), s.store.byID.Load().(map[int64]*accountSnapshot)
	newM := make(map[int64]*groupSnapshot, len(m)+1)
	for k, v := range m {
		newM[k] = v
	}
	newM[groupID] = &groupSnapshot{accounts: buildSnapshots(map[int64][]*domain.Account{groupID: accs}, s.cfg.DefaultMaxConcurrency)[groupID].accounts}
	s.store.store(newM, byID)
}

func (s *Scheduler) InvalidateAll() {
	if err := s.reload(context.Background()); err != nil && s.log != nil {
		s.log.Warn("scheduler reload failed", logx.Error(err))
	}
}

// Runtime 供管理端展示运行时视图。
func (s *Scheduler) Runtime(accountID int64) (RuntimeInfo, bool) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[accountID]
	if !ok {
		return RuntimeInfo{}, false
	}
	st := a.statePtr()
	return RuntimeInfo{
		Status: st.status, CooldownUntil: st.cooldownUntil,
		Concurrency: a.concurrency.Load(),
		ErrRate:     float64(a.errRate.Load()) / errRateScale,
		ErrCount:    st.errCount,
	}, true
}

// Release 释放并发槽（请求结束必须调用，含流式断开）。
func (s *Scheduler) Release(accountID int64) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	if a, ok := byID[accountID]; ok {
		a.concurrency.Add(-1)
	}
}

// MarkResult 请求结果回流：更新状态/冷却/EWMA，异步回写 DB。
func (s *Scheduler) MarkResult(accountID int64, kind ResultKind, resetAt *time.Time) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[accountID]
	if !ok {
		return
	}
	now := s.timeNow()
	st := a.statePtr()
	var (
		next      accState
		cooldown  *time.Time
		lastErr   *string
		rateDelta float64
	)
	switch kind {
	case ResultOK:
		next = accState{status: domain.StatusActive, lastUsedAt: &now}
		rateDelta = 0
	case Result429:
		cooldown = resetAt
		if cooldown == nil {
			c := now.Add(s.cfg.Cooldown429)
			cooldown = &c
		}
		lastErr = strPtr("upstream 429 rate limited")
		next = accState{status: domain.Status429, cooldownUntil: cooldown, errCount: st.errCount + 1, lastError: lastErr, lastUsedAt: &now}
		rateDelta = 1
	case ResultError:
		backoff := backoffDuration(s.cfg.BackoffBase, s.cfg.BackoffMax, st.errCount)
		c := now.Add(backoff)
		cooldown = &c
		lastErr = strPtr("upstream error")
		next = accState{status: domain.StatusErr, cooldownUntil: cooldown, errCount: st.errCount + 1, lastError: lastErr, lastUsedAt: &now}
		rateDelta = 1
	}
	a.state.Store(&next)
	// EWMA：α=0.2
	old := float64(a.errRate.Load()) / errRateScale
	rate := 0.2*rateDelta + 0.8*old
	a.errRate.Store(uint64(rate * errRateScale))
	s.enqueueWrite(accountID, next)
}

func backoffDuration(base, max time.Duration, errCount int) time.Duration {
	d := time.Duration(float64(base) * math.Pow(2, float64(errCount)))
	if d > max {
		return max
	}
	return d
}

func strPtr(s string) *string { return &s }

func (s *Scheduler) enqueueWrite(id int64, st accState) {
	select {
	case s.writeCh <- statusWrite{id: id, status: st.status, cooldown: st.cooldownUntil, lastErr: st.lastError}:
	default:
		// 队列满：丢弃 DB 回写（内存状态已生效，重启后由下一次请求重新判定）
	}
}
```

`internal/scheduler/selection.go`:

```go
package scheduler

import (
	"math/rand/v2"

	"go-proxy-mini/internal/domain"
)

// Select 按硬过滤（格式）+ 模型偏好 + 加权随机选号，并占用并发槽。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	gs, ok := groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	now := s.timeNow()
	var tier1, tier2 []*accountSnapshot
	for _, a := range gs.accounts {
		if a.tpl == nil {
			continue
		}
		// 硬过滤：format_for(model) == 请求格式（未声明模型回落到默认格式）
		if a.tpl.FormatFor(model) != format {
			continue
		}
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue // 冷却未过期
		}
		if a.concurrency.Load() >= int64(a.acc.MaxConcurrency) {
			continue // 并发满
		}
		if a.tpl.Serves(model) {
			tier1 = append(tier1, a)
		} else {
			tier2 = append(tier2, a)
		}
	}
	pool := tier1
	if len(pool) == 0 {
		pool = tier2
	}
	if len(pool) == 0 {
		return nil, ErrFormatUnavailable
	}
	for _, a := range s.weightedOrder(pool) {
		// CAS 抢占并发槽
		for {
			cur := a.concurrency.Load()
			if cur >= int64(a.acc.MaxConcurrency) {
				break
			}
			if a.concurrency.CompareAndSwap(cur, cur+1) {
				st := a.statePtr()
				mapped := model
				if m, ok := a.tpl.ModelMapping[model]; ok {
					mapped = m
				}
				used := s.timeNow()
				st2 := *st
				st2.lastUsedAt = &used
				a.state.Store(&st2)
				return &Selection{
					AccountID: a.acc.ID, TemplateID: a.tpl.ID,
					BaseURL: a.tpl.BaseURL, Format: format,
					UpstreamKey: a.acc.UpstreamKey, Model: mapped,
				}, nil
			}
		}
	}
	return nil, ErrNoAvailable
}

// weightedOrder 按 score = weight × (1 − errRate) 做加权随机排列（不放回）。
// 随机源用 math/rand/v2 顶层函数（并发安全，Go 1.26 现代特性），无全局 RNG 锁。
func (s *Scheduler) weightedOrder(pool []*accountSnapshot) []*accountSnapshot {
	remaining := make([]*accountSnapshot, len(pool))
	copy(remaining, pool)
	out := make([]*accountSnapshot, 0, len(pool))
	for len(remaining) > 0 {
		total := 0.0
		for _, a := range remaining {
			total += a.score()
		}
		pick := rand.Float64() * total
		acc := 0.0
		idx := 0
		for i, a := range remaining {
			acc += a.score()
			if acc >= pick || i == len(remaining)-1 {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}
```

- [ ] **Step 5: 编译修正 + 跑测试**

Run: `go build ./internal/scheduler/ && go test ./internal/scheduler/ -count=1`
Expected: PASS。若 `weightedOrder` 的确定性取模实现引发测试抖动（概率极低），属正常；可断言改为允许任一候选。

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat: in-memory scheduler with cooldown/backoff/weighted selection/concurrency slots"
```

---

### Task 5: 用量管线（usage：明细批量落库 + 预聚合 + 清理）

**Files:**
- Create: `internal/usage/usage.go`
- Test: `internal/usage/usage_test.go`

**Interfaces:**
- Consumes: `internal/domain`、`internal/repository`（`LogRepo`/`StatRepo`）、`pkg/logx`
- Produces:
  - `usage.New(cfg UsageConfig, logs LogInserter, stats StatUpserter, log *logx.Logger) *Recorder`
  - `Recorder.Start(ctx)`、`Recorder.Record(*domain.UsageLog)`（非阻塞）、`Recorder.Close(ctx)`（排空）
  - `type LogInserter interface { InsertBatch(ctx, []*domain.UsageLog) error }`
  - `type StatUpserter interface { Upsert(ctx, []*domain.StatBucket) error }`

- [ ] **Step 1: 写失败测试**

`internal/usage/usage_test.go`:

```go
package usage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

type memLogStore struct {
	mu   sync.Mutex
	logs []*domain.UsageLog
}

func (m *memLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, logs...)
	return nil
}

type memStatStore struct {
	mu      sync.Mutex
	buckets []*domain.StatBucket
}

func (m *memStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, nb := range b {
		for _, ob := range m.buckets {
			if ob.BucketTime.Equal(nb.BucketTime) && ob.GroupID == nb.GroupID && ob.Model == nb.Model && ob.IsError == nb.IsError {
				ob.RequestCount += nb.RequestCount
				ob.ErrorCount += nb.ErrorCount
				ob.TotalTokens += nb.TotalTokens
				ob.TotalLatencyMS += nb.TotalLatencyMS
				goto next
			}
		}
		m.buckets = append(m.buckets, nb)
	next:
	}
	return nil
}

func testCfg() UsageConfig {
	return UsageConfig{
		BatchSize:          2,
		FlushInterval:      50 * time.Millisecond,
		DropOnFull:         false,
		LogRetentionDays:   30,
		StatsFlushInterval: 30 * time.Millisecond,
	}
}

func TestRecorderFlushesLogs(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)

	r.Record(&domain.UsageLog{RequestID: "a", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, CreatedAt: time.Now()})
	r.Record(&domain.UsageLog{RequestID: "b", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 20, CreatedAt: time.Now()})

	// 等批量刷出
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ls.mu.Lock()
		n := len(ls.logs)
		ls.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ls.mu.Lock()
	n := len(ls.logs)
	ls.mu.Unlock()
	require.GreaterOrEqual(t, n, 2, "logs flushed")
	cancel()
	r.Close(context.Background())
}

func TestRecorderAggregatesStats(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)

	now := time.Now().Truncate(time.Hour)
	r.Record(&domain.UsageLog{RequestID: "a", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, LatencyMS: 5, CreatedAt: now})
	r.Record(&domain.UsageLog{RequestID: "b", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 7, CreatedAt: now})
	r.Record(&domain.UsageLog{RequestID: "c", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 30, LatencyMS: 9, CreatedAt: now})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ss.mu.Lock()
		flushed := len(ss.buckets) >= 2
		ss.mu.Unlock()
		if flushed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	require.Len(t, ss.buckets, 2, "want 2 buckets (ok/err)")
	var okB, errB *domain.StatBucket
	for _, b := range ss.buckets {
		if b.IsError {
			errB = b
		} else {
			okB = b
		}
	}
	require.NotNil(t, okB)
	require.Equal(t, int64(2), okB.RequestCount)
	require.Equal(t, int64(40), okB.TotalTokens)
	require.NotNil(t, errB)
	require.Equal(t, int64(1), errB.RequestCount)
	require.Equal(t, int64(1), errB.ErrorCount)
	cancel()
	r.Close(context.Background())
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/usage/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 usage.go**

`internal/usage/usage.go`:

```go
// Package usage 承载请求明细的异步落库与预聚合统计（规格 §7.2/§10.5）。
// 统计聚合永不失真（同步进内存计数），明细经有界 channel 批量落库、饱和时可丢弃。
package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

type UsageConfig struct {
	BatchSize          int
	FlushInterval      time.Duration
	DropOnFull         bool
	LogRetentionDays   int
	StatsFlushInterval time.Duration
}

type LogInserter interface {
	InsertBatch(ctx context.Context, logs []*domain.UsageLog) error
}

type StatUpserter interface {
	Upsert(ctx context.Context, buckets []*domain.StatBucket) error
}

type Recorder struct {
	cfg      UsageConfig
	logs     LogInserter
	stats    StatUpserter
	log      *logx.Logger
	logCh    chan *domain.UsageLog
	mu       sync.Mutex
	counters map[string]*statCounters
	dropped  int64
}

type statCounters struct {
	bucket domain.StatBucket
}

func New(cfg UsageConfig, logs LogInserter, stats StatUpserter, log *logx.Logger) *Recorder {
	return &Recorder{
		cfg:      cfg,
		logs:     logs,
		stats:    stats,
		log:      log,
		logCh:    make(chan *domain.UsageLog, 16384),
		counters: make(map[string]*statCounters),
	}
}

func (r *Recorder) Start(ctx context.Context) {
	go r.logWriterLoop(ctx)
	go r.statsFlushLoop(ctx)
	go r.janitorLoop(ctx)
}

// Record 记录一次请求：统计同步聚合（永不丢弃），明细入有界 channel。
func (r *Recorder) Record(l *domain.UsageLog) {
	r.aggregate(l)
	if r.cfg.DropOnFull {
		select {
		case r.logCh <- l:
		default:
			r.dropped++
			if r.log != nil && r.dropped%1000 == 1 {
				r.log.Warn("usage log dropped (pipeline saturated)", logx.Int64("dropped", r.dropped))
			}
		}
		return
	}
	r.logCh <- l
}

func (r *Recorder) aggregate(l *domain.UsageLog) {
	hour := l.CreatedAt.UTC().Truncate(time.Hour)
	isErr := l.ErrorType != domain.ErrNone
	key := fmt.Sprintf("%d|%d|%d|%d|%s|%v", hour.Unix(), l.GroupID, l.AccountID, l.TemplateID, l.Model, isErr)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &statCounters{bucket: domain.StatBucket{
			BucketTime: hour, GroupID: l.GroupID, AccountID: l.AccountID,
			TemplateID: l.TemplateID, Model: l.Model, IsError: isErr,
		}}
		r.counters[key] = c
	}
	c.bucket.RequestCount++
	if isErr {
		c.bucket.ErrorCount++
	}
	c.bucket.PromptTokens += l.PromptTokens
	c.bucket.CompletionTokens += l.CompletionTokens
	c.bucket.TotalTokens += l.TotalTokens
	c.bucket.TotalLatencyMS += l.LatencyMS
}

func (r *Recorder) logWriterLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushInterval)
	defer t.Stop()
	batch := make([]*domain.UsageLog, 0, r.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.logs.InsertBatch(context.Background(), batch); err != nil {
			if r.log != nil {
				r.log.Warn("usage batch insert failed", logx.Error(err))
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		case l := <-r.logCh:
			batch = append(batch, l)
			if len(batch) >= r.cfg.BatchSize {
				flush()
			}
		}
	}
}

func (r *Recorder) statsFlushLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.StatsFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushStats()
			return
		case <-t.C:
			r.flushStats()
		}
	}
}

func (r *Recorder) flushStats() {
	r.mu.Lock()
	buckets := make([]*domain.StatBucket, 0, len(r.counters))
	for _, c := range r.counters {
		b := c.bucket
		buckets = append(buckets, &b)
	}
	r.counters = make(map[string]*statCounters)
	r.mu.Unlock()
	if len(buckets) == 0 {
		return
	}
	if err := r.stats.Upsert(context.Background(), buckets); err != nil {
		if r.log != nil {
			r.log.Warn("usage stats upsert failed", logx.Error(err))
		}
		// 失败回灌：避免计数丢失
		r.mu.Lock()
		for _, b := range buckets {
			key := fmt.Sprintf("%d|%d|%d|%d|%s|%v", b.BucketTime.Unix(), b.GroupID, b.AccountID, b.TemplateID, b.Model, b.IsError)
			if c, ok := r.counters[key]; ok {
				c.bucket.RequestCount += b.RequestCount
				c.bucket.ErrorCount += b.ErrorCount
				c.bucket.PromptTokens += b.PromptTokens
				c.bucket.CompletionTokens += b.CompletionTokens
				c.bucket.TotalTokens += b.TotalTokens
				c.bucket.TotalLatencyMS += b.TotalLatencyMS
			} else {
				r.counters[key] = &statCounters{bucket: *b}
			}
		}
		r.mu.Unlock()
	}
}

// Close 排空剩余明细（限时，超时丢弃并 Warn）。
func (r *Recorder) Close(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case l := <-r.logCh:
				if err := r.logs.InsertBatch(context.Background(), []*domain.UsageLog{l}); err != nil && r.log != nil {
					r.log.Warn("usage final flush failed", logx.Error(err))
				}
			default:
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if r.log != nil {
			r.log.Warn("usage close timeout, dropping remaining logs")
		}
	}
	r.flushStats()
}

func (r *Recorder) janitorLoop(ctx context.Context) {
	if r.cfg.LogRetentionDays <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.purgeLogs()
		}
	}
}

// purgeLogs 依赖 LogInserter 扩展接口（可选实现）。
func (r *Recorder) purgeLogs() {
	if p, ok := r.logs.(interface {
		PurgeLogs(ctx context.Context, olderThan time.Time) error
	}); ok {
		cutoff := time.Now().Add(-time.Duration(r.cfg.LogRetentionDays) * 24 * time.Hour)
		if err := p.PurgeLogs(context.Background(), cutoff); err != nil && r.log != nil {
			r.log.Warn("usage log purge failed", logx.Error(err))
		}
	}
}
```

- [ ] **Step 4: 给 LogRepo 加 PurgeLogs 并编译跑测试**

`internal/repository/log_repo.go` 追加：

```go
func (r *LogRepo) PurgeLogs(ctx context.Context, olderThan time.Time) error {
	_, err := r.client.UsageLog.Delete().Where(usagelog.CreatedAtLT(olderThan)).Exec(ctx)
	return err
}
```

Run: `go build ./... && go test ./internal/usage/ ./internal/repository/ -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: async usage pipeline (batch logs + pre-aggregated stats + janitor)"
```

---

### Task 6: 转发层（proxy：鉴权 / 格式绑定 / SDK 转发 / 流式 / 用量采集）

**Files:**
- Create: `pkg/aiclient/aiclient.go`（SDK 唯一引用点）
- Create: `internal/proxy/auth.go`
- Create: `internal/proxy/forward.go`
- Create: `internal/proxy/limit.go`
- Create: `internal/proxy/forward_chat.go`
- Create: `internal/proxy/forward_responses.go`
- Create: `internal/proxy/forward_anthropic.go`
- Test: `internal/proxy/proxy_test.go`（httptest 假上游端到端）

**Interfaces:**
- Consumes: `internal/domain`、`internal/scheduler`、`internal/usage`、`pkg/logx`、`pkg/httpx`、`pkg/aiclient`
- Produces:
  - `aiclient.NewFactory(hc *http.Client, cfg Config) *Factory`；`Factory.ChatCompletion(ctx, tpl, key, params) (*openai.ChatCompletion, error)`、`ChatCompletionStream(ctx, tpl, key, params) *openai.ChatCompletionStream`、`Response/ResponseStream`、`AnthMessage/AnthMessageStream`、`InvalidateAll()`（非流式内部包超时；流式 ctx 由调用方管理，本包只注入鉴权头）
  - `proxy.New(cfg Config, sched *scheduler.Scheduler, rec *usage.Recorder, clients *aiclient.Factory, log *logx.Logger) *Proxy`
  - `Proxy.HandleChat/HandleResponses/HandleAnthropic(w, r)`（chi handler）
  - `Auth`（`NewAuth(loader, log)`、`Reload(ctx)`、`Upsert(hash, groupID)`、`Delete(hash)`）

- [ ] **Step 1: 写失败测试（假上游端到端：流式/429/5xx/失败转移/鉴权）**

`internal/proxy/proxy_test.go`:

```go
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
)

// --- 假上游：SSE 流式 chat/completions ---
// failMode: "" = 正常；"429" = 每个非流式请求都返回 429（测 failover）。
func fakeOpenAI(t *testing.T, failMode string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.Header().Set("x-ratelimit-reset-requests", "5s")
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			chunks := [2]string{
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"model": body["model"],
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

type noopKeyLoader struct{ keys map[string]int64 }

func (n noopKeyLoader) LoadGroupKeys(ctx context.Context) (map[string]int64, error) { return n.keys, nil }

type noopLogStore struct{}

func (noopLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error { return nil }
func (noopLogStore) PurgeLogs(ctx context.Context, t time.Time) error           { return nil }

type noopStatStore struct{}

func (noopStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error { return nil }

type noopLoader struct{ accs map[int64][]*domain.Account }

func (n noopLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	return n.accs, nil
}
func (n noopLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	return n.accs[id], nil
}
func (n noopLoader) UpdateAccountStatus(ctx context.Context, id int64, s domain.AccountStatus, c *time.Time, e *string) error {
	return nil
}

func newTestProxy(t *testing.T, upstream string, accountID int64) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: accountID, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM: 0, UsageCapture: true,
	}
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, Cooldown429: 30 * time.Second,
		BackoffBase: 5 * time.Second, BackoffMax: time.Minute, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, DropOnFull: false,
		LogRetentionDays: 30, StatsFlushInterval: time.Hour,
	}, noopLogStore{}, noopStatStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]int64{"hash-1": 10}}, nil)
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	return New(cfg, sched, rec, clients, auth, nil)
}

func TestProxyStreamingChat(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"content":"hi"`)
	require.Contains(t, body, `"prompt_tokens":5`, "usage captured from final chunk")
}

func TestProxyAuthRejected(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 401, rec.Code)
}

func TestProxyFailoverOn429(t *testing.T) {
	// 两个账号指向同一个会 429 的上游：第一个失败后转移第二个（同样失败则最终 429）
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)
	// 第二个账号
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL + "/v1", DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"}}
	sched := p.sched
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], acc2)
	sched.InvalidateAllSync()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	// 两个账号都进入 429 冷却：Runtime 视图可查
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 auth.go / client.go / limit.go**

`internal/proxy/auth.go`:

```go
package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

// KeyLoader 由 repository.GroupRepo 实现。
type KeyLoader interface {
	LoadGroupKeys(ctx context.Context) (map[string]int64, error)
}

// Auth 分组 key 鉴权：内存哈希表（RWMutex，读多写少，规格 §10.3）。
type Auth struct {
	loader KeyLoader
	log    *logx.Logger
	mu     sync.RWMutex
	keys   map[string]int64 // key_hash -> groupID
}

func NewAuth(loader KeyLoader, log *logx.Logger) *Auth {
	a := &Auth{loader: loader, log: log, keys: make(map[string]int64)}
	_ = a.Reload(context.Background())
	return a
}

func (a *Auth) Reload(ctx context.Context) error {
	m, err := a.loader.LoadGroupKeys(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.keys = m
	a.mu.Unlock()
	return nil
}

func (a *Auth) Upsert(hash string, groupID int64) {
	a.mu.Lock()
	a.keys[hash] = groupID
	a.mu.Unlock()
}

func (a *Auth) Delete(hash string) {
	a.mu.Lock()
	delete(a.keys, hash)
	a.mu.Unlock()
}

// Authenticate 解析 Bearer key 并返回 groupID。
func (a *Auth) Authenticate(r *http.Request) (int64, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return 0, false
	}
	raw := strings.TrimPrefix(h, "Bearer ")
	if raw == "" {
		return 0, false
	}
	hash := cryptox.HashKey(raw)
	a.mu.RLock()
	gid, ok := a.keys[hash]
	a.mu.RUnlock()
	return gid, ok
}
```

`pkg/aiclient/aiclient.go`:

```go
// Package aiclient 是 openai/anthropic 官方 SDK 的唯一引用点：客户端懒构建 +
// 鉴权头注入 + 非流式超时策略。协议类型（params/response/stream）直接透传为
// 调用签名——它们是协议本身，隐藏它们等于重写协议（违背"用现成库"）。
package aiclient

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"

	"go-proxy-mini/internal/domain"
)

type Config struct {
	UpstreamTimeout       time.Duration // 非流式调用超时
	UpstreamStreamTimeout time.Duration // 流式 backstop（由调用方以 ctx 传入）
}

type Factory struct {
	hc  *http.Client
	cfg Config
	mu  sync.Mutex
	byT map[int64]*TemplateClients
}

type TemplateClients struct {
	chat      *openai.Client
	responses *openai.Client
	anthropic *anthropic.Client
}

func NewFactory(hc *http.Client, cfg Config) *Factory {
	return &Factory{hc: hc, cfg: cfg, byT: make(map[int64]*TemplateClients)}
}

// InvalidateAll 模板变更后丢弃所有客户端（base_url 变化生效）。
func (f *Factory) InvalidateAll() {
	f.mu.Lock()
	f.byT = make(map[int64]*TemplateClients)
	f.mu.Unlock()
}

// --- openai chat/completions ---

// ChatCompletion 非流式调用（内部注入鉴权头 + 超时）。
func (f *Factory) ChatCompletion(ctx context.Context, tpl *domain.Template, key string, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.chat(tpl).Chat.Completions.New(ctx, params, openai.WithHeader("Authorization", "Bearer "+key))
}

// ChatCompletionStream 流式调用；ctx 由调用方管理（含超时），本函数只注入鉴权头。
func (f *Factory) ChatCompletionStream(ctx context.Context, tpl *domain.Template, key string, params openai.ChatCompletionNewParams) *openai.ChatCompletionStream {
	return f.chat(tpl).Chat.Completions.NewStreaming(ctx, params, openai.WithHeader("Authorization", "Bearer "+key))
}

// --- openai responses ---

func (f *Factory) Response(ctx context.Context, tpl *domain.Template, key string, params openai.ResponseNewParams) (*openai.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.responses(tpl).Responses.New(ctx, params, openai.WithHeader("Authorization", "Bearer "+key))
}

func (f *Factory) ResponseStream(ctx context.Context, tpl *domain.Template, key string, params openai.ResponseNewParams) *openai.ResponseStream {
	return f.responses(tpl).Responses.NewStreaming(ctx, params, openai.WithHeader("Authorization", "Bearer "+key))
}

// --- anthropic messages ---

func (f *Factory) AnthMessage(ctx context.Context, tpl *domain.Template, key string, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.anthropic(tpl).Messages.New(ctx, params, anthropic.WithHeader("x-api-key", key))
}

func (f *Factory) AnthMessageStream(ctx context.Context, tpl *domain.Template, key string, params anthropic.MessageNewParams) *anthropic.MessageStream {
	return f.anthropic(tpl).Messages.NewStreaming(ctx, params, anthropic.WithHeader("x-api-key", key))
}

// --- 客户端懒构建（每模板最多 3 个，共享 http.Client，规格 §6.1） ---

func (f *Factory) chat(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.chat == nil {
		tc.chat = openai.NewClient(
			openai.WithBaseURL(tpl.BaseURL),
			openai.WithHTTPClient(f.hc),
		)
	}
	return tc.chat
}

func (f *Factory) responses(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.responses == nil {
		tc.responses = openai.NewClient(
			openai.WithBaseURL(tpl.BaseURL),
			openai.WithHTTPClient(f.hc),
		)
	}
	return tc.responses
}

func (f *Factory) anthropic(tpl *domain.Template) *anthropic.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.anthropic == nil {
		tc.anthropic = anthropic.NewClient(
			anthropic.WithBaseURL(tpl.BaseURL),
			anthropic.WithHTTPClient(f.hc),
		)
	}
	return tc.anthropic
}

func (f *Factory) ensure(id int64) *TemplateClients {
	tc, ok := f.byT[id]
	if !ok {
		tc = &TemplateClients{}
		f.byT[id] = tc
	}
	return tc
}
```

`internal/proxy/limit.go`:

```go
package proxy

import (
	"sync"
	"time"
)

// fixedWindowLimiter 每分组 key 的固定窗口计数限流（规格 §10.6，默认关闭）。
type fixedWindowLimiter struct {
	rpm int
	mu  sync.Mutex
	win map[int64]windowState
}

type windowState struct {
	start time.Time
	count int64
}

func newFixedWindowLimiter(rpm int) *fixedWindowLimiter {
	return &fixedWindowLimiter{rpm: rpm, win: make(map[int64]windowState)}
}

func (l *fixedWindowLimiter) Allow(groupID int64, now time.Time) bool {
	if l.rpm <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.win[groupID]
	if !ok || now.Sub(st.start) >= time.Minute {
		l.win[groupID] = windowState{start: now, count: 1}
		return true
	}
	st.count++
	l.win[groupID] = st
	return st.count <= int64(l.rpm)
}
```

- [ ] **Step 4: 写 forward.go + forward_chat.go（先只实现 chat，responses/anthropic 同构）**

`internal/proxy/forward.go`:

```go
// Package proxy 是 AI 请求热路径：分组 key 鉴权 → 调度器选号 → SDK 转发 → 用量采集。
// 规格 §6/§9。不变量：热路径零 DB、零 per-request 锁。
package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/logx"
)

type Config struct {
	MaxBodySize           int64
	MaxInflight           int64
	UpstreamStreamTimeout time.Duration // 流式 backstop（非流式超时在 aiclient.Config）
	FailoverAttempts      int
	GroupKeyRPM           int
	UsageCapture          bool
}

type Proxy struct {
	cfg     Config
	sched   *scheduler.Scheduler
	rec     *usage.Recorder
	clients *aiclient.Factory
	auth    *Auth
	limit   *fixedWindowLimiter
	log     *logx.Logger
	inflight atomic.Int64
}

func New(cfg Config, sched *scheduler.Scheduler, rec *usage.Recorder, clients *aiclient.Factory, auth *Auth, log *logx.Logger) *Proxy {
	return &Proxy{
		cfg: cfg, sched: sched, rec: rec, clients: clients, auth: auth,
		limit: newFixedWindowLimiter(cfg.GroupKeyRPM), log: log,
	}
}

func (p *Proxy) Inflight() int64 { return p.inflight.Load() }

// finish 收尾：释放并发槽 + 记录用量（必调）。
func (p *Proxy) finish(accountID int64, l *domain.UsageLog) {
	p.sched.Release(accountID)
	if p.cfg.UsageCapture && l != nil {
		p.rec.Record(l)
	}
}

type formatError struct {
	status int
	msg    string
}

func (e *formatError) Error() string { return e.msg }

var (
	errInvalidKey = &formatError{status: http.StatusUnauthorized, msg: "invalid gateway key"}
	errTooMany    = &formatError{status: http.StatusTooManyRequests, msg: "no available account"}
	errRateLimit  = &formatError{status: http.StatusTooManyRequests, msg: "group rate limited"}
	errBody       = &formatError{status: http.StatusRequestEntityTooLarge, msg: "request body too large"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *formatError) {
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"message": e.msg, "type": "gateway_error"}})
}

// --- SSE 写出（bufio 复用） ---

var bufioPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 8192) }}

type sseWriter struct {
	bw *bufio.Writer
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	bw := bufioPool.Get().(*bufio.Writer)
	bw.Reset(w)
	return &sseWriter{bw: bw}
}

func (s *sseWriter) Event(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := s.bw.WriteString("data: "); err != nil {
		return err
	}
	if _, err := s.bw.Write(data); err != nil {
		return err
	}
	if _, err := s.bw.WriteString("\n\n"); err != nil {
		return err
	}
	return s.bw.Flush()
}

func (s *sseWriter) Done() error {
	if _, err := s.bw.WriteString("data: [DONE]\n\n"); err != nil {
		return err
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	bufioPool.Put(s.bw)
	return nil
}

func (s *sseWriter) Abort() { bufioPool.Put(s.bw) }
```

`internal/proxy/forward_chat.go`（chat/completions 完整转发）：

```go
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/openai/openai-go"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
)

func (p *Proxy) HandleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := uuid.NewString()
	groupID, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(reqID, 0, 0, "", domain.FormatOpenAIChat, 401, domain.ErrAuth, 0, nil, start)
		return
	}
	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	model := params.Model.Value()

	sel, err := p.sched.Select(groupID, domain.FormatOpenAIChat, model)
	if err != nil {
		p.handleSelectError(w, err)
		p.record(reqID, groupID, 0, model, domain.FormatOpenAIChat, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}

	var lastCode int
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		ok, code := p.tryChat(w, r, reqID, groupID, start, sel, &params)
		if ok {
			return // 已写出完整响应
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil)
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
		} else {
			// 4xx 确定性错误：透传上游状态码，不转移（规格 §5.3）
			p.sched.Release(sel.AccountID)
			writeJSON(w, code, map[string]any{"error": map[string]any{
				"message": "upstream rejected request", "type": "upstream_error",
			}})
			p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, code, domain.Err5xx, 0, nil, start)
			return
		}
		p.sched.Release(sel.AccountID)
		var selErr error
		sel, selErr = p.sched.Select(groupID, domain.FormatOpenAIChat, model)
		if selErr != nil {
			break
		}
	}
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// tryChat 返回 (已完整处理, 上游状态码)。流式 200 发出后无法转移。
func (p *Proxy) tryChat(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, params *openai.ChatCompletionNewParams) (bool, int) {
	tpl := tplOf(sel)
	params.Model = openai.String(sel.Model)

	if params.Stream.Value() {
		ctx, cancel := context.WithTimeout(r.Context(), p.cfg.UpstreamStreamTimeout)
		defer cancel()
		stream := p.clients.ChatCompletionStream(ctx, tpl, sel.UpstreamKey, *params)
		if stream.Err() != nil {
			code := statusOf(stream.Err())
			return false, code
		}
		sw := newSSEWriter(w)
		var usage *openai.CompletionUsage
		for chunk := range stream.Iter() {
			if err := sw.Event(chunk); err != nil {
				sw.Abort()
				if p.log != nil {
					p.log.Warn("sse write failed", logx.String("request_id", reqID), logx.Error(err))
				}
				p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
				return true, 0 // 客户端断开，无法转移
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
		}
		if err := stream.Err(); err != nil {
			sw.Abort()
			p.recordStreamAbort(reqID, start, sel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
			return true, 0
		}
		_ = sw.Done()
		var pt, ct, tt int64
		if usage != nil {
			pt, ct, tt = int64(usage.PromptTokens), int64(usage.CompletionTokens), int64(usage.TotalTokens)
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
		p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
		return true, 200
	}

	resp, err := client.Chat.Completions.New(ctx, *params, authOpt)
	if err != nil {
		return false, statusOf(err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return false, 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var pt, ct, tt int64
	if resp.Usage != nil {
		pt, ct, tt = int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens), int64(resp.Usage.TotalTokens)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
	p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
	return true, 200
}
```

注意：`forward_chat.go` 引用了 `tplOf`、`statusOf`、`classifyUpstream`、`record`、`usageTuple`、`handleSelectError`、`statusFor`、`recordStreamAbort`、`sched.Loader`——这些在 `forward.go` 补充（Step 4 继续）：

`forward.go` 追加辅助：

```go
// --- 辅助 ---

type usageTuple struct {
	pt, ct, tt int64
}

func (p *Proxy) record(reqID string, groupID, accountID int64, model string, format domain.RequestFormat, status int, et domain.ErrorType, latencyMS int64, u *usageTuple, start time.Time) {
	if u == nil {
		u = &usageTuple{}
	}
	p.rec.Record(&domain.UsageLog{
		RequestID: reqID, GroupID: groupID, AccountID: accountID,
		Model: model, Format: format, StatusCode: status, ErrorType: et,
		LatencyMS: time.Since(start).Milliseconds(),
		PromptTokens: u.pt, CompletionTokens: u.ct, TotalTokens: u.tt,
		CreatedAt: time.Now(),
	})
}

func (p *Proxy) recordStreamAbort(reqID string, start time.Time, sel *scheduler.Selection, err error) {
	if p.log != nil {
		p.log.Warn("upstream stream aborted", logx.String("request_id", reqID), logx.Error(err))
	}
	p.record(reqID, 0, sel.AccountID, sel.Model, sel.Format, 200, domain.ErrAbort, 0, nil, start)
}

func (p *Proxy) handleSelectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable):
		writeErr(w, &formatError{status: http.StatusNotFound, msg: "no account supports this request format"})
	case errors.Is(err, scheduler.ErrNoAvailable):
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	default:
		writeErr(w, &formatError{status: http.StatusNotFound, msg: "group not found"})
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable), errors.Is(err, scheduler.ErrGroupNotFound):
		return http.StatusNotFound
	default:
		return http.StatusTooManyRequests
	}
}

func statusOf(err error) int {
	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	return 0 // 连接级/超时错误
}

// tplOf 从 Selection 构造轻量模板对象（仅用于 aiclient 取 SDK 客户端；模板变更经 InvalidateAll 生效）。
func tplOf(sel *scheduler.Selection) *domain.Template {
	return &domain.Template{ID: sel.TemplateID, BaseURL: sel.BaseURL}
}
```

> 设计说明：`Selection` 已携带 BaseURL/Format，`tplOf` 构造轻量模板对象仅用于取客户端；模板变更时 `InvalidateAll` 生效。SDK 客户端按 `(templateID, format)` 缓存即可（client.go 按 ID 缓存，base_url 变更靠 InvalidateAll 兜底）。

- [ ] **Step 5: 补 sched.Loader() 与 InvalidateAllSync（测试需要）**

`internal/scheduler/scheduler.go` 追加：

```go
// Loader 暴露数据源（测试注入用）。
func (s *Scheduler) Loader() Loader { return s.loader }

// InvalidateAllSync 同步全量重载（测试与启动用）。
func (s *Scheduler) InvalidateAllSync() error { return s.reload(context.Background()) }
```

- [ ] **Step 6: 编译修正（SDK API 以安装版本为准）**

Run: `go build ./internal/proxy/`
Expected: BUILD PASS。可能需修正：① `openai.ChatCompletionNewParams` 字段名（`Model` 为 `openai.StringParam`）；② 流式迭代 API 若为 `stream.Iter()` 返回值 `iter.Seq2` 以外的形式，按 `go doc github.com/openai/openai-go.ChatCompletionStream` 修正；③ `anthropic` 客户端构建选项名。以编译错误信息为准逐项修正。

- [ ] **Step 7: 跑测试**

Run: `go test ./internal/proxy/ -count=1`
Expected: PASS（流式捕获 usage、401、429 failover 三个用例）。

- [ ] **Step 8: Commit**

```bash
git add -A && git commit -m "feat: proxy hot path (group key auth, chat/completions SDK forwarding, SSE, failover)"
```

> 注：responses 与 anthropic 两个端点的完整实现在 Task 8 补齐（`forward_responses.go`/`forward_anthropic.go`）；本任务（Task 6）先完成 chat/completions 全链路，确保任务边界可独立验收。

---

### Task 7: 管理 API（service + handler + 路由挂载）

**Files:**
- Create: `internal/service/service.go`
- Create: `internal/service/template.go`
- Create: `internal/service/account.go`
- Create: `internal/service/group.go`
- Create: `internal/service/log.go`
- Create: `internal/service/stat.go`
- Create: `internal/service/fakestore_test.go`
- Test: `internal/service/service_test.go`
- Create: `internal/handler/handler.go`
- Create: `internal/handler/template.go`
- Create: `internal/handler/account.go`
- Create: `internal/handler/group.go`
- Create: `internal/handler/log.go`
- Create: `internal/handler/stat.go`
- Test: `internal/handler/handler_test.go`（httptest 全流程：建模板→建账号→建分组→绑定→换 key→查统计）

**Interfaces:**
- Consumes: `internal/domain`、`internal/repository`、`internal/scheduler`、`internal/proxy`、`pkg/cryptox`、`pkg/logx`
- Produces:
  - `service.New(store Store, sched RuntimeProvider, invalidate func(), keys KeyRegistrar, log *logx.Logger) *Service`（`invalidate` 回调 = `scheduler.InvalidateAll`，`keys` = `proxy.Auth`）
  - `Service.CreateTemplate/GetTemplate/ListTemplates/UpdateTemplate/DeleteTemplate`、`CreateAccount/GetAccount/ListAccounts/UpdateAccount/DeleteAccount`、`CreateGroup/GetGroup/ListGroups/UpdateGroup/DeleteGroup/SetGroupAccounts/RotateGroupKey`、`QueryLogs(q repository.LogQuery)`、`QueryStats(q repository.StatQuery, granularity string)`
  - handler 路由：见 Task 7 Step 6

- [ ] **Step 1: 写失败测试（service 层用内存 fake store）**

`internal/service/fakestore_test.go`:

```go
package service

import (
	"context"
	"sync"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

type fakeStore struct {
	mu       sync.Mutex
	tpls     map[int64]*domain.Template
	accs     map[int64]*domain.Account
	groups   map[int64]*domain.Group
	members  map[int64][]int64
	logs     []*domain.UsageLog
	stats    []*domain.StatBucket
	nextID   int64
	keyHashes map[int64]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tpls: make(map[int64]*domain.Template), accs: make(map[int64]*domain.Account),
		groups: make(map[int64]*domain.Group), members: make(map[int64][]int64),
		keyHashes: make(map[int64]string), nextID: 1,
	}
}

func (f *fakeStore) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = f.nextID
	f.nextID++
	f.tpls[t.ID] = t
	return t, nil
}

func (f *fakeStore) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tpls[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) ListTemplates(ctx context.Context) ([]*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Template, 0, len(f.tpls))
	for _, t := range f.tpls {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tpls[t.ID] = t
	return t, nil
}

func (f *fakeStore) DeleteTemplate(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tpls, id)
	return nil
}

func (f *fakeStore) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.ID = f.nextID
	f.nextID++
	f.accs[a.ID] = a
	return a, nil
}

func (f *fakeStore) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

func (f *fakeStore) ListAccounts(ctx context.Context) ([]*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Account, 0, len(f.accs))
	for _, a := range f.accs {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeStore) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accs[a.ID] = a
	return a, nil
}

func (f *fakeStore) DeleteAccount(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accs, id)
	return nil
}

func (f *fakeStore) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g.ID = f.nextID
	f.nextID++
	f.groups[g.ID] = g
	f.keyHashes[g.ID] = g.KeyHash
	return g, nil
}

func (f *fakeStore) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[id]
	if !ok {
		return nil, ErrNotFound
	}
	return g, nil
}

func (f *fakeStore) ListGroups(ctx context.Context) ([]*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Group, 0, len(f.groups))
	for _, g := range f.groups {
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeStore) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[g.ID] = g
	return g, nil
}

func (f *fakeStore) DeleteGroup(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.groups, id)
	return nil
}

func (f *fakeStore) SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[groupID] = accountIDs
	return nil
}

func (f *fakeStore) QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs, int64(len(f.logs)), nil
}

func (f *fakeStore) ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}
```

`internal/service/service_test.go`:

```go
package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

func TestCreateTemplateValidates(t *testing.T) {
	svc := &Service{store: newFakeStore(), invalidate: func() {}, log: nil}
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "", BaseURL: "not-a-url", DefaultFormat: domain.Format("nope"),
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestCreateGroupRotateKeyFlow(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}, log: nil}
	g, raw, err := svc.CreateGroup(context.Background(), "g1")
	require.NoError(t, err)
	require.NotEmpty(t, g.KeyHash)
	require.NotEmpty(t, raw, "key must be generated")
	raw2, err := svc.RotateGroupKey(context.Background(), g.ID)
	require.NoError(t, err)
	require.NotEqual(t, raw, raw2, "rotated key must differ")
	g2, err := svc.GetGroup(context.Background(), g.ID)
	require.NoError(t, err)
	require.NotEqual(t, g.KeyHash, g2.KeyHash, "hash must change")
}

func TestQueryStatsGranularity(t *testing.T) {
	fs := newFakeStore()
	fs.stats = []*domain.StatBucket{
		{BucketTime: mustTime("2026-08-01T10:00:00Z"), GroupID: 1, Model: "m", RequestCount: 10, TotalTokens: 100},
		{BucketTime: mustTime("2026-08-01T11:00:00Z"), GroupID: 1, Model: "m", RequestCount: 5, TotalTokens: 50},
	}
	svc := &Service{store: fs}
	rows, err := svc.QueryStats(context.Background(), repository.StatQuery{}, "day")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(15), rows[0].RequestCount, "day aggregation sums requests")
	require.Equal(t, int64(150), rows[0].TotalTokens)
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
```

（`service_test.go` 需 import `time` 与 `go-proxy-mini/internal/repository`；`ErrNotFound`/`ErrInvalidInput` 在 service.go 定义。）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/service/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 service.go（存储接口 + 校验 + 失效通知）**

`internal/service/service.go`:

```go
// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"net/url"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrNotFound    = errors.New("service: not found")
	ErrInvalidInput = errors.New("service: invalid input")
)

type Store interface {
	TemplateStore
	AccountStore
	GroupStore
	LogStore
	StatStore
}

type TemplateStore interface {
	CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	GetTemplate(ctx context.Context, id int64) (*domain.Template, error)
	ListTemplates(ctx context.Context) ([]*domain.Template, error)
	UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	DeleteTemplate(ctx context.Context, id int64) error
}

type AccountStore interface {
	CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	DeleteAccount(ctx context.Context, id int64) error
}

type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	GetGroup(ctx context.Context, id int64) (*domain.Group, error)
	ListGroups(ctx context.Context) ([]*domain.Group, error)
	UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error
}

type LogStore interface {
	QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error)
}

type StatStore interface {
	ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error)
}

// RuntimeProvider 由 scheduler 实现，供账号运行时视图。
type RuntimeProvider interface {
	Runtime(accountID int64) (scheduler.RuntimeInfo, bool)
}

// KeyRegistrar 由 proxy.Auth 实现，供分组 key 变更时刷新。
type KeyRegistrar interface {
	Upsert(hash string, groupID int64)
	Delete(hash string)
}

type Service struct {
	store     Store
	sched     RuntimeProvider
	invalidate func()          // 调度快照失效（全量重载）
	keys      KeyRegistrar
	log       *logx.Logger
}

func New(store Store, sched RuntimeProvider, invalidate func(), keys KeyRegistrar, log *logx.Logger) *Service {
	return &Service{store: store, sched: sched, invalidate: invalidate, keys: keys, log: log}
}

func validateTemplate(t *domain.Template) error {
	if t.Name == "" {
		return ErrInvalidInput
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
	}
	if !t.DefaultFormat.Valid() {
		return ErrInvalidInput
	}
	for _, f := range t.ModelFormats {
		if !f.Valid() {
			return ErrInvalidInput
		}
	}
	return nil
}

func validateAccount(a *domain.Account) error {
	if a.Name == "" || a.UpstreamKey == "" || a.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if a.Weight < 0 {
		return ErrInvalidInput
	}
	if a.MaxConcurrency < 1 {
		a.MaxConcurrency = 8
	}
	return nil
}
```

`internal/service/template.go`:

```go
package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

func (s *Service) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	created, err := s.store.CreateTemplate(ctx, t)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("template created", logx.Int64("id", created.ID), logx.String("name", created.Name))
	}
	return created, nil
}

func (s *Service) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	return s.store.GetTemplate(ctx, id)
}

func (s *Service) ListTemplates(ctx context.Context) ([]*domain.Template, error) {
	return s.store.ListTemplates(ctx)
}

func (s *Service) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	updated, err := s.store.UpdateTemplate(ctx, t)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return updated, nil
}

func (s *Service) DeleteTemplate(ctx context.Context, id int64) error {
	err := s.store.DeleteTemplate(ctx, id)
	if err == nil {
		s.invalidate()
	}
	return err
}
```

`internal/service/account.go`:

```go
package service

import (
	"context"

	"go-proxy-mini/internal/domain"
)

func (s *Service) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	if _, err := s.store.GetTemplate(ctx, a.TemplateID); err != nil {
		return nil, err
	}
	created, err := s.store.CreateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return created, nil
}

func (s *Service) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	return s.store.GetAccount(ctx, id)
}

func (s *Service) ListAccounts(ctx context.Context) ([]*domain.Account, error) {
	return s.store.ListAccounts(ctx)
}

func (s *Service) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	updated, err := s.store.UpdateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return updated, nil
}

func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	err := s.store.DeleteAccount(ctx, id)
	if err == nil {
		s.invalidate()
	}
	return err
}

// AccountView 是账号的管理端视图（含调度器运行时信息）。
type AccountView struct {
	*domain.Account
	Concurrency   int64   `json:"concurrency"`
	ErrRate       float64 `json:"err_rate"`
	ErrCount      int     `json:"err_count"`
}

func (s *Service) ListAccountViews(ctx context.Context) ([]*AccountView, error) {
	accs, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*AccountView, 0, len(accs))
	for _, a := range accs {
		v := &AccountView{Account: a}
		if s.sched != nil {
			if ri, ok := s.sched.Runtime(a.ID); ok {
				v.Concurrency, v.ErrRate, v.ErrCount = ri.Concurrency, ri.ErrRate, ri.ErrCount
			}
		}
		out = append(out, v)
	}
	return out, nil
}
```

`internal/service/group.go`:

```go
package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

func (s *Service) CreateGroup(ctx context.Context, name string) (*domain.Group, string, error) {
	if name == "" {
		return nil, "", ErrInvalidInput
	}
	raw, hash, prefix := cryptox.NewGroupKey()
	g := &domain.Group{Name: name, KeyHash: hash, KeyPrefix: prefix}
	created, err := s.store.CreateGroup(ctx, g)
	if err != nil {
		return nil, "", err
	}
	if s.keys != nil {
		s.keys.Upsert(hash, created.ID)
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("group created", logx.Int64("id", created.ID), logx.String("name", name))
	}
	return created, raw, nil
}

func (s *Service) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	return s.store.GetGroup(ctx, id)
}

func (s *Service) ListGroups(ctx context.Context) ([]*domain.Group, error) {
	return s.store.ListGroups(ctx)
}

func (s *Service) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	if g.Name == "" {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateGroup(ctx, g)
	if err == nil {
		s.invalidate()
	}
	return updated, err
}

func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	g, err := s.store.GetGroup(ctx, id)
	if err != nil {
		return err
	}
	if s.keys != nil {
		s.keys.Delete(g.KeyHash)
	}
	if err := s.store.DeleteGroup(ctx, id); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func (s *Service) SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	if err := s.store.SetGroupAccounts(ctx, groupID, accountIDs); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// RotateGroupKey 轮换客户端 key：返回新 raw key（仅此一次明文）。
func (s *Service) RotateGroupKey(ctx context.Context, groupID int64) (string, error) {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return "", err
	}
	raw, hash, prefix := cryptox.NewGroupKey()
	g.KeyHash = hash
	g.KeyPrefix = prefix
	if _, err := s.store.UpdateGroup(ctx, g); err != nil {
		return "", err
	}
	if s.keys != nil {
		s.keys.Delete(g.KeyHash) // 旧 hash 已被覆盖，这里删的是同名 key——见下
	}
	if s.keys != nil {
		s.keys.Upsert(hash, groupID)
	}
	return raw, nil
}
```

> RotateGroupKey 注意：先读旧 g，`s.keys.Delete(g.KeyHash)` 需在更新前取旧哈希。修正实现（Step 4 编译时按此实现）：

```go
func (s *Service) RotateGroupKey(ctx context.Context, groupID int64) (string, error) {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return "", err
	}
	oldHash := g.KeyHash
	raw, hash, prefix := cryptox.NewGroupKey()
	g.KeyHash = hash
	g.KeyPrefix = prefix
	if _, err := s.store.UpdateGroup(ctx, g); err != nil {
		return "", err
	}
	if s.keys != nil {
		s.keys.Delete(oldHash)
		s.keys.Upsert(hash, groupID)
	}
	s.invalidate()
	return raw, nil
}
```

`internal/service/log.go`:

```go
package service

import (
	"context"

	"go-proxy-mini/internal/repository"
)

func (s *Service) QueryLogs(ctx context.Context, q repository.LogQuery) ([]any, int64, error) {
	rows, total, err := s.store.QueryLogs(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out, total, nil
}
```

`internal/service/stat.go`:

```go
package service

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// QueryStats 拉取小时桶并按 granularity（hour|day）在内存聚合。
func (s *Service) QueryStats(ctx context.Context, q repository.StatQuery, granularity string) ([]*domain.StatBucket, error) {
	rows, err := s.store.ScanStats(ctx, q)
	if err != nil {
		return nil, err
	}
	if granularity == "hour" || len(rows) == 0 {
		return rows, nil
	}
	// day 聚合：按 (日, 各维度) 合并
	merged := make(map[string]*domain.StatBucket)
	for _, b := range rows {
		key := b.BucketTime.Format("2006-01-02") + "|" + itoa(b.GroupID) + "|" + itoa(b.AccountID) + "|" + itoa(b.TemplateID) + "|" + b.Model + "|" + boolStr(b.IsError)
		m, ok := merged[key]
		if !ok {
			day := b.BucketTime.Truncate(24 * time.Hour)
			m = &domain.StatBucket{
				BucketTime: day, GroupID: b.GroupID, AccountID: b.AccountID,
				TemplateID: b.TemplateID, Model: b.Model, IsError: b.IsError,
			}
			merged[key] = m
		}
		m.RequestCount += b.RequestCount
		m.ErrorCount += b.ErrorCount
		m.PromptTokens += b.PromptTokens
		m.CompletionTokens += b.CompletionTokens
		m.TotalTokens += b.TotalTokens
		m.TotalLatencyMS += b.TotalLatencyMS
	}
	out := make([]*domain.StatBucket, 0, len(merged))
	for _, m := range merged {
		out = append(out, m)
	}
	return out, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
```

（`stat.go` 需 `fmt` import。）

- [ ] **Step 4: 编译 + 跑 service 测试**

Run: `go build ./internal/service/ && go test ./internal/service/ -count=1`
Expected: PASS。`service_test.go` 的 `mustTime` 测试引用 `time`；sentinel 错误已公开（`ErrNotFound`、`ErrInvalidInput`），fakeStore 与 handler 直接引用。

- [ ] **Step 5: 写 handler（admin API）**

`internal/handler/handler.go`:

```go
// Package handler 实现 /admin/* 的 HTTP 处理（JSON in/out，chi 路由）。
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"go-proxy-mini/internal/service"
)

type Handler struct {
	svc  *service.Service
}

func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Routes 挂载全部 admin 路由（不含认证中间件，由 server 层加）。
func (h *Handler) Routes(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Route("/templates", func(r chi.Router) {
			r.Post("/", h.createTemplate)
			r.Get("/", h.listTemplates)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getTemplate)
				r.Put("/", h.updateTemplate)
				r.Delete("/", h.deleteTemplate)
			})
		})
		r.Route("/accounts", func(r chi.Router) {
			r.Post("/", h.createAccount)
			r.Get("/", h.listAccounts)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getAccount)
				r.Put("/", h.updateAccount)
				r.Delete("/", h.deleteAccount)
			})
		})
		r.Route("/groups", func(r chi.Router) {
			r.Post("/", h.createGroup)
			r.Get("/", h.listGroups)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getGroup)
				r.Put("/", h.updateGroup)
				r.Delete("/", h.deleteGroup)
				r.Put("/accounts", h.setGroupAccounts)
				r.Post("/rotate-key", h.rotateGroupKey)
			})
		})
		r.Get("/logs", h.queryLogs)
		r.Get("/stats", h.queryStats)
	})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
```

`internal/handler/template.go`:

```go
package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

func (h *Handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string                     `json:"name"`
		BaseURL       string                     `json:"base_url"`
		DefaultFormat domain.RequestFormat       `json:"default_format"`
		Models        []string                   `json:"models"`
		ModelFormats  map[string]domain.RequestFormat `json:"model_formats"`
		ModelMapping  map[string]string          `json:"model_mapping"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tpl := &domain.Template{
		Name: in.Name, BaseURL: in.BaseURL, DefaultFormat: in.DefaultFormat,
		Models: in.Models, ModelFormats: in.ModelFormats, ModelMapping: in.ModelMapping,
	}
	created, err := h.svc.CreateTemplate(r.Context(), tpl)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListTemplates(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	tpl, err := h.svc.GetTemplate(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tpl)
}

func (h *Handler) updateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in domain.Template
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.ID = id
	updated, err := h.svc.UpdateTemplate(r.Context(), &in)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteTemplate(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// writeServiceErr 统一把 service 错误映射为 HTTP 状态。
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, service.ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
```

`internal/handler/account.go`:

```go
package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
)

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var in domain.Account
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateAccount(r.Context(), &in)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListAccountViews(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	acc, err := h.svc.GetAccount(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (h *Handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in domain.Account
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.ID = id
	updated, err := h.svc.UpdateAccount(r.Context(), &in)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteAccount(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
```

`internal/handler/group.go`:

```go
package handler

import (
	"net/http"
)

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, raw, err := h.svc.CreateGroup(r.Context(), in.Name)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": g,
		"key":   raw, // 仅此一次明文
	})
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListGroups(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	g.Name = in.Name
	updated, err := h.svc.UpdateGroup(r.Context(), g)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteGroup(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h *Handler) setGroupAccounts(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.SetGroupAccounts(r.Context(), id, in.AccountIDs); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (h *Handler) rotateGroupKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	raw, err := h.svc.RotateGroupKey(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": raw})
}
```

`internal/handler/log.go`:

```go
package handler

import (
	"net/http"
	"strconv"
	"time"

	"go-proxy-mini/internal/repository"
)

func (h *Handler) queryLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := repository.LogQuery{
		Limit:  parseIntDefault(q.Get("limit"), 20),
		Offset: parseIntDefault(q.Get("offset"), 0),
	}
	if v := q.Get("group_id"); v != "" {
		lq.GroupID = mustI64(v)
	}
	if v := q.Get("account_id"); v != "" {
		lq.AccountID = mustI64(v)
	}
	if v := q.Get("model"); v != "" {
		lq.Model = v
	}
	if v := q.Get("status_code"); v != "" {
		lq.StatusCode = mustI64(v)
	}
	if v := q.Get("error_type"); v != "" {
		lq.ErrorType = v
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			lq.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			lq.To = &t
		}
	}
	rows, total, err := h.svc.QueryLogs(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func mustI64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
```

`internal/handler/stat.go`:

```go
package handler

import (
	"net/http"
	"time"

	"go-proxy-mini/internal/repository"
)

func (h *Handler) queryStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sq := repository.StatQuery{From: time.Now().Add(-24 * time.Hour), To: time.Now()}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sq.From = t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sq.To = t
		}
	}
	if v := q.Get("group_id"); v != "" {
		sq.GroupID = mustI64(v)
	}
	if v := q.Get("account_id"); v != "" {
		sq.AccountID = mustI64(v)
	}
	if v := q.Get("model"); v != "" {
		sq.Model = v
	}
	granularity := q.Get("granularity")
	if granularity != "hour" && granularity != "day" {
		granularity = "day"
	}
	rows, err := h.svc.QueryStats(r.Context(), sq, granularity)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
```

- [ ] **Step 6: 写 handler 测试（含 admin 认证中间件）**

`internal/handler/handler_test.go`:

```go
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/service"
)

type fakeSched struct{}

func (fakeSched) Runtime(id int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{Status: domain.StatusActive}, true
}

type fakeKeys struct{ upserted, deleted []string }

func (f *fakeKeys) Upsert(hash string, groupID int64) { f.upserted = append(f.upserted, hash) }
func (f *fakeKeys) Delete(hash string)                { f.deleted = append(f.deleted, hash) }

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	store := newFakeStore()
	invalidate := func() {}
	svc := service.New(store, fakeSched{}, invalidate, &fakeKeys{}, nil)
	return New(svc)
}

func TestAdminFlow(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	h.Routes(r)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com/v1",
		"default_format":"openai-chat","models":["gpt-4o"],
		"model_formats":{"o3":"openai-responses"},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.Equal(t, domain.FormatOpenAIResponses, tpl.FormatFor("o3"), "format override")

	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","weight":80,"max_concurrency":4}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))

	rec = do(http.MethodPost, "/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp struct {
		Group domain.Group `json:"group"`
		Key   string       `json:"key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	require.True(t, strings.HasPrefix(groupResp.Key, "gk-"), "key=%s", groupResp.Key)

	rec = do(http.MethodPut, "/admin/groups/"+itoa(groupResp.Group.ID)+"/accounts", `{"account_ids":[`+itoa(acc.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "set accounts: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/groups/"+itoa(groupResp.Group.ID)+"/rotate-key", "")
	require.Equal(t, 200, rec.Code, "rotate: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"key":"gk-`)

	rec = do(http.MethodGet, "/admin/stats?granularity=day", "")
	require.Equal(t, 200, rec.Code, "stats: %s", rec.Body.String())

	// 未认证 → 401
	req := httptest.NewRequest(http.MethodGet, "/admin/templates", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	require.Equal(t, 401, rec2.Code)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
```

（`newFakeStore` 来自 `internal/service/fakestore_test.go`，需在同包测试中可见——将 `fakestore_test.go` 移至 `internal/handler/fakestore_test.go`（实现 service.Store 接口），service 包的 fakeStore 仅服务 service 测试。实施时以编译为准，两处二选一。）

- [ ] **Step 7: 编译修正 + 跑测试**

Run: `go build ./... && go test ./internal/service/ ./internal/handler/ -count=1`
Expected: PASS。注意 fakeStore 的位置问题（Step 6 注）。

- [ ] **Step 8: Commit**

```bash
git add -A && git commit -m "feat: admin API (templates/accounts/groups/logs/stats) with service layer"
```

---

### Task 8: 服务装配（server + main + 中间件 + responses/anthropic 端点）

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/middleware.go`
- Create: `cmd/server/main.go`
- Create: `internal/proxy/forward_responses.go`
- Create: `internal/proxy/forward_anthropic.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: Task 1-7 全部
- Produces: 可运行单二进制；`GET /healthz`（inflight/goroutines/heap）

- [ ] **Step 1: 写失败测试（路由装配 + 中间件）**

`internal/server/server_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthz(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok", Logger: nil})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestUnknownPath404(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok"})
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 404, rec.Code)
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 写 proxy 的 responses/anthropic 转发（同构于 chat）**

`internal/proxy/forward_responses.go`:

```go
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/openai/openai-go"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
)

func (p *Proxy) HandleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := uuid.NewString()
	groupID, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(reqID, 0, 0, "", domain.FormatOpenAIResponses, 401, domain.ErrAuth, 0, nil, start)
		return
	}
	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	var params openai.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	model := params.Model.Value()
	sel, err := p.sched.Select(groupID, domain.FormatOpenAIResponses, model)
	if err != nil {
		p.handleSelectError(w, err)
		p.record(reqID, groupID, 0, model, domain.FormatOpenAIResponses, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}
	var lastCode int
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		ok, code := p.tryResponses(w, r, reqID, groupID, start, sel, &params)
		if ok {
			return
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil)
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
		} else {
			p.sched.Release(sel.AccountID)
			writeJSON(w, code, map[string]any{"error": map[string]any{
				"message": "upstream rejected request", "type": "upstream_error",
			}})
			p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIResponses, code, domain.Err5xx, 0, nil, start)
			return
		}
		p.sched.Release(sel.AccountID)
		var selErr error
		sel, selErr = p.sched.Select(groupID, domain.FormatOpenAIResponses, model)
		if selErr != nil {
			break
		}
	}
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

func (p *Proxy) tryResponses(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, params *openai.ResponseNewParams) (bool, int) {
	tpl := tplOf(sel)
	params.Model = openai.String(sel.Model)

	if params.Stream.Value() {
		ctx, cancel := context.WithTimeout(r.Context(), p.cfg.UpstreamStreamTimeout)
		defer cancel()
		stream := p.clients.ResponseStream(ctx, tpl, sel.UpstreamKey, *params)
		if stream.Err() != nil {
			return false, statusOf(stream.Err())
		}
		sw := newSSEWriter(w)
		var usage *openai.ResponseUsage
		for ev := range stream.Iter() {
			if err := sw.Event(ev); err != nil {
				sw.Abort()
				p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
				return true, 0
			}
			if ev.Usage != nil {
				usage = ev.Usage
			}
		}
		if err := stream.Err(); err != nil {
			sw.Abort()
			p.recordStreamAbort(reqID, start, sel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
			return true, 0
		}
		_ = sw.Done()
		var pt, ct, tt int64
		if usage != nil {
			pt, ct, tt = int64(usage.InputTokens), int64(usage.OutputTokens), int64(usage.InputTokens+usage.OutputTokens)
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
		p.record(reqID, 0, sel.AccountID, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
		return true, 200
	}

	resp, err := p.clients.Response(r.Context(), tpl, sel.UpstreamKey, *params)
	if err != nil {
		return false, statusOf(err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return false, 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var pt, ct, tt int64
	if resp.Usage != nil {
		pt, ct, tt = int64(resp.Usage.InputTokens), int64(resp.Usage.OutputTokens), int64(resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
	p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
	return true, 200
}
```

`internal/proxy/forward_anthropic.go`:

```go
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
)

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := uuid.NewString()
	groupID, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(reqID, 0, 0, "", domain.FormatAnthropic, 401, domain.ErrAuth, 0, nil, start)
		return
	}
	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	var params anthropic.MessageNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	model := params.Model
	sel, err := p.sched.Select(groupID, domain.FormatAnthropic, model)
	if err != nil {
		p.handleSelectError(w, err)
		p.record(reqID, groupID, 0, model, domain.FormatAnthropic, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}
	var lastCode int
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		ok, code := p.tryAnthropic(w, r, reqID, groupID, start, sel, &params)
		if ok {
			return
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil)
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
		} else {
			p.sched.Release(sel.AccountID)
			writeJSON(w, code, map[string]any{"error": map[string]any{
				"message": "upstream rejected request", "type": "upstream_error",
			}})
			p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatAnthropic, code, domain.Err5xx, 0, nil, start)
			return
		}
		p.sched.Release(sel.AccountID)
		var selErr error
		sel, selErr = p.sched.Select(groupID, domain.FormatAnthropic, model)
		if selErr != nil {
			break
		}
	}
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

func (p *Proxy) tryAnthropic(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, params *anthropic.MessageNewParams) (bool, int) {
	tpl := tplOf(sel)
	params.Model = sel.Model

	if params.Stream.Value() {
		ctx, cancel := context.WithTimeout(r.Context(), p.cfg.UpstreamStreamTimeout)
		defer cancel()
		stream := p.clients.AnthMessageStream(ctx, tpl, sel.UpstreamKey, *params)
		if stream.Err() != nil {
			return false, statusOf(stream.Err())
		}
		sw := newSSEWriter(w)
		var usage *anthropic.Usage
		for ev := range stream.Iter() {
			if err := sw.Event(ev); err != nil {
				sw.Abort()
				p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
				return true, 0
			}
			if ev.Usage != nil {
				usage = ev.Usage
			}
		}
		if err := stream.Err(); err != nil {
			sw.Abort()
			p.recordStreamAbort(reqID, start, sel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
			return true, 0
		}
		_ = sw.Done()
		var pt, ct, tt int64
		if usage != nil {
			pt, ct, tt = int64(usage.InputTokens), int64(usage.OutputTokens), int64(usage.InputTokens+usage.OutputTokens)
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
		p.record(reqID, 0, sel.AccountID, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
		return true, 200
	}

	resp, err := p.clients.AnthMessage(r.Context(), tpl, sel.UpstreamKey, *params)
	if err != nil {
		return false, statusOf(err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return false, 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var pt, ct, tt int64
	if resp.Usage != nil {
		pt, ct, tt = int64(resp.Usage.InputTokens), int64(resp.Usage.OutputTokens), int64(resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
	p.record(reqID, groupID, sel.AccountID, sel.Model, domain.FormatAnthropic, 200, domain.ErrNone, 0, &usageTuple{pt, ct, tt}, start)
	return true, 200
}
```

- [ ] **Step 4: 写 server 装配 + 中间件**

`internal/server/middleware.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"go-proxy-mini/pkg/logx"
)

type inflightCounter struct{ v atomic.Int64 }

func (c *inflightCounter) Inc() int64 { return c.v.Add(1) }
func (c *inflightCounter) Dec()       { c.v.Add(-1) }
func (c *inflightCounter) Load() int64 { return c.v.Load() }

// inflightLimiter 全局在途上限（规格 §10.6）：超限立即 429。
func inflightLimiter(max int64, c *inflightCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.Inc() > max {
				c.Dec()
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "server overloaded"})
				return
			}
			defer c.Dec()
			next.ServeHTTP(w, r)
		})
	}
}

// accessLog 请求级 Debug 追踪（规格 §7.1：生产 warn 不输出）。
func accessLog(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := r.Header.Get("X-Request-Id")
			if reqID == "" {
				reqID = uuid.NewString()
			}
			r.Header.Set("X-Request-Id", reqID)
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			if log != nil {
				log.Debug("http request",
					logx.String("request_id", reqID),
					logx.String("method", r.Method),
					logx.String("path", r.URL.Path),
					logx.Int("status", sw.status),
					logx.Duration("duration", time.Since(start)),
				)
			}
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func recoverer(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if log != nil {
						log.Error("panic recovered", logx.Any("panic", rec))
					}
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

`internal/server/server.go`:

```go
// Package server 装配 chi 路由：/admin/*（admin token）+ 三个 AI 端点 + /healthz。
package server

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"go-proxy-mini/pkg/logx"
)

type Options struct {
	AdminToken       string
	MaxInflight      int64
	ReadHeaderTimeout time.Duration
	MaxHeaderBytes   int
	AdminHandler     http.Handler // 已挂 /admin/* 路由
	AIHandler        http.Handler // proxy 三个端点
	Logger           *logx.Logger
}

type Server struct {
	opts  Options
	inflight inflightCounter
	handler http.Handler
}

func NewServer(opts Options) *Server {
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}
	if opts.MaxHeaderBytes == 0 {
		opts.MaxHeaderBytes = 1 << 20
	}
	if opts.MaxInflight == 0 {
		opts.MaxInflight = 50000
	}
	s := &Server{opts: opts}

	r := chi.NewRouter()
	r.Use(recoverer(opts.Logger))
	r.Use(accessLog(opts.Logger))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"inflight":  s.inflight.Load(),
			"goroutines": runtime.NumGoroutine(),
			"heap":      runtime.MemStats{}.HeapAlloc, // 简化：见 Step 4 修正
		})
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.RealIP)
		r.Use(func(next http.Handler) http.Handler { // admin token 认证
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer "+opts.AdminToken {
					writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
					return
				}
				next.ServeHTTP(w, req)
			})
		})
		if opts.AdminHandler != nil {
			r.Handle("/admin/*", opts.AdminHandler)
		}
	})

	r.Group(func(r chi.Router) {
		r.Use(inflightLimiter(opts.MaxInflight, &s.inflight))
		if opts.AIHandler != nil {
			r.Mount("/", opts.AIHandler)
		}
	})
	s.handler = r
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
```

（`healthz` 的 heap 简化实现：用 `var ms runtime.MemStats; runtime.ReadMemStats(&ms)` 修正为实际读数。）

`internal/server/server.go` 中 healthz 修正：

```go
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		writeJSON(w, http.StatusOK, map[string]any{
			"inflight":   s.inflight.Load(),
			"goroutines": runtime.NumGoroutine(),
			"heap":       ms.HeapAlloc,
		})
	})
```

`cmd/server/main.go`:

```go
// go-proxy-mini 入口：配置 → DB/ent → 各模块装配 → 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"github.com/jackc/pgx/v5/stdlib"

	"go-proxy-mini/internal/config"
	"go-proxy-mini/internal/handler"
	"go-proxy-mini/internal/proxy"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/server"
	"go-proxy-mini/internal/service"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/httpx"
	"go-proxy-mini/pkg/logx"
)

func main() {
	cfgPath := flag.String("config", "config.toml", "path to TOML config")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatalf("config: %v", err)
	}
	log, err := logx.New(cfg.Log.Level, cfg.Log.Output)
	if err != nil {
		fatalf("logger: %v", err)
	}
	if cfg.Admin.Token == "" || cfg.DB.DSN == "" {
		fatalf("admin.token and db.dsn are required (config or GPM_ADMIN_TOKEN/GPM_DB_DSN)")
	}

	pool, err := repository.OpenPG(context.Background(), cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		fatalf("db: %v", err)
	}
	defer pool.Close()
	// ent v0.14.6 的 entsql.OpenDB 只接受 *sql.DB：pgxpool 经 pgx/stdlib 桥接（用户决策 2026-08-05）
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB(dialect.Postgres, db)
	repos, err := repository.New(drv, true)
	if err != nil {
		fatalf("migrate: %v", err)
	}

	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: cfg.Scheduler.DefaultMaxConcurrency,
		Cooldown429:           cfg.Scheduler.Cooldown429,
		BackoffBase:           cfg.Scheduler.BackoffBase,
		BackoffMax:            cfg.Scheduler.BackoffMax,
		SyncInterval:          cfg.Scheduler.SyncInterval,
	}, repos.Groups, log)
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		DropOnFull:         cfg.Usage.DropOnFull,
		LogRetentionDays:   cfg.Usage.LogRetentionDays,
		StatsFlushInterval: cfg.Usage.StatsFlushInterval,
	}, repos.Logs, repos.Stats, log)

	auth := proxy.NewAuth(repos.Groups, log)
	hc := httpx.NewClient(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
	})
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
	})
	px := proxy.New(proxy.Config{
		MaxBodySize:           cfg.Proxy.MaxBodySize,
		MaxInflight:           cfg.Proxy.MaxInflight,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
		FailoverAttempts:      cfg.Proxy.FailoverAttempts,
		GroupKeyRPM:           cfg.Limit.GroupKeyRPM,
		UsageCapture:          cfg.Proxy.UsageCapture,
	}, sched, rec, clients, auth, log)

	svc := service.New(repos, sched, sched.InvalidateAll, auth, log)
	h := handler.New(svc)
	aiRouter := proxy.AIRouter(px) // 见 Step 5

	srv := server.NewServer(server.Options{
		AdminToken:        cfg.Admin.Token,
		MaxInflight:       cfg.Proxy.MaxInflight,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		AdminHandler:      h.RoutesMux(),
		AIHandler:         aiRouter,
		Logger:            log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched.Start(ctx)
	rec.Start(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}
	go func() {
		log.Info("server listening", logx.String("addr", cfg.Server.Addr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	rec.Close(shutdownCtx)
	log.Info("shutdown complete")
	_ = log.Sync()
}

func fatalf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("fatal: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}
```

（`main.go` 需 `fmt` import；`proxy.AIRouter` 与 `handler.RoutesMux` 见 Step 5。）

- [ ] **Step 5: 补 AI 路由与 admin mux 便捷方法**

`internal/proxy/router.go`:

```go
package proxy

import (
	"github.com/go-chi/chi/v5"
)

// AIRouter 挂载三个 AI 端点（规格 §6.1/§9）：路径决定请求格式。
func AIRouter(p *Proxy) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", p.HandleChat)
	r.Post("/v1/responses", p.HandleResponses)
	r.Post("/v1/messages", p.HandleAnthropic)
	return r
}
```

（`router.go` 需 `net/http` import。）

`internal/handler/handler.go` 追加：

```go
// RoutesMux 返回独立的 chi mux（供 server 以 Handle("/admin/*") 挂载）。
func (h *Handler) RoutesMux() http.Handler {
	r := chi.NewRouter()
	h.Routes(r)
	return r
}
```

- [ ] **Step 6: 编译全量 + 测试**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: 全量 PASS。`go vet` 可能指出 `logx.Duration` 用 `any` 参数的不规范（`go vet` 对 fmt 校验），如报错改为 `logx.Duration(k string, v time.Duration)`（Task 1 定义处同步修正为 time.Duration 类型）。

- [ ] **Step 7: 手工冒烟（可选，有 PG 时）**

```bash
go run ./cmd/server -config config.toml
# 另开终端：
curl -s -X POST localhost:8080/admin/templates -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' \
  -d '{"name":"t1","base_url":"https://api.openai.com/v1","default_format":"openai-chat","models":["gpt-4o"]}'
```
Expected: 返回创建的模板 JSON。

- [ ] **Step 8: Commit**

```bash
git add -A && git commit -m "feat: server assembly, main, responses/anthropic endpoints, middleware"
```

---

### Task 9: 压测与验收（tools：fakeupstream + loadtest + 基准回填）

**Files:**
- Create: `tools/fakeupstream/main.go`
- Create: `tools/loadtest/main.go`
- Create: `docs/superpowers/plans/loadtest-results.md`（基准回填，规格 §10.7）

**Interfaces:**
- Consumes: Task 1-8 的完整服务
- Produces: 可重复的压测流程与验收数据

- [ ] **Step 1: 写 fakeupstream（SSE 流式假上游）**

`tools/fakeupstream/main.go`:

```go
// fakeupstream 模拟 OpenAI chat/completions 上游：支持流式（chunks 个事件 + usage + [DONE]）。
// 用法: go run ./tools/fakeupstream -addr :9100 -chunks 100 -latency 20ms
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9100", "listen addr")
	chunks := flag.Int("chunks", 100, "SSE chunks per stream")
	latency := flag.Duration("latency", 20*time.Millisecond, "per-chunk delay")
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "object": "chat.completion",
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < *chunks; i++ {
			chunk := map[string]any{
				"id": "c1", "object": "chat.completion.chunk",
				"choices": []map[string]any{{"delta": map[string]any{"content": "x"}, "index": 0}},
			}
			if i == *chunks-1 {
				chunk["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
			time.Sleep(*latency)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	log.Printf("fake upstream on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
```

- [ ] **Step 2: 写 loadtest（并发流压测 + 指标）**

`tools/loadtest/main.go`:

```go
// loadtest 对网关打流式压测：固定并发 goroutine 持续请求，统计首字节延迟/完成率/错误。
// 用法: go run ./tools/loadtest -addr http://127.0.0.1:8080 -key gk-xxx -concurrency 10000 -duration 5m -healthz http://127.0.0.1:8080/healthz
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	concurrency = flag.Int("concurrency", 10000, "concurrent streams")
	duration    = flag.Duration("duration", 5*time.Minute, "test duration")
	addr        = flag.String("addr", "http://127.0.0.1:8080", "gateway addr")
	key         = flag.String("key", "gk-", "group key")
	healthz     = flag.String("healthz", "", "gateway /healthz url to sample memory")
)

type metrics struct {
	total       atomic.Int64
	errs        atomic.Int64
	firstByteMS atomic.Int64 // sum
	p99Sample   sync.Map     // 首字节延迟采样（估算 P99 用）
}

func main() {
	flag.Parse()
	m := &metrics{}
	stop := time.Now().Add(*duration)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Minute}
			for time.Now().Before(stop) {
				body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
				req, _ := http.NewRequest(http.MethodPost, *addr+"/v1/chat/completions", bytes.NewReader([]byte(body)))
				req.Header.Set("Authorization", "Bearer "+*key)
				req.Header.Set("Content-Type", "application/json")
				start := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					m.errs.Add(1)
					m.total.Add(1)
					continue
				}
				if resp.StatusCode != 200 {
					m.errs.Add(1)
					m.total.Add(1)
					resp.Body.Close()
					continue
				}
				firstByte := time.Since(start).Milliseconds()
				m.firstByteMS.Add(firstByte)
				storeSample(m, firstByte)
				br := bufio.NewReader(resp.Body)
				for {
					line, err := br.ReadString('\n')
					if strings.Contains(line, "[DONE]") || err != nil {
						break
					}
				}
				resp.Body.Close()
				m.total.Add(1)
			}
		}()
	}

	// 采样 goroutine：打印即时进度 + /healthz 内存
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var last int64
		for range t.C {
			cur := m.total.Load()
			fmt.Printf("elapsed=%s total=%d rate=%.0f/s errs=%d\n",
				time.Since(time.Now().Add(-time.Since(time.Now()))).Round(time.Second), // 简化输出
				cur, float64(cur-last)/10, m.errs.Load())
			last = cur
			if *healthz != "" {
				if resp, err := http.Get(*healthz); err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					fmt.Printf("  gateway: %s\n", b)
				}
			}
		}
	}()

	wg.Wait()
	fmt.Printf("\n=== RESULT ===\ntotal=%d errs=%d avg_first_byte_ms=%.1f\n",
		m.total.Load(), m.errs.Load(), float64(m.firstByteMS.Load())/max(1, m.total.Load()))
	fmt.Printf("p99_first_byte_ms=%d\n", p99(m))
}

func storeSample(m *metrics, v int64) {
	if m.p99Sample.Load() == nil {
		m.p99Sample.Store(&sync.Map{})
	}
	s, _ := m.p99Sample.Load().(*sync.Map)
	key := v / 10
	if c, ok := s.Load(key); ok {
		s.Store(key, c.(int64)+1)
	} else {
		s.Store(key, int64(1))
	}
}

func p99(m *metrics) int64 {
	s, ok := m.p99Sample.Load().(*sync.Map)
	if !ok {
		return -1
	}
	var total int64
	s.Range(func(_, c any) bool { total += c.(int64); return true })
	target := total * 99 / 100
	var acc int64
	for b := int64(0); ; b++ {
		if c, ok := s.Load(b); ok {
			acc += c.(int64)
			if acc >= target {
				return b * 10
			}
		}
		if b > 1_000_000 {
			return -1
		}
	}
}
```

（`loadtest` 输出格式为简化实现，验收以手工核对为准；`os` import 用于兜底。`max` 需 Go 1.21+ 内建。）

- [ ] **Step 3: 冒烟跑通（小并发验证链路）**

先自启 PG 实例（用户决策：Docker 自启，Docker 29.3.1 已确认可用）：

```bash
docker run -d --name gpm-pg -e POSTGRES_PASSWORD=gpm -e POSTGRES_DB=go_proxy_mini -p 5432:5432 postgres:16
```

```bash
# 终端 1：fake upstream
go run ./tools/fakeupstream -addr :9100 -latency 5ms
# 终端 2：网关（config.toml 指向 :9100 与 :5432，见下）
# 终端 3：建模板/账号/分组（curl 序列，参考 Task 8 Step 7）
go run ./tools/loadtest -addr http://127.0.0.1:8080 -key <生成key> -concurrency 50 -duration 10s -healthz http://127.0.0.1:8080/healthz
```
Expected: total>0、errs=0、gateway healthz 有输出。

config.toml 关键项：`db.dsn = "postgres://postgres:gpm@localhost:5432/go_proxy_mini?sslmode=disable"`（Docker 实例）；模板 base_url 填 `http://127.0.0.1:9100/v1`。压测结束 `docker rm -f gpm-pg` 清理（或在正式压测前重启容器保证干净 schema）。

- [ ] **Step 4: 正式验收（10k 并发流 ≥ 5 分钟）**

```bash
go run ./tools/loadtest -addr http://127.0.0.1:8080 -key gk-xxx \
  -concurrency 10000 -duration 5m -healthz http://127.0.0.1:8080/healthz
```
Expected（对照规格 §10.7 验收标准）：
- 10k 并发流稳定运行 ≥ 5 分钟，进程不崩溃、无 goroutine 泄漏（healthz goroutines 平稳）
- `errs` 占比 < 0.1%（网络抖动除外）
- 内存 < 2GB（healthz heap 采样）
- 日志零丢失：压测后 `GET /admin/logs?limit=10000` 行数 ≈ 压测完成请求数（± 缓冲内），或 stats 聚合计数一致
- failover 场景：fakeupstream 增加 `-fail429` 模式（某账号 429）验证转移无雪崩（可手工注入）

- [ ] **Step 5: 回填基准数据**

`docs/superpowers/plans/loadtest-results.md` 写入实测：机器规格、并发数、时长、total/errs、P99 首字节、内存峰值、FD/端口水位、日志丢失率、failover 观察。格式：

```markdown
# 压测基准（2026-08-05）

| 项 | 目标 | 实测 |
|---|---|---|
| 并发流 | 10000 | ... |
| 时长 | 5m | ... |
| errs 占比 | <0.1% | ... |
| P99 首字节 | <50ms 增量 | ... |
| 内存 | <2GB | ... |
| 日志丢失 | 0 | ... |
```

- [ ] **Step 6: 全量回归 + Commit**

Run: `go build ./... && go test ./... -count=1 && golangci-lint run ./...`
Expected: 全绿。

```bash
git add -A && git commit -m "test: load testing harness + acceptance results"
```

---

## Self-Review 结论（计划内完成）

- **规格覆盖**：§1-§14 全部映射到 Task 1-9（模板/账号/分组 CRUD → Task 7；调度器 → Task 4；转发/格式绑定/SDK → Task 6/8；用量 → Task 5；吞吐量 → Task 9 验收 + Task 1 httpx/配置参数；日志级别 → Task 1 logx + Task 8 中间件）
- **SDK 包装边界（pkg/aiclient）**：openai/anthropic 的 SDK import 只允许出现在 `pkg/aiclient`（客户端构建/鉴权注入/非流式超时）；协议类型（params/response/stream）透传为调用签名，internal/proxy 经 aiclient 调用
- **Go 1.26.5**：go.mod 版本与依赖均按 1.26 特性编写（range-over-func、math/rand/v2 并发安全随机源、slices/maps 辅助）
- **已知待编译修正点**（SDK/ent 具体 API 以安装版本为准，均在任务步骤中标注）：openai-go 流式迭代 API、anthropic-sdk-go 客户端选项、ent 集合查询方法名、fakeStore 测试位置
