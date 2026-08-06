# go-proxy-mini — 超 mini AI 中转网关 · 设计文档

> 版本: v0.1 (2026-08-05)
> 状态: v0.1 已实施（2026-08-06：Task 1-9 完成、终审通过、golangci-lint 全绿；实测验收见 §10.7）
> 技术栈参考: `teagent/DESIGN.md` §20（Go + ent + pgx + chi + zap）；调度器设计参考 `sub2api-openai-codex-fingerprint`（取其机制精华，避开其账号字段设计缺陷）

---

## 1. 目标与非目标

**定位**：单机单二进制、零额外中间件依赖的高性能 AI 流量中转网关。客户端用分组级 key 调用，网关把请求原样转发到厂商上游，不做请求格式转换。

**目标**：
- 热路径（AI 请求）零 DB 访问：鉴权、选号、状态判定全部走进程内存
- **单实例抗 W 级并发（≥10k 在途）**：吞吐量设计见 §10（连接池/无锁热路径/管线容量/过载保护，全部有量化预算与压测验收）
- 请求格式直接路由、不转换；仅支持模型名映射（模板级）
- 分组 = 路由池 + 跨厂商故障转移；账号状态机（active/unhealthy/429/disabled）+ 冷却自动恢复
- 账号级并发控制；用量明细落库 + 预聚合统计
- 管理 API 与 AI API 分离；无前端

**非目标（v1 明确不做，接口预留）**：
- 请求体/协议转换（openai↔anthropic 等）、多实例/Redis 分布式调度、前端 UI、计费/多租户、请求体缓存、复杂重试（流式中断不重试）

## 2. 核心概念与关键决策

| 概念 | 定义 |
|---|---|
| 厂商模板 (template) | 声明上游网关地址、默认请求格式 + 模型级格式覆盖（openai-chat / openai-responses / anthropic，决定所用 SDK 客户端与 429 解析语义）、可服务模型列表、模型映射 |
| 账号 (account) | 挂在一个模板下、持有上游真实 key 的转发通道；有状态与冷却 |
| 分组 (group) | 账号的 N:M 集合；持有客户端入口 key（`gk-` 前缀）；请求经分组路由到组内账号 |

**关键决策**：
1. **调度器内存优先（单实例）**：状态/冷却/并发全在内存，DB 只存配置与用量；管理端变更主动失效快照 + 定时全量同步兜底
2. **不校验模型**：模型仅用于选号偏好；组内无命中模型时仍选号转发，由上游返回真实错误；4xx 不触发故障转移
3. **账号模型刻意瘦身**：单一暂停机制 `cooldown_until` + 单一状态位 `status`；无 god-object、无多暂停机制叠加、无 JSON map 字段（吸取 sub2api 教训：其账号 40+ 字段、6 条件可调度链、双并发字段、JSON map 全被否决）
4. **协议处理不手写**：上游调用一律走官方 SDK —— OpenAI 用 `github.com/openai/openai-go`（chat/completions + responses 两个客户端）、Anthropic 用 `github.com/anthropics/anthropic-sdk-go`。请求构建、SSE 解析、429/重试语义、usage 解析全部由 SDK 承担，网关不自研任何协议逻辑。**路径决定请求格式**（§6.1），格式匹配是选号的硬过滤条件
5. **用量采集**：SDK 响应类型自带 usage 字段直接取，零扫描开销；可配置开关
6. **日志级别化**：请求级追踪一律 Debug，生产默认 Warn 级别不输出；业务日志落库独立于日志级别
7. **迁移用 ent 自动迁移**（v1 不引入 Atlas 版本化）

## 3. 总体架构

```
┌─────────────┐   Bearer gk-xxx    ┌──────────────────────────────┐
│  AI 客户端   │ ─────────────────► │  Proxy 热路径（零 DB 访问）     │
│ (OpenAI SDK │   /v1/chat/...     │  ┌───────────┐  ┌──────────┐ │
│  Anthropic  │                    │  │ 鉴权(内存) │→ │ 调度器(内存)│ │
│  SDK 等)    │                    │  └───────────┘  └─────┬────┘ │
└─────────────┘                    │  ┌───────────────────┴────┐ │
                                   │  │ 转发：SDK(openai-go/      │ │
                                   │  │ anthropic-sdk-go)模型映射/流式│ │
┌─────────────┐   Bearer admin     │  └───────────────────┬────┘ │
│ 管理端/CLI   │ ─────────────────► │  ┌───────────────────▼────┐ │
└─────────────┘   /admin/*         │  │ 用量采集 → 异步聚合/落库  │ │
                                   │  └────────────────────────┘ │
                                   └───────┬──────────┬──────────┘
                                           │ 变更失效   │ 快照/状态回写
                                    ┌──────▼──────────▼──────┐
                                    │  PostgreSQL（仅配置+用量） │
                                    └─────────────────────────┘
```

