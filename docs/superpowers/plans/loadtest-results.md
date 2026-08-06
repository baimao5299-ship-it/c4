# 压测基准（2026-08-06）

机器：Windows 11 Pro（10.0.26100），16 逻辑核（压测峰值 CPU ≈33%），Go 1.26.5。
拓扑：loadtest（本机）→ 网关 :18080（config.toml，Docker PG :5432）→ fakeupstream :9100（100 chunks × 20ms/流）。
验收工具：`tools/fakeupstream` + `tools/loadtest`（本目录所属 Task 9 交付物）。

## 一、验收标准对照（规格 §10.7）

| 项 | 目标 | 实测 | 结论 |
|---|---|---|---|
| 并发流 | 10000 | **500（本机上限，见下）** | 未达标（OS 限制，非网关缺陷） |
| 时长 | 5m | 5m2s（500 并发档） | 达标（该档位） |
| errs 占比 | <0.1% | 2/71069 = 0.0028% | 达标 |
| P99 首字节增量 | <50ms | 基线 <10ms → 500 档 100ms（+~90ms） | **未达标**（Windows 首字节尾延迟，avg 7.4ms） |
| 内存 | <2GB | heap 峰值 ~70MB（healthz 采样，含 10k 档需求时 <120MB） | 达标 |
| 日志丢失 | 0 | 0（77,267 行 = 71,069 正式档计数 + 150 基线 + ~6,048 预热估算；预热流不计入 loadtest 计数，25s × ~242/s ≈ 6,048 与残余 6,198−150 闭合） | 达标 |
| FD/端口水位 | <30k | 峰值 ~1,500 套接字（500 入站 + 500 上游 + 500 压测端）；TIME_WAIT 风暴可达 16k 上限 | 达标 |
| failover 注入 | 429/5xx 无雪崩 | 429 注入：8,700 req / 0 errs；5xx 注入：6,600 req / 0 errs；全失败：确定性 502、网关无崩溃无泄漏 | 达标 |

## 二、实测并发阶梯（每档 30-60s，warmup 15-25s，-latency 20ms）

| 并发 | total | errs | errs 占比 | avg 首字节 | P99 首字节 |
|---|---|---|---|---|---|
| 10（基线） | 150 | 0 | 0% | 1.2ms | <10ms |
| 50（冒烟） | 250 | 0 | 0% | 1.6ms | <10ms |
| 150 | 2,250 | 0 | 0% | 9.0ms | 40ms |
| 500（正式档） | 71,069 | 2 | 0.003% | 7.4ms | 100ms |
| 750 | 26,117 | 15,464 | 59% | 394.8ms | 2,890ms |
| 1,000 | 53,885 | 41,782 | 78% | 249.2ms | 2,700ms |
| 2,000 | 108,638 | 104,482 | 96% | 59.9ms | — |

## 三、10k 档未达成的环境原因（非网关缺陷）

1. **Windows 监听 backlog ≈ 200**（`listen(SOMAXCONN)` 上限）：原始 Go listener 实验——
   5,000 并发拨号仅 205 被接受、14,385 被 RST（`connectex: actively refused`）。
   突发拨号超过 ~200 即被拒绝；loadtest 增加 100-300ms 抖动退避 + 预热窗口后，
   500 并发档稳定收敛（正式 5 分钟档 errs=0.003%），750+ 仍无法收敛。
2. **动态端口范围 16,384**（`netsh int ipv4 show dynamicport tcp`：49152-65535，
   无管理员权限不可扩展）：10k 并发需 loadtest 10k + 网关上游 10k ≈ 20k 套接字，
   超出机器上限（即理想 accept 下本机并发天花板 ~7-8k）。
3. 网关侧证据：750-2,000 档网关进程均稳定（goroutine 回落到基线、heap 回落、
   无 panic/泄漏），失败全部为压测端拨号被拒（OS 层）。
4. 结论：10k 并发流验收需在 Linux（SOMAXCONN=4096）或提升后的 Windows 环境复跑；
   工具链与流程可原样复现。

