# c3api 前端（web/）

c3api 网关管理台前端，构建产物经 `go:embed` 打进 Go 二进制。

## 技术栈

React + TypeScript + Vite + shadcn/ui + Tailwind CSS。

## 契约生成

```bash
pnpm gen:api
```

`openapi-typescript` 从 `openapi/openapi.yaml` 生成类型到 `src/lib/api/schema.d.ts`，**修改 OpenAPI 契约后必须重跑**。

## 开发

```bash
pnpm install   # 本机需 hoisted linker：pnpm install --config.node-linker=hoisted
pnpm run dev   # Vite dev server：:5173，/admin 代理到网关 :18080
```

## 构建

```bash
pnpm run build   # tsc -b && vite build，产物 dist/ 经 Go go:embed 打进二进制
```
