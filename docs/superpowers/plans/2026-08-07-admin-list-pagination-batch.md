# 管理面列表增强（分页/批量/筛选）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 模板/账号/分组三个列表资源支持 limit/offset 分页、name 模糊搜索、枚举/关联精确筛选、多值筛选、白名单排序；新增 6 个批量端点（删除 + 任意字段子集更新），事务全成或全败；契约先行（openapi.yaml + oapi-codegen + openapi-typescript 重新生成）。**只做后端**，前端适配为下一轮。

**Architecture:** 查询能力下沉 repo 层（ListQuery 结构参照既有 LogQuery 模式），service 薄委托 + 校验 + invalidate，handler 消费生成类型参数；批量操作用 ent.Tx 事务 + 事务前存在性检查。

**Tech Stack:** Go 1.26 / ent v0.14.6 / chi / oapi-codegen v2.4.1 / openapi-typescript 7 / pgxmock（测试）

## Global Constraints

- 契约唯一权威：`openapi/openapi.yaml`；生成命令：`cd internal/handler && go generate`（generate.go 内 oapi-codegen v2.4.1 types,chi-server）+ `cd web && pnpm_config_verify_deps_before_run=false pnpm run gen:api`（openapi-typescript → web/src/lib/api/schema.d.ts）
- 响应大写 Go 风格字段（`ID`/`Name`…），请求体 snake_case；枚举：format（openai-chat/openai-responses/anthropic）、status（active/unhealthy/429/disabled）
- 错误响应统一 ErrorResponse `{"error": "..."}`；404 = 资源不存在，400 = 参数/引用非法，500 = 服务端
- 分页默认 limit=20、offset=0；limit 1–100 越界 400；ids 1–100、去重、空 400
- 排序白名单：账号 `id name template_id status cooldown_until weight max_concurrency last_used_at created_at updated_at`（**除 LastError 外全部 DB 列**；UpstreamKey 排除）；模板 `id name base_url default_format created_at updated_at`；分组 `id name created_at updated_at`；非法 sort/order 400
- 运行时字段（concurrency/err_rate/err_count）不参与排序/筛选
- 批量语义：事务全成或全败；模板删除违反外键（账号引用）→ 400；id 不存在 → 404 含缺失 id
- 前端适配不在本轮；schema.d.ts 重新生成后 web 编译可暂时不检查（web 页面下轮适配）
- 现有单资源 GET/PUT/DELETE 行为与断言不变（除三个列表响应结构按契约变更）
- 变更后 invalidate（调度快照）与 KeyRegistrar 清理（分组）逻辑与单操作一致

---

### Task 1: repo + service 层列表查询能力

**Files:**
- Create: `internal/repository/list_query.go`
- Modify: `internal/repository/template_repo.go`、`internal/repository/account_repo.go`、`internal/repository/group_repo.go`、`internal/repository/repository.go`（Store 门面）
- Modify: `internal/service/service.go`（Store 接口）、`internal/service/template.go`、`internal/service/account.go`、`internal/service/group.go`、`internal/service/fakestore_test.go`
- Modify: `internal/handler/template.go`、`internal/handler/account.go`、`internal/handler/group.go`（传空查询，编译适配）
- Test: `internal/repository/repository_test.go`（追加）

**Interfaces:**
- 生产：`repository.ListQuery{Limit,Offset int; Name string; Sort,Order string; StatusList []string; TemplateID int64; DefaultFormat string}`；`ListTemplates(ctx, q) ([]*domain.Template, int64, error)`、`ListAccounts(ctx, q) ([]*domain.Account, int64, error)`、`ListGroups(ctx, q) ([]*domain.Group, int64, error)`
- 消费：Task 2 handler 列表方法、Task 3/4 批量（不依赖）

- [ ] **Step 1: 写失败测试（repo 层查询行为）**

在 `internal/repository/repository_test.go` 追加（参照既有 mockDriver 模式，测试文件顶部已有 setup 工具——先 Read 文件确认 helper 名与断言风格再写）：

```go
func TestListTemplatesQuery(t *testing.T) {
	// 用既有 mock 建立 Repos（参照文件内其他测试的 setup），然后：
	// 1) name 模糊：mock 期望 Query(Templates).Where(name.Contains("main"))
	// 2) default_format 筛选：Where(DefaultFormatEQ("openai-chat"))
	// 3) 分页+排序：Order(ent.Desc(FieldID)).Offset(20).Limit(50) → 返回 2 行 + Count
	// 4) 非法 sort 值 → 返回 error（含 "sort"）
	// 5) Count 与 List 同条件
}
```

先 Read repository_test.go 头部（setup/helper/mock 匹配方式），按文件既有写法写测试。断言：filter/order/offset/limit SQL 参数正确、total 正确、非法 sort 报错。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repository/ -run TestListTemplatesQuery`
Expected: FAIL（编译失败：`ListQuery` 未定义 / `ListTemplates` 签名不匹配）

- [ ] **Step 3: 新建 `internal/repository/list_query.go`**

```go
package repository

import (
	"errors"
	"strings"

	"go-proxy-mini/internal/ent/account"
	"go-proxy-mini/internal/ent/group"
	"go-proxy-mini/internal/ent/template"
)

// ListQuery 列表查询：分页/筛选/排序。Sort 为白名单内字段名（如 "name"），
// 非法值返回 ErrInvalidSort；Order 仅 "asc"/"desc"（空 = desc）。
type ListQuery struct {
	Limit         int      // <=0 → 20
	Offset        int      // <0 → 0
	Name          string   // 模糊匹配（不区分大小写）
	Sort          string   // 空 → id
	Order         string   // asc/desc；空 → desc
	StatusList    []string // 账号专属：多值 status
	TemplateID    int64    // 账号专属：0 = 不过滤
	DefaultFormat string   // 模板专属："" = 不过滤
}

var ErrInvalidSort = errors.New("invalid sort field")

