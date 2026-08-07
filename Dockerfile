# go-proxy-mini 多阶段构建：前端 dist 内嵌进 Go 二进制（cmd/server/embed.go），
# 运行时为单二进制 + 挂载 config。
# 用法：docker compose -f deploy/compose.yml up -d --build

# ---- Stage 1: 前端构建（vite build → dist） ----
FROM node:22-alpine AS web
RUN corepack enable
WORKDIR /web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
# frozen-lockfile 保证可复现；hoisted linker 与本地开发一致（Linux 容器内无 junction 问题，保持统一）
RUN pnpm install --config.node-linker=hoisted --frozen-lockfile
COPY web/ .
RUN pnpm build

# ---- Stage 2: Go 构建（前端 dist 复制进 embed 目录） ----
FROM golang:1.26-alpine AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 多阶段产物经 COPY --from=web 传递（阶段间是独立容器，不能直接 cp 前阶段路径）
RUN rm -rf cmd/server/dist && mkdir -p cmd/server/dist
COPY --from=web /web/dist ./cmd/server/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 3: 运行时（精简 alpine，非 root） ----
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=gobuild /out/server /app/server
WORKDIR /app
EXPOSE 18080
ENTRYPOINT ["/app/server"]
CMD ["-config", "/app/config.toml"]