组件：`cmd/server`（装配）→ `internal/server`（chi 路由）→ `internal/handler`（admin）→ `internal/proxy`（热路径）→ `internal/scheduler`（调度）→ `internal/usage`（用量）→ `internal/repository`（ent 持久化）→ `internal/ent`（生成的 ORM）。

**数据流（AI 请求）**：客户端 → 鉴权（内存 key hash map）→ 调度器选号（内存快照，格式硬匹配 + 模型偏好）→ 转发（SDK 调用 + 模型映射 + 流式重序列化）→ 用量采集 → 异步落库与聚合。整条路径无同步 DB 调用。

## 4. 数据模型（ent schema）

### 4.1 template（厂商模板）

| 字段 | 类型 | 说明 |
|---|---|---|
| name | string, unique | 模板名 |
| base_url | string | 上游网关地址，**含版本段**（如 `https://api.openai.com/v1`、`https://api.anthropic.com/v1`） |
| request_format | enum | **默认格式**（必填）：`openai-chat` / `openai-responses` / `anthropic`；决定所用 SDK 客户端与 429 解析语义（§6.1）。v1 仅支持这三种，gemini 等后续扩展 |
| models | []string | 可服务模型名；仅用于选号偏好，不校验。与 `model_formats` 的 key 取并集 |
| model_formats | map[model]format, 可选 | **模型级格式覆盖**：空值 = 用默认格式。`format_for(m) = model_formats[m] ?? request_format`；key 自动计入可服务模型集合 |
| model_mapping | map[string]string | 模型映射（入→出）；转发时仅改写 model 字段，命中才改写 |
| created_at / updated_at | time | |

### 4.2 account（账号）

刻意瘦身——拒绝 sub2api 式 40+ 字段 god-object。**单一暂停机制 + 单一状态位**：

| 字段 | 类型 | 说明 |
|---|---|---|
| name | string | 展示名 |
| template_id | FK → template | 所属模板 |
| upstream_key | string | 上游真实 API key |
| status | enum | `active` / `unhealthy` / `429` / `disabled`；disabled 仅 admin 手动切换，永不调度 |
| cooldown_until | *time | 唯一暂停机制：err/429 的冷却到期时间；过期自动回 active（惰性检查） |
| weight | int | 调度权重，默认 100 |
| max_concurrency | int | 并发上限，默认取全局配置 `scheduler.default_max_concurrency` |
| last_error | *string | 最近一次错误信息（admin 排查用） |
| last_used_at | *time | 最近调度时间（统计展示用） |
| created_at / updated_at | time | |

无：Concurrency/LoadFactor 双字段（并发数在内存计数）、RateLimitResetAt/OverloadUntil/TempUnschedulableUntil 多暂停机制（统一并入 cooldown_until）、JSON map 凭据字段、多重分组表示（分组关系只走 N:M 边）。

### 4.3 group（分组）

| 字段 | 类型 | 说明 |
|---|---|---|
| name | string, unique | 分组名 |
| key_hash | string | 客户端 key 的 SHA-256 哈希（存哈希不存明文） |
| key_prefix | string | 展示用前缀（`gk-xxxx…`），`POST /admin/groups/{id}/rotate-key` 轮换 |
| created_at / updated_at | time | |

关系：`Group N—M Account`（ent 边，`group_accounts` 中间表）。**分组与模板无任何关联**。

### 4.4 usage_log（请求明细）