// sortOrder 把 ListQuery 翻译为 ent OrderTerm；sortField 为调用方白名单映射。
func (q ListQuery) sortOrder(sortField map[string]string) (entOrder, error) { // 见下
}
```

（ent 的 Order 项类型是 `ent.OrderFunc`，`ent.Asc/ent.Desc` 返回 `ent.OrderFunc`——签名写 `func (q ListQuery) sortOrder(sortField map[string]string) (ent.OrderFunc, error)`。实现：白名单查 sort → asc/desc 选 `ent.Asc(field)/ent.Desc(field)`；order 非法值（非 ""/asc/desc）返回 `errors.New("invalid order")`。）

三个白名单常量放本文件：

```go
var (
	templateSortFields = map[string]string{
		"id": template.FieldID, "name": template.FieldName, "base_url": template.FieldBaseURL,
		"default_format": template.FieldDefaultFormat, "created_at": template.FieldCreatedAt,
		"updated_at": template.FieldUpdatedAt,
	}
	accountSortFields = map[string]string{
		"id": account.FieldID, "name": account.FieldName, "template_id": account.FieldTemplateID,
		"status": account.FieldStatus, "cooldown_until": account.FieldCooldownUntil,
		"weight": account.FieldWeight, "max_concurrency": account.FieldMaxConcurrency,
		"last_used_at": account.FieldLastUsedAt, "created_at": account.FieldCreatedAt,
		"updated_at": account.FieldUpdatedAt,
	}
	groupSortFields = map[string]string{
		"id": group.FieldID, "name": group.FieldName, "created_at": group.FieldCreatedAt,
		"updated_at": group.FieldUpdatedAt,
	}
)
```

- [ ] **Step 4: 三个 repo 的 List 改签名 + 排序/分页/筛选**

`template_repo.go`：

```go
func (r *TemplateRepo) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	pred := r.client.Template.Query()
	if q.Name != "" {
		pred = pred.Where(template.NameContainsFold(q.Name))
	}
	if q.DefaultFormat != "" {
		pred = pred.Where(template.DefaultFormatEQ(template.DefaultFormat(q.DefaultFormat)))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(templateSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, total, nil
}
```

`account_repo.go` 的 ListAccounts：`Where(account.NameContainsFold(q.Name))`；`q.StatusList` 非空 → `Where(account.StatusIn(toAccountStatusList(q.StatusList)...))`（helper：`[]string → []account.Status`，非法 status 值返回 error——实现为 `account.Status` 转换时校验，非法 → `ErrInvalidSort` 不行，用 `errors.New("invalid status")`；handler 已校验枚举，repo 兜底）；`q.TemplateID > 0` → `Where(account.TemplateIDEQ(q.TemplateID))`；`WithTemplate()` 保留（AccountView 需要嵌套 Template）；排序 `q.sortOrder(accountSortFields)`。

`group_repo.go` 的 ListGroups：`Where(group.NameContainsFold(q.Name))`；排序 `q.sortOrder(groupSortFields)`。

- [ ] **Step 5: service 层接口与委托扩展**

`service/service.go` Store 接口三处签名改：

```go
ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error)
ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error)
ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error)
```

`service/template.go`：

```go
func (s *Service) ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error) {
	return s.store.ListTemplates(ctx, q)
}
```

`service/account.go` 同样 + ListAccountViews 适配：

```go
func (s *Service) ListAccountViews(ctx context.Context, q repository.ListQuery) ([]*AccountView, int64, error) {
	accs, total, err := s.store.ListAccounts(ctx, q)
	if err != nil {
		return nil, 0, err
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
	return out, total, nil
}
```

`service/group.go` 同样。`fakestore_test.go` 三个 List 方法签名同步（返回 `(nil, 0, nil)` 风格按既有 fake 数据改——fake 有内部切片，返回 `(f.templates, int64(len(f.templates)), nil)` 即可，q 忽略）。

`repository.go` Store 门面三处同步：

```go
func (r *Repos) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	return r.Templates.ListTemplates(ctx, q)
}
```

- [ ] **Step 6: handler 三列表方法编译适配（行为暂不变）**

`handler/template.go` GetTemplates、`handler/account.go` GetAccounts、`handler/group.go` GetGroups 改为传空查询并适配返回：

```go
func (h *AdminAPI) GetTemplates(w http.ResponseWriter, r *http.Request) {
	rows, _, err := h.svc.ListTemplates(r.Context(), repository.ListQuery{})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Template, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPITemplate(t))
	}
	writeJSON(w, http.StatusOK, out)
}
```

GetAccounts：`rows, _, err := h.svc.ListAccountViews(r.Context(), repository.ListQuery{})`（响应仍数组，Task 2 改）。GetGroups 同理。handler 文件加 `repository` import。

- [ ] **Step 7: 跑全量测试**

Run: `go test ./...`
Expected: 全绿（含新增 ListQuery 测试与既有全部断言——行为不变）

- [ ] **Step 8: Commit**

```bash
git add internal/repository internal/service internal/handler
git commit -m "feat: repo/service list queries (pagination/filter/sort) with whitelist"
```

---

### Task 2: 契约层列表增强 + handler 适配

**Files:**
- Modify: `openapi/openapi.yaml`
- Modify: `internal/handler/api.gen.go`（生成）、`internal/handler/handler.go`（Router 不变）、`internal/handler/template.go`、`internal/handler/account.go`、`internal/handler/group.go`、`internal/handler/handler_test.go`
- Modify: `web/src/lib/api/schema.d.ts`（生成，仅契约先行，web 编译下轮适配）
- Test: `internal/handler/handler_test.go`（列表断言更新 + 新增参数绑定测试）

**Interfaces:**
- 消费：Task 1 的 `repository.ListQuery`、`svc.ListTemplates/ListAccountViews/ListGroups(ctx, q)`
- 生产：生成类型 `GetTemplatesParams{Limit,Offset *int; Name *string; Sort,Order *string; DefaultFormat *string}`、`GetAccountsParams{... Status *string ...}`、`GetGroupsParams{...}`、`TemplateListResponse{Total int64; Rows []Template}`、`AccountListResponse`、`GroupListResponse`

- [ ] **Step 1: 更新 openapi.yaml（三个 GET 参数 + 列表响应）**

`/templates` 的 `get:` 加 parameters（operationId GetTemplates）与响应：

```yaml
    get:
      operationId: GetTemplates
      tags: [templates]
      summary: 模板列表（分页/筛选/排序）
      parameters:
        - { name: limit, in: query, schema: { type: integer, default: 20 } }
        - { name: offset, in: query, schema: { type: integer, default: 0 } }
        - { name: name, in: query, schema: { type: string } }
        - { name: sort, in: query, schema: { type: string } }
        - { name: order, in: query, schema: { type: string, enum: [asc, desc] } }
        - { name: default_format, in: query, schema: { type: string, enum: [openai-chat, openai-responses, anthropic] } }
      responses:
        '200':
          description: 模板列表
          content:
            application/json:
              schema: { $ref: '#/components/schemas/TemplateListResponse' }
        default:
          $ref: '#/components/responses/Error'
