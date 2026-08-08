# Admin API 文档

管理端 API（配置模板 / 账号 / 分组 / 兑换码 / 日志 / 统计）。与 AI 推理请求（`/v1/*`，模型请求）相对，本组接口为网关管理面，不对上游转发。

## 通用约定

- **Base URL**：`http://<gateway>/admin`
- **认证**：所有请求必须带 `Authorization: Bearer <admin_token>`（`config.toml` 的 `admin.token`，或环境变量 `GPM_ADMIN_TOKEN`）。缺失或错误返回 `401`。
- **Content-Type**：请求体与响应均为 `application/json`（`rotate-key` 等无请求体操作除外）。
- **错误格式**：非 2xx 响应体为 `{"error": "<消息>"}`。404 的消息含缺失资源 id（如 `service: not found: id=999 missing`），便于定位。
- **ID**：路径参数 `{id}` 为模板/账号/分组的整数 ID。
- **更新语义**：`PUT` 为**全量替换**——请求体中的字段整体覆盖，未提供的字段清零（仅提供部分字段的 `PUT` 会把缺失字段重置为空/零值）。批量 `batch-update` 为**部分更新**（只改 `fields` 中提供的字段）。
- **列表响应**：三个列表端点（templates / accounts / groups）统一返回 `{"total": <满足筛选的总数>, "rows": [...]}`，支持 `limit` / `offset` 分页、筛选参数与白名单 `sort` / `order` 排序（非法 `sort` / `order` → `400`）。

## 枚举值

| 枚举 | 取值 |
|---|---|
| `format`（请求格式） | `openai-chat` / `openai-responses` / `anthropic` |
| `status`（账号） | `active` / `unhealthy` / `429` / `disabled` |
| `error_type`（日志） | `none` / `429` / `4xx` / `5xx` / `network` / `auth` / `no_account` / `abort` |
| `type`（兑换码） | `balance`（充值余额，最小单位分）/ `concurrency`（加并发数）/ `temp_balance`（临时余额，兑换后资源到期） |
| `status`（兑换码） | `active` / `disabled`（不可编辑，仅可失效） |

---

## 模板 Templates

模板定义上游厂商：base_url、支持的请求格式集合、可服务模型集合、格式级模型覆盖与模型映射。

> **破坏性变更**：`default_format` 已移除（由必填的 `supported_formats` 数组取代），`model_formats` 已移除（由反转为按格式组织的 `format_models` 取代）。旧数据未迁移，使用旧字段的客户端需按下方新结构调整。

### 创建模板

`POST /admin/templates`

请求体：

