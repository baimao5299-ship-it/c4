# Web UI + OpenAPI Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an OpenAPI contract layer (oapi-codegen server stub + openapi-typescript client types) and a full admin web UI (React + Vite + TS + Tailwind + shadcn/ui + Framer Motion + Lucide) embedded into the Go binary.

**Architecture:** `openapi/openapi.yaml` is the single contract source. oapi-codegen generates Go types + chi server interface (handler methods renamed to match, bodies preserved, responses converted to generated types). openapi-typescript generates TS types consumed by a thin fetch client in `/web`. Dev: Vite proxy to the gateway. Prod: `go:embed web/dist` served by chi with SPA fallback.

**Tech Stack:** Go 1.26.5, oapi-codegen v2, chi; React 18 + Vite + TypeScript + Tailwind + shadcn/ui + Framer Motion + Lucide + TanStack Query + react-router.

## Global Constraints

- OpenAPI spec (`openapi/openapi.yaml`) covers ALL 15 admin operations; AI endpoints (`/v1/*`) are NOT in the spec.
- Response schemas use **uppercase Go-style field names** (ID/Name/BaseURL…) matching current wire format exactly; request bodies stay snake_case. No breaking change to the deployed admin API.
- operationId → Go method names: PostTemplates/GetTemplates/GetTemplatesId/PutTemplatesId/DeleteTemplatesId, PostAccounts/GetAccounts/GetAccountsId/PutAccountsId/DeleteAccountsId, PostGroups/GetGroups/GetGroupsId/PutGroupsId/DeleteGroupsId/PutGroupsIdAccounts/PostGroupsIdRotateKey, GetLogs/GetStats.
- oapi-codegen v2 with `-generate types,chi-server`; generated file `internal/handler/api.gen.go` committed to the repo (build does not depend on the toolchain).
- Generated TS types at `web/src/lib/api/schema.d.ts` via openapi-typescript, committed.
- Auth: existing admin Bearer middleware unchanged; frontend stores token in localStorage (`gpm_admin_token`), injects `Authorization: Bearer`, clears on 401.
- Frontend pages: login + dashboard + templates + accounts + groups + logs + stats (full management surface, spec §前端).
- Prod embedding: `cmd/server/embed.go` with `//go:embed all:web/dist`, chi serves `/assets/*` + SPA fallback; empty/missing dist must not crash the server.
- Verify: `go test ./...`, `go vet ./...`, `golangci-lint run ./...`; `web`: `npm run build` + `tsc --noEmit`; end-to-end smoke on the built single binary.

---

### Task 1: OpenAPI contract + handler adaptation

**Files:**
- Create: `openapi/openapi.yaml`
- Create: `internal/handler/generate.go` (go:generate directive)
- Create: `internal/handler/api.gen.go` (generated — committed)
- Create: `internal/handler/convert.go` (domain → generated types)
- Modify: `internal/handler/template.go`, `account.go`, `group.go`, `log.go`, `stat.go`, `handler.go` (method renames, request/response types, route wiring)
- Modify: `internal/server/server.go` (admin router mount)
- Modify: `internal/handler/handler_test.go` (route wiring)
- Modify: `go.mod` (oapi-codegen is a tool dep; add to tools if needed)

**Interfaces:**
- Produces: `api.gen.go` types (`Template`, `Account`, `AccountView`, `Group`, `CreateGroupResponse`, `UsageLog`, `LogsResponse`, `StatBucket`, `DeletedResponse`, `UpdatedResponse`, `ErrorResponse`, request body types, `GetLogsParams`, `GetStatsParams`); `ServerInterface`; `HandlerWithOptions` chi router with `/admin` base; `convert.go` conversion functions.
- Consumes: `domain.*`, `service.Service`, existing `writeJSON`/`writeErr` helpers (adapted to ErrorResponse).

- [ ] **Step 1: Verify oapi-codegen tooling availability**

Run:
```bash
go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 --help | head -5
```
Expected: prints help (network fetch of the tool). If offline, use the module cache version (`go env GOMODCACHE`); pin v2.4.1 in the go:generate directive.

- [ ] **Step 2: Write the OpenAPI spec**

Create `openapi/openapi.yaml` with EXACTLY this content (complete spec — do not abbreviate; all 15 operations, all schemas with uppercase response fields):

