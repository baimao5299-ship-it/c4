# Admin API 文档

管理端 API（配置模板 / 账号 / 分组 / 日志 / 统计）。与 AI 推理请求（`/v1/*`，模型请求）相对，本组接口为网关管理面，不对上游转发。

## 通用约定

- **Base URL**：`http://<gateway>/admin`
- **认证**：所有请求必须带 `Authorization: Bearer <admin_token>`（`config.toml` 的 `admin.token`，或环境变量 `GPM_ADMIN_TOKEN`）。缺失或错误返回 `401`。
- **Content-Type**：请求体与响应均为 `application/json`（`rotate-key` 等无请求体操作除外）。
- **错误格式**：非 2xx 响应体为 `{"error": "<消息>"}`。
- **ID**：路径参数 `{id}` 为模板/账号/分组的整数 ID。
- **更新语义**：`PUT` 为**全量替换**——请求体中的字段整体覆盖，未提供的字段清零（仅提供部分字段的 `PUT` 会把缺失字段重置为空/零值）。

## 枚举值

| 枚举 | 取值 |
|---|---|
| `default_format` / `format` | `openai-chat` / `openai-responses` / `anthropic` |
| `status`（账号） | `active` / `unhealthy` / `429` / `disabled` |
| `error_type`（日志） | `none` / `429` / `4xx` / `5xx` / `network` / `auth` / `no_account` / `abort` |

---

## 模板 Templates

模板定义上游厂商：base_url、默认请求格式、可服务模型集合、模型级格式覆盖与模型映射。

### 创建模板

`POST /admin/templates`

请求体：

```json
{
  "name": "openai-main",
  "base_url": "https://api.openai.com/v1",
  "default_format": "openai-chat",
  "models": ["gpt-4o", "gpt-4o-mini"],
  "model_formats": { "gpt-4o-vision": "openai-chat" },
  "model_mapping": { "gpt-4o": "gpt-4o-2024-11-20" }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 模板名 |
| `base_url` | string | ✅ | 上游地址（含 `/v1` 前缀；流式/非流式均基于此） |
| `default_format` | string | ✅ | 默认请求格式（枚举见上） |
| `models` | string[] | 否 | 可服务模型名集合 |
| `model_formats` | object | 否 | 模型级格式覆盖：`{模型名: 格式}`，优先于 `default_format` |
| `model_mapping` | object | 否 | 模型映射：`{客户端模型名: 上游实际模型名}` |

响应 `200`：创建后的模板对象（字段为大写，见下方模板对象结构）。

### 模板对象结构（响应）

```json
{
  "ID": 1,
  "Name": "openai-main",
  "BaseURL": "https://api.openai.com/v1",
  "DefaultFormat": "openai-chat",
  "Models": ["gpt-4o", "gpt-4o-mini"],
  "ModelFormats": { "gpt-4o-vision": "openai-chat" },
  "ModelMapping": { "gpt-4o": "gpt-4o-2024-11-20" },
  "CreatedAt": "2026-08-06T10:00:00Z",
  "UpdatedAt": "2026-08-06T10:00:00Z"
}
```

> 注意：响应字段为 **Go 默认大写命名**（`ID` / `Name` / `BaseURL`…），请求字段为 snake_case。`Models` / `ModelFormats` / `ModelMapping` 为 `null` 时表示空。

### 模板其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/templates` | 列表 | `200`：模板对象数组 |
| `GET /admin/templates/{id}` | 单个模板 | `200`：模板对象；`404` 不存在 |
| `PUT /admin/templates/{id}` | 全量更新（字段同创建） | `200`：更新后模板对象 |
| `DELETE /admin/templates/{id}` | 删除 | `200`：`{"deleted": true}`；仍被账号引用时返回 `500`（DB 外键约束） |

> 模板变更（含 base_url / models / model_formats / model_mapping）通过 invalidate 回调即时生效于调度器快照与上游 SDK 客户端（无需重启）。

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

响应 `200`：账号视图数组。每个元素为账号对象 + 三个运行时字段（来自调度器内存快照）：

```json
{
  "ID": 1,
  "Name": "a1",
  "...": "账号对象字段",
  "concurrency": 3,
  "err_rate": 0.05,
  "err_count": 2
}
```

| 运行时字段 | 说明 |
|---|---|
| `concurrency` | 当前在途并发数（内存计数） |
| `err_rate` | 错误率 EWMA（0.0–1.0，定点 1e6 缩放后输出） |
| `err_count` | 连续错误计数（决定退避指数） |

### 账号其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/accounts/{id}` | 单个账号 | `200`：账号对象；`404` 不存在 |
| `PUT /admin/accounts/{id}` | 全量更新（字段同创建；`status` 可改为 `disabled` 禁用） | `200`：更新后账号对象 |
| `DELETE /admin/accounts/{id}` | 删除 | `200`：`{"deleted": true}` |

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

### 分组其他端点

| 方法/路径 | 说明 | 响应 |
|---|---|---|
| `GET /admin/groups` | 列表 | `200`：分组对象数组（不含明文 key） |
| `GET /admin/groups/{id}` | 单个分组 | `200`：分组对象 |
| `PUT /admin/groups/{id}` | 重命名 | `200`：更新后分组对象 |
| `DELETE /admin/groups/{id}` | 删除（先删注册 key 再删 DB） | `200`：`{"deleted": true}` |
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

## 认证失败与错误码

| 状态码 | 场景 |
|---|---|
| `400` | 请求体非法 / 路径 ID 非法 / 资源仍被引用（删除） |
| `401` | admin token 缺失或错误 |
| `404` | 资源不存在 |
| `500` | 服务端错误（DB 等） |

错误响应体统一为 `{"error": "<消息>"}`。
