#!/bin/bash
# 生产构建：前端（pnpm）→ 拷贝产物到 embed 目录 → Go 单二进制。
# 产物：bin/server（UI 已嵌入，单文件部署）。
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -d web ] && [ -f web/package.json ]; then
  # --frozen-lockfile：构建机必须与 pnpm-lock.yaml 完全一致，防 lockfile 漂移
  (cd web && pnpm install --frozen-lockfile && pnpm run build)
  # go:embed 需要 cmd/server/dist 与 web/dist 同步（.gitkeep 为仓库占位，保留）
  rm -rf cmd/server/dist
  mkdir -p cmd/server/dist
  cp -r web/dist/. cmd/server/dist/
  touch cmd/server/dist/.gitkeep
fi

go build -o bin/server ./cmd/server
echo "built: bin/server (UI embedded)"