```json
{
  "name": "openai-main",
  "base_url": "https://api.openai.com",
  "supported_formats": ["openai-chat", "openai-responses"],
  "models": ["gpt-4o", "gpt-4o-mini"],
  "format_models": { "openai-responses": ["gpt-4o-mini"] },
  "model_mapping": { "gpt-4o": "gpt-4o-2024-11-20" }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 模板名 |
| `base_url` | string | ✅ | 上游**根**地址（**不含 `/v1`**——`/v1` 是协议细节，网关按格式追加：openai 系拼 `/v1/...`，anthropic SDK 自带 `v1` 前缀；含尾 `/v1` 会被拒（`400`）） |
| `supported_formats` | string[] | ✅ | 支持的请求格式枚举数组（至少 1 项，项枚举见上；重复/非法枚举返回 `400`） |
| `models` | string[] | 否 | 可服务模型名集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集、模型必须是 `models` 子集（否则 `400`）；未配置的格式 = 全部 `models` |
| `model_mapping` | object | 否 | 模型映射：`{客户端模型名: 上游实际模型名}` |

响应 `200`：创建后的模板对象（字段为大写，见下方模板对象结构）。

### 模板对象结构（响应）

```json
{
  "ID": 1,
  "Name": "openai-main",
  "BaseURL": "https://api.openai.com",
  "SupportedFormats": ["openai-chat", "openai-responses"],
  "Models": ["gpt-4o", "gpt-4o-mini"],
  "FormatModels": { "openai-responses": ["gpt-4o-mini"] },
  "ModelMapping": { "gpt-4o": "gpt-4o-2024-11-20" },
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

> 注意：响应字段为 **Go 默认大写命名**（`ID` / `Name` / `BaseURL`…），请求字段为 snake_case。`Models` / `FormatModels` / `ModelMapping` 为 `null` 时表示空。

### 模板列表

`GET /admin/templates`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数（≤0 视为 20） |
| `offset` | int | 0 | 分页偏移（<0 视为 0） |
| `sort` | string | `id` | 白名单：`id` / `name` / `base_url` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |

响应 `200`：

```json
{
  "total": 2,
  "rows": [
    { "ID": 1, "Name": "openai-main", "BaseURL": "https://api.openai.com", "...": "模板对象字段" }
  ]
}
```

### 模板批量操作

`POST /admin/templates/batch-delete`

请求体：`{"ids": [1, 2, 3]}`（1–100 条，重复 id 自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 3}`（按去重后的条数计）；事务全成或全败 |
| `400` | `ids` 为空或超过 100 条；`fields` 为空（batch-update） |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败，不部分删除） |

`POST /admin/templates/batch-update`

请求体：`{"ids": [1, 2], "fields": {"name": "renamed"}}`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 否 | 模板名（非空） |
| `base_url` | string | 否 | 上游地址（合法 URL） |
| `supported_formats` | string[] | 否 | 支持的请求格式枚举数组（至少 1 项，枚举见上；重复/非法枚举 → `400`） |
| `models` | string[] | 否 | 可服务模型集合 |
| `format_models` | object | 否 | 格式级模型覆盖：`{格式: [模型名]}`，key 必须是 `supported_formats` 子集（同批提供时校验）、模型必须是 `models` 子集 |
| `model_mapping` | object | 否 | 模型映射 |

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 2}`。

### 模板其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/templates/{id}` | 单个模板 | `200`：模板对象；`404` 不存在 |
| `PUT /admin/templates/{id}` | 全量更新（字段同创建） | `200`：更新后模板对象 |
| `DELETE /admin/templates/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id）；仍被账号引用时返回 `500`（DB 外键约束） |

> 模板变更（含 base_url / supported_formats / format_models / model_mapping）通过 invalidate 回调即时生效于调度器快照与上游 SDK 客户端（无需重启）。

---

## 账号 Accounts

账号绑定模板并持有上游 API key，是调度的基本单元。

### 创建账号

`POST /admin/accounts`

```json
{
  "name": "a1",
  "template_id": 1,
  "upstream_key": "sk-xxx",
  "status": "active",
  "weight": 100,
  "max_concurrency": 8
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 账号名 |
| `template_id` | int | ✅ | 所属模板 ID |
| `upstream_key` | string | ✅ | 上游 API key（发送给上游鉴权） |
| `status` | string | 否 | 初始状态，默认 `active` |
| `weight` | int | 否 | 选号权重（预生成加权序列，权重比决定命中比例），默认 0 |
| `max_concurrency` | int | 否 | 账号并发上限；`0` 时使用调度器 `default_max_concurrency` |

响应 `200`：账号对象（含嵌套 `Template`）。

### 账号对象结构（响应）

```json
{
  "ID": 1,
  "Name": "a1",
  "TemplateID": 1,
  "Template": { "...": "嵌套模板对象" },
  "UpstreamKey": "sk-xxx",
  "Status": "active",
  "CooldownUntil": null,
  "Weight": 100,
  "MaxConcurrency": 8,
  "LastError": null,
  "LastUsedAt": null,
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

### 账号状态机

| 状态 | 进入条件 | 恢复 |
|---|---|---|
| `active` | 创建/成功请求 | — |
| `unhealthy` | 上游 5xx / 连接级错误 / 流中断 | 指数退避冷却（5s×2ⁿ，上限 5min）后自动恢复 |
| `429` | 上游 429 | 固定冷却（默认 30s）后自动恢复 |
| `disabled` | 管理端手动设置 | 手动改回（`PUT`） |

> `unhealthy` / `429` 为**健康轴**（自动退避），`disabled` 为**启用轴**（手动）。管理端 `PUT` 设 `disabled` 后，在途请求完成不会覆盖回写（防复活守卫）。

### 账号列表（含运行时视图）

`GET /admin/accounts`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `sort` | string | `id` | 白名单：`id` / `name` / `template_id` / `status` / `cooldown_until` / `weight` / `max_concurrency` / `last_used_at` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |
| `status` | string | — | 多值过滤，逗号分隔（如 `active,disabled`）；非法枚举 → `400` |
| `template_id` | int | — | 按所属模板过滤 |

响应 `200`：`{"total": <总数>, "rows": [...]}`。每个元素为账号对象 + 三个运行时字段（来自调度器内存快照）：

```json
{
  "total": 1,
  "rows": [
    {
      "ID": 1,
      "Name": "a1",
      "...": "账号对象字段",
      "concurrency": 3,
      "err_rate": 0.05,
      "err_count": 2
    }
  ]
}
```

| 运行时字段 | 说明 |
|---|---|
| `concurrency` | 当前在途并发数（内存计数） |
| `err_rate` | 错误率 EWMA（0.0–1.0，定点 1e6 缩放后输出） |
| `err_count` | 连续错误计数（决定退避指数） |

### 账号批量操作

`POST /admin/accounts/batch-delete`

请求体：`{"ids": [1, 2]}`（1–100 条，重复自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 2}`（按去重后的条数计）；事务全成或全败 |
| `400` | `ids` 为空或超过 100 条 |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败） |

`POST /admin/accounts/batch-update`

请求体：`{"ids": [1], "fields": {"status": "disabled", "weight": 50}}`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 否 | 账号名（非空） |
| `template_id` | int | 否 | 所属模板 ID（>0） |
| `upstream_key` | string | 否 | 上游 API key（非空） |
| `status` | string | 否 | 状态枚举（见上；非法枚举 → `400`） |
| `weight` | int | 否 | 选号权重（≥0） |
| `max_concurrency` | int | 否 | 并发上限（≥1） |

`fields` 必须至少提供一字段；`ids` 中任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 1}`。

### 账号其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/accounts/{id}` | 单个账号 | `200`：账号对象；`404` 不存在 |
| `PUT /admin/accounts/{id}` | 全量更新（字段同创建；`status` 可改为 `disabled` 禁用） | `200`：更新后账号对象 |
| `DELETE /admin/accounts/{id}` | 删除 | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id） |