| 字段 | 类型 | 说明 |
|---|---|---|
| request_id | string | 网关生成的 UUID |
| group_id / account_id / template_id | FK | 空值允许（鉴权失败等前置错误无账号） |
| model | string | 请求中的模型名（映射前） |
| mapped_model | *string | 映射后实际发往上游的模型名 |
| format | enum | 模板请求格式 |
| status_code | int | 最终返回客户端的码 |
| error_type | enum | `none` / `429` / `4xx` / `5xx` / `network` / `auth` / `no_account` / `abort` |
| latency_ms | int | 网关内总耗时 |
| prompt_tokens / completion_tokens / total_tokens | int | 采集到的用量（采不到为 0） |
| created_at | time | |

索引：`(created_at)`、`(group_id, created_at)`、`(account_id, created_at)`。保留期可配置，默认 30 天，janitor 定期清理。

### 4.5 usage_stat（预聚合）

bucket key = `(小时桶, group_id, account_id, template_id, model, is_error)`：

| 字段 | 说明 |
|---|---|
| bucket_time | 对齐到小时 |
| 各维度 id + is_error | |
| request_count / error_count | 请求数、错误数 |
| prompt_tokens / completion_tokens / total_tokens | 累计用量 |
| total_latency_ms | 累计耗时（供均值） |

内存聚合 → 每 10s 批量 UPSERT（`ON CONFLICT DO UPDATE`）。查询走这张表（可再按日二次聚合），明细表只留近 30 天，聚合表长留。

## 5. 调度器（internal/scheduler，内存优先）

### 5.1 状态机

```
active ──429响应──► 429 (cooldown = 固定 cooldown_429，默认 30s)
  ▲                    │
  │    cooldown 到期   ▼
  └────────────────────┘
active ──5xx/网络错误/超时──► unhealthy (cooldown = 指数退避 5s×2ⁿ，上限 5min)
  ▲                              │
  └──────── 成功即重置 ────────────┘
disabled：仅 admin 切换，永不调度
```

- **429 判定**：上游返回 429 → 进入 429 状态，冷却 = 固定 `scheduler.cooldown_429`（默认 30s，可配）。v0.1 不做上游 reset/Retry-After 头解析（终审裁定固定值语义安全；头解析留待规则引擎演进，见 §14）
- **unhealthy 判定**：5xx、连接错误、读超时、EOF 中断 → unhealthy 状态（指数退避冷却）；下次成功调用即重置退避与状态
- 冷却恢复是**惰性**的：选号时检查 `cooldown_until < now`，到期即视为可用，无需后台唤醒
- 状态变更：内存立即生效 + 异步批量回写 DB；启动时全量加载

### 5.2 选号算法（每个请求）

1. **硬过滤**：`format_for(请求model) == 请求路径决定的格式`（§6.1），其中 `format_for(m) = model_formats[m] ?? request_format`——模型有格式覆盖用覆盖值，否则用模板默认；模型在 `model_formats` 中声明但格式 ≠ 路径格式 → 该账号跳过。其余硬条件：`status ∈ {active}` 且 `cooldown_until` 已过 且 `并发数 < max_concurrency`
2. **模型偏好**：候选按「模板 models 或 mapping keys 是否含请求 model」分两档——tier1（命中档）优先，tier1 全不可用回落 tier2（未命中档）；不校验、不拒绝
3. **加权轮询序列**：选号 = 预生成加权轮询序列取用——快照重建时按 weight 归一化生成循环段并 shuffle（`weightedSeq`，长度上限 4096），请求时原子游标取模取用，**O(1)**；动态状态（冷却/禁用/并发满）在请求时逐候选检查跳过（状态每请求变化，不可预计算）
4. **并发槽**：选中后单遍单次 CAS 抢占并发计数 +1（atomic），失败不重试（返回 ErrNoAvailable，调用方重试）；请求结束（含流式断开、超时）释放

**语义变化（2026-08-06，预生成序列实现落定）**：
- **加权随机（每请求独立）→ 加权轮询（序列循环）**：长期分布更均匀（无随机突刺），短期分布等价（序列已 shuffle）
- **tier1 并发满时回落 tier2**：旧实现直接 ErrNoAvailable——可用性优先的刻意变化，可用性由调用方重试兜底
- **并发 CAS 竞争败者返回 ErrNoAvailable**：旧实现 CAS 自旋重试必成功——现为调用方重试语义，竞态窗口极小
- **未知模型回落默认格式桶**：请求 model 不在任何模板可服务集合内时，行为等价于默认格式的 tier2（`routeKey{format, ""}` 桶）

