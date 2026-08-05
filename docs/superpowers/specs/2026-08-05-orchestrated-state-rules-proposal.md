# 可编排状态管理（规则引擎）——设计草案

> **状态：DRAFT — 未批准，不实施。** 用户 2026-08-05 提出想法，讨论未完成（4 个待确认点开放）。主计划执行完毕后（Task 1-9）再回到本草案讨论。本文档不产生任何任务。
> 相关：主设计 `2026-08-05-go-proxy-mini-design.md`（§4 账号状态机、§10.5 用量管线）、实施计划 `2026-08-05-go-proxy-mini.md`。

## 1. 动机（用户原话要点）

- 账号状态（429/err）应当**带冷却时间**（现状：cooldown_until 已是状态的组成部分）
- 状态调整**可编排**：规则写入数据库，使用的时候启动一个专用 worker；请求错误交给它，它根据**内存中的规则**按需调整状态
- 规则可以 **disabled 掉账号**（动作不止 429/err，还包括禁用）
- 规则应像 **Cloudflare 规则**一样：根据传给 worker 的字段，可编排，不同路径进行不同的状态设置

## 2. 目标

把 `scheduler.MarkResult` 的硬编码状态机（429→30s 冷却、5xx/网络→指数退避、成功→重置）替换为**规则驱动**的状态调整器：错误/成功事件 → 专用 worker → 按内存规则表（priority 顺序，第一条命中生效）执行"状态 + 冷却"设置。规则存 DB、可写、启动加载进内存。

## 3. 规则模型

**事件字段**（投递给 worker 的输入）：
```
account_id, template_id, group_id, model,
result (429 | 5xx | network | ok),
http_status (实际状态码), error_message, occurred_at
```

**匹配器（when）**：字段等值 + `error_message contains` 子串 + 窗口计数（worker 内存中维护的派生字段：最近 N 秒内该账号的 429 次数等）。

**动作（then）**：`{ status: active|err|429|disabled, cooldown: 时长 }` —— 即"带冷却时间的状态设置"。

**语义**：按 priority 升序匹配，第一条全命中生效。

## 4. 用户示例规则

```json
// 规则1（priority 10，先匹配；优先级在默认规则前面）
{ "name": "模板123不健康", "priority": 10,
  "when": { "template_id": 123, "http_status": 503, "error_message_contains": "unhealthy" },
  "then": { "status": "err", "cooldown": "1h" } }

// 默认规则（priority 100）
{ "name": "默认：窗口内429过多", "priority": 100,
  "when": { "window_seconds": 10, "count_429_ge": 3 },
  "then": { "status": "429", "cooldown": "5h" } }
```

表达力覆盖：按账号/模板/分组限定作用域、按错误码+信息子串匹配、按窗口频率升级惩罚、连续 N 次 5xx → disabled。

## 5. 关键设计建议：双轨（快速路径 + 规则引擎）

时序问题（错误发生后冷却必须尽快生效，否则错误账号可能被再次选中）：

- **快速路径**（同步，热路径）：任何 429/5xx/网络错误**立即**设置短冷却（30s，现行为）——保证下一个选号立刻看不到错误账号
- **规则引擎**（异步 worker）：同一事件投递进有界 channel，按规则评估；命中 → **升级**冷却/改状态（如 5h、disabled）。聚合惩罚（窗口计数）必须异步——窗口计数在 worker 内存中

规则引擎的输入是"发生了什么"，输出是"状态怎么升级"；错误账号永远有即时保护（时序安全）。

## 6. 待确认点（讨论未完成，4 个开放问题）

1. **双轨**：同意快速路径 30s + 规则引擎升级？还是完全规则化（连立即冷却也由规则决定，代价是几 ms 窗口期）？
2. **窗口计数作用域**：per 账号？（默认规则未指定 template_id，理解是 per 账号全量）还是 per 账号+模板？
3. **动作边界**：动作只有 `{status, cooldown}`（最多加 reset 计数器）？还是允许调 weight？
4. **规则 CRUD 归属**：规则表 + 管理 API `GET/POST/PUT/DELETE /admin/rules`（复用 invalidate 重载模式）；建议排 Task 8 之后作为独立任务，不塞进当时的 Task 7。

## 7. 实施影响（若批准）

- 新表 `rule`（id、name、enabled、priority、scope、when JSON、then JSON、timestamps）
- 新包 `internal/rule`（规则加载/评估/窗口计数），规则 worker 挂统一 worker 管理器（Global Constraints #5）
- `scheduler.MarkResult` 改造：快速路径保留 + 事件投递（与现有 writeback 模式一致，热路径不变重）
- 默认种子规则 = 现有行为等价（429 立即冷却等），保证升级前后行为不变
- 管理 API 扩展 `/admin/rules`（CRUD + invalidate 重载）
- 与"落盘热路径异步化原则"（Global Constraints #6）一致：规则本身不落盘，状态回写已异步批量

## 8. 决策记录

- 2026-08-05：想法提出（可编排状态管理），讨论两轮；用户指示先落文档，主计划完成后（Task 1-9）再讨论；本草案 DRAFT，未批准，不实施