```yaml
openapi: 3.0.3
info:
  title: go-proxy-mini Admin API
  version: 1.0.0
  description: 管理端 API（模板/账号/分组/日志/统计）。认证：Authorization: Bearer <admin_token>。
servers:
  - url: /admin
tags:
  - name: templates
  - name: accounts
  - name: groups
  - name: logs
  - name: stats
paths:
  /templates:
    post:
      operationId: PostTemplates
      tags: [templates]
      summary: 创建模板
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/TemplateCreate' }
      responses:
        '200':
          description: 创建后的模板
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Template' }
        default:
          $ref: '#/components/responses/Error'
    get:
      operationId: GetTemplates
      tags: [templates]
      summary: 模板列表
      responses:
        '200':
          description: 模板数组
          content:
            application/json:
              schema:
                type: array
                items: { $ref: '#/components/schemas/Template' }
        default:
          $ref: '#/components/responses/Error'
  /templates/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: integer, format: int64 } }
    get:
      operationId: GetTemplatesId
      tags: [templates]
      responses:
        '200':
          description: 模板
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Template' }
        default:
          $ref: '#/components/responses/Error'
    put:
      operationId: PutTemplatesId
      tags: [templates]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/TemplateCreate' }
      responses:
        '200':
          description: 更新后模板
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Template' }
        default:
          $ref: '#/components/responses/Error'
    delete:
      operationId: DeleteTemplatesId
      tags: [templates]
      responses:
        '200':
          description: 删除成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/DeletedResponse' }
        default:
          $ref: '#/components/responses/Error'
  /accounts:
    post:
      operationId: PostAccounts
      tags: [accounts]
      summary: 创建账号
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/AccountCreate' }
      responses:
        '200':
          description: 创建后的账号
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Account' }
        default:
          $ref: '#/components/responses/Error'
    get:
      operationId: GetAccounts
      tags: [accounts]
      summary: 账号列表（含运行时视图）
      responses:
        '200':
          description: 账号视图数组
          content:
            application/json:
              schema:
                type: array
                items: { $ref: '#/components/schemas/AccountView' }
        default:
          $ref: '#/components/responses/Error'
  /accounts/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: integer, format: int64 } }
    get:
      operationId: GetAccountsId
      tags: [accounts]
      responses:
        '200':
          description: 账号
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Account' }
        default:
          $ref: '#/components/responses/Error'
    put:
      operationId: PutAccountsId
      tags: [accounts]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/AccountCreate' }
      responses:
        '200':
          description: 更新后账号
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Account' }
        default:
          $ref: '#/components/responses/Error'
    delete:
      operationId: DeleteAccountsId
      tags: [accounts]
      responses:
        '200':
          description: 删除成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/DeletedResponse' }
        default:
          $ref: '#/components/responses/Error'
  /groups:
    post:
      operationId: PostGroups
      tags: [groups]
      summary: 创建分组（响应含明文 key，仅此一次）
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/GroupCreate' }
      responses:
        '200':
          description: 分组 + 明文 key
          content:
            application/json:
              schema: { $ref: '#/components/schemas/CreateGroupResponse' }
        default:
          $ref: '#/components/responses/Error'
    get:
      operationId: GetGroups
      tags: [groups]
      responses:
        '200':
          description: 分组数组
          content:
            application/json:
              schema:
                type: array
                items: { $ref: '#/components/schemas/Group' }
        default:
          $ref: '#/components/responses/Error'
  /groups/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: integer, format: int64 } }
    get:
      operationId: GetGroupsId
      tags: [groups]
      responses:
        '200':
          description: 分组
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Group' }
        default:
          $ref: '#/components/responses/Error'
    put:
      operationId: PutGroupsId
      tags: [groups]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/GroupCreate' }
      responses:
        '200':
          description: 更新后分组
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Group' }
        default:
          $ref: '#/components/responses/Error'
    delete:
      operationId: DeleteGroupsId
      tags: [groups]
      responses:
        '200':
          description: 删除成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/DeletedResponse' }
        default:
          $ref: '#/components/responses/Error'
  /groups/{id}/accounts:
    parameters:
      - { name: id, in: path, required: true, schema: { type: integer, format: int64 } }
    put:
      operationId: PutGroupsIdAccounts
      tags: [groups]
      summary: 全量绑定账号集合
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/SetGroupAccountsBody' }
      responses:
        '200':
          description: 绑定成功
          content:
            application/json:
              schema: { $ref: '#/components/schemas/UpdatedResponse' }
        default:
          $ref: '#/components/responses/Error'
  /groups/{id}/rotate-key:
    parameters:
      - { name: id, in: path, required: true, schema: { type: integer, format: int64 } }
    post:
      operationId: PostGroupsIdRotateKey
      tags: [groups]
      summary: 轮换分组 key
      responses:
        '200':
          description: 新明文 key
          content:
            application/json:
              schema: { $ref: '#/components/schemas/RotateKeyResponse' }
        default:
          $ref: '#/components/responses/Error'
  /logs:
    get:
      operationId: GetLogs
      tags: [logs]
      summary: 用量日志分页查询
      parameters:
        - { name: limit, in: query, schema: { type: integer, default: 20 } }
        - { name: offset, in: query, schema: { type: integer, default: 0 } }
        - { name: group_id, in: query, schema: { type: integer, format: int64 } }
        - { name: account_id, in: query, schema: { type: integer, format: int64 } }
        - { name: model, in: query, schema: { type: string } }
        - { name: status_code, in: query, schema: { type: integer } }
        - { name: error_type, in: query, schema: { type: string } }
        - { name: from, in: query, schema: { type: string, format: date-time } }
        - { name: to, in: query, schema: { type: string, format: date-time } }
      responses:
        '200':
          description: 分页结果
          content:
            application/json:
              schema: { $ref: '#/components/schemas/LogsResponse' }
        default:
          $ref: '#/components/responses/Error'
  /stats:
    get:
      operationId: GetStats
      tags: [stats]
      summary: 用量统计聚合
      parameters:
        - { name: from, in: query, schema: { type: string, format: date-time } }
        - { name: to, in: query, schema: { type: string, format: date-time } }
        - { name: granularity, in: query, schema: { type: string, enum: [hour, day], default: day } }
        - { name: group_id, in: query, schema: { type: integer, format: int64 } }
        - { name: account_id, in: query, schema: { type: integer, format: int64 } }
        - { name: model, in: query, schema: { type: string } }
      responses:
        '200':
          description: 统计桶数组
          content:
            application/json:
              schema:
                type: array
                items: { $ref: '#/components/schemas/StatBucket' }
        default:
          $ref: '#/components/responses/Error'
components:
  responses:
    Error:
      description: 错误响应
      content:
        application/json:
          schema: { $ref: '#/components/schemas/ErrorResponse' }
  schemas:
    ErrorResponse:
      type: object
      required: [error]
      properties:
        error: { type: string }
    DeletedResponse:
      type: object
      required: [deleted]
      properties:
        deleted: { type: boolean }
    UpdatedResponse:
      type: object
      required: [updated]
      properties:
        updated: { type: boolean }
    RequestFormat:
      type: string
      enum: [openai-chat, openai-responses, anthropic]
    AccountStatus:
      type: string
      enum: [active, unhealthy, "429", disabled]
    ErrorType:
      type: string
      enum: [none, "429", "4xx", "5xx", network, auth, no_account, abort]
    TemplateCreate:
      type: object
      required: [name, base_url, default_format]
      properties:
        name: { type: string }
        base_url: { type: string }
        default_format: { $ref: '#/components/schemas/RequestFormat' }
        models:
          type: array
          items: { type: string }
        model_formats:
          type: object
          additionalProperties: { $ref: '#/components/schemas/RequestFormat' }
        model_mapping:
          type: object
          additionalProperties: { type: string }
    Template:
      type: object
      properties:
        ID: { type: integer, format: int64 }
        Name: { type: string }
        BaseURL: { type: string }
        DefaultFormat: { $ref: '#/components/schemas/RequestFormat' }
        Models:
          type: array
          items: { type: string }
        ModelFormats:
          type: object
          additionalProperties: { $ref: '#/components/schemas/RequestFormat' }
        ModelMapping:
          type: object
          additionalProperties: { type: string }
        CreatedAt: { type: string, format: date-time }
        UpdatedAt: { type: string, format: date-time }
    AccountCreate:
      type: object
      required: [name, template_id, upstream_key]
      properties:
        name: { type: string }
        template_id: { type: integer, format: int64 }
        upstream_key: { type: string }
        status: { $ref: '#/components/schemas/AccountStatus' }
        weight: { type: integer }
        max_concurrency: { type: integer }
    Account:
      type: object
      properties:
        ID: { type: integer, format: int64 }
        Name: { type: string }
        TemplateID: { type: integer, format: int64 }
        Template: { $ref: '#/components/schemas/Template' }
        UpstreamKey: { type: string }
        Status: { $ref: '#/components/schemas/AccountStatus' }
        CooldownUntil: { type: string, format: date-time, nullable: true }
        Weight: { type: integer }
        MaxConcurrency: { type: integer }
        LastError: { type: string, nullable: true }
        LastUsedAt: { type: string, format: date-time, nullable: true }
        CreatedAt: { type: string, format: date-time }
        UpdatedAt: { type: string, format: date-time }
    AccountView:
      allOf:
        - $ref: '#/components/schemas/Account'
        - type: object
          properties:
            concurrency: { type: integer, format: int64 }
            err_rate: { type: number, format: double }
            err_count: { type: integer }
    GroupCreate:
      type: object
      required: [name]
      properties:
        name: { type: string }
    Group:
      type: object
      properties:
        ID: { type: integer, format: int64 }
        Name: { type: string }
        KeyHash: { type: string }
        KeyPrefix: { type: string }
        CreatedAt: { type: string, format: date-time }
        UpdatedAt: { type: string, format: date-time }
    CreateGroupResponse:
      type: object
      required: [group, key]
      properties:
        group: { $ref: '#/components/schemas/Group' }
        key: { type: string }
    RotateKeyResponse:
      type: object
      required: [key]
      properties:
        key: { type: string }
    SetGroupAccountsBody:
      type: object
      required: [account_ids]
      properties:
        account_ids:
          type: array
          items: { type: integer, format: int64 }
    UsageLog:
      type: object
      properties:
        ID: { type: integer, format: int64 }
        RequestID: { type: string }
        GroupID: { type: integer, format: int64 }
        AccountID: { type: integer, format: int64 }
        TemplateID: { type: integer, format: int64 }
        Model: { type: string }
        MappedModel: { type: string, nullable: true }
        Format: { $ref: '#/components/schemas/RequestFormat' }
        StatusCode: { type: integer }
        ErrorType: { $ref: '#/components/schemas/ErrorType' }
        LatencyMS: { type: integer, format: int64 }
        PromptTokens: { type: integer, format: int64 }
        CompletionTokens: { type: integer, format: int64 }
        TotalTokens: { type: integer, format: int64 }
        CreatedAt: { type: string, format: date-time }
    LogsResponse:
      type: object
      required: [total, rows]
      properties:
        total: { type: integer, format: int64 }
        rows:
          type: array
          items: { $ref: '#/components/schemas/UsageLog' }
    StatBucket:
      type: object
      properties:
        BucketTime: { type: string, format: date-time }
        GroupID: { type: integer, format: int64 }
        AccountID: { type: integer, format: int64 }
        TemplateID: { type: integer, format: int64 }
        Model: { type: string }
        IsError: { type: boolean }
        RequestCount: { type: integer, format: int64 }
        ErrorCount: { type: integer, format: int64 }
        PromptTokens: { type: integer, format: int64 }
        CompletionTokens: { type: integer, format: int64 }
        TotalTokens: { type: integer, format: int64 }
        TotalLatencyMS: { type: integer, format: int64 }
```

