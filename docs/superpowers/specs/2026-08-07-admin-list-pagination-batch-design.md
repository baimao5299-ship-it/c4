# 管理面列表增强：分页 + 批量 CRUD + 筛选（先后端）

## 状态

已批准（用户确认：批量删除+批量更新、全成或全败事务、name 模糊+枚举/关联精确+多值+排序全能力、limit/offset + {total, rows}、批量更新任意字段子集、账号排序除 LastError 外全部 DB 列）。

## 目标

为管理面三个列表资源（模板 / 账号 / 分组）补齐管理能力：

1. **分页**：limit/offset 分页（与 GET /logs 一致），响应从裸数组改为 `{total, rows}`；
2. **筛选**：name 模糊搜索（三资源）+ 账号 status 多值 / template_id + 模板 default_format；
3. **排序**：sort + order 参数，每资源白名单，非法值 400；
4. **批量 CRUD**：批量删除 + 批量更新（任意字段子集），事务语义（全成或全败）；
5. **契约先行**：openapi.yaml 为唯一权威，oapi-codegen + openapi-typescript 重新生成；**先完善后端**，前端适配为下一轮。

## 契约变更（openapi/openapi.yaml）

### 通用分页/筛选/排序参数

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `limit` | int | 20 | 每页行数；1–100，越界 400 |
| `offset` | int | 0 | 分页偏移；≥0，越界 400 |
| `name` | string | — | 模糊匹配（不区分大小写，子串） |
| `sort` | string | `id` | 排序字段（白名单见下）；非法值 400 |
| `order` | string | `desc` | `asc` / `desc`；非法值 400 |

### sort 白名单

| 资源 | sort 可取 |
|---|---|
| 账号 | `id` `name` `template_id` `status` `cooldown_until` `weight` `max_concurrency` `last_used_at` `created_at` `updated_at`（除 LastError 外全部 DB 列；UpstreamKey 敏感且无排序意义，排除） |
| 模板 | `id` `name` `base_url` `default_format` `created_at` `updated_at`（models/model_formats/model_mapping 为关系/JSON 结构，不可排） |
| 分组 | `id` `name` `created_at` `updated_at` |

> 运行时字段（concurrency/err_rate/err_count，账号）为调度器内存快照值，不在 DB，**不参与排序/筛选**。

### 资源专属筛选参数

| 资源 | 参数 | 说明 |
|---|---|---|
| 账号 | `status` | **多值**：逗号分隔（`active,disabled`）；每个值须为枚举，非法 400 |
| 账号 | `template_id` | int 精确筛选 |
| 模板 | `default_format` | 枚举精确筛选（openai-chat/openai-responses/anthropic） |

### 列表响应

`GET /admin/templates`、`/admin/accounts`、`/admin/groups` 响应从裸数组改为：

```json
{ "total": 123, "rows": [ { "...": "元素对象（结构不变）" } ] }
```

- `total` = 筛选后全量计数（分页前）；
- 元素对象结构与现有完全一致（Template / AccountView / Group）；
- 无筛选时 `total` = 表全量。

### 批量端点

```
POST /admin/templates/batch-delete   {"ids": [1,2,3]}            → 200 {"deleted": 3}
POST /admin/accounts/batch-delete    {"ids": [...]}              → 200 {"deleted": 3}
POST /admin/groups/batch-delete      {"ids": [...]}              → 200 {"deleted": 3}
POST /admin/templates/batch-update   {"ids": [...], "fields": {"default_format": "openai-chat"}}  → 200 {"updated": 3}
POST /admin/accounts/batch-update    {"ids": [...], "fields": {"status": "disabled", "weight": 50}} → 200 {"updated": 3}
POST /admin/groups/batch-update      {"ids": [...], "fields": {"name": "x"}}                      → 200 {"updated": 3}
```

**请求体 schema**：

| 字段 | 类型 | 说明 |
|---|---|---|
| `ids` | int[] | 1–100 个；空 → 400；去重 |
| `fields`（仅 update） | object | **任意字段子集**，字段名 snake_case，校验规则与单资源更新（PUT）完全一致；空对象 → 400 |