---

## 分组 Groups

分组持有客户端 key（`gk-` 前缀），N:M 绑定账号。AI 请求以分组 key 鉴权，请求在组内账号中调度。

### 创建分组

`POST /admin/groups`

```json
{ "name": "bench" }
```

响应 `200`：

```json
{
  "group": { "ID": 1, "Name": "bench", "KeyHash": "<sha256>", "KeyPrefix": "gk-2d61", "CreatedAt": "...", "UpdatedAt": "..." },
  "key": "gk-2d61..."
}
```

> `key` 为**明文分组 key，仅此一次返回**（数据库只存 SHA-256 哈希）。遗失需 `rotate-key` 轮换。

### 分组列表

`GET /admin/groups`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `sort` | string | `id` | 白名单：`id` / `name` / `created_at` / `updated_at`；非法值 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |
| `name` | string | — | 名称模糊匹配（不区分大小写） |

响应 `200`：`{"total": <总数>, "rows": [分组对象...]}`（不含明文 key）。

### 分组批量操作

`POST /admin/groups/batch-delete`

请求体：`{"ids": [1, 2]}`（1–100 条，重复自动去重）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deleted": 2}`；事务全成或全败；删除前先清理各组注册 key |
| `400` | `ids` 为空或超过 100 条 |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（全败） |

`POST /admin/groups/batch-update`

请求体：`{"ids": [1], "fields": {"name": "renamed"}}`（`name` 非空，`fields` 必须提供）。任一 id 不存在 → `404`（事务全败）。成功 `200`：`{"updated": 1}`。