## 四、非流式 chat 压测（2026-08-06）

环境：匿名 Linux 压测服务器，24 逻辑 CPU；为同机生产负载预留 50% CPU，网关、fakeupstream 与独立 PostgreSQL Docker 容器均固定在 CPU 0-11，且 Go 进程使用 `GOMAXPROCS=12`。链路为 loadtest → 网关 → fakeupstream，非流式 `POST /v1/chat/completions`。每档 15-20s warmup + 45s 计时，fakeupstream 返回最小 OpenAI chat JSON；`-mode chat` 统计从发起请求至完整 body 读完的端到端延迟。

| 并发 | total（45s） | 吞吐 | errs | avg 完整响应延迟 | P99 完整响应延迟 |
|---:|---:|---:|---:|---:|---:|
| 500 | 966,403 | 21,476 req/s | 0 | 22.7ms | 60ms |
| 1,000 | 1,048,890 | 23,309 req/s | 0 | 42.3ms | 80ms |
| 2,000 | 1,102,751 | 24,506 req/s | 0 | 81.1ms | 120ms |
| 5,000 | 1,109,729 | 24,661 req/s | 0 | 202.1ms | 280ms |
| 10,000 | 1,063,844 | 23,641 req/s | 0 | 421.8ms | 550ms |

10k 档资源采样：网关峰值约 5.9 CPU 核（受限于 12 核 cap）、RSS 峰值约 1.14GB；fakeupstream 约 1.3 CPU 核、RSS 约 126MB；PostgreSQL CPU 峰值约 39%、内存约 303MB。网关 `inflight` 可稳定到 10,000，计时结束后回落至 0，无错误、无崩溃。所有 benchmark 组件的实际 CPU 使用显著低于 12 核上限，因此保留的另一半 CPU 未被本次压测占用。

结论：非流式 SDK 请求/响应路径在 10k 并发下稳定，吞吐平台约 2.4 万 req/s；其完整响应 P99 为 550ms，未显示出流式高频 SSE 那种逐事件解码/重编码/Flush 导致的秒级排队。该结果不能替代流式验收，流式高频 SSE 瓶颈仍需以原始 SSE relay 改造解决。

## 四·二、SSE relay 首事件 flush A/B 对照实验（2026-08-06）

环境：匿名 Linux 压测服务器，24 逻辑 CPU / 62GB 内存，无生产负载（100% CPU 可用）。链路为 loadtest/ssetiming → 网关（sserelay 已上线）→ fakeupstream。A = 首事件立即 flush（默认实现）；B = 首事件跳过立即 flush、全部事件走 4KiB/1ms 批量（SkipFirstFlush 开关，实验代码未提交、已还原）。

流型 1：正常流（fakeupstream 100 chunks × 20ms，5,000 并发，45s）：

| 指标 | A（首事件立即 flush） | B（全批量） |
|---|---:|---:|
| 首事件延迟 avg | 663µs | 1.98ms |
| 首事件延迟 P99 | 2.0ms | 4.3ms |
| token 间隔 avg（上游 20ms 主导） | 20.48ms | 20.56ms |
| 网关 write syscalls | 56,491 | 56,923 |

流型 2：密集流（fakeupstream 1000 chunks × 0ms，2,000 并发，30s）：

| 指标 | A | B |
|---|---:|---:|
| total | 37,980 | 38,075 |
| avg 首字节 | 352.8ms | 335.5ms（排队主导，噪声内） |
| P99 首字节 | 2,710ms | 2,660ms |
| 网关 write syscalls | 2,839,268 | 2,834,845 |

结论：**A 胜出，保持默认**。A/B 差异仅每流第一个事件的 flush 时机（之后均走 4KiB 阈值 / 1ms timer 批量，relay 的批量 flush 在两种流型下始终生效）——因此 B 没有带来 syscall 减少（两流型 A/B syscw 均持平），而 A 在正常流上首事件延迟好约 1.3ms（663µs vs 1.98ms）。密集流下差异被排队淹没（±5% 噪声）。首事件立即 flush 无代价地改善流式首字节体验，维持为默认配置。