**响应 schema**：`BatchDeleteResponse`（`{"deleted": N}`）/ `BatchUpdateResponse`（`{"updated": N}`）。

**错误语义**（全成或全败，事务）：

| 场景 | 状态码 | 响应 |
|---|---|---|
| ids 空 / >100 / 重复字段非法 | 400 | ErrorResponse，消息指明 |
| 任一 id 不存在 | 404 | ErrorResponse 含缺失 id |
| 任一删除违反外键（模板仍被账号引用） | 400 | ErrorResponse 指明资源与 id |
| 任一字段校验失败 | 400 | ErrorResponse 指明失败项 |
| 事务中 DB 错误 | 500 | ErrorResponse |
| 未认证 | 401 | 既有中间件不变 |

## handler 适配

- 三个列表方法（GetTemplates/GetAccounts/GetGroups）加查询参数解析（生成类型 GetTemplatesParams 等），组装 repo Query → 返回 `{total, rows}`；
- 新增 6 个批量方法（PostTemplatesBatchDelete / PostAccountsBatchDelete / PostGroupsBatchDelete / PostTemplatesBatchUpdate / PostAccountsBatchUpdate / PostGroupsBatchUpdate），复用既有字段校验逻辑（批量 update 的 fields 校验 = 单 PUT 的字段校验，抽公共校验函数）；
- ids 去重、长度校验在 handler 层；
- 事务在 repo 层发起（ent.Tx），handler 层不感知。

## repo 层

- 三个资源 List 方法扩展为带 `Query` 结构（参照既有 `repository.LogQuery` 模式）：`Limit/Offset/Name/Sort/Order/StatusList/TemplateID/DefaultFormat`；
- 排序映射：白名单 → ent 字段名，**在 repo 层做白名单校验**（不信任 handler 传入的任意字符串）；
- `Count(ctx, query)` 方法（ent.Count 带同样筛选条件）；
- 批量删除：`DeleteBatch(ctx, ids)` 在单个 ent.Tx 内逐个删除，任一个失败整体回滚；
- 批量更新：`UpdateBatch(ctx, ids, fields)` 在 ent.Tx 内逐项 `UpdateOne`，任一项校验/执行失败回滚；字段更新用 ent 的 `SetX` 链（与单更新一致），仅设置 fields 提供的字段；
- 存在性：事务开始先 SELECT 检查 ids 全部存在，缺任何一个直接失败（不删任何行）。

## 测试

- **repo 层**（pgxmock，参照现有 repository_test.go 模式）：
  - 筛选：name 模糊、status 多值、template_id、default_format 各一例；
  - 分页/排序：limit/offset、sort 映射、白名单外 sort 拒绝；
  - 事务：批量删除失败回滚（mock 中途报错 → 断言无部分删除）；批量更新同理；
  - Count 与 List 同条件一致性。
- **handler 层**（HTTP，参照现有 handler_test 模式）：
  - 列表：分页/筛选/排序参数绑定、{total, rows} 结构、非法参数 400；
  - 批量：成功路径（{deleted}/{updated}）、空 ids 400、超长 ids 400、id 不存在 404、字段非法 400、重复 id 去重；
  - 兼容回归：单资源 GET/PUT/DELETE 既有断言不变。
- **契约**：oapi-codegen 生成后 `go test ./...` 全绿；spec 与生成由工具保证。

## 前端影响（下一轮，不在本次范围）

- openapi-typescript 重新生成 schema.d.ts（契约先行，本次提交）；
- 页面适配：列表页分页控件 + 筛选栏 + 多选批量操作条（下一轮单独实施）。

## 兼容性

- **破坏性变更**：三个列表响应从裸数组 → `{total, rows}`。当前前端（web/ Task 3-4 页面）依赖裸数组，将在下一轮同步适配；后端轮次结束前，前端列表页暂时不可用（API 契约已先行更新）。
- 日志/统计接口不受影响。
