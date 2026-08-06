# 通用高性能 SSE Relay 设计

## 状态

已批准。修订版修复了首版审查发现的 5 个高风险点：flush 阈值语义、timer 并发正确性、无上限 frame 累积、aiclient 原始流方法签名、DONE 不作为结束判断。

## 背景与目标

压测证明高频 SSE 流（5,000 并发、100 chunks × 20ms）下，openai-go / anthropic-sdk-go 的逐事件强类型解码与二次编码成为主要 CPU 瓶颈（`ssestream.Next` + `apijson` 约 30-50% CPU，逐事件 Flush/write syscall 另占约 25-30%）。低频 SSE（10 chunks × 100ms）同样 5,000 并发时延迟恢复正常（P99 40ms vs 2.55s），证明瓶颈是逐事件协议处理而非连接数。

非流式 SDK 路径 10k 并发稳定（约 2.36 万 req/s，P99 550ms，0 errors），不构成瓶颈。

本设计引入 pkg 级低解析 SSE relay 替换三条流式路径的 SDK 流式解码，保留非流式 SDK 路径。

## 包边界

```text
pkg/sserelay/
  relay.go
  relay_test.go
```

`sserelay` 只负责：从 `io.Reader` 增量读取 SSE → 原样转发 frame → 自适应 Flush → 旁路 observer。

不负责：发起 HTTP、鉴权、BaseURL、failover、调度器状态、usage 日志落盘、重新编码事件。

`pkg/aiclient` 新增原始 HTTP 流方法，返回 `*http.Response`；proxy 侧控制 status 检查与 body 关闭。非流式保留官方 SDK。

## 原始字节转发（无上限）

- 增量 parser，不设 frame 大小上限；
- 逐行识别，空行 = 帧结束；
- 原始 bytes 原样写出，不重排字段、不改变换行；
- 兼容 CRLF/LF、多行 data、event:/id:/retry:/comment、data-only SSE、typed SSE。

单帧累积实现：

```go
var frame []byte // 当前帧原始字节（含行分隔符）
var data  []byte // 当前帧合并后的 data payload
var event []byte // 当前帧 event 字段
```

`bufio.Reader.ReadSlice('\n')` 遇 `ErrBufferFull` 继续累积该行剩余部分，直到该行结束。帧结束（空行）时：写出原始 frame → 构造 observer 输入 → 检查 flush 阈值。累积 buffer 仅当前帧生命周期有效，不跨帧保留。

## 字节扫描与 usage 提取

事件类型判断走标准库 SWAR 优化路径（bytes.IndexByte / bytes.HasPrefix / bytes.Equal），常规 token 帧零 JSON 解析。仅命中 usage 事件时调用 `gjson.GetBytes`（gjson v1.18.0 从 indirect 提升为 direct 依赖）。

usage 路径：

| 格式 | 事件条件 | 路径 |
|---|---|---|
| openai-chat | data-only，含 usage 字段 | `usage.prompt_tokens` / `usage.completion_tokens` / `usage.total_tokens` |
| openai-responses | event == `response.completed` | `response.usage.input_tokens` / `output_tokens` / `total_tokens` |
| anthropic | event == `message_start` | `message.usage.input_tokens` |
| anthropic | event == `message_delta` | `usage.output_tokens` |

## 自适应 Flush

```go
type flushState struct {
    bw        *bufio.Writer
    fl        http.Flusher
    pending   int      // 已写入但未 flush 的字节数
    lastFlush time.Time
}
```

默认参数：`FlushBytes = 4096`，`FlushInterval = 1ms`。

规则（明确定义，防退化）：

1. 首事件写出后立即 flush（保证首字节延迟）——**待对照验证项**，见下；
2. 之后每个事件写出后检查：`pending >= FlushBytes` → flush，`pending = 0`；
3. 独立 timer（FlushInterval）到期后持锁 flush（仅 `pending > 0` 时）；
4. 读循环结束时 flush 残余并停止 timer。

### 首事件 flush 对照实验

实现后在同一环境、同一压测工具下做 A/B 对照：

- A：首事件立即 flush + 4KiB/1ms 批量（默认方案）
- B：全部事件走 4KiB/1ms 批量（首事件不特殊）

对照指标：首字节 P99、token 到达间隔、总 write syscall 数。数据出来后定论默认值；结论写入压测记录。

## timer 并发正确性

```go
type relay struct {
    mu    sync.Mutex
    bw    *bufio.Writer
    fl    http.Flusher
    done  chan struct{}
    timer *time.Timer
}
```