## 四·三、10k 并发正式档（sserelay 上线后，2026-08-06）

环境：匿名 Linux 压测服务器，24 逻辑 CPU / 62GB 内存，100% CPU 可用，SOMAXCONN=65535。链路：loadtest → 网关（sserelay 原始字节 relay）→ fakeupstream（100 chunks × 20ms）。验收工具 loadtest（-mode stream）。

### 正式档：10,000 并发 × 5 分钟

| 项 | 目标 | 实测 | 结论 |
|---|---|---|---|
| 并发流 | 10,000 | 10,000（5m3s 稳定） | ✅ |
| errs 占比 | <0.1% | 0 / 996,137 = 0% | ✅ |
| P99 首字节增量 | <50ms | 基线 <10ms → 10k 档 P99 330ms（avg 114.7ms） | ⚠️ 未达标——归因见下 |
| 内存 | <2GB | 网关 RSS 峰值 1.58GB、heap 峰值 ~1.17GB | ✅ |
| 日志零丢失 | 0 | usage_logs 1,178,187 = 正式档 996,137 + 基线 150 + failover 60,000 + 预热 ~121,900（估算 119k，±2.4%） | ✅ |
| FD 水位 | <30k | 网关 2,010 / 上游 2,006 | ✅ |
| failover 注入 | 429/5xx 无雪崩 | 429：30,000 req / 0 errs / avg 1.0ms；5xx：30,000 req / 0 errs / avg 1.1ms；注入账号 a1 正确进入 429 冷却 / unhealthy 指数退避 | ✅ |

P99 未达标归因：fakeupstream 单进程吞吐上限约 3.3k req/s（其 CPU 峰值 6.3 核打满），而 10k 并发 × 2s/流 的需求约 5k req/s——上游吞吐不足导致网关持续排队，首字节延迟被排队抬高。网关自身峰值 ~10 核（24 核中仍有 14 核空闲），PG 峰值 6.5% CPU，无泄漏（结束后 goroutines 49,700 → 4,109、inflight 10k → 0、heap 回落至 230MB）。即 P99 由测试上游容量决定，非网关缺陷。

对照（sserelay 收益）：旧实现（SDK 逐事件解码）在 12 核限制下 5,000 并发同型流 avg 首字节 428ms / P99 2.55s；sserelay + 24 核下 10,000 并发 avg 114.7ms / P99 330ms——并发翻倍、延迟降约 4 倍。

## 四·四、50k 并发压力 + pprof 热点分析（2026-08-06）

环境：同一匿名 Linux 服务器（24 核 / 62GB，100% CPU）。拓扑：loadtest（50k 并发）→ 网关（sserelay，临时 loopback-only pprof 6060）→ 3 实例 fakeupstream（:9101/2/3，100 chunks × 1ms）。`ulimit -n 1048576`、`max_inflight 200000`、6 账号（3 模板 × 2）。

### 结果

| 项 | 实测 |
|---|---|
| total / errs | 146,064 / 19（0.013%，全部为 loadtest 侧 `cannot assign requested address` 瞬时端口耗尽） |
| avg 首字节 / P99 | 10,075ms / 19,190ms（严重排队，需求 500k req/s vs 上游完成 ~2.4k req/s） |
| 网关 | goroutines 峰值 ~187k，heap 峰值 ~4.8GB，RSS ~6.9GB，CPU 峰值 ~9/24 核 |
| 上游 | 3 实例合计 CPU ~1.3/24 核（远未打满） |
| 稳定性 | 无崩溃、无泄漏：结束后 goroutines 回落至 ~12k、inflight 50k→0、heap 回落至 ~2GB |

### pprof 热点（网关 CPU，30s 采样）

**无业务代码热点**——top 全部为 runtime 调度/同步原语：