- [ ] **Step 3: Create the go:generate directive**

Create `internal/handler/generate.go`:

```go
package handler

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -package handler -o api.gen.go ../../openapi/openapi.yaml
```

- [ ] **Step 4: Generate api.gen.go and commit**

Run (from the repo root):
```bash
go generate ./internal/handler/
go build ./internal/handler/
```
Expected: `internal/handler/api.gen.go` created, compiles. Verify it contains: `ServerInterface` with all 19 methods (15 operations + path-param variants are the same 15; count 15 unique operationIds), `HandlerWithOptions`, types `Template/Account/AccountView/Group/CreateGroupResponse/RotateKeyResponse/SetGroupAccountsBody/UsageLog/LogsResponse/StatBucket/DeletedResponse/UpdatedResponse/ErrorResponse/TemplateCreate/AccountCreate/GroupCreate`, `GetLogsParams`, `GetStatsParams`, and the enums.

Do NOT commit yet (Step 8 commits with the handler adaptation).

- [ ] **Step 5: Implement the ServerInterface on Handler (rename methods, convert responses)**

In each handler file, rename the methods to the generated interface names and adapt bodies. The generated interface has this shape (verify from api.gen.go; chi-server mode does NOT pass path params — extract via `chi.URLParam(r, "id")`; query params come as a `*GetLogsParams` second argument):