### 5.3 故障转移

- 触发条件：仅 429 / 5xx / 网络错误 / 超时；**4xx 不触发**（确定性错误原样透传）
- 流程：标记该账号状态 → 从候选集中换下一个 → 最多 N 次（默认 3）→ 全部失败返回最后一次上游错误（保留上游 status 与 body；连接级失败返回 502）
- 流式响应 200 已发出后无法转移（首字节之后不再 failover），记录 `error_type=abort`
- 429 风暴保护：单组短时间内连续 429 转移全部失败 → 返回 429 + Retry-After，不再逐个试

### 5.4 缓存与一致性（单实例）

- 组→账号快照：双 atomic.Value（groups / byID）整块换入换出，无锁读（快照含：账号基础信息、status、cooldown、weight、并发计数、EWMA）；InvalidateGroup 重建时 byID 与 groups 同步重建（防记账分裂，Task 4 修复）
- 失效链路：管理端变更（模板/账号/分组/绑定关系）→ service 调 `scheduler.InvalidateGroup(groupID)` 立即回读 DB 重建；定时全量同步兜底（默认 30s）
- 并发计数与 EWMA 仅内存（单实例语义）；重启丢失可接受（冷启动由 DB 重建）
- 重建失败仅 Warn，下个同步周期重试（30s 定时兜底）

## 6. 转发层（internal/proxy，基于官方 SDK）

协议处理不手写：上游调用一律走官方 SDK —— OpenAI 用 `github.com/openai/openai-go`（Chat Completions + Responses 两个客户端）、Anthropic 用 `github.com/anthropics/anthropic-sdk-go`。请求构建、SSE 解析、429/重试语义、usage 解析全部由 SDK 承担。

### 6.1 端点与格式绑定

网关只挂载三个端点，**路径决定请求格式**（客户端路径 = 上游路径，无路径改写、无格式转换）：

| 客户端端点 | 请求格式 | 上游 SDK 客户端 |
|---|---|---|
| `POST /v1/chat/completions` | openai-chat | openai-go `Chat.Completions` |
| `POST /v1/responses` | openai-responses | openai-go `Responses` |
| `POST /v1/messages` | anthropic | anthropic-sdk-go `Messages` |

其他路径直接 404（网关不再透传任意路径）。**格式匹配是调度器选号的硬过滤条件**：候选账号的模板 `format_for(请求model)`（模型级覆盖 ?? 默认格式）必须等于请求路径决定的格式，否则不可选（§5.2 第 1 步）。

每个模板懒构建 SDK 客户端（`pkg/aiclient.Factory` 按模板 ID 缓存，`option.WithBaseURL(template.base_url)` + 共享 `http.Client`；每模板最多 3 个客户端：chat / responses / anthropic）；账号差异经请求级鉴权头注入（openai：`Authorization: Bearer <upstream_key>`；anthropic：`x-api-key`，均 `option.WithHeader`），不为每个账号重建客户端。模板变更（create/update/delete）经 `Factory.InvalidateAll()` 重建（与 `scheduler.InvalidateAll` 组合接线，终审修复）。共享 Transport 调优：MaxIdleConnsPerHost、IdleConnTimeout、HTTP/2 尝试。

### 6.2 请求解析与保真

- 客户端 body（上限默认 4MB，超限 413）→ 对应 SDK 参数类型；未知字段不显式保留（网关层不设置 ExtraBody，以所用 SDK 版本解析行为为准，§14 边界）
- 不做任何跨格式转换：openai 端点只走 openai SDK，anthropic 端点只走 anthropic SDK

### 6.3 模型映射

- 仅当所选账号模板的 `model_mapping` 命中请求 model 时，改写参数 `Model` 字段，其余参数原样
- 映射 key 为请求侧模型名，value 为上游期望值（如 `"gpt-4" → "gpt-4-2026-01-01"`）
- 无映射命中则 model 原样透传

### 6.4 流式

- SDK 流式迭代（`*ssestream.Stream`：`Next()/Current()/Err()`）逐事件 → 网关重序列化为 SSE 帧（`data: {json}\n\n`，结束 `data: [DONE]`；anthropic 流另加 `event: <type>` 行——官方 SDK 按 event 行分发，Task 8 修复）；非流式 → SDK 完整响应原样 JSON 回写
- 客户端断开 → ctx 取消 → 上游流随之中断；首字节后中断不重试，记 `error_type=abort`
- 转发的请求头仅由 SDK 按协议生成（Authorization/x-api-key/anthropic-version 等），不逐头透传