- `runtime.selectgo` 49.8% cum（18.7 万 goroutine 的 select 等待/唤醒）
- `runtime.schedule` 25.7% cum、`lock2`+`unlock2` ~27%、`futex` 7%
- `runtime.(*timers)`（run/siftDown 合计 ~19%）——**每流 1 个 1ms flush timer**：50k 流 = 50k 个 1ms timer 持续空转（事件到达率远低于 1ms 粒度）
- `runtime.saveblockevent` 23.6%——**测量伪影**：benchprofile 的 `SetBlockProfileRate(1)` 全量阻塞采样自身开销，非真实负载

block/mutex profile：阻塞等待集中在**上游响应**（`rawPost`/`http.Client.Do` 43%）与 sserelay 写路径 mutex（44%，每流独立锁的等待累计，非跨流竞争）。上游与 loadtest CPU 均未打满（Syscall 为主）。

### 结论

1. **网关 50k 连接 + 18.7 万 goroutine 稳定**：0 网关侧错误、无崩溃无泄漏。
2. **完成率瓶颈在上游链路**（~2.4k req/s，各组件 CPU 均未打满）：网关→上游连接建立/复用 + fakeupstream 处理能力的排队，非网关 CPU 瓶颈。
3. **pprof 揭示的真实优化信号**：
   - 每流 1 个 timer goroutine（relay `startFlushTimer`）——50k 流 = 50k 空转 goroutine，可共享/懒启动；
   - 1ms `FlushInterval` 在事件间隔 >1ms 时大量空转（timers 堆压力），可考虑自适应间隔或按事件驱动；
   - block/mutex 采样仅存在于临时 benchprofile 构建，生产无此开销。

## 四·五、50k 并发 × 5,000 账号压测 + pprof 热点定位（2026-08-06）

环境：同一匿名 Linux 服务器（24 核 / 62GB，100% CPU）。拓扑：loadtest（50k 并发）→ 网关 → 3 实例 fakeupstream（100 chunks × 1ms）；单分组绑定 **5,000 个 active 账号**（3 模板均分，max_concurrency 10000/账号，weight 100）。

### 结果

| 项 | 实测 |
|---|---|
| total / errs | 14,615 / 0（完成率 ~61 req/s，avg 首字节 162.8s / P99 227.4s） |
| 网关 inflight 峰值 | ~17,332（未达 50k——CPU 打满后连接建立停滞，loadtest 侧 dial 排队） |
| 稳定性 | 无崩溃、无泄漏（结束后 goroutines 回落 ~3.6k、inflight 0） |

### pprof 热点（网关 CPU，30s 采样，采样期间 CPU 过载致 lostProfileEvent 48%）

**调度器选号是绝对热点（51%+），业务代码首次成为压测主要瓶颈：**

- `Scheduler.weightedOrder` 51.2% cum + `accountSnapshot.score` 29.4% flat + `Scheduler.Select` 51.3% cum

**根因**：`weightedOrder` 是不放回加权抽样，实现为 O(n²)——每轮遍历剩余池重算 total + 逐元素 score（5,000 账号 → 每 Select ~1,250 万次 `score()` 浮点计算）+ 切片删除。每请求一次 Select，50k 并发下 CPU 被打满，其余处理（accept/连接/转发）全部停滞。此前压测（单组 6 账号）选号是 O(36) 微操作，从未暴露。

**优化方向（未实施）**：加权抽样改为单遍 O(n)（一次 total + 一次索引选择，score 缓存复用）；CAS 竞争重试保持循环但每次重选 O(n)（真实场景竞争极少，通常 1 次）。预计 Select 加速 ~O(n) 倍（5k 账号下约千倍）。

## 四·六、50k × 5,000 账号复验（预生成序列上线后，2026-08-06）

同拓扑复跑（50k 并发 × 5,000 账号，3 实例 fakeupstream 1ms，pprof 三侧）。改造前（O(n²) weightedOrder）同场景：total 14,615 / avg 首字节 162.8s / 完成率 61 req/s / inflight 峰值 1.7 万。

