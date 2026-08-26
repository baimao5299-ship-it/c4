<div align="center">

# ⚡ c3api

**轻量 AI 网关**——一个入口统一接入 OpenAI Responses API、Anthropic Messages API 与 OpenAI Chat Completions API，内置管理台、用量统计与计费。

[English](./README.md) | [中文](./README_zh.md)

[![Release](https://img.shields.io/github/v/release/baimao5299-ship-it/c3api?color=2563eb)](https://github.com/baimao5299-ship-it/c3api/releases)
[![Stars](https://img.shields.io/github/stars/baimao5299-ship-it/c3api?color=2563eb)](https://github.com/baimao5299-ship-it/c3api)
[![License](https://img.shields.io/badge/license-AGPL--3.0--or--later%20%2B%20Commercial-2563eb)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-2563eb)](https://go.dev/)

[![OpenAI Responses API](https://img.shields.io/badge/OpenAI%20Responses%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/responses)
[![Anthropic Messages API](https://img.shields.io/badge/Anthropic%20Messages%20API-✓-d97757)](https://docs.anthropic.com/en/api/messages)
[![OpenAI Chat Completions API](https://img.shields.io/badge/OpenAI%20Chat%20Completions%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/chat)

</div>

本项目基于原版 c3api，主要增加了这些实用功能：

- 兼容 Sub2API 的账号导入，支持多文件和 ZIP。
- 重复账号预检测、导入预览和详细失败原因。
- 可选的 Sub2 TLS 指纹与收敛设置。

网关核心架构仍沿用原版项目。

当前维护者：[baimao5299-ship-it](https://github.com/baimao5299-ship-it)。

## 状态：Beta

c3api 处于 **beta**。当前 API 已可使用，但 beta 版本之间仍可能调整表结构和配置格式。

- **暂不提供自动迁移**——beta 版本之间不保证数据库表结构和配置兼容。
- **升级按全新部署处理**——先创建新数据库并重新核对配置，再切换流量。
- 发布说明见 [CHANGELOG.md](./CHANGELOG.md)。

## 特性

| | |
|---|---|
| **六格式一网关** | OpenAI Responses API（REST + WebSocket）、Anthropic Messages API、OpenAI Chat Completions API、OpenAI Images API、Codex 网页搜索与 OpenAI 兼容模型列表——各自独立的上游编排与协议转换 |
| **模板与账号管理** | 模型模板、上游账号、分组、凭证，以及按模板的格式/模型白名单 |
| **管理台** | React 前端内嵌进二进制（`/app`），另有 OpenAPI 契约定义的管理 API（`/api/admin`） |
| **计费与用量** | 用户余额预检扣费、FEFO 临时额度、litellm 价格同步的按模型计费、按日分区的用量日志与统计——计费默认开启（`config.example.toml` billing.enabled=true） |
| **规则引擎** | 可自定义的路由、限流与 429/错误退避规则，内置调度器 |
| **多实例就绪** | 状态全在 PostgreSQL，`NOTIFY` 跨实例失效广播，Redis 心跳实例发现（集群规模自动感知）——水平扩容零配置 |
| **单二进制** | Go 二进制内嵌前端，非 root 容器镜像，即插即用部署 |

### 请求格式

| 格式 | 端点 | 上游 |
|---|---|---|
| OpenAI Responses API | `POST /v1/responses` | OpenAI Responses（REST） |
| OpenAI Responses API — WebSocket | `WS /v1/responses`（带 upgrade 头的 GET） | OpenAI Responses WebSocket（如 Codex 客户端） |
| Anthropic Messages API | `POST /v1/messages` | Anthropic Messages（REST + SSE） |
| OpenAI Chat Completions API | `POST /v1/chat/completions` | OpenAI Chat Completions（REST + SSE） |
| OpenAI Images API | `POST /v1/images/generations` / `POST /v1/images/edits` | OpenAI Images（JSON + multipart，REST + SSE） |
| Codex 网页搜索 | `POST /v1/alpha/search` | Codex SDK Search（仅 Codex 客户端） |
| OpenAI 模型列表 | `GET /v1/models` | 调度器内存快照（零 DB） |

## 快速开始

### Docker Compose（推荐）

```bash
cp .env.example .env
# 在 .env 中填写 AUTH_JWT_SECRET（ADMIN_TOKEN 可选）。
docker compose up -d --build
```

这会在本地构建镜像，并一起启动 PostgreSQL、Redis 和 c3api。若要使用预构建镜像，先从 `compose.yml` 删除 `build:` 段，再运行 `docker compose pull` 和 `docker compose up -d`。

网关监听 `http://127.0.0.1:18080`——管理台 `/app`，用户台 `/user`，健康检查 `/healthz`。

**首个 admin 用户（bootstrap）**——新库上第一个注册的用户自动成为 `platform_admin`，启动后即可登录管理台（`/app`）；之后的注册都是普通 `user` 角色。

预构建镜像发布在 GHCR（`ghcr.io/baimao5299-ship-it/c3api`）：`:beta` 跟随最新 beta 版本，也提供固定版本 tag。单独拉取：`docker pull ghcr.io/baimao5299-ship-it/c3api:beta`。

### 本地开发

```bash
# 0. 注入本地开发密钥（config.toml 留空值；占位符如 change-me 会被 Load 拒绝）
export C3API_ADMIN_TOKEN=local-admin-token
export C3API_AUTH_JWT_SECRET=$(openssl rand -hex 16)

# 1. 启动网关（默认 :18080）
go run ./cmd/server -config config.toml

# 2. 启动前端 dev server（:5173，代理 /api 到 18080）
cd web && pnpm install && pnpm run dev
```

任意兼容 OpenAI/Anthropic 的 SDK 指向网关地址即可——请求格式由路径决定，一个 base URL 同时服务六种请求格式。

## 架构

```
                    ┌───────────────────────────────┐
 OpenAI SDK / curl ─▶│   c3api 网关（单二进制）        │
 Anthropic SDK ─────▶│  ┌─────────────────────────┐  │
 Codex 客户端 ──────▶│  │ chi 路由                │  │──▶ OpenAI 上游（REST + SSE）
   浏览器（SPA） ───▶│  │ /healthz /api/admin /api/user + SPA / /user /app │  │──▶ Anthropic 上游（REST + SSE）
                    │  │ /v1/*                    │  │──▶ Responses / resp-ws 上游
                    │  │ proxy：鉴权 → 门禁 →      │  │
                    │  │         选号 → 转发       │  │
                    │  └──────────┬──────────────┘  │
                    │   workers：billing / usage /  │
                    │   errlog / scheduler / notify │
                    │   retention / stats-agg /     │
                    │   pricing-sync / rule-engine  │
                    │   auth-sync / invalidate /    │
                    │   discovery                   │
                    └───────┼───────────────┼──────┘
                            ▼               ▼
                  PostgreSQL 18（状态 + NOTIFY）│
                                             ▼
                      Redis 8（易失状态：协调 + 短时效验证码）
```

- **单二进制**：前端经 `go:embed` 内嵌，运行时 = 一个 `server` 进程 + 挂载的配置文件。
- **网关无状态、状态在 DB**：共享状态全部在 PostgreSQL，实例间经 `c3api_invalidate` 通道 `NOTIFY` 协调；多实例预算分摊基数 N 经 Redis 心跳自动发现——加实例即扩容（无手工设置）。
- **常驻 worker**：计费扣减、用量/统计落库、错误审计、分区保留、离线聚合、价格同步与规则调度均为长驻 worker，支持优雅停机排空。

## 性能

网关 30 秒 CPU profile（`go tool pprof -http=:8081 <profile.pprof>`）：

![CPU 火焰图](docs/images/pprof-flamegraph.png)

热点集中在 IO 与 GC，而非请求路径逻辑：

- **~23%** `syscall`——网络读写（上游连接、pgx）
- **~15%** GC——分配密集路径（span 分级 + 对象/span 扫描）
- **~4%** `selectgo`——并发等待；**~1%** `memmove`——数据拷贝

请求路径本身（JSON 解析、路由、配额/计费）只占 CPU 个位数百分比，吞吐留有富余。GC 调优钩子（`GOGC` / `GOMEMLIMIT`）已接入 compose 栈——见 `.env.example`。

## 配置

网关加载 `config.toml`（模板见 `config.example.toml`），`C3API_` 前缀环境变量可覆盖（前缀**必须大写**）：

| 变量 | 说明 |
|---|---|
| `C3API_ADMIN_TOKEN` | 管理端 token（可选；留空 = 不启用静态 token 鉴权，`/api/admin` 仅接受 `platform_admin` JWT） |
| `C3API_AUTH_JWT_SECRET` | 用户鉴权 JWT 密钥（必填；跨重启与多实例须稳定） |
| `C3API_DB_DSN` | PostgreSQL 连接串 |
| `C3API_REDIS_ADDR` | Redis 地址（必填；如 `127.0.0.1:6379`——实例发现、短时效验证码等易失状态） |

完整配置项（server / log / admin / auth / db / redis / proxy / upstream / limit / scheduler / usage / billing）见 `config.example.toml`。

- **beta 升级按全新部署处理**——表结构与配置可能随版本调整，升级前请新建实例并重新核对配置（见上方"状态：Beta"说明）。
- **纯 env 部署**（如 K8s）：传 `-config ""` 完全跳过配置文件——flag 默认 config.toml，无文件即启动失败。
- **配置仅启动时读取**，变更需滚动重启（无热更新）。
- **非法配置启动即报错**（含字段名）：duration/间隔 ≤0 或 <1ms、未知键（拼写错误/已移除旧键）、必填密钥缺失、占位符（change-me、dev-admin-token 等）一律拒绝。

## 部署

- `compose.yml` — 生产编排：`db`（postgres:18-alpine，数据挂载在 `deploy/data/pg`）+ `redis`（redis:8-alpine，易失状态（协调 + 短时效验证码）——不持久化）+ `app`（单容器，非 root、配置只读挂载自 `deploy/config.toml`、健康检查）。
- `Dockerfile` — 三阶段构建（node → go → alpine），产出内嵌 UI 的静态单二进制。
- **双必需依赖**：PostgreSQL 18（全部持久状态，唯一真相源）+ Redis 8（可丢的易失状态——实例发现心跳与短时效邮箱验证码；永不作为缓存层）。二者均为启动强制项；勿配 `allkeys-lru` 等淘汰策略——验证码被淘汰仅致用户重发，无害但应避免。

## 许可

c3api 以 **GNU AGPL v3.0-or-later**（`LICENSE`）开源——可自由使用、修改与部署，**包括商用与托管服务**，前提是遵守所运行版本的许可证条款。**符合 AGPL 的部署无需购买授权。**

需要**闭源部署**（豁免部署上的 AGPL 义务）？可获取商业授权（`LICENSE.commercial`）——付费获得对部署的 AGPL 义务豁免。

外部代码贡献需签署 CLA（贡献者版权转让协议），以便贡献代码并入本双许可体系——详见 [CLA.md](./CLA.md) 与 [CONTRIBUTING.md](./CONTRIBUTING.md)。

## 联系

- GitHub：[baimao5299-ship-it/c3api](https://github.com/baimao5299-ship-it/c3api)
- 问题：欢迎通过 GitHub issue 提交 bug、咨询或功能建议
- 安全：漏洞请经 [SECURITY.md](./SECURITY.md) 私有报告
- 发布说明：[CHANGELOG.md](./CHANGELOG.md)