### 6.5 用量采集

- SDK 响应类型自带 usage 字段直接取，零扫描开销：chat/responses 流式取末事件 usage；anthropic 流式 input_tokens 取 message_start 事件、output_tokens 取 message_delta 事件（终审修复）
- 开关 `proxy.usage_capture`（默认 on）；响应字节不被改写

## 7. 日志与用量

### 7.1 zap 日志级别规范（pkg/logx 包装，业务代码不直接碰 zap）

| 级别 | 内容 | 生产（默认 warn）表现 |
|---|---|---|
| Debug | 每请求追踪：收到请求、命中分组、选中账号、上游响应、用量采集结果 | 不输出 |
| Info | 服务启停、管理端配置变更 | 不输出 |
| Warn | 账号状态流转（标记 429/err、冷却开始/恢复）、故障转移触发、DB 回写失败 | 输出 |
| Error | 配置错误、DB 连接失败、启动失败等不可恢复错误 | 输出 |

- `logx` API：`Debug/Info/Warn/Error` + `With(fields)`；logger 以构造参数注入（不走 context）
- 级别来源 `log.level`（debug/info/warn/error），默认 `warn`，环境变量覆盖；JSON 编码，stdout（生产由部署侧收日志）
- **业务数据与日志级别隔离**：`usage_log` 落库不受日志级别影响（Debug 追踪行不输出 ≠ 业务明细不落库）

### 7.2 用量管线

请求完成 → 组装 `usage_log` 记录 + 内存聚合增量（永不失真）→ 推入有界 channel（16384）→ 批量写（默认 500 条或 500ms 先到先写）→ 明细落库 + 10s 预聚合 UPSERT。**饱和时阻塞反压（用户决策 2026-08-05：不得丢数据）**，由 max_inflight 过载保护兜底；落库失败 Warn 日志 + 丢弃该批（DB 故障降级，不重试）。优雅退出排空（仅 DB 不可达时超时丢弃 + Warn）。

## 8. 管理 API（`/admin/*`，Bearer admin token）

| 方法/路径 | 说明 |
|---|---|
| `POST/GET /admin/templates`、`GET/PUT/DELETE /admin/templates/{id}` | 模板 CRUD |
| `POST/GET /admin/accounts`、`GET/PUT/DELETE /admin/accounts/{id}` | 账号 CRUD；列表接口附运行时视图（当前并发、冷却剩余、err_rate） |
| `POST/GET /admin/groups`、`GET/PUT/DELETE /admin/groups/{id}` | 分组 CRUD |
| `PUT /admin/groups/{id}/accounts` | 全量设置成员 `{account_ids: [...]}` |
| `POST /admin/groups/{id}/rotate-key` | 轮换客户端 key（返回新 key，只此一次明文） |
| `GET /admin/logs` | 明细分页：group_id/account_id/model/status_code/error_type/时间范围 |
| `GET /admin/stats` | 聚合：granularity=hour\|day、维度 group/account/model、时间范围、字段（请求数/错误数/各 token/均值耗时） |

管理端认证：静态 admin token（配置 `admin.token`），Bearer 校验；与 AI 请求认证完全隔离。所有 admin 写操作记 Info 日志。

## 9. AI 请求 API

- 挂载：仅三个端点 `POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`（格式由路径决定，§6.1）；其余路径 404
- 认证：`Authorization: Bearer <group_key>`；SHA-256 比对内存哈希表；失败 401
- 网关自身错误码：401 无效 key；404 未知路径/组内无该格式账号；413 请求体超限；429 组内无可用账号（含 Retry-After）；502 全部故障转移失败（连接级）；其余透传上游状态码与 body
- 不校验模型（模型仅影响选号偏好，错误由上游返回）

## 10. 吞吐量与性能设计（W 级并发）

### 10.1 目标与量化预算

