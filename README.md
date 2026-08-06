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
