# 非流式压测模式设计

## 状态

已批准方案 A（扩展现有 `tools/loadtest`）。本设计只覆盖非流式压测工具扩展，不改变网关转发实现。

## 目标

补充对 `openai-chat` 非流式请求的性能测量，验证 SDK 非流式请求/响应编码解码是否构成瓶颈，并与现有流式 SSE 压测结果分开统计。默认流式模式的行为和输出保持兼容。

## 资源约束

Linux 压测服务器有生产负载。压测期间 benchmark 相关进程和 benchmark PostgreSQL 容器最多使用 12/24 个 CPU（50%）：

- 进程 `taskset` 固定到 CPU 0-11；
- `GOMAXPROCS=12`；
- Docker PostgreSQL 设置 `cpuset-cpus=0-11`；
- 不在代码、文档、commit message 或结果文件中记录服务器 IP、主机名或 SSH 地址。

## 工具接口

扩展 `tools/loadtest`：

```text
-mode stream|chat
```

- 默认值为 `stream`，保持现有 SSE 测试行为；
- `chat` 发送不带 `stream: true` 的 `/v1/chat/completions` 请求；
- 两种模式共用地址、key、并发、持续时间、warmup、healthz、输出文件和错误统计。

## 指标

### stream 模式

保留现有指标：

- `total`、`errs`；
- `avg_first_byte_ms`；
- `p99_first_byte_ms`；
- `elapsed`、`concurrency`；
- `err_detail`。

### chat 模式

每个成功请求读取完整响应 body 后计为完成，记录请求开始到完整 body 读取结束的延迟：

- `mode=chat`；
- `total`、`errs`；
- `avg_latency_ms`；
- `p99_latency_ms`；
- `elapsed`、`concurrency`；
- `err_detail`。

非流式模式不输出或计算首字节指标，避免把两个延迟口径混淆。HTTP 非 2xx、请求失败、响应读取失败都计入 `errs` 和 `total`，并关闭 response body。

## 实现边界

- 不新增第二个压测工具；
- 不修改网关业务代码；
- 不新增第三方依赖；
- 请求 body 继续使用固定的最小 OpenAI chat 请求；
- fakeupstream 现有非流式 chat 响应直接复用；
- 统计桶沿用现有 mutex map，新增非流式 latency 样本桶；
- `-mode` 只影响请求构造、响应读取和结果指标。

## 验收测试

1. `-mode` 默认值为 `stream`，现有 stream 行为测试继续通过；
2. `chat` 请求体不含 `stream:true`，成功请求读取完整 body 后计数；
3. chat HTTP 错误计入 `total`/`errs` 并记录 `status:<code>`；
4. chat 读取响应失败计入 `total`/`errs` 并记录 `read:<error>`；
5. chat 结果包含 `mode=chat`、完整响应延迟 avg/P99，不包含首字节指标；
6. `go test ./...`、`go vet ./...` 和 `golangci-lint run ./...` 通过；
7. 服务器压测只在 50% CPU 上限下执行，结果文档只记录匿名硬件/资源约束。

## 压测计划

代码完成后在匿名 Linux 压测环境中运行非流式并发阶梯：500、1000、2000、5000；只有在 5000 稳定且生产 CPU 余量不受影响时才考虑 10000。每档记录 RPS、错误率、完整响应 avg/P99，以及 gateway/fakeupstream 的 Go pprof CPU profile。