| 指标 | 目标 | 依据 |
|---|---|---|
| 并发在途请求 | **≥ 10k 单实例** | 流式场景网络等待为主，CPU 瓶颈小；goroutine/连接开销可控 |
| 完成率支撑 | ≥ 1k 行/s 日志写入 | 10k 并发 × 均时 30s ≈ 333 完成/s，留 3x 余量 |
| 内存预算 | < 2GB | 10k × (goroutine 栈 ~10KB + SDK 请求/响应对象 ~50-100KB + SSE bufio 2×8KB) ≈ 1.2-1.8GB 量级 |
| FD/句柄 | < 30k | 10k 入 + 10k 出 + 杂项；部署需调 `ulimit -n` 65535 |
| 首字节延迟增量 | P99 < 50ms | 转发开销（选号+SDK+解码）应低于此 |

### 10.2 连接层（首要瓶颈）

**上游 Transport（共享单例，httpx 包构造）**：

| 参数 | 值 | 理由 |
|---|---|---|
| `MaxIdleConns` | 8192 | 支撑大并发复用 |
| `MaxIdleConnsPerHost` | 2048 | 默认 2 是 W 级并发第一杀手 |
| `MaxConnsPerHost` | 0（不限） | 流式独占连接，限制会饿死请求 |
| `IdleConnTimeout` | 90s | 与上游 keep-alive 对齐 |
| `DialTimeout` | 10s | 快速失败不积压 |
| `ForceAttemptHTTP2` | true | **HTTP/2 多路复用：单连接承载大量并发流，显著降低连接数与端口占用** |
| `MaxIdleConnsPerHost` 之上的流式连接 | — | 流式期间连接被占用，靠多实例连接自动补足 |

**部署要求（写进运维文档，Windows 同样适用）**：
- 临时端口预算：10k 出站连接 ≈ 10k 临时端口；Windows 默认动态端口区间仅 ~16k 个，需 `netsh int ipv4 set dynamicport tcp start=20000 num=44000` 扩宽；Linux 调 `ip_local_port_range` + `tcp_tw_reuse`
- FD：`ulimit -n 65535`（Linux）/ 对应句柄预算（Windows）
- 出站 TCP keep-alive 由 Transport 设置（默认开启，30s 探测）；流式防挂死用整体 backstop 超时 `proxy.upstream_stream_timeout`（默认 30m，计划偏差 #2：SDK 层无法注入 per-read 空闲超时）

**入站 http.Server**：`ReadHeaderTimeout`（10s，防 slowloris）、`MaxHeaderBytes`（1MB）、不设 `WriteTimeout`（流式必需）、keep-alive 开启。

### 10.3 锁与热路径（无锁读）

| 点 | 机制 |
|---|---|
| 调度快照读 | `atomic.Value` 整块读（无锁）；重建在后台执行后整体换入 |
| 并发计数 / EWMA / 冷却判定 | 全部 atomic 整型（EWMA 用原子 uint64 定点数） |
| 分组 key 鉴权 | RWMutex 保护的 map（key 数十级，读多写极少，无热点） |
| 选号 | O(1)（预生成序列取用 + 原子游标；序列在快照重建时生成），无跨账号全局锁 |
| 日志/统计 | 无锁 channel（有界）+ 聚合端单消费者 |

不变量：**热路径上不存在任何 per-request 互斥锁与 per-request DB 调用**；快照重建频率受限（变更失效 + 30s 定时），永不触发在请求路径上。

**落盘异步化原则（用户决策 2026-08-05，为高并发/高性能与后续扩展）：所有需落盘的热点路径一律经异步批量写 worker 处理**——有界 channel + 批量 flush（≥1000 行/s，§10.5）+ 不丢数据（饱和反压）；worker 生命周期由统一 worker 管理器（§7.4 扩展：`internal/worker`）装配。当前唯一落盘热路径是用量管线（§10.5）；例外：scheduler 状态回写（cooldown/EWMA）为 best-effort 批量写，允许丢弃——下一轮 30s DB 同步自愈重建，不属记账数据。后续新增落盘热路径（如限流状态持久化、审计日志）必须遵循本原则。

### 10.4 内存与 JSON

- 流式零缓冲：SDK 事件级迭代 → 逐事件写出，不缓存响应体；SSE 帧 buffer 用 `sync.Pool` 复用
- 请求体**单次解码**：SDK（Stainless 生成代码自带高性能自定义 JSON 层）解码为参数类型；网关自身不再引入第二遍 JSON 解析（模型映射只是改参数对象字段，usage 直接取自 SDK 类型）
- 请求体上限默认 4MB：10k 并发 × 均体 ~10-50KB 的内存压力可忽略；极端大 body 由上限兜底
- 健康检查盯堆内存 + goroutine 数 + FD 水位三个指标

