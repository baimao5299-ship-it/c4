# Web UI + OpenAPI 契约设计

## 状态

已批准（用户确认：完整 oapi-codegen 双端、响应保持大写 Go 风格、TS 端同样大写、界面范围 = 完整管理面 A、Framer Motion 保留）。

## 目标

为网关补管理界面与契约层：

1. **OpenAPI spec 成为唯一权威**：admin API 全部操作由 `openapi/openapi.yaml` 定义，oapi-codegen 生成 Go server stub（types + chi-server），openapi-typescript 生成前端 TS 类型——两端从同一 spec 生成，漂移在结构上不可能；
2. **前端**：React + Vite + TS + Tailwind + shadcn/ui + Framer Motion + Lucide + TanStack Query + react-router，完整管理面（登录/Dashboard/模板/账号/分组/日志/统计）；
3. **部署**：开发期 Vite proxy 分离开发；生产构建产物 `embed.FS` 编入 Go 二进制，单文件部署；
4. **鉴权**：UI 壳公开，API Bearer 鉴权（现有 admin 中间件不变）；登录页收集 admin token 存 localStorage，401 清 token 回登录。

## 契约层

### OpenAPI spec（openapi/openapi.yaml）

- 覆盖 admin API 全部 15 个操作（模板 5 + 账号 5 + 分组 7 + 日志 1 + 统计 1）；
- **AI 端点（/v1/*）不进 spec**：前端无消费，透传协议无管理意义；
- 响应 schema 字段名**保持现状大写**（`ID` / `Name` / `BaseURL`…）；请求体 snake_case（与现有 wire 格式完全一致，不破坏已部署客户端）；
- operationId 命名（决定 Go 方法名）：
  - `PostTemplates` / `GetTemplates` / `GetTemplatesId` / `PutTemplatesId` / `DeleteTemplatesId`
  - `PostAccounts` / `GetAccounts` / `GetAccountsId` / `PutAccountsId` / `DeleteAccountsId`
  - `PostGroups` / `GetGroups` / `GetGroupsId` / `PutGroupsId` / `DeleteGroupsId` / `PutGroupsIdAccounts` / `PostGroupsIdRotateKey`
  - `GetLogs` / `GetStats`
- 响应 schema：`Template` / `Account`（含嵌套 Template）/ `AccountView`（Account + concurrency/err_rate/err_count）/ `Group` / `CreateGroupResponse`（group + key 明文一次）/ `UsageLog` / `LogsResponse`（total + rows）/ `StatBucket` / `ErrorResponse`（`{"error": "..."}`）/ `DeletedResponse`（`{"deleted": true}`）/ `UpdatedResponse`（`{"updated": true}`）；
- 查询参数 schema：`GetLogsParams`（limit/offset/group_id/account_id/model/status_code/error_type/from/to）、`GetStatsParams`（from/to/granularity/group_id/account_id/model）；
- 枚举：format（openai-chat/openai-responses/anthropic）、account status（active/unhealthy/429/disabled）、error_type（none/429/4xx/5xx/network/auth/no_account/abort）；
- 401/400/404/500 错误响应引用 ErrorResponse。

### 生成管线

```bash
# Go（go:generate 指令放 internal/handler/generate.go）
oapi-codegen -generate types,chi-server -package handler \
  -o internal/handler/api.gen.go openapi/openapi.yaml

# TS（web/package.json script）
openapi-typescript openapi/openapi.yaml -o web/src/lib/api/schema.d.ts
```

- oapi-codegen v2（github.com/oapi-codegen/oapi-codegen/v2）；
- 生成文件提交入仓（构建不依赖工具链）。

## handler 适配

- `Handler` 实现生成的 `ServerInterface`：现有 15 个方法改名（`createTemplate` → `PostTemplates` 等），方法体保留（decode → svc 调用 → 编码）；
- 请求体解码改用生成类型（`CreateTemplateJSONBody` 等，字段与现有 templateBody/accountBody 一致）；
- 响应编码改用生成类型 + 转换函数（domain.Template/Account/Group/UsageLog/StatBucket/AccountView → 生成类型，字段复制）；
- 路由：`HandlerWithOptions`（chi 模式）替代手写 `Routes`；`RoutesMux` 删除或内部委托；
- 错误响应：`writeErr`/`writeServiceErr` 输出 ErrorResponse 结构；
- 现有 handler_test 走 HTTP（r.ServeHTTP）→ 只改路由挂载方式，断言不变；service/fakestore 测试不动。

## 前端 /web

### 脚手架

```text
Vite + React 18 + TypeScript + Tailwind CSS + shadcn/ui（Radix 系）+ Framer Motion + Lucide
+ TanStack Query（列表/表格缓存与失效）+ react-router（页面路由）
```

### 目录

```text
web/
  package.json  vite.config.ts（proxy /admin → http://127.0.0.1:18080）  tailwind.config.ts
  src/
    main.tsx  App.tsx（RouterProvider + QueryClientProvider）
    lib/api/client.ts      # openapi-typescript 类型 + fetch 封装（token 注入、401 处理）
    lib/auth.ts            # token localStorage 读写、登录态
    lib/format.ts          # 时间/时长/数字格式化
    components/ui/         # shadcn/ui 组件（button/table/dialog/form/tabs/...）
    components/layout.tsx  # 侧边导航 + 顶栏（Framer Motion 过渡）
    pages/
      login.tsx            # token 输入 → localStorage → 跳 Dashboard
      dashboard.tsx        # 账号健康总览（状态计数、err_rate 排行、并发水位）
      templates.tsx        # 模板 CRUD（models/model_formats/model_mapping 编辑）
      accounts.tsx         # 账号 CRUD + 运行时视图 + 禁用/启用
      groups.tsx           # 分组 CRUD + 账号绑定 + key 轮换（明文展示）
      logs.tsx             # 日志分页 + 6 维过滤
      stats.tsx            # hour/day 聚合 + 图表
```

### 关键行为

- 登录：输入 admin token → localStorage（key `gpm_admin_token`）；所有 fetch 注入 `Authorization: Bearer`；401 → 清 token → 回登录页；
- API client：薄封装（fetch + 类型化返回 + 错误归一化），不引重型 HTTP 库；
- 图表：统计页用轻量方案（ECharts 或自绘 SVG 柱状/折线——实现时按包体与复杂度取舍，spec 不锁死）；
- 运行时视图：账号列表轮询（TanStack Query refetchInterval ~10s）展示 concurrency/err_rate。

## embed 集成

- `cmd/server/embed.go`：`//go:embed all:web/dist` → `embed.FS`；
- chi 挂载：`/assets/*` 静态文件 + SPA fallback（非 /admin /v1 /healthz 路径回 index.html）；
- 生产构建：`npm run build`（产出 web/dist）→ `go build`（embed 生效）；
- `web/dist` 为空时 embed 空 FS，服务端不 panic（开发期无 dist 也能跑）。

## 测试与验证

- handler 层：现有 handler_test 全绿（路由挂载适配后断言不变）+ 新增 spec 合规抽查（如响应字段大写断言）；
- 前端：`npm run build` 通过 + `tsc --noEmit` 类型检查 + 手工冒烟（登录 → CRUD → 日志 → 统计）；
- 端到端：构建后单二进制启动，curl 首页/静态资源/SPA fallback + admin API 鉴权回归；
- 契约一致性：spec 与 handler 由生成保证；handler 与前端由生成类型保证；
- go test ./... / vet / golangci-lint 全绿；前端独立 lint（eslint + prettier，web/ 内）。

## 任务拆分（SDD）

| Task | 内容 | 关键产出 |
|---|---|---|
| 1 | OpenAPI spec + oapi-codegen 管线 + handler 适配 + 测试适配 | openapi/openapi.yaml、internal/handler/api.gen.go、ServerInterface 实现、测试全绿 |
| 2 | 前端脚手架 + client + 登录 + 布局 | web/ 可运行（vite dev + proxy + 登录跳转） |
| 3 | 页面：Dashboard + 模板 + 账号 + 分组 | 四页 CRUD 完成 |
| 4 | 页面：日志 + 统计 | 分页过滤 + 图表 |
| 5 | embed 集成 + 构建 + 端到端验证 | 单二进制带 UI、冒烟通过 |
