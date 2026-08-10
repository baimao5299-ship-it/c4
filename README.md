# go-proxy-mini

轻量 AI 网关管理台：OpenAPI 契约 + 管理 API + React 前端（Web UI 嵌入单二进制）。

## 工作流

### 开发（前后端分离，HMR）

```bash
# 1. 启动网关（后端，默认 :18080）
go run ./cmd/server -config config.toml

# 2. 启动前端 dev server（:5173，/admin 代理到 18080）
cd web && pnpm install && pnpm run dev
```

浏览器访问 http://127.0.0.1:5173 ，admin token 即 config.toml 的 `admin.token`。
此模式不依赖 Web UI 嵌入：后端不带 UI 也能独立跑（API 始终可用）。

### 生产（单二进制，UI 已嵌入）

```bash
scripts/build.sh   # pnpm 构建前端 → 产物拷入 cmd/server/dist → go build 单二进制
```

产物 `bin/server` 直接部署（`-config config.toml`），`/` 提供 UI，`/admin`、`/v1`、`/healthz`
不受 SPA fallback 影响。

### 前端产物与 go:embed

- 构建产物路径：`web/dist`（gitignore，不入库）。
- `cmd/server/embed.go` 以 `//go:embed all:dist` 嵌入 `cmd/server/dist`，该目录仅
  `.gitkeep` 占位入库（未构建前端时 go build 不报错，`/` 返回 404 但不影响 API）。
- `scripts/build.sh` 负责把 `web/dist` 同步到 `cmd/server/dist`。

## 测试

### 单元测试（无外部依赖）

```bash
go build ./... && go vet ./...
go test ./...            # 跳过真实 PG 用例
golangci-lint run ./...
```

### 真实 PostgreSQL 集成测试（internal/repository 等）

PG 测试共享 `localhost:15432/gpm_test`（经 `TEST_DATABASE_URL` 注入），每个测试
开头执行 `DROP SCHEMA public CASCADE` 重建 schema。**该库是所有 worktree / 会话
共享的**——跨 worktree（或并行终端）同时跑 repository 测试会随机互踩：A 会话的
测试清掉 B 会话的表，B 报"表不存在 / id 缺失 / 数值不符"等与本次改动无关的误报
（失败测试隔离重跑全部通过即证）。约定：

- **串行跑 repository 测试**：同一时刻只允许一个会话执行 PG 测试；
- 或为并行 worktree 配置独立测试库（`TEST_DATABASE_URL` 指向独立库，如
  `postgres://.../gpm_test_wt_i21`）。

```bash
TEST_DATABASE_URL=postgres://postgres:gpm@localhost:15432/gpm_test go test ./internal/repository/ -count=1
```

判定互踩误报：失败测试不触碰本次改动代码 + 隔离重跑通过 → 先确认当前无其他
会话在跑 PG 测试，再重跑验证。