### 10.5 用量管线（重算后的容量）

- 有界 channel（默认容量 16384，防背压打爆）
- **批量 500 行或 500ms 先到先写 → 支撑 ≥ 1000 行/s**（v0.2 的 200 行/2s 只有 100 行/s，按 10k 并发算差 3 倍，已修正）
- 聚合在内存先行（按 bucket 计数），10s 批量 UPSERT——聚合开销不随请求数放大
- **饱和策略（用户决策 2026-08-05：不得丢数据）**：管线满时阻塞反压（绝不丢数据，内存有界）——反压传导至请求路径，由 HTTP 层过载保护（max_inflight，§10.6）兜底
- 优雅退出排空（尽力而为；仅当 DB 不可达时超时丢弃并 Warn）

### 10.6 过载保护

- **全局在途上限** `proxy.max_inflight`（默认 50000）：原子计数，超限立即 429 + Retry-After（保护自身不被打死）
- 每分组 key 令牌桶限流 `limit.group_key_rpm`（默认 0 = 关闭，可配置启用）
- 已有：请求体上限 413、非流式上游超时、流式 backstop 超时（`upstream_stream_timeout`）
- 全部保护走快速路径（原子/桶），不引入额外锁

### 10.7 压测验证（验收标准，实施后回填实测值）

- tools/fakeupstream（模拟 30s SSE 流、latency、429/5xx fail 注入）+ tools/loadtest（并发档位）打满目标并发
- 验收：10k 并发流稳定运行 ≥ 5 分钟；P99 首字节延迟增量 < 50ms；内存 < 2GB；日志零丢失（验证管线容量）；FD 水位 < 30k；failover 场景（注入 429/5xx）不产生雪崩
- 压测基准数据回填本节的表（实施阶段产出）

#### 压测基准（2026-08-06 实测回填，Task 9；工具：tools/fakeupstream + tools/loadtest，详见 docs/superpowers/plans/loadtest-results.md）

机器：Windows 11 Pro（10.0.26100），16 逻辑核，Go 1.26.5。拓扑：loadtest → 网关 :18080 → fakeupstream :9100（100 chunks × 20ms/流）。

| 项 | 目标 | 实测 | 结论 |
|---|---|---|---|
| 并发流 | 10000 | 500（本机上限：Windows 监听 backlog≈200 突发即 RST + 动态端口 16,384 需 20k 套接字；网关非瓶颈，750-2,000 档网关无崩溃/泄漏） | ❌ 未达标（环境限制，需 Linux SOMAXCONN=4096 或提升后 Windows 复跑） |
| 时长 | 5m | 5m2s（500 并发档，71,069 请求） | ✅ |
| errs 占比 | <0.1% | 2/71,069 = 0.003% | ✅ |
| P99 首字节增量 | <50ms | 基线 <10ms → 500 档 100ms（+~90ms；avg 7.4ms） | ❌ 未达标（本机） |
| 内存 | <2GB | heap 峰值 ~70MB（healthz 采样） | ✅ |
| 日志丢失 | 0 | 0（/admin/logs 77,267 行 = 71,069 计数 + 150 基线 + ~6,048 预热估算，等式闭合） | ✅ |
| FD 水位 | <30k | 峰值 ~1,500 套接字（估算：500 入站 + 500 上游 + 500 压测端） | ✅ |
| failover 注入（429/5xx） | 无雪崩 | -fail429 注入 8,700 req / 0 errs；-fail500 注入 6,600 req / 0 errs；双账号全失败确定性 502、网关无崩溃/泄漏 | ✅ |

## 11. 配置（TOML + env 覆盖）