- 读循环每次写出 + 阈值检查持 `mu`；
- timer goroutine 每次到点持 `mu` 检查 `pending`；
- relay 返回前：停止 timer → 等待 timer goroutine 退出（`<-done`）→ 释放 writer（归还 bufioPool）；
- 保证 timer 绝不触碰已归还的 writer；
- 无跨请求全局锁，每条流独立。

## 结束语义

- 流结束 = reader EOF / 上游读错误 / ctx 取消；
- `[DONE]` 不作为结束判断，仅作为 observer 观察信息；
- Anthropic 无 `[DONE]`，正常 EOF 结束；
- relay 结束时 flush 残余并返回错误（如有）。

## Observer 接口

```go
type Event struct {
    Raw   []byte // 完整原始帧（含结尾空行）
    Event []byte // event 字段；data-only 为空
    Data  []byte // 合并后的 data payload
}

type Observer func(Event)

type Config struct {
    FlushBytes    int           // 默认 4096
    FlushInterval time.Duration // 默认 1ms
    Observer      Observer
}

func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error
```

约定：

- observer 在原始帧写入后调用；
- observer 不得阻塞 relay；
- observer 不改变已写出的字节；
- observer panic 不吞，测试保证实现不 panic；
- relay 不生成、不删除 `[DONE]`；
- dst 无 Flusher 时完整写出（不 panic、不空转）。

## aiclient 原始流方法

```go
func (f *Factory) ChatCompletionStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error)
func (f *Factory) ResponseStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error)
func (f *Factory) AnthMessageStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error)
```

实现基于现有 SDK 客户端配置（BaseURL、WithMaxRetries(0)、WithHTTPClient(f.hc)），通过 http.Client 原始请求 + 注入鉴权头（openai: Authorization Bearer；anthropic: x-api-key），请求体为 proxy 已 ReadAll 的原始 body。不调用 SDK 流解析。上游连接由 proxy 负责关闭。实现前确认两个 SDK 是否有公开原始请求入口，否则直接基于 http.Client.Do 构造。

## 三端点迁移

### OpenAI Chat

```go
resp, err := p.clients.ChatCompletionStreamRaw(ctx, tpl, sel.UpstreamKey, body)
if err != nil { return false, statusOf(err), upstreamBody(err) }
if resp.StatusCode != 200 {
    resp.Body.Close()
    return false, resp.StatusCode, readUpstreamBody(resp)
}
err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
    Observer: func(ev sserelay.Event) {
        if len(ev.Event) == 0 && gjson.GetBytes(ev.Data, "usage").Exists() {
            // 提取 pt/ct/tt
        }
    },
})
resp.Body.Close()
```

### OpenAI Responses

只对 `event == "response.completed"` 的帧扫描 `response.usage`。

### Anthropic

保留 `event:` 行；message_start 采 input_tokens，message_delta 采 output_tokens。

所有现有行为保留：首字节后不 failover、上游读错误记 ErrAbort、客户端写失败释放槽位并标记错误、成功重置账号状态、UsageCapture 关闭不 Record、4xx 透传原始 body。

## 与规格 §6.1 的关系

规格 §6.1 "上游调用一律走官方 SDK" 需同步更新：**非流式走 SDK，流式走原始 HTTP + sserelay**（流式性能优化例外，本设计已批准）。

## 测试设计

### pkg/sserelay/relay_test.go

1. 原始字节完整保留（含 event/data/id/retry/comment/CRLF/LF）；
2. 超长行/超长帧可增量转发，无 scanner 上限；
3. data-only `[DONE]` 被 observer 标记；
4. typed event 被 observer 收到；
5. 首事件立即 Flush；
6. 后续按 4KiB 阈值批量 Flush；
7. 1ms timer 在无下一帧时仍 Flush；
8. reader 错误向上返回；
9. writer 错误向上返回；
10. ctx cancel 停止 relay；
11. 无 Flusher 的 writer 完整写出；
12. 单流 timer 退出无 goroutine 泄漏（-race 验证）。

### internal/proxy 回归

- 三条流式路径原始 SSE 输出不变；
- Anthropic event: 行不变；
- 三端点 usage 数值不变；
- stream abort / client disconnect 释放槽位；
- failover 行为不变；
- 4xx 透传原始 body 不变。

### 性能验证

- go test -race ./pkg/sserelay ./internal/proxy；
- 基准测试对比 SDK stream 与 relay stream 的 allocs/op；
- 服务器 50% CPU 约束下重复高频 SSE 压测（与改造前对照）；
- 首事件 flush A/B 对照实验；
- 只提交匿名结果，不提交服务器身份。