### 分组其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/groups/{id}` | 单个分组 | `200`：分组对象 |
| `PUT /admin/groups/{id}` | 重命名 | `200`：更新后分组对象 |
| `DELETE /admin/groups/{id}` | 删除（先删注册 key 再删 DB） | `200`：`{"deleted": true}`；`404` 资源不存在（消息含缺失 id） |
| `PUT /admin/groups/{id}/accounts` | 绑定账号集合 | 请求体 `{"account_ids": [1, 2, 3]}`；`200`：`{"updated": true}` |
| `POST /admin/groups/{id}/rotate-key` | 轮换分组 key | `200`：`{"key": "gk-<新明文>"}`（旧 key 立即失效） |

> `setGroupAccounts` 为**全量替换**绑定关系（传空数组清空）。变更即时触发调度器快照重建（invalidate）。

---

## 日志与统计

### 查询用量日志

`GET /admin/logs?limit=20&offset=0&group_id=1&account_id=2&model=gpt-4o&status_code=200&error_type=none&from=2026-08-06T00:00:00Z&to=2026-08-06T23:59:59Z`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数 |
| `offset` | int | 0 | 分页偏移 |
| `group_id` | int | — | 按分组过滤 |
| `account_id` | int | — | 按账号过滤 |
| `model` | string | — | 按模型名过滤 |
| `status_code` | int | — | 按最终状态码过滤 |
| `error_type` | string | — | 按错误类型过滤（枚举见上） |
| `from` / `to` | RFC3339 | — | 时间范围过滤 |

响应 `200`：

```json
{
  "total": 1234,
  "rows": [
    {
      "ID": 1,
      "RequestID": "uuid",
      "GroupID": 1,
      "AccountID": 2,
      "TemplateID": 1,
      "Model": "gpt-4o",
      "MappedModel": "gpt-4o-2024-11-20",
      "Format": "openai-chat",
      "StatusCode": 200,
      "ErrorType": "none",
      "LatencyMS": 125,
      "PromptTokens": 10,
      "CompletionTokens": 20,
      "TotalTokens": 30,
      "CreatedAt": "2026-08-06T10:00:00Z"
    }
  ]
}
```

### 查询用量统计

`GET /admin/stats?from=2026-08-06T00:00:00Z&to=2026-08-06T23:59:59Z&granularity=day&group_id=1&account_id=2&model=gpt-4o`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `from` / `to` | RFC3339 | 近 24 小时 | 时间范围 |
| `granularity` | `hour` / `day` | `day` | 聚合粒度（`day` 为 UTC 日对齐，`hour` 为 UTC 小时对齐） |
| `group_id` / `account_id` / `model` | int / int / string | — | 维度过滤 |

响应 `200`：统计行数组（按粒度对齐的桶）：

```json
[
  {
    "BucketTime": "2026-08-06T00:00:00Z",
    "GroupID": 1,
    "AccountID": 2,
    "TemplateID": 1,
    "Model": "gpt-4o",
    "IsError": false,
    "RequestCount": 100,
    "ErrorCount": 0,
    "PromptTokens": 1000,
    "CompletionTokens": 2000,
    "TotalTokens": 3000,
    "TotalLatencyMS": 12500
  }
]
```

> 统计由用量管线异步预聚合（批量 upsert），查询结果可能有秒级延迟。

---

## 规则 Rules

规则引擎驱动账号状态管理（替代旧硬编码状态机）：请求结果事件按 `priority` 升序逐规则首中匹配，命中即执行 `then` 动作（状态/冷却/权重）。规则变更（增删改）即时生效（自动触发引擎重载）。

### 规则模型

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 规则名（唯一） |
| `priority` | int | 优先级（唯一，升序匹配、首中即停） |
| `enabled` | bool | 缺省 `true`；`false` = 停用（不参与匹配） |
| `when` | object | 匹配条件（字段白名单，未知字段拒绝 400） |
| `then` | object | 动作（`status`/`cooldown`/`weight` 至少一个） |

`when` 字段（全部可选，nil = 不限）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `kind` | string | `ok` / `429` / `error`（nil = 任意事件） |
| `http_status` | int | 上游状态码等值匹配 |
| `error_message_contains` | string | 错误消息子串匹配 |
| `account_id` / `template_id` / `group_id` | int | 维度等值匹配（nil = 不限） |
| `model` | string | 模型等值匹配 |
| `window_seconds` | int | 统计窗口（≥1，缺省 60；固定粒度近似，误差 ≤ 一个粒度） |
| `count_429_ge` / `count_error_ge` / `count_ok_ge` / `count_total_ge` | int | 窗口内计数阈值（≥0） |
| `ratio_429_ge` / `ratio_error_ge` | float | 窗口内比例阈值（[0,1]，**必须配 `count_total_ge`**） |