```go
type ServerInterface interface {
	// 创建模板
	// (POST /templates)
	PostTemplates(w http.ResponseWriter, r *http.Request)
	// 模板列表
	// (GET /templates)
	GetTemplates(w http.ResponseWriter, r *http.Request)
	// (GET /templates/{id})
	GetTemplatesId(w http.ResponseWriter, r *http.Request, id int64)
	// (PUT /templates/{id})
	PutTemplatesId(w http.ResponseWriter, r *http.Request, id int64)
	// (DELETE /templates/{id})
	DeleteTemplatesId(w http.ResponseWriter, r *http.Request, id int64)
	// (POST /accounts)
	PostAccounts(w http.ResponseWriter, r *http.Request)
	// (GET /accounts)
	GetAccounts(w http.ResponseWriter, r *http.Request)
	// (GET /accounts/{id})
	GetAccountsId(w http.ResponseWriter, r *http.Request, id int64)
	// (PUT /accounts/{id})
	PutAccountsId(w http.ResponseWriter, r *http.Request, id int64)
	// (DELETE /accounts/{id})
	DeleteAccountsId(w http.ResponseWriter, r *http.Request, id int64)
	// (POST /groups)
	PostGroups(w http.ResponseWriter, r *http.Request)
	// (GET /groups)
	GetGroups(w http.ResponseWriter, r *http.Request)
	// (GET /groups/{id})
	GetGroupsId(w http.ResponseWriter, r *http.Request, id int64)
	// (PUT /groups/{id})
	PutGroupsId(w http.ResponseWriter, r *http.Request, id int64)
	// (DELETE /groups/{id})
	DeleteGroupsId(w http.ResponseWriter, r *http.Request, id int64)
	// (PUT /groups/{id}/accounts)
	PutGroupsIdAccounts(w http.ResponseWriter, r *http.Request, id int64)
	// (POST /groups/{id}/rotate-key)
	PostGroupsIdRotateKey(w http.ResponseWriter, r *http.Request, id int64)
	// (GET /logs)
	GetLogs(w http.ResponseWriter, r *http.Request, params GetLogsParams)
	// (GET /stats)
	GetStats(w http.ResponseWriter, r *http.Request, params GetStatsParams)
}
```

IMPORTANT: verify the actual generated signatures from api.gen.go — chi-server mode historically passes path params explicitly (as shown above) and query params as a struct value (not pointer). If the generated signature differs, adapt to the ACTUAL generated interface, not the sketch above.

Adaptation pattern per method (example — template.go create):

```go
// PostTemplates 创建模板（ServerInterface）。
func (h *Handler) PostTemplates(w http.ResponseWriter, r *http.Request) {
	var in TemplateCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateTemplate(r.Context(), &domain.Template{
		Name: in.Name, BaseURL: in.BaseURL, DefaultFormat: in.DefaultFormat,
		Models: in.Models, ModelFormats: in.ModelFormats, ModelMapping: in.ModelMapping,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITemplate(created))
}
```

For each of the 15 methods, the mapping is:

| Old method | New method | Body decode type | Response conversion |
|---|---|---|---|
| createTemplate | PostTemplates | TemplateCreate | toAPITemplate |
| listTemplates | GetTemplates | — | slice of toAPITemplate |
| getTemplate | GetTemplatesId | — | toAPITemplate |
| updateTemplate | PutTemplatesId | TemplateCreate | toAPITemplate |
| deleteTemplate | DeleteTemplatesId | — | DeletedResponse{Deleted: true} |
| createAccount | PostAccounts | AccountCreate | toAPIAccount |
| listAccounts | GetAccounts | — | slice of toAPIAccountView |
| getAccount | GetAccountsId | — | toAPIAccount |
| updateAccount | PutAccountsId | AccountCreate | toAPIAccount |
| deleteAccount | DeleteAccountsId | — | DeletedResponse |
| createGroup | PostGroups | GroupCreate | CreateGroupResponse{Group, Key} |
| listGroups | GetGroups | — | slice of toAPIGroup |
| getGroup | GetGroupsId | — | toAPIGroup |
| updateGroup | PutGroupsId | GroupCreate | toAPIGroup |
| deleteGroup | DeleteGroupsId | — | DeletedResponse |
| setGroupAccounts | PutGroupsIdAccounts | SetGroupAccountsBody | UpdatedResponse |
| rotateGroupKey | PostGroupsIdRotateKey | — | RotateKeyResponse |
| queryLogs | GetLogs | params | LogsResponse |
| queryStats | GetStats | params | slice of toAPIStatBucket |

Notes:
- `pathID` currently extracts via `chi.URLParam(r, "id")`; with generated path params, use the passed `id int64` argument instead (keep pathID for any remaining callers or delete it).
- `deleteTemplate`'s service error (referenced template → 500) is unchanged.
- `updateTemplate`/`updateAccount` keep full-replacement semantics (build domain from the request body exactly as the old tagged structs did).
- `GetLogs` maps `params` to `repository.LogQuery`: limit/offset defaults 20/0 (spec default; if the generated type has pointers, apply defaults when nil); group_id/account_id/model/status_code/error_type when non-nil; from/to parsed from `*time.Time` (generated types for date-time query params are `*time.Time` when format: date-time — verify).
- `GetStats`: defaults from = now-24h, to = now (existing behavior); granularity hour/day (invalid → day); parse from/to only if non-nil.
- `createGroup` returns `CreateGroupResponse{Group: toAPIGroup(g), Key: raw}`.
- `writeErr`/`writeServiceErr` continue to write `{"error": "..."}` — they already match ErrorResponse; no change needed beyond keeping them.

- [ ] **Step 6: Write convert.go**

Create `internal/handler/convert.go`:

```go
package handler

import (
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

func toAPITemplate(t *domain.Template) Template {
	return Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL, DefaultFormat: t.DefaultFormat,
		Models: t.Models, ModelFormats: t.ModelFormats, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toAPIAccount(a *domain.Account) Account {
	return Account{
		ID: a.ID, Name: a.Name, TemplateID: a.TemplateID,
		UpstreamKey: a.UpstreamKey, Status: a.Status,
		CooldownUntil: a.CooldownUntil, Weight: a.Weight, MaxConcurrency: a.MaxConcurrency,
		LastError: a.LastError, LastUsedAt: a.LastUsedAt,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func toAPIAccountView(v *service.AccountView) AccountView {
	av := AccountView{
		Concurrency: v.Concurrency, ErrRate: v.ErrRate, ErrCount: v.ErrCount,
	}
	av.Account = toAPIAccount(v.Account)
	return av
}

func toAPIGroup(g *domain.Group) Group {
	return Group{
		ID: g.ID, Name: g.Name, KeyHash: g.KeyHash, KeyPrefix: g.KeyPrefix,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
}

func toAPIUsageLog(l *domain.UsageLog) UsageLog {
	return UsageLog{
		ID: l.ID, RequestID: l.RequestID, GroupID: l.GroupID, AccountID: l.AccountID,
		TemplateID: l.TemplateID, Model: l.Model, MappedModel: l.MappedModel,
		Format: l.Format, StatusCode: l.StatusCode, ErrorType: l.ErrorType,
		LatencyMS: l.LatencyMS, PromptTokens: l.PromptTokens,
		CompletionTokens: l.CompletionTokens, TotalTokens: l.TotalTokens,
		CreatedAt: l.CreatedAt,
	}
}

func toAPIStatBucket(b *domain.StatBucket) StatBucket {
	return StatBucket{
		BucketTime: b.BucketTime, GroupID: b.GroupID, AccountID: b.AccountID,
		TemplateID: b.TemplateID, Model: b.Model, IsError: b.IsError,
		RequestCount: b.RequestCount, ErrorCount: b.ErrorCount,
		PromptTokens: b.PromptTokens, CompletionTokens: b.CompletionTokens,
		TotalTokens: b.TotalTokens, TotalLatencyMS: b.TotalLatencyMS,
	}
}
```

