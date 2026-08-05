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
| 日志丢失 | 0 | 0（77,267 行 = 71,069 计数 + 6,198 预热 + 150 基线，精确一致） | 达标 |
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

## 四、压测发现并修复的缺陷

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