`then` 字段（至少一个）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `status` | string | `active` / `unhealthy` / `429` / `disabled` |
| `cooldown` | string | 冷却时长（`time.ParseDuration` 可解析且 >0，如 `"30s"`、`"5h"`） |
| `weight` | int | 权重（0-100，变更立即重建该组选号序列） |

种子规则（规则表为空时启动自动写入）：`429 → status=429 + cooldown 30s`（priority 10）、`error → status=unhealthy + cooldown 5s`（priority 20）、`ok → status=active`（priority 30，无冷却）。删除全部规则后，下次引擎重载（任意规则 CRUD 或重启）会自动重新播种——规则表不会保持真空。

### 事件模型与匹配语义

- **事件来源**：每次请求结算（MarkResult）产生一个事件 `{kind, http_status, error_message, account_id, template_id, group_id, model, occurred_at, reset_at}`，经有界队列投递规则 worker 匹配（队列满时丢弃并告警，不阻塞请求路径）。
- **条件投递**：仅当规则集中存在 `when.kind` 为 `nil`（任意）或 `ok` 的规则时，`ok` 事件才进入匹配；否则 ok 事件直接被跳过（性能优化）。想用 ok 事件恢复状态，必须保留 kind=ok 或全匹配规则（种子规则自带 ok 规则）。
- **首中即停**：按 `priority` 升序逐规则匹配，首个命中即执行其 `then` 全部动作，不再继续。
- **命中不清零窗口**：计数窗口为滑动窗口，命中不重置计数（自然衰减）；统计窗口固定粒度近似，误差 ≤ 一个粒度。
- **冷却语义**：命中且 `then.cooldown` 提供 → `cooldown_until = 命中时刻 + cooldown`。`then` 只设 status 无 cooldown 时，事件自带 `reset_at`（如上游 `Retry-After`）作为冷却兜底。
- **OK 不清除冷却**：`ok` 事件命中只恢复状态（如 `active`），**不**清除既有的 `cooldown_until`——调度器在冷却期内仍抑制该账号，避免 429 风暴后立即被打满。
- **状态变更即时生效**：规则增删改自动触发引擎重载；`weight` 动作立即重建对应组的选号序列（无需等 sync 周期）。

### 创建规则

`POST /admin/rules`

```json
{
  "name": "escalate-on-5xx",
  "priority": 40,
  "enabled": true,
  "when": { "kind": "error", "count_error_ge": 5, "window_seconds": 60 },
  "then": { "status": "unhealthy", "cooldown": "30s" }
}
```

响应 `201`：创建后的规则（含 `id`/`created_at`/`updated_at`，`when`/`then` 原样返回）。

### 规则列表

`GET /admin/rules?enabled=true`（`enabled` 可选，缺省返回全部；priority 升序，无分页）

响应 `200`：`{"total": N, "rows": [...]}`。

### 更新规则

`PUT /admin/rules/{id}`——部分更新：未提供的字段保持原值（`when`/`then` 提供即整体替换）。

响应 `200`：更新后的规则；`404` 含缺失 id。

### 删除规则

`DELETE /admin/rules/{id}`——响应 `204`；`404` 含缺失 id。

`POST /admin/rules/batch-delete`——请求体 `{"ids": [...]}`（1-100 条，自动去重）；事务全成或全败；响应 `200` `{"deleted": N}`；`404` 消息含缺失 id。注意：批量删除后触发规则引擎重载，若规则表删空则下次重载自动重建种子规则。

### 错误语义

| 状态码 | 场景 |
|---|---|
| `400` | `when`/`then` 含未知字段、`kind` 非法、计数为负、`window_seconds` < 1、比例越界或缺 `count_total_ge`、`then` 无动作、`cooldown` 非法、`weight` 越界 |
| `409` | `priority` 或 `name` 唯一冲突 |