```

`/accounts` 的 get 加同样 5 个通用参数 + `status`（string，`format` 不加——多值逗号分隔，schema 仍 string）+ `template_id`（integer int64）；响应 `AccountListResponse`。`/groups` 的 get 加 5 个通用参数；响应 `GroupListResponse`。

components/schemas 追加：

```yaml
    TemplateListResponse:
      type: object
      required: [total, rows]
      properties:
        total: { type: integer, format: int64 }
        rows:
          type: array
          items: { $ref: '#/components/schemas/Template' }
    AccountListResponse:
      type: object
      required: [total, rows]
      properties:
        total: { type: integer, format: int64 }
        rows:
          type: array
          items: { $ref: '#/components/schemas/AccountView' }
    GroupListResponse:
      type: object
      required: [total, rows]
      properties:
        total: { type: integer, format: int64 }
        rows:
          type: array
          items: { $ref: '#/components/schemas/Group' }
```

- [ ] **Step 2: 重新生成 Go 契约**

Run: `cd internal/handler && go generate`
Expected: `api.gen.go` 更新（三个列表方法签名带 params、三个 ListResponse 类型、GetTemplatesParams 等）；此时 handler 编译失败（签名不匹配）——正常。

- [ ] **Step 3: 写失败测试（handler 列表参数绑定）**

`handler_test.go` 追加（参照既有 `TestParamBindErrorIsErrorResponse` 的接线：chi router + auth 中间件 + `r.Mount("/", h.Router())`）：

```go
func TestGetTemplatesParams(t *testing.T) {
	// GET /admin/templates?limit=5&offset=10&name=openai&sort=name&order=asc&default_format=openai-chat
	// 断言 200 + body 为 {"total": N, "rows": [...]} 结构（fake store 数据）
}
func TestGetAccountsStatusMulti(t *testing.T) {
	// GET /admin/accounts?status=active,disabled&template_id=1
	// 断言 200 + {total, rows}
}
func TestGetGroupsSortInvalid(t *testing.T) {
	// GET /admin/groups?sort=bogus → 400 {"error": ...}
}
```

fake store：handler_test 的 newTestHandler 用 service fake（读 handler_test.go 现有 fake 数据确认 total 期望值）。若 fake 查询不支持筛选（fake 实现 ListTemplates 忽略 q 返回全量）——筛选断言不可行，则降级为断言：参数不报错 + {total, rows} 结构正确 + 非法 sort 400（service 校验在 Task 4？不——非法 sort 的 400 来自 repo 层 ErrInvalidSort → service 转 ErrInvalidInput → writeServiceErr 400。Task 1 已实现 repo 校验，fake store 不校验——handler 测试用 fake store 的话非法 sort 不会 400。**裁决**：非法 sort/order 的 400 校验放 **service 层**（对 q 前置校验，fake 也生效）——Task 1 Step 5 补 service 校验：

```go
func validateListQuery(q repository.ListQuery, sortFields []string) error {
	if q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		return ErrInvalidInput
	}
	if q.Sort != "" && !slices.Contains(sortFields, q.Sort) {
		return ErrInvalidInput
	}
	return nil
}
```

三个 service List 方法调用它（白名单数组常量放 service）。repo 层白名单校验保留（防御）。Task 1 的测试补 service 校验用例。）

- [ ] **Step 4: handler 三列表方法适配**

`handler/template.go`：

```go
// GetTemplates 模板列表（分页/筛选/排序，ServerInterface）。
func (h *AdminAPI) GetTemplates(w http.ResponseWriter, r *http.Request, params GetTemplatesParams) {
	q := repository.ListQuery{
		Limit:         int(deref(params.Limit)),
		Offset:        int(deref(params.Offset)),
		Name:          deref(params.Name),
		Sort:          deref(params.Sort),
		Order:         deref(params.Order),
		DefaultFormat: deref(params.DefaultFormat),
	}
	rows, total, err := h.svc.ListTemplates(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Template, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPITemplate(t))
	}
	writeJSON(w, http.StatusOK, TemplateListResponse{Total: total, Rows: out})
}
```

`deref` 已存在于 handler 包（Task 1 用过的泛型 helper——确认存在，不存在则加：`func deref[T any](p *T) T { if p == nil { var z T; return z }; return *p }`）。

`handler/account.go` GetAccounts：

```go
func (h *AdminAPI) GetAccounts(w http.ResponseWriter, r *http.Request, params GetAccountsParams) {
	q := repository.ListQuery{
		Limit:      int(deref(params.Limit)),
		Offset:     int(deref(params.Offset)),
		Name:       deref(params.Name),
		Sort:       deref(params.Sort),
		Order:      deref(params.Order),
		TemplateID: deref(params.TemplateID),
	}
	if params.Status != nil && *params.Status != "" {
		q.StatusList = strings.Split(*params.Status, ",")
	}
	rows, total, err := h.svc.ListAccountViews(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAPIAccountView(v))
	}
	writeJSON(w, http.StatusOK, AccountListResponse{Total: total, Rows: out})
}
```

`handler/group.go` GetGroups 同理（无资源专属参数）。

- [ ] **Step 5: handler_test 既有列表断言更新**

Read handler_test.go 找三个列表的既有断言（期望裸数组）→ 改为 `{"total": N, "rows": [...]}` 断言。其余断言不动。

- [ ] **Step 6: 跑测试**

Run: `go test ./internal/handler/ ./internal/service/ ./internal/repository/`
Expected: 全绿

- [ ] **Step 7: 重新生成 TS 类型（契约先行）**

Run: `cd web && pnpm_config_verify_deps_before_run=false pnpm run gen:api`
Expected: `web/src/lib/api/schema.d.ts` 更新（三个 ListResponse + params 类型）。**不要跑 web build**（前端页面还没适配，编译会挂——下轮任务适配）。

- [ ] **Step 8: Commit**

```bash
git add openapi/openapi.yaml internal/handler/api.gen.go internal/handler/*.go web/src/lib/api/schema.d.ts
git commit -m "feat: list pagination/filter/sort contract + handler adaptation"
```

---

### Task 3: repo 层批量事务

**Files:**
- Create: `internal/repository/batch.go`
- Modify: `internal/repository/template_repo.go`、`internal/repository/account_repo.go`、`internal/repository/group_repo.go`（不改，放 batch.go）
- Test: `internal/repository/repository_test.go`（追加）

**Interfaces:**
- 生产：`repository.TemplatePatch{Name *string; BaseURL *string; DefaultFormat *domain.RequestFormat; Models *[]string; ModelFormats *map[string]domain.RequestFormat; ModelMapping *map[string]string}`、`AccountPatch{Name *string; TemplateID *int64; UpstreamKey *string; Status *domain.AccountStatus; Weight *int; MaxConcurrency *int}`、`GroupPatch{Name *string}`；`DeleteTemplatesBatch(ctx, ids []int64) error`、`UpdateTemplatesBatch(ctx, ids []int64, p TemplatePatch) error`（账号/分组同）；不存在 id → `domain.ErrNotFound`（检查 repository 现有错误约定——若 repository 无 sentinel，定义 `ErrNotFound = errors.New("repository: not found")` 由 service 映射）
- 消费：Task 4 handler/service

- [ ] **Step 1: 写失败测试（事务语义）**

repository_test.go 追加（pgxmock 事务匹配参照既有测试写法）：

```go
func TestDeleteTemplatesBatchRollback(t *testing.T) {
	// mock: BEGIN → 存在性 SELECT（IDIn 返回 2 行，ids 3 个）→ 缺 1 → 断言返回 ErrNotFound 且无 DELETE 执行
	// 第二个场景：存在性通过 → 逐个 DELETE → 中途 mock 报错 → 断言返回 error（事务回滚，无 Commit）
}
func TestUpdateAccountsBatch(t *testing.T) {
	// mock: BEGIN → 存在性 SELECT 通过 → UpdateOneID SetStatus/SetWeight → Save → Commit
	// 断言 SQL 只含 patch 提供的字段（Set 的列）
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repository/ -run "TestDeleteTemplatesBatchRollback|TestUpdateAccountsBatch"`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 新建 `internal/repository/batch.go`**

```go
package repository

import (
	"context"
	"errors"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent/account"
	"go-proxy-mini/internal/ent/group"
	"go-proxy-mini/internal/ent/template"
)

// ErrNotFound 批量操作中存在性检查失败（缺失 id）。
var ErrNotFound = errors.New("repository: not found")

// --- 批量更新字段子集（nil 字段 = 不更新） ---

type TemplatePatch struct {
	Name          *string
	BaseURL       *string
	DefaultFormat *domain.RequestFormat
	Models        *[]string
	ModelFormats  *map[string]domain.RequestFormat
	ModelMapping  *map[string]string
}

type AccountPatch struct {
	Name           *string
	TemplateID     *int64
	UpstreamKey    *string
	Status         *domain.AccountStatus
	Weight         *int
	MaxConcurrency *int
}

type GroupPatch struct {
	Name *string
}

// --- 批量删除（事务，全成或全败） ---

func (r *TemplateRepo) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	if err := checkExist(ctx, tx.Template.Query(), template.FieldID, ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Template.DeleteOneID(id).Exec(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

`checkExist` 泛型（ent 的 Query 类型不同——用接口不好搞。简化：三个 repo 各写存在性检查，或传 ent.Query 接口。ent 的 `*ent.TemplateQuery` 等都有 `Count(ctx)`——抽公共函数用 `interface{ Count(context.Context) (int, error) }` 不够（要加 Where）。**写三个小 helper 或按资源内联**：

```go
// 事务内存在性检查：ids 去重后必须全部存在，否则 ErrNotFound。
func checkTemplateExist(ctx context.Context, q *ent.TemplateQuery, ids []int64) error {
	n, err := q.Where(template.IDIn(ids...)).Count(ctx)
	if err != nil {
		return err
	}
	if int(n) != len(ids) {
		return ErrNotFound
	}
	return nil
}
```

（同 checkAccountExist / checkGroupExist。handler 已去重，ids 唯一。）

删除方法（账号/分组同模式）：

```go
func (r *AccountRepo) DeleteAccountsBatch(ctx context.Context, ids []int64) error { /* 同模板 */ }
func (r *GroupRepo) DeleteGroupsBatch(ctx context.Context, ids []int64) error { /* 同模板 */ }
```

批量更新：

```go
func (r *TemplateRepo) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkTemplateExist(ctx, tx.Template.Query(), ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Template.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.BaseURL != nil {
			u = u.SetBaseURL(*p.BaseURL)
		}
		if p.DefaultFormat != nil {
			u = u.SetDefaultFormat(template.DefaultFormat(*p.DefaultFormat))
		}
		if p.Models != nil {
			u = u.SetModels(*p.Models)
		}
		if p.ModelFormats != nil {
			u = u.SetModelFormats(toStringMap(*p.ModelFormats))
		}
		if p.ModelMapping != nil {
			u = u.SetModelMapping(*p.ModelMapping)
		}
		if _, err := u.Save(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

账号批量更新：SetName/SetTemplateID/SetUpstreamKey/SetStatus(account.Status(*p.Status))/SetWeight/SetMaxConcurrency 按非 nil 字段。分组：SetName。

- [ ] **Step 4: 跑测试**

Run: `go test ./internal/repository/`
Expected: 全绿（新增事务测试 + 既有）

- [ ] **Step 5: Commit**

```bash
git add internal/repository/batch.go internal/repository/repository_test.go
git commit -m "feat: repo batch delete/update with transactional semantics"
```

---

### Task 4a: service 层批量（Store 接口 + 校验 + 测试）

**Files:**
- Modify: `internal/service/service.go`（Store 接口 + validateIDs + patch 校验）、`internal/service/template.go`、`internal/service/account.go`、`internal/service/group.go`、`internal/service/fakestore_test.go`、`internal/service/service_test.go`
- Modify: `internal/repository/repository.go`（Store 门面 6 个批量委托）
- Test: `internal/service/service_test.go`

**Interfaces:**
- 消费：Task 3 的 `repository.*Patch`/`Delete*Batch`/`Update*Batch` 与 `repository.ErrNotFound`
- 生产：service 方法 `DeleteTemplatesBatch(ctx, ids) error`、`UpdateTemplatesBatch(ctx, ids, p repository.TemplatePatch) error`（账号/分组同）；404 映射：repo.ErrNotFound → `fmt.Errorf("%w: %s", ErrNotFound, 缺失详情)`
- **不碰契约/handler/api.gen.go**（Task 4b 范围）；Store 接口扩展后 fakestore 同步

**并行协议（与 Task 4b 并行执行，用户确认）:**
- 4a 只动 service/repository 包，4b 只动契约/handler——文件无重叠
- **4a 验证只用局部包**：`go test ./internal/service/ ./internal/repository/`（4b 生成 api.gen.go 后全量编译会挂，属预期）
- 4a 先提交（service 方法就位），提交后通知控制器 → 控制器通知 4b 验证

- [ ] **Step 1: Store 接口扩展 + 门面委托 + fakestore 同步（代码见下方 Task 4a 执行范围内的 Step 3 原文，即"Step 3: service 层批量方法 + Store 接口"小节）**
- [ ] **Step 2: service 6 个批量方法 + validateIDs + validateTemplatePatch/validateAccountPatch + 分组 key 清理（同 Step 3 原文）**
- [ ] **Step 3: service_test.go 批量测试**（成功路径 invalidate 调用、空/超长/重复 ids → ErrInvalidInput、patch 校验失败 → ErrInvalidInput、404 映射含缺失 id）
- [ ] **Step 4: 验证**：`go test ./internal/service/ ./internal/repository/` 全绿（**不跑 ./...**）
- [ ] **Step 5: Commit**（消息：`feat: service batch delete/update with validation`）

---

### Task 4b: 契约批量端点 + 生成 + handler + 测试

**Files:**
- Modify: `openapi/openapi.yaml`、`internal/handler/api.gen.go`（生成）、`internal/handler/template.go`、`internal/handler/account.go`、`internal/handler/group.go`、`internal/handler/handler.go`（normalizeIDs 等 helper）、`internal/handler/handler_test.go`
- Modify: `web/src/lib/api/schema.d.ts`（生成，仅契约）
- Test: `internal/handler/handler_test.go`

**Interfaces:**
- 消费：Task 4a 的 6 个 service 批量方法；Task 3 的 repository 层
- 生产：生成类型 `BatchDeleteBody{Ids []int64}`、`BatchUpdateTemplatesBody{Ids; Fields *TemplatePatch}`（Accounts/Groups 同）、`TemplatePatch`/`AccountPatch`/`GroupPatch`（字段全 optional 指针）、`BatchDeleteResponse{Deleted int}`、`BatchUpdateResponse{Updated int}`
- **不碰 service 包**（Task 4a 范围）

**并行协议（与 Task 4a 并行执行，用户确认）:**
- 4b 只动契约/handler 文件；**4a 提交前不验证编译**（svc 方法未实现 + 生成后 ServerInterface 未实现，编译挂是预期）
- 4b 先写完全部代码 → 等控制器"4a 已提交"信号 → 验证（handler/service 测试）→ 提交
- 生成类型字段名以 api.gen.go 实际为准

- [ ] **Step 1: 更新 openapi.yaml（6 个批量端点 + schema）**

三个资源各加 batch-delete / batch-update 路径（`/templates/batch-delete` 必须定义在 `/templates/{id}` **之前**，否则 chi 会把 "batch-delete" 匹配为 {id}——openapi 路径顺序无约束但 oapi-codegen chi 路由注册顺序按文件顺序，**务必放在 `/templates/{id}` 前**）：

```yaml
  /templates/batch-delete:
    post:
      operationId: PostTemplatesBatchDelete
      tags: [templates]
      summary: 批量删除模板（事务，全成或全败）
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/BatchDeleteBody' }
      responses:
        '200':
          description: 删除成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/BatchDeleteResponse' }
        default:
          $ref: '#/components/responses/Error'
  /templates/batch-update:
    post:
      operationId: PostTemplatesBatchUpdate
      tags: [templates]
      summary: 批量更新模板（fields 为任意字段子集）
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/BatchUpdateTemplatesBody' }
      responses:
        '200':
          description: 更新成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/BatchUpdateResponse' }
        default:
          $ref: '#/components/responses/Error'
```

（/accounts/batch-delete、/accounts/batch-update、/groups/batch-delete、/groups/batch-update 同型，operationId 对应 PostAccountsBatchDelete/PostAccountsBatchUpdate/PostGroupsBatchDelete/PostGroupsBatchUpdate，update body 用 BatchUpdateAccountsBody/BatchUpdateGroupsBody。）

components/schemas 追加：

```yaml
    BatchDeleteBody:
      type: object
      required: [ids]
      properties:
        ids:
          type: array
          items: { type: integer, format: int64 }
          minItems: 1
          maxItems: 100
    BatchUpdateTemplatesBody:
      type: object
      required: [ids, fields]
      properties:
        ids:
          type: array
          items: { type: integer, format: int64 }
          minItems: 1
          maxItems: 100
        fields:
          $ref: '#/components/schemas/TemplatePatch'
    TemplatePatch:
      type: object
      properties:
        name: { type: string }
        base_url: { type: string }
        default_format: { type: string, enum: [openai-chat, openai-responses, anthropic] }
        models:
          type: array
          items: { type: string }
        model_formats:
          type: object
          additionalProperties: { type: string }
        model_mapping:
          type: object
          additionalProperties: { type: string }
    AccountPatch:
      type: object
      properties:
        name: { type: string }
        template_id: { type: integer, format: int64 }
        upstream_key: { type: string }
        status: { type: string, enum: [active, unhealthy, '429', disabled] }
        weight: { type: integer }
        max_concurrency: { type: integer }
    GroupPatch:
      type: object
      properties:
        name: { type: string }
    BatchUpdateAccountsBody:
      type: object
      required: [ids, fields]
      properties:
        ids:
          type: array
          items: { type: integer, format: int64 }
          minItems: 1
          maxItems: 100
        fields: { $ref: '#/components/schemas/AccountPatch' }
    BatchUpdateGroupsBody:
      type: object
      required: [ids, fields]
      properties:
        ids:
          type: array
          items: { type: integer, format: int64 }
          minItems: 1
          maxItems: 100
        fields: { $ref: '#/components/schemas/GroupPatch' }
    BatchDeleteResponse:
      type: object
      required: [deleted]
      properties:
        deleted: { type: integer }
    BatchUpdateResponse:
      type: object
      required: [updated]
      properties:
        updated: { type: integer }
```

- [ ] **Step 2: 生成**

Run: `cd internal/handler && go generate`
Expected: api.gen.go 加 6 个方法 + 类型；handler 编译失败（未实现）——正常。

- [ ] **Step 2.5（Task 4a 执行，4b 跳过）: 4b 的 Step 3 原为 service 层内容——已归 Task 4a（见上方 Task 4a Step 1-2），4b 不执行此步。下方 "Step 3: service 层批量方法 + Store 接口" 小节保留为 Task 4a 的代码原文（含 validateIDs/validateTemplatePatch/validateAccountPatch/分组 key 清理/404 映射），Task 4a 照此实现。**

- [ ] **Step 3: service 层批量方法 + Store 接口（Task 4a 代码原文，4a 执行）**

service.go Store 接口加：

```go
type TemplateStore interface {
	...
	DeleteTemplatesBatch(ctx context.Context, ids []int64) error
	UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error
}
// AccountStore: DeleteAccountsBatch / UpdateAccountsBatch(ctx, ids, repository.AccountPatch)
// GroupStore: DeleteGroupsBatch / UpdateGroupsBatch(ctx, ids, repository.GroupPatch)
```

`service/template.go`：

```go
func (s *Service) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := s.store.DeleteTemplatesBatch(ctx, ids); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func (s *Service) UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := validateTemplatePatch(p); err != nil {
		return err
	}
	if err := s.store.UpdateTemplatesBatch(ctx, ids, p); err != nil {
		return err
	}
	s.invalidate()
	return nil
}
```

公共 helper（service.go）：

```go
// validateIDs ids 1–100 且去重（handler 已做，service 兜底）。
func validateIDs(ids []int64) error {
	if len(ids) == 0 || len(ids) > 100 {
		return ErrInvalidInput
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	return nil
}
```

批量 patch 校验（复用单更新语义，仅校验提供的字段）：

```go
func validateTemplatePatch(p repository.TemplatePatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.BaseURL != nil {
		u, err := url.Parse(*p.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return ErrInvalidInput
		}
	}
	if p.DefaultFormat != nil && !p.DefaultFormat.Valid() {
		return ErrInvalidInput
	}
	if p.ModelFormats != nil {
		for _, f := range *p.ModelFormats {
			if !f.Valid() {
				return ErrInvalidInput
			}
		}
	}
	return nil
}

func validateAccountPatch(p repository.AccountPatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.UpstreamKey != nil && *p.UpstreamKey == "" {
		return ErrInvalidInput
	}
	if p.TemplateID != nil && *p.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if p.Weight != nil && *p.Weight < 0 {
		return ErrInvalidInput
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency < 1 {
		return ErrInvalidInput
	}
	return nil
}
```

`service/account.go`：DeleteAccountsBatch（校验 ids + store + invalidate）、UpdateAccountsBatch（校验 ids + validateAccountPatch + store + invalidate）、`service/group.go`：DeleteGroupsBatch（**key 清理**——对齐单删：

```go
func (s *Service) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	for _, id := range ids {
		g, err := s.store.GetGroup(ctx, id)
		if err != nil {
			return err // 404 缺 id
		}
		if s.keys != nil {
			s.keys.Delete(g.KeyHash)
		}
	}
	if err := s.store.DeleteGroupsBatch(ctx, ids); err != nil {
		return err // 事务回滚；key 已删但 DB 未删——与单删同性质（失败自愈：DB 仍在则 key 下次重载恢复）
	}
	s.invalidate()
	return nil
}

func (s *Service) UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if err := s.store.UpdateGroupsBatch(ctx, ids, p); err != nil {
		return err
	}
	s.invalidate()
	return nil
}
```

fakestore_test.go：加 6 个方法（fake 内部切片删除/更新，按既有 fake 风格）。

repository.go Store 门面：6 个批量委托。

service_test.go：加批量测试（fakestore 验证：成功路径 invalidate 调用、空/超长/重复 ids → ErrInvalidInput、patch 校验失败 → ErrInvalidInput）。

- [ ] **Step 4: handler 6 个批量方法**

`handler/template.go` 追加（账号/分组同型）：

```go
// PostTemplatesBatchDelete 批量删除模板（事务，全成或全败，ServerInterface）。
func (h *AdminAPI) PostTemplatesBatchDelete(w http.ResponseWriter, r *http.Request) {
	var in BatchDeleteBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.DeleteTemplatesBatch(r.Context(), ids); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostTemplatesBatchUpdate 批量更新模板（fields 任意子集，ServerInterface）。
func (h *AdminAPI) PostTemplatesBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateTemplatesBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := templatePatchFromBody(in.Fields)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateTemplatesBatch(r.Context(), ids, p); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}
```

公共 helper（handler.go 或新文件）：

```go
// normalizeIDs 校验 ids 1–100 且去重（返回去重后列表）。
func normalizeIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, errors.New("ids must contain 1-100 entries")
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// templatePatchFromBody 生成类型 fields → repo patch（nil 表示字段未提供）。
func templatePatchFromBody(f *TemplatePatch) (repository.TemplatePatch, error) {
	if f == nil {
		return repository.TemplatePatch{}, errors.New("fields is required")
	}
	p := repository.TemplatePatch{
		Name:          f.Name,
		BaseURL:       f.BaseUrl,
		DefaultFormat: (*domain.RequestFormat)(f.DefaultFormat),
		Models:        f.Models,
		ModelMapping:  f.ModelMapping,
	}
	if f.ModelFormats != nil {
		m := make(map[string]domain.RequestFormat, len(*f.ModelFormats))
		for k, v := range *f.ModelFormats {
			m[k] = domain.RequestFormat(v)
		}
		p.ModelFormats = &m
	}
	return p, nil
}
```

（`f.DefaultFormat` 是 `*RequestFormat`（生成类型），`(*domain.RequestFormat)` 转换。字段全 optional 生成指针。）

account 的转换：

```go
func accountPatchFromBody(f *AccountPatch) (repository.AccountPatch, error) {
	if f == nil {
		return repository.AccountPatch{}, errors.New("fields is required")
	}
	return repository.AccountPatch{
		Name:           f.Name,
		TemplateID:     f.TemplateId,
		UpstreamKey:    f.UpstreamKey,
		Status:         (*domain.AccountStatus)(f.Status),
		Weight:         f.Weight,
		MaxConcurrency: f.MaxConcurrency,
	}, nil
}
```

（注意：生成类型 AccountPatch 的字段名——`TemplateId`/`UpstreamKey`/`MaxConcurrency`，与 accountBody 一致——生成后以 api.gen.go 实际为准，Task 4 实现者按生成文件核对。）

`handler/account.go`：PostAccountsBatchDelete/PostAccountsBatchUpdate（patch 转换 accountPatchFromBody + 存在性检查——**账号批量更新/删除后调度快照失效**：service 已 invalidate）。`handler/group.go`：PostGroupsBatchDelete/PostGroupsBatchUpdate（groupPatchFromBody：仅 Name）。

**错误映射**：repo.ErrNotFound → service 转 ErrNotFound（service 层各批量方法把 repo.ErrNotFound 映射为 service.ErrNotFound → writeServiceErr 404）。service 层：

```go
// 在 service 批量方法中：
if errors.Is(err, repository.ErrNotFound) {
	return service.ErrNotFound  // 或直接返回映射后的错误
}
```

**简化**：service 批量方法直接返回 err，并在 service.go 加映射 helper：`func mapRepoErr(err error) error { if errors.Is(err, repository.ErrNotFound) { return ErrNotFound }; return err }`，各批量方法 `return mapRepoErr(s.store.DeleteTemplatesBatch(...))`。404 消息含缺失 id？spec 说"404 ErrorResponse 含缺失 id"——ErrNotFound 不带 id。扩展：repo 返回 `fmt.Errorf("%w: id=%d", ErrNotFound, id)`？事务中先检查存在性（checkExist 返回 ErrNotFound 不带 id）。实现：checkExist 返回带缺失 id 的错误：先 SELECT 存在的 ids，差集算出缺失 id 拼进错误。**裁决**：checkExist 改为查询实际存在的 ids，缺的 id 拼进错误消息：

```go
func checkTemplateExist(ctx context.Context, q *ent.TemplateQuery, ids []int64) error {
	got, err := q.Where(template.IDIn(ids...)).Select(template.FieldID).Ints(ctx)
	if err != nil {
		return err
	}
	have := make(map[int64]struct{}, len(got))
	for _, id := range got {
		have[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := have[id]; !ok {
			return fmt.Errorf("%w: id=%d", ErrNotFound, id)
		}
	}
	return nil
}
```

service 映射后 404 消息 "not found: id=5"。handler writeServiceErr 的 404 分支目前写死 "not found"——改 404 分支输出 err.Error()？既有行为 "not found" 会被改。**裁决**：404 分支 `writeErr(w, http.StatusNotFound, err.Error())`（ErrNotFound 的 Error() = "service: not found"，映射后带 id 的消息需自定义错误类型。简单方案：service 定义 `type NotFoundError struct{ ID int64 }`？过度设计。**简化**：404 响应体消息 "not found" 保持，id 信息通过 400？不——spec 已写"404 ErrorResponse 含缺失 id"。实现：service 层把 repo 错误包装为带 id 的消息：

service 批量方法：

```go
if err := s.store.DeleteTemplatesBatch(ctx, ids); err != nil {
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, strings.TrimPrefix(err.Error(), "repository: not found: "))
	}
	return err
}
```

太绕。**最终裁决（写进计划）**：handler 的 writeServiceErr 404 分支改为输出 err.Error()；repo.ErrNotFound 错误消息格式 `repository: not found: id=5`；service 映射为 `fmt.Errorf("not found: id=5")`（不 wrap service.ErrNotFound，直接 errors.Is 判断后自定义）：service 批量方法：

```go
if err := s.store.DeleteTemplatesBatch(ctx, ids); err != nil {
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("not found: %v", err) // 消息 "not found: repository: not found: id=5" 太丑
	}
	return err
}
```

**再简化**：404 时消息直接 "not found"（不带 id）+ **400 语义不需要**——spec 说含缺失 id 是我 spec 里写的。修正 spec 语义为"404 + 消息含缺失 id（如 'id 5 not found'）"——实现：repo 错误 `ErrNotFound = errors.New("repository: not found")`，包装 `fmt.Errorf("%w: id %d not found", ErrNotFound, id)`；service 映射 `errors.Is(err, repository.ErrNotFound) → fmt.Errorf("id %d not found", 从消息解析？)` 不行。

**最简可行**：handler 层 writeServiceErr 对 404 输出 `err.Error()`；repo 包装错误消息为 `"id 5 not found"`（wrap ErrNotFound 仅用于 errors.Is 判定）；service 用 `%w` 包 service.ErrNotFound 并保留内部消息：`fmt.Errorf("%w: %v", ErrNotFound, id缺失信息)`？ErrNotFound 本身 "service: not found" + 追加 "id 5"。

**定案（写进计划，简洁）**：

```go
// repo/batch.go
func checkExist(ctx, q, field, ids) error {
	// q.Where(field In ids).Select(field).Ints(ctx) → 差集
	// 缺失: return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
}
// service 批量方法:
if err := s.store.DeleteTemplatesBatch(ctx, ids); err != nil {
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("not found: %v", err) // → "not found: id=5 missing"
	}
	return err
}
// handler writeServiceErr 404 分支: writeErr(w, 404, err.Error())
```

writeServiceErr 404 分支改：`writeErr(w, http.StatusNotFound, err.Error())`——对既有 404（单资源 Get 不存在）消息从 "not found" 变 "service: not found"（writeServiceErr 现有 404 是写死 "not found"；Get 路径 svc 返回 service.ErrNotFound，Error()="service: not found"）——既有 handler_test 断言 404 body 需要同步（Read handler_test 查 404 断言，统一改为 "service: not found" 或保持写死 "not found"——**保持写死**避免破坏既有断言：404 分支 `writeErr(w, 404, "not found")` 不动；批量 404 的特殊消息在 handler 批量方法内单独处理：`if errors.Is(err, service.ErrNotFound) { writeErr(w, 404, err.Error()) }` else writeServiceErr。ErrNotFound wrap 的消息是 "not found: id=5 missing"？service 返回 `fmt.Errorf("%w: id=5 missing", ErrNotFound)` → Error() = "service: not found: id=5 missing" → 404 响应 "service: not found: id=5 missing"。可以接受（管理端 API）。

好——计划里写：service 批量方法 wrap：`fmt.Errorf("%w: %s", ErrNotFound, missingMsg)`，handler 批量方法先 `errors.Is(err, service.ErrNotFound)` → `writeErr(w, 404, err.Error())` 否则 writeServiceErr。

- [ ] **Step 5: handler_test 批量测试（4b 执行；含成功/空 ids 400/超长 400/重复去重/字段非法 400/缺 id 404）**

```go
func TestPostTemplatesBatchDelete(t *testing.T) {
	// 成功：POST /admin/templates/batch-delete {"ids":[1,2]} → 200 {"deleted":2}
	// 空 ids → 400；重复 ids 去重 → {"deleted":1}
}
func TestPostAccountsBatchUpdate(t *testing.T) {
	// {"ids":[1,2],"fields":{"status":"disabled"}} → 200 {"updated":2}
	// fields 非法 status → 400；fields 空对象 → 400
}
func TestPostGroupsBatchDeleteMissing(t *testing.T) {
	// {"ids":[999]}（fake 不存在）→ 404
}
```

（fake store 需实现批量方法——fakestore_test.go Step 3 已加；fake 的 DeleteTemplatesBatch 对缺失 id 返回 repository.ErrNotFound 模拟。）

- [ ] **Step 6: 跑测试（4b 执行；**必须在 Task 4a 提交后**）**

Run: `go test ./internal/handler/ ./internal/service/ ./internal/repository/`
Expected: 全绿

- [ ] **Step 7: 重新生成 TS 类型（4b 执行）**

Run: `cd web && pnpm_config_verify_deps_before_run=false pnpm run gen:api`
Expected: schema.d.ts 更新（批量端点 + patch 类型）。不跑 web build。

- [ ] **Step 8: Commit（4b 执行）**

```bash
git add openapi/openapi.yaml internal/handler web/src/lib/api/schema.d.ts
git commit -m "feat: batch delete/update endpoints (contract + handler)"
```

---

### Task 5: 全量回归 + e2e 验证 + 文档同步

**Files:**
- Modify: `docs/admin-api.md`（列表参数/响应 + 批量端点文档）
- Modify: `docs/superpowers/specs/2026-08-07-admin-list-pagination-batch-design.md`（如实现有偏差，回填）
- Test: 全量

**Interfaces:**
- 消费：全部前面任务的产出

- [ ] **Step 1: 全量测试与静态检查**

Run: `go test ./... && go vet ./... && golangci-lint run ./...`
Expected: 全绿

- [ ] **Step 2: e2e curl 级冒烟**

本地起网关（config.toml 存在，token dev-admin-token，端口 18080——控制器维护中；若不在则 docker PG + 起实例）：

```bash
TOKEN=dev-admin-token
# 分页/筛选/排序
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:18080/admin/templates?limit=5&offset=0&sort=name&order=asc"
#   → 200 {"total": N, "rows": [...]}
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:18080/admin/accounts?status=active,disabled&template_id=1"
#   → 200 {total, rows}
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:18080/admin/groups?sort=bogus"
#   → 400 {"error": ...}
# 批量
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"ids":[1,2]}' "http://127.0.0.1:18080/admin/templates/batch-delete"  # 用临时创建的模板 id
#   → 200 {"deleted":2}
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"ids":[1],"fields":{"status":"disabled"}}' "http://127.0.0.1:18080/admin/accounts/batch-update"
#   → 200 {"updated":1}
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"ids":[99999]}' "http://127.0.0.1:18080/admin/templates/batch-delete"
#   → 404 {"error": "...id=99999..."}
```

（e2e 数据用临时创建的资源，测完清理；不在控制器 dev server 上破坏既有演示数据——用新创建的数据测。）

- [ ] **Step 3: 文档同步**

docs/admin-api.md：三个列表端点补参数表 + {total, rows} 响应示例；新增批量端点小节（请求/响应/错误语义表）。

- [ ] **Step 4: Commit**

```bash
git add docs/admin-api.md docs/superpowers/specs
git commit -m "docs: admin list pagination/batch API reference"
```

---

## Self-Review 记录

- **Spec 覆盖**：分页（Task 1/2）✓；筛选（Task 1/2）✓；排序白名单（Task 1 service+repo 双层校验）✓；多值 status（Task 2 handler 拆逗号 + repo StatusIn）✓；{total, rows}（Task 2）✓；批量删除/更新（Task 3/4）✓；事务全成或全败 + 存在性检查（Task 3）✓；404 含缺失 id（Task 4）✓；契约先行 + schema.d.ts（Task 2/4）✓；测试矩阵（各任务）✓；文档（Task 5）✓
- **占位符扫描**：无 TBD/TODO；每个代码步骤含完整代码
- **类型一致性**：ListQuery 字段名统一；Patch 结构 repo→service→handler 一致；生成类型字段名以 api.gen.go 实际为准（Task 4 注明核对）；service.ListAccountViews 返回 (views, total, err) 在 Task 1/2 一致
- **已知偏差**：spec 的 404 消息含缺失 id 通过 service wrap 实现（ErrNotFound wrap 消息）；writeServiceErr 404 分支保持写死 "not found"，批量 404 在 handler 单独处理