```toml
server = { addr = ":8080", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn" }              # debug/info/warn/error
admin = { token = "" }
db = { dsn = "", max_conns = 10 }
proxy = {
  max_body_size = 4194304,            # 请求体解析上限（SDK 需完整解析）
  max_inflight = 50000,               # 全局在途上限，超限立即 429（§10.6）
  upstream_timeout = "120s",          # 非流式上游超时；流式用空闲超时
  failover_attempts = 3,
  usage_capture = true,               # SDK 自带 usage，开关仅为统计开关
}
upstream = {                          # 共享 Transport（§10.2）
  max_idle_conns = 8192,
  max_idle_conns_per_host = 2048,
  idle_conn_timeout = "90s",
  dial_timeout = "10s",
  force_http2 = true,
}
limit = { group_key_rpm = 0 }         # 每分组 key 令牌桶限流，0 = 关闭（§10.6）
scheduler = {
  default_max_concurrency = 8,
  cooldown_429 = "30s",
  backoff_base = "5s",
  backoff_max = "5m",
  sync_interval = "30s",
}
usage = {
  batch_size = 500,                   # §10.5 容量重算：≥1000 行/s 支撑
  flush_interval = "500ms",
  log_retention_days = 30,
  stats_flush_interval = "10s",
}
```

## 12. 目录结构（标准 Go 布局）

```
go-proxy-mini/
├── cmd/server/main.go          # 装配：配置/DB/logger/http
├── pkg/
│   ├── logx/                   # zap 包装（级别/JSON/With）
│   ├── httpx/                  # 共享 http.Client/Transport 构造
│   └── cryptox/                # SHA-256 哈希/校验、随机 key 生成
├── internal/
│   ├── config/                 # TOML+env 加载
│   ├── domain/                 # 枚举/错误/公共类型
│   ├── ent/schema/             # ent schema 定义
│   ├── repository/             # ent 持久化层
│   ├── service/                # 模板/账号/分组 CRUD + 失效通知
│   ├── scheduler/              # 状态机/选号/并发/冷却/快照
│   ├── proxy/                  # 鉴权/转发/流式/用量采集
│   ├── usage/                  # 明细+聚合管线、janitor
│   ├── handler/                # admin HTTP handlers
│   ├── worker/                 # 统一 worker 管理器（生命周期编排/panic 捕获）
│   └── server/                 # chi 路由、中间件、优雅退出
├── tools/
│   ├── fakeupstream/           # 压测假上游（latency/SSE/fail 注入）
│   └── loadtest/               # 压测器（并发档位/healthz/计数校验）

无独立迁移目录：启动时 ent 自动迁移（repository.New(drv, migrate=true)）
```

## 13. 错误处理 / 测试

**性能**：见 §10 吞吐量与性能设计（W 级并发）——热路径零 DB、零 per-request 锁、atomic.Value 快照、共享连接池、批量写、zap 异步。

**测试策略**：
- 单元：调度器状态机/冷却退避/选号/EWMA、模型映射、格式↔路径绑定
- 端到端：httptest 假上游 + SDK `WithBaseURL` 指向它（含流式 SSE、429+reset 头、5xx、断连），覆盖故障转移与并发槽、SSE 帧重序列化保真
- 仓库层：pgxmock（经 ent v0.14 的 dialect.Driver 薄适配器桥接，无真实 DB；用户决策 2026-08-05）；生产/压测验证用 Docker postgres:16
- **压测验收（§10.7）**：tools/fakeupstream + tools/loadtest（自研，用户决策替代 k6），10k 并发流 ≥ 5 分钟；P99 首字节增量 < 50ms；内存 < 2GB；日志零丢失；failover 无雪崩；基准已回填 §10.7（2026-08-06）
- 断言用 testify（require 前置、assert 独立）；golangci-lint 全绿；调度器与转发核心逻辑不依赖 DB，可全内存测

## 14. 未来演进（v1 不做）

- 多实例调度（内存快照 → Redis 版）
- 请求格式转换、计费/配额、前端管理 UI、WebSocket 透传
- 更多请求格式（gemini 等）：request_format 为可扩展枚举，接入 = 挂载新端点 + 接入对应官方 SDK
- 保真度边界：客户端 body 经 SDK 参数类型解码（未显式设置 ExtraBody）；SDK 不覆盖的极端字段存在丢失可能，以所用 SDK 版本行为为准——这是"协议处理用现成库"的既定取舍
- **可编排状态管理（规则引擎）**：规则存 DB + 专用 worker 按内存规则调整账号状态（429/err/disabled + 冷却，事件驱动、可编排、可写）——见 `2026-08-05-orchestrated-state-rules-proposal.md`（DRAFT，待讨论）