> 配置变更：`scheduler.cooldown_429` / `scheduler.backoff_base` / `scheduler.backoff_max` **已废弃**（不再参与任何决策，仅保留读取兼容）。429 冷却、错误退避与恢复节奏统一由规则引擎的种子规则与自定义规则接管。

## 兑换码 Redemption Codes

兑换码是资源发放的通用载体（Phase 5 计费前基础设施）：生成一批码 → 分发给用户 → 用户在 `/user/redemptions` 兑换 → 资源按码类型即时生效。管理面 5 个端点 + 用户面 2 个端点。

### 生成兑换码

`POST /admin/redemption-codes`

请求体：

```json
{
  "type": "balance",
  "value": 100,
  "remark": "618 活动",
  "expires_at": "2026-12-31T23:59:59+08:00",
  "resource_expires_at": "2027-01-15T00:00:00+08:00",
  "max_uses": 1,
  "count": 5
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | ✅ | 枚举：`balance` / `concurrency` / `temp_balance`；非法 → `400` |
| `value` | int64 | ✅ | 资源值（最小单位：分为 `balance` 的"分"、`concurrency` 为并发数）；`> 0`，否则 `400` |
| `remark` | string | 否 | 备注 |
| `expires_at` | datetime | 否 | 码**未兑换即过期**；缺省 = 永久；必须晚于当前时间（过去时间 → `400`） |
| `resource_expires_at` | datetime | 否 | 兑换后**资源到期**；`temp_balance` 必填且必须晚于当前时间，其余类型恒为 `null` |
| `max_uses` | int | 否 | 可兑换次数：`1` = 单次码（缺省）；`>1` = 多人码；`< 0` → `400` |
| `count` | int | 否 | 一次生成个数 `1–1000`（缺省 `1`）；`0` 或缺省 = 1；越界 → `400` |

响应 `200`：生成的完整码列表（`count` 个，码格式 `XXXXXX-XXXXXX`，字符集去易混淆的 `I/O/0/1`）。

```json
{
  "codes": [
    {
      "ID": 1,
      "Code": "JQVF2X-LD7SJQ",
      "Type": "balance",
      "Value": 100,
      "Remark": "618 活动",
      "ExpiresAt": "2026-12-31T23:59:59+08:00",
      "ResourceExpiresAt": null,
      "MaxUses": 1,
      "UsedCount": 0,
      "Status": "active",
      "CreatedBy": 0,
      "CreatedAt": "2026-08-08T10:00:00Z",
      "UpdatedAt": "2026-08-08T10:00:00Z"
    }
  ]
}
```

### 兑换码列表

`GET /admin/redemption-codes`

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 页码，**1-based**；缺省或 `< 1` 按 1（不报错） |
| `page_size` | int | 20 | 每页行数；越界（`< 1` 或 `> 100`）→ `400` |
| `type` | string | — | 筛选枚举：`balance` / `concurrency` / `temp_balance`；非法 → `400` |
| `status` | string | — | 筛选枚举：`active` / `disabled`；非法 → `400` |
| `sort` | string | `id` | 白名单：`id` / `code` / `type` / `value` / `max_uses` / `used_count` / `status` / `created_by` / `created_at` / `updated_at`；非法 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |

响应 `200`：`{"total": N, "rows": [兑换码对象]}`（增强分页范式，与 templates 等旧 `limit`/`offset` 端点不同）。

### 批量失效

`POST /admin/redemption-codes/batch-deactivate`

请求体：`{"ids": [1, 2, 3]}`（`1–100` 条，去重；空或超 100 → `400`）。

| 响应 | 说明 |
|---|---|
| `200` | `{"deactivated": N}`——N 为**新失效数**（已 `disabled` 的 id 为 no-op 不计入） |
| `404` | 任一 id 不存在：`{"error": "...id=999 missing..."}`（先查后失效，事务全成或全败） |

批量失效为单事务；重复提交（全部已失效）返回 `{"deactivated": 0}`（幂等重放友好）。

### 单码失效

`POST /admin/redemption-codes/{id}/deactivate`

无请求体。已失效再次调用为 no-op 成功（响应仍为 `{"deactivated": true}`，表示操作成功而非"本次新失效"）；`404`：id 不存在（消息含缺失 id）。

### 兑换记录（审计）

`GET /admin/redemption-codes/{id}/uses`

响应 `200`：

```json
{
  "total": 1,
  "rows": [
    { "ID": 1, "CodeID": 1, "UserID": 7, "Value": 100, "ResourceExpiresAt": null, "CreatedAt": "2026-08-08T10:05:00Z" }
  ]
}
```

`Value` 为兑换时的值快照（码后续失效不影响历史记录）；`404`：码不存在。

### 用户面：兑换

`POST /user/redemptions`（JWT 鉴权，非 admin 面——见下方"鉴权与 created_by 约定"）

请求体：`{"code": "JQVF2X-LD7SJQ"}`。

响应 `200`：

```json
{
  "applied": {
    "type": "balance",
    "value": 100,
    "resource_expires_at": null
  }
}
```

`applied` 为实际生效的资源（事务内应用）：`balance` 加余额、`concurrency` 加并发数、`temp_balance` 加临时余额（`resource_expires_at` 非空）。任一步失败（含并发用尽/重复兑换）整体回滚，资源不变。

| 状态码 | 场景 |
|---|---|
| `200` | 兑换成功 |
| `400` | `invalid code`：码不存在 / 已失效 / 已过期 / 用尽——**统一不泄露具体原因**（防枚举探测） |
| `409` | `already redeemed`：本用户已兑换过该码（重复请求稳定 409，即使码随后失效也用尽，也与"已兑换"事实一致） |
| `401` | 无 / 非法 JWT |

### 用户面：我的兑换记录

`GET /user/redemptions`（JWT 鉴权）

| 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `page` | int | 1 | 1-based；缺省或 `< 1` 按 1 |
| `page_size` | int | 20 | 越界（`< 1` 或 `> 100`）→ `400` |
| `sort` | string | `id` | 白名单：`id` / `code_id` / `user_id` / `value` / `created_at`；非法 → `400` |
| `order` | string | `desc` | `asc` / `desc`；其他值 → `400` |

响应 `200`：`{"total": N, "rows": [兑换记录]}`——记录含码的 `Code` / `CodeType` / `Remark` 联查快照。**强制只返回当前 JWT 用户本人的记录**（`user_id` 取自 JWT，无法通过参数指定他人——用户 A 永远看不到用户 B 的兑换记录）。

### 鉴权与 created_by 约定

| 路径 | 鉴权方式 | `created_by` 语义 |
|---|---|---|
| 静态 admin token（`Authorization: Bearer <admin.token>`） | `config.toml` 的 `admin.token` | 生成码时 `created_by = 0`（**0 = 系统**，未注入用户身份） |
| platform_admin JWT（`Authorization: Bearer <jwt>`） | 与 /user 面同签发的 JWT，且 `role == platform_admin` | 生成码时 `created_by = 该用户 id`（>`0`） |

`/admin/*` 两条路径任一通过即可；普通 `user` 角色的 JWT 访问 `/admin/*` → `401`。`created_by` 用于审计"哪个管理员/系统创建了这批发码"。

---

## 认证失败与错误码

| 状态码 | 场景 |
|---|---|
| `400` | 请求体非法 / 路径 ID 非法 / 非法 `sort` 或 `order` / 非法 `status` 枚举 / 批量 `ids` 为空或超 100 条 / 批量 `fields` 为空 / 规则 `when`/`then` 校验失败 / 兑换码生成参数非法（`type` 非法、`value ≤ 0`、`temp_balance` 缺 `resource_expires_at`、`expires_at` 过去、`count` 越界）/ 兑换码无效（`invalid code`：不存在/失效/过期/用尽，统一不泄露细节） |
| `401` | admin token 缺失或错误；普通 `user` 角色 JWT 访问 `/admin/*` |
| `404` | 资源不存在（单资源与批量均返回，消息含缺失 id，如 `service: not found: id=999 missing`） |
| `409` | 规则 `priority`/`name` 唯一冲突 / 兑换码重复兑换（`already redeemed`） |
| `500` | 服务端错误（DB 等） |

错误响应体统一为 `{"error": "<消息>"}`。