NOTE: the generated type names/field names must match api.gen.go exactly (oapi-codegen v2 generates struct names equal to schema names: `Template`, `TemplateCreate`, `AccountView`, etc.; nullable fields like CooldownUntil become `*time.Time` with `json:"CooldownUntil,omitempty"` — the conversion above assigns `*time.Time` directly). If AccountView is generated as an allOf composition, the generated struct may embed `Account` — verify and adapt the conversion to the actual generated shape (if it embeds, `AccountView{Account: toAPIAccount(...), Concurrency: ...}` still works with the embedded field name).

- [ ] **Step 7: Wire the generated router**

Modify `internal/handler/handler.go`:

```go
// Handler 实现生成的 ServerInterface（契约层唯一实现）。
type Handler struct { svc *service.Service }

// NewHandler 构造契约处理器（路由由 HandlerWithOptions 生成）。
func NewHandler(svc *service.Service) *Handler { return &Handler{svc: svc} }

// Router 返回带 /admin 前缀的 chi 路由（替代原 Routes/RoutesMux）。
func (h *Handler) Router() http.Handler {
	return HandlerWithOptions(h, chi.ServerOptions{BaseURL: "/admin"})
}
```

Delete `Routes` and `RoutesMux` (no remaining callers after server.go change). Keep `pathID`/`decode` if still used.

Modify `internal/server/server.go` — the admin mount changes from `r.Handle("/admin/*", opts.AdminHandler)` to:

```go
if opts.AdminHandler != nil {
    r.Mount("/", opts.AdminHandler) // AdminHandler 自带 /admin 前缀（HandlerWithOptions BaseURL）
}
```

Verify the generated router does NOT also register `/admin` twice — with BaseURL "/admin" the generated paths are `/admin/templates` etc., so `r.Mount("/", ...)` is correct.

- [ ] **Step 8: Adapt tests and verify**

In `internal/handler/handler_test.go`:

- Replace `h.Routes(r)` (line ~46) with mounting the generated router. The test's `do()` helper uses `r.ServeHTTP(rec, req)` with paths like `/admin/templates` — with the generated router mounted at `/`, the paths stay the same:

```go
r := chi.NewRouter()
r.Use(func(next http.Handler) http.Handler { // admin token 中间件（测试内联，与 server 层一致）
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer admin-tok" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, req)
	})
})
r.Mount("/", h.Router())
```

Run: `go test ./internal/handler -v`
Expected: all existing tests pass with assertions unchanged (response bodies are byte-identical because the generated types encode the same JSON field names — verify this by running; if any assertion fails on field naming, the conversion is wrong, fix the conversion not the test).

Then full verification:
```bash
go test ./... && go vet ./... && golangci-lint run ./...
```
Expected: all green.

- [ ] **Step 9: Commit**

```bash
git add openapi/openapi.yaml internal/handler/generate.go internal/handler/api.gen.go internal/handler/convert.go internal/handler/template.go internal/handler/account.go internal/handler/group.go internal/handler/log.go internal/handler/stat.go internal/handler/handler.go internal/server/server.go internal/handler/handler_test.go go.mod go.sum
git commit -m "feat: OpenAPI contract layer (oapi-codegen) + handler adaptation"
```

---

### Task 2: Frontend scaffold + API client + login + layout