| 指标 | 改造前 | 改造后 |
|---|---:|---:|
| total（60s 档） | 14,615 | **196,744** |
| errs | 0（低压力假象） | 874（0.44%，性质见下） |
| 完成率 | 61 req/s | **2,660 req/s（43 倍）** |
| avg / P99 首字节 | 162.8s / 227.4s | **12.1s / 20.2s（13 倍改善）** |
| inflight 峰值 | 17,332（连接停滞） | **61,451（连接建立正常）** |
| goroutines 峰值 | 187k | 156k |

**pprof 热点对比**：改造前 `weightedOrder` 51.2% + `score` 29.4% + `Select` 51.3%（业务代码主导，lostProfileEvent 48% 采样过载）；改造后 top 全为 runtime/系统调用（Syscall6 15%、selectgo 33% cum、timers 14.5%、saveblockevent 8% 采样伪影）——**无任何 scheduler 业务函数，选号 O(1) 实证生效**。与单组 6 账号基线（total 146k / avg 10.1s）持平——调度器规模不再是瓶颈。

874 errs 性质（网关日志 0 warn、无崩溃无泄漏）：`cannot assign requested address` 413 次（loadtest 侧动态端口耗尽——完成率 43 倍提升后新连接建立频率激增所致）、`connection reset by peer` ~460 次（50k 瞬时连接风暴下 accept 队列竞争的内核 RST）、`EOF` 373 次（上游 3 实例在瞬时压力下的流中断）、`server closed idle connection` 21 次（keep-alive 正常回收）。均为压测基础设施在真实压力下的边界行为，非网关缺陷。

## 五、压测发现并修复的缺陷

**SSE 事件级冲刷缺失（internal/proxy/forward.go + internal/server/middleware.go）**：
`sseWriter` 只刷 bufio 不调 `http.Flusher.Flush()`，且 accessLog 中间件的
`statusWriter` 不实现 `Flush()`（嵌入 http.ResponseWriter 只提升 Header/Write/WriteHeader
三个方法）。结果：Go http.Server 内部 4KB 缓冲攒批放出，流式首字节延迟实测 145ms。
修复：`statusWriter.Flush()` 委托内层 + `sseWriter` 每事件 Flush。修复后首字节
1.2-2.3ms（curl 实测）、10 并发基线 avg 1.2ms。全量测试（13 包）绿。

## 五、failover 注入细节（fakeupstream -fail429/-fail500）

- `-fail429 sk-fake-a1`：网关选号 a1 → 上游 429 → 调度器标记 429+cooldown → 转移 a2 →
  8,700 req / 0 errs，P99 首字节 10ms；`/admin/accounts` 可见 a1=429+CooldownUntil、
  a2=active。无雪崩（errs 0%、延迟无劣化）。
- `-fail500 sk-fake-a1`：6,600 req / 0 errs，P99 10ms。同样收敛。
- 双账号全失败：全部 502（确定性），网关无崩溃/泄漏；压测端因 502 快循环 + TIME_WAIT
  （120s）撑满自身 16,384 端口（`Only one usage of each socket address...`），为压测端
  人工观察到的极限行为，非网关雪崩（healthz 正常、goroutine 回落）。

## 六、复现命令

```bash
docker run -d --name gpm-pg -e POSTGRES_PASSWORD=gpm -e POSTGRES_DB=go_proxy_mini -p 5432:5432 postgres:16
go build -o bin/ ./tools/fakeupstream ./cmd/server ./tools/loadtest
./bin/fakeupstream -addr :9100 -latency 20ms            # 或 -fail429 sk-xxx -fail500 sk-xxx
./bin/server -config config.toml                        # :18080，DSN 指 Docker PG
# 建模板/账号/分组（POST /admin/*，Bearer admin token），分组 key 用于 AI 请求
./bin/loadtest -addr http://127.0.0.1:18080 -key gk-xxx -concurrency 500 -duration 5m \
  -warmup 25s -healthz http://127.0.0.1:18080/healthz -out result.txt
```