**Files:**
- Create: `web/` (package.json, vite.config.ts, tsconfig.json, tailwind.config.ts, postcss.config.js, index.html, src/main.tsx, src/App.tsx, src/lib/api/client.ts, src/lib/api/schema.d.ts (generated), src/lib/auth.ts, src/lib/format.ts, src/components/layout.tsx, src/components/ui/* (shadcn/ui), src/pages/login.tsx, src/index.css)
- Create: `web/scripts/gen-api.sh` (openapi-typescript)

**Interfaces:**
- Consumes: `openapi/openapi.yaml` (Task 1).
- Produces: a runnable Vite dev app at `web/` proxying `/admin` to the gateway; login flow; authenticated layout shell.

- [ ] **Step 1: Scaffold the Vite React TS project**

```bash
cd web
npm create vite@latest . -- --template react-ts   # scaffold in place
npm install
npm install tailwindcss @tailwindcss/vite lucide-react framer-motion @tanstack/react-query react-router-dom
npx shadcn@latest init -d   # defaults; then npx shadcn@latest add button card table dialog input label select tabs badge alert separator skeleton dropdown-menu
npm install -D openapi-typescript
```

NOTE: this task's environment may lack npm registry access — if offline, the implementer must scaffold manually: write `package.json` with pinned deps (react 18, vite 5, typescript 5, tailwind 3.4, framer-motion 11, lucide-react, @tanstack/react-query 5, react-router-dom 6, shadcn/ui components copied per shadcn convention, openapi-typescript 7), and report the offline deviation. The exact dependency versions must be recorded in the report for reproducibility.

- [ ] **Step 2: vite.config.ts with proxy**

```ts
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { '@': path.resolve(__dirname, './src') } },
  server: {
    proxy: {
      '/admin': { target: 'http://127.0.0.1:18080', changeOrigin: true },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
```

- [ ] **Step 3: Generate the TS schema**

Add to `web/package.json` scripts:
```json
"gen:api": "openapi-typescript ../openapi/openapi.yaml -o src/lib/api/schema.d.ts"
```
Run: `npm run gen:api` → `web/src/lib/api/schema.d.ts` generated, committed.

- [ ] **Step 4: Write the API client**

`web/src/lib/api/client.ts`:

```ts
// 薄 fetch 封装：token 注入、401 归一化、类型化返回（schema.d.ts 生成）。
// 响应字段为 Go 大写风格（ID/Name/...），前端按此使用，不做 camelCase 转换。
import type { components } from './schema.d.ts'

export type ApiError = { status: number; message: string }

export class ApiClient {
  private base = '/admin'
  constructor(private getToken: () => string | null) {}

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const token = this.getToken()
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(init?.headers as Record<string, string> | undefined) }
    if (token) headers['Authorization'] = `Bearer ${token}`
    const res = await fetch(`${this.base}${path}`, { ...init, headers })
    if (res.status === 401) throw new ApiUnauthorized()
    if (!res.ok) {
      const body = await res.json().catch(() => null)
      throw new ApiError(res.status, (body as { error?: string } | null)?.error ?? `HTTP ${res.status}`)
    }
    return res.json() as Promise<T>
  }
  // —— 模板 ——
  listTemplates = () => this.request<components['schemas']['Template'][]>('/templates')
  createTemplate = (b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>('/templates', { method: 'POST', body: JSON.stringify(b) })
  updateTemplate = (id: number, b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>(`/templates/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteTemplate = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/templates/${id}`, { method: 'DELETE' })
  // —— 账号 ——
  listAccounts = () => this.request<components['schemas']['AccountView'][]>('/accounts')
  createAccount = (b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>('/accounts', { method: 'POST', body: JSON.stringify(b) })
  updateAccount = (id: number, b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>(`/accounts/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteAccount = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/accounts/${id}`, { method: 'DELETE' })
  // —— 分组 ——
  listGroups = () => this.request<components['schemas']['Group'][]>('/groups')
  createGroup = (b: components['schemas']['GroupCreate']) => this.request<components['schemas']['CreateGroupResponse']>('/groups', { method: 'POST', body: JSON.stringify(b) })
  updateGroup = (id: number, b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteGroup = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/groups/${id}`, { method: 'DELETE' })
  setGroupAccounts = (id: number, accountIds: number[]) => this.request<components['schemas']['UpdatedResponse']>(`/groups/${id}/accounts`, { method: 'PUT', body: JSON.stringify({ account_ids: accountIds }) })
  rotateGroupKey = (id: number) => this.request<components['schemas']['RotateKeyResponse']>(`/groups/${id}/rotate-key`, { method: 'POST' })
  // —— 日志 / 统计 ——
  getLogs = (params: Record<string, string | number | undefined>) => {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') qs.set(k, String(v))
    const s = qs.toString()
    return this.request<components['schemas']['LogsResponse']>(`/logs${s ? `?${s}` : ''}`)
  }
  getStats = (params: Record<string, string | number | undefined>) => {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') qs.set(k, String(v))
    const s = qs.toString()
    return this.request<components['schemas']['StatBucket'][]>(`/stats${s ? `?${s}` : ''}`)
  }
}

export class ApiUnauthorized extends Error {
  constructor() { super('unauthorized'); this.name = 'ApiUnauthorized' }
}
```

`web/src/lib/auth.ts`:

```ts
const KEY = 'gpm_admin_token'
export const auth = {
  getToken: () => localStorage.getItem(KEY),
  setToken: (t: string) => localStorage.setItem(KEY, t),
  clear: () => localStorage.removeItem(KEY),
}
```

`web/src/App.tsx` — router + QueryClient + 401 interceptor (on ApiUnauthorized: clear token, redirect /login):

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { useEffect } from 'react'
import { ApiClient, ApiUnauthorized } from '@/lib/api/client'
import { auth } from '@/lib/auth'
import Login from '@/pages/login'
import Layout from '@/components/layout'
import Dashboard from '@/pages/dashboard'
import Templates from '@/pages/templates'
import Accounts from '@/pages/accounts'
import Groups from '@/pages/groups'
import Logs from '@/pages/logs'
import Stats from '@/pages/stats'

export const api = new ApiClient(auth.getToken)

const router = createBrowserRouter([
  { path: '/login', element: <Login /> },
  {
    path: '/',
    element: <Layout />,
    children: [
      { index: true, element: <Navigate to="/dashboard" replace /> },
      { path: 'dashboard', element: <Dashboard /> },
      { path: 'templates', element: <Templates /> },
      { path: 'accounts', element: <Accounts /> },
      { path: 'groups', element: <Groups /> },
      { path: 'logs', element: <Logs /> },
      { path: 'stats', element: <Stats /> },
    ],
  },
])

export default function App() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: 0, refetchOnWindowFocus: false } },
  })
  // 401 全局拦截：清 token 回登录（页面内抛 ApiUnauthorized 由 query onError 统一处理）
  return (
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
```

`web/src/pages/login.tsx` — token input form, on submit: setToken + navigate('/dashboard'):

```tsx
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { KeyRound } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { auth } from '@/lib/auth'

export default function Login() {
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const nav = useNavigate()
  const submit = () => {
    if (!token.trim()) { setErr('请输入 admin token'); return }
    auth.setToken(token.trim())
    nav('/dashboard')
  }
  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-950">
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><KeyRound className="h-5 w-5" /> 网关管理台</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">输入 admin token（config.toml 的 admin.token）</p>
            <Input type="password" placeholder="admin token" value={token} onChange={e => { setToken(e.target.value); setErr('') }} />
            {err && <p className="text-sm text-red-500">{err}</p>}
            <Button className="w-full" onClick={submit}>进入</Button>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
```

`web/src/components/layout.tsx` — sidebar nav (Dashboard/模板/账号/分组/日志/统计) + top bar (token 状态 + 退出):

```tsx
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, FolderOpen, FileText, BarChart3, LogOut } from 'lucide-react'
import { auth } from '@/lib/auth'
import { Button } from '@/components/ui/button'

const nav = [
  { to: '/dashboard', label: '总览', icon: LayoutDashboard },
  { to: '/templates', label: '模板', icon: Boxes },
  { to: '/accounts', label: '账号', icon: Users },
  { to: '/groups', label: '分组', icon: FolderOpen },
  { to: '/logs', label: '日志', icon: FileText },
  { to: '/stats', label: '统计', icon: BarChart3 },
]

export default function Layout() {
  const navTo = useNavigate()
  return (
    <div className="flex min-h-screen">
      <aside className="w-56 border-r bg-slate-950 text-slate-100 flex flex-col">
        <div className="p-4 font-semibold text-lg">网关管理台</div>
        <nav className="flex-1 space-y-1 p-2">
          {nav.map(({ to, label, icon: Icon }) => (
            <NavLink key={to} to={to}
              className={({ isActive }) => `flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-slate-800 text-white' : 'text-slate-400 hover:bg-slate-900 hover:text-slate-200'}`}>
              <Icon className="h-4 w-4" /> {label}
            </NavLink>
          ))}
        </nav>
        <div className="p-3 border-t">
          <Button variant="ghost" className="w-full justify-start text-slate-400" onClick={() => { auth.clear(); navTo('/login') }}>
            <LogOut className="h-4 w-4 mr-2" /> 退出
          </Button>
        </div>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
          <Outlet />
        </motion.div>
      </main>
    </div>
  )
}
```

NOTE: `location.pathname` needs `useLocation` — add the import and hook.

- [ ] **Step 5: shadcn/ui base setup**

Run `npx shadcn@latest init -d` and add the components used above (button, card, input, table, dialog, select, label, tabs, badge, alert, separator, skeleton, dropdown-menu). If offline, copy the shadcn component sources manually per shadcn/ui convention (components/ui/*.tsx + lib/utils.ts with cn()) and report the deviation. The `cn` util must exist at `src/lib/utils.ts`.

- [ ] **Step 6: Verify dev build + login flow**

```bash
cd web && npm run build && npx tsc --noEmit
```
Expected: build passes, type check passes.

Then manual smoke (if a gateway is running locally): `npm run dev`, open the page, login with a real admin token, confirm redirect to /dashboard and sidebar renders. If no gateway is available, verify the build + types only and report.

- [ ] **Step 7: Commit**

```bash
git add web/ openapi/openapi.yaml
git commit -m "feat: web UI scaffold (Vite/React/TS + shadcn/ui) + API client + login"
```

---

### Task 3: Pages — dashboard, templates, accounts, groups

**Files:**
- Create: `web/src/pages/dashboard.tsx`, `templates.tsx`, `accounts.tsx`, `groups.tsx`
- Modify: `web/src/components/ui/*` as needed

**Interfaces:**
- Consumes: `api` client from Task 2 (`api.listAccounts`, `api.listTemplates`, `api.listGroups`, CRUD methods), TanStack Query.
- Produces: four functional pages.

- [ ] **Step 1: Dashboard**

Data: `api.listAccounts()` (runtime view). Render:
- 状态计数卡片（active / unhealthy / 429 / disabled 数量，用 badge 颜色区分）；
- err_rate 排行（Top 5，err_rate > 0 降序，表格或列表）；
- 并发水位（concurrency 求和 + max_concurrency 求和 → 进度条）；
- 总账号数、总模板数（listTemplates 长度）、总分组数（listGroups 长度）。
Use `useQuery` with `refetchInterval: 10_000` for accounts; cards with `motion` entrance animation (staggered, framer-motion). Format err_rate as percent (`(v * 100).toFixed(1)%`).

- [ ] **Step 2: Templates page**

Full CRUD:
- Table: ID, Name, BaseURL, DefaultFormat, Models (count or comma list), ModelFormats/Mapping (badge list), CreatedAt, actions (编辑/删除);
- Create/Edit dialog: name, base_url, default_format (select), models (comma-separated input → split), model_formats (key:format 行编辑，动态行), model_mapping (key:value 行编辑);
- Delete with confirm dialog (alert-dialog or dialog + confirm);
- After mutation: `queryClient.invalidateQueries({ queryKey: ['templates'] })`;
- Empty state + loading skeleton.

- [ ] **Step 3: Accounts page**

Full CRUD + runtime view:
- Table: ID, Name, Template (name), Status (badge), Weight, MaxConcurrency, Concurrency (runtime), ErrRate, ErrCount, LastError (tooltip), actions;
- `useQuery(['accounts'], api.listAccounts, { refetchInterval: 10_000 })` — runtime view live;
- Create/Edit dialog: name, template_id (select from listTemplates), upstream_key (password input), status (select), weight, max_concurrency;
- 禁用/启用 quick action: PUT with status disabled/active (single-field update via full-replacement PUT — send the current object with status flipped);
- Delete with confirm.

- [ ] **Step 4: Groups page**

- Table: ID, Name, KeyPrefix, KeyHash (truncated), CreatedAt, actions (编辑/绑定账号/轮换 key/删除);
- Create dialog: name → on success show the plaintext key in a highlighted box with copy button (CreateGroupResponse.key — 仅此一次);
- Edit dialog: rename;
- Bind accounts dialog: multi-select checkboxes of all accounts (listAccounts) + save (setGroupAccounts with the selected ids);
- Rotate key: confirm dialog → show new plaintext key with copy;
- Delete with confirm.

- [ ] **Step 5: Verify**

```bash
cd web && npm run build && npx tsc --noEmit
```
Manual smoke against a running gateway: login → dashboard numbers render → template CRUD round-trip → account CRUD + runtime view updates → group create shows key, bind accounts, rotate. Report what was verified.

- [ ] **Step 6: Commit**

```bash
git add web/src/pages web/src/components
git commit -m "feat: web pages — dashboard, templates, accounts, groups"
```

---

### Task 4: Pages — logs + stats

**Files:**
- Create: `web/src/pages/logs.tsx`, `stats.tsx`

**Interfaces:**
- Consumes: `api.getLogs` / `api.getStats`.
- Produces: logs query page with filters + pagination; stats page with charts.

- [ ] **Step 1: Logs page**

- Filter bar: group_id, account_id, model, status_code, error_type (select with all enum values), from/to (datetime-local inputs → RFC3339);
- Table: CreatedAt, GroupID, AccountID, Model, MappedModel, Format, StatusCode, ErrorType (badge), LatencyMS, tokens (pt/ct/tt);
- Pagination: limit (10/20/50 select), offset controls (prev/next + total display from LogsResponse.total);
- `useQuery(['logs', params])` — filters change → new query;
- ErrorType badge colors: none → green, 4xx → yellow, 5xx/network/abort → red, 429 → orange, auth/no_account → gray.

- [ ] **Step 2: Stats page**

- Controls: from/to (datetime-local, default last 24h), granularity (hour/day tabs or select);
- Charts: request count and tokens over time. Use a lightweight SVG bar chart implemented in-page (no heavy chart lib dependency; ~60 lines: group buckets by BucketTime, render rects scaled to max, axis labels sparse) — or ECharts if the implementer prefers; the spec does not lock the chart lib. Record the choice in the report;
- Table below charts: bucket rows (time, requests, errors, pt/ct/tt, avg latency);
- `useQuery(['stats', params])`.

- [ ] **Step 3: Verify**

```bash
cd web && npm run build && npx tsc --noEmit
```
Manual smoke: logs filter round-trip + pagination; stats granularity switch renders different bucket counts. Report what was verified.

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/logs.tsx web/src/pages/stats.tsx
git commit -m "feat: web pages — logs query + stats charts"
```

---

### Task 5: Embed integration + build pipeline + e2e verification

**Files:**
- Create: `cmd/server/embed.go`
- Modify: `internal/server/server.go` (static + SPA fallback routes)
- Modify: `cmd/server/main.go` (pass embedded FS to server options if needed)
- Create: `Makefile` or `scripts/build.sh` (frontend build + go build pipeline)

**Interfaces:**
- Consumes: `web/dist` build output.
- Produces: single binary serving the UI at `/`.

- [ ] **Step 1: embed.go**

Create `cmd/server/embed.go`:

```go
package main

import (
	"embed"
	"io/fs"
)

// webDist 前端构建产物（npm run build → web/dist）。
// 未构建时为空 FS，服务端正常启动（开发期无 UI 也能跑）。
//go:embed all:web/dist
var webDist embed.FS

func webUI() fs.FS {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return fs.FS(emptyFS{})
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }
```

NOTE: `//go:embed all:web/dist` fails the build if the directory does not exist. Since the repo commits no `web/dist` (build artifact), the go:embed directive with a missing dir breaks `go build`. Handle this by either: (a) committing a minimal `web/dist/index.html` placeholder (a static "UI not built" page), or (b) `//go:embed` only when dist exists — Go requires the pattern to match at least one file. The recommended approach: commit `web/dist/.gitkeep`-style placeholder — but `all:` matches files, `.gitkeep` counts. Verify `//go:embed all:web/dist` with only a `.gitkeep` present compiles; if it does not (empty dir with only dotfile), commit a minimal `web/dist/index.html` placeholder instead. The placeholder must NOT break `go test`/`go build` when the real frontend is absent.

- [ ] **Step 2: Static + SPA fallback in server**

Modify `internal/server/server.go` — add options `WebFS fs.FS` and mount:

```go
// 静态资源 + SPA fallback（在 admin/AI/healthz 之后注册）。
if opts.WebFS != nil {
    r.Handle("/assets/*", http.FileServerFS(opts.WebFS))
    r.Get("/", func(w http.ResponseWriter, r *http.Request) {
        index, err := fs.ReadFile(opts.WebFS, "index.html")
        if err != nil {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        _, _ = w.Write(index)
    })
    // SPA fallback：非 API/静态路径回 index.html
    r.NotFound(func(w http.ResponseWriter, r *http.Request) {
        if strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/v1") || r.URL.Path == "/healthz" {
            http.NotFound(w, r)
            return
        }
        index, err := fs.ReadFile(opts.WebFS, "index.html")
        if err != nil {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        _, _ = w.Write(index)
    })
}
```

In `cmd/server/main.go`, pass `WebFS: webUI()` to `server.NewServer`. Add `"io/fs"` and `"strings"` imports as needed.

- [ ] **Step 3: Build pipeline**

Create `scripts/build.sh`:

```bash
#!/bin/bash
# 前端构建 + Go 单二进制构建
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -d web ] && [ -f web/package.json ]; then
  (cd web && npm install && npm run build)
fi
go build -o bin/server ./cmd/server
echo "built: bin/server (UI embedded)"
```

Also document in README or the plan's final report: dev workflow (vite dev + gateway), prod workflow (scripts/build.sh).

- [ ] **Step 4: End-to-end verification**

1. `cd web && npm run build` (if web/dist missing, produce it);
2. `go test ./... && go vet ./... && golangci-lint run ./...` — green;
3. `go build -o /tmp/gpm-e2e/server ./cmd/server` + run with config + Docker PG (same setup as load tests) — OR a lighter smoke: run the server binary and curl:
   - `curl -s http://127.0.0.1:18080/ | head -c 200` → HTML (SPA index or placeholder);
   - `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/assets/<a built asset>` → 200 (if real dist built);
   - `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/some/spa/route` → 200 (SPA fallback returns index);
   - `curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' http://127.0.0.1:18080/admin/templates` → 401 (admin auth intact);
   - `curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:18080/admin/templates` → 200 JSON.
4. If a full gateway is available, manual UI smoke: login → CRUD → logs → stats (report what was verified).

- [ ] **Step 5: Commit**

```bash
git add cmd/server/embed.go internal/server/server.go cmd/server/main.go scripts/build.sh web/dist 2>/dev/null || git add cmd/server/embed.go internal/server/server.go cmd/server/main.go scripts/build.sh
git commit -m "feat: embed web UI into single binary + SPA fallback"
```
