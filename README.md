<div align="center">

# ⚡ C4

**C4 is a lightweight AI gateway** — one entry point for the OpenAI Responses API, the Anthropic Messages API, and the OpenAI Chat Completions API, with a built-in admin console, usage tracking, and billing.

[English](./README.md) | [中文](./README_zh.md)

[![Release](https://img.shields.io/github/v/release/baimao5299-ship-it/c4?color=2563eb)](https://github.com/baimao5299-ship-it/c4/releases)
[![Stars](https://img.shields.io/github/stars/baimao5299-ship-it/c4?color=2563eb)](https://github.com/baimao5299-ship-it/c4)
[![License](https://img.shields.io/badge/license-AGPL--3.0--or--later%20%2B%20Commercial-2563eb)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-2563eb)](https://go.dev/)

[![OpenAI Responses API](https://img.shields.io/badge/OpenAI%20Responses%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/responses)
[![Anthropic Messages API](https://img.shields.io/badge/Anthropic%20Messages%20API-✓-d97757)](https://docs.anthropic.com/en/api/messages)
[![OpenAI Chat Completions API](https://img.shields.io/badge/OpenAI%20Chat%20Completions%20API-✓-10a37f)](https://platform.openai.com/docs/api-reference/chat)

</div>

This C4 release builds on the original gateway and adds a few practical improvements:

- Sub2API-compatible account import, including multi-file and ZIP import.
- Duplicate pre-checks, import previews, and detailed failure reasons.
- Optional Sub2 TLS profile and convergence settings.
- Per-request failover exclusion so a failed account is not retried before async rules settle.
- Extensible provider-balance adapters with explicit unconfigured and stale states.

The core gateway architecture remains based on the original project.

Maintained by [@baimao5299-ship-it](https://github.com/baimao5299-ship-it).

## Status: Beta

C4 is in **beta**. The current API is usable, but the schema and configuration may still change between beta releases.

### Compatibility

The public product name is **C4**. The Go module path (`github.com/is7qin/c3api`), executable/container names, API paths, and `C3API_*` environment variables remain unchanged for compatibility with existing deployments.

- **No automatic migration yet** — database schemas and configuration are not guaranteed to stay compatible between beta releases.
- **Plan beta upgrades as a fresh deployment** — use a new `RemoteDir` and database, then re-check the configuration before switching traffic. The deployment helper refuses to reuse an existing database unless `-ReuseDatabase` is explicitly supplied after a schema-compatibility review.
- See [CHANGELOG.md](./CHANGELOG.md) for release notes.

## Features

| | |
|---|---|
| **Six formats, one gateway** | OpenAI Responses API (REST + WebSocket), Anthropic Messages API, OpenAI Chat Completions API, OpenAI Images API, Codex web search, and the OpenAI-compatible model list — each with its own upstream orchestration and protocol conversion |
| **Template & account management** | Model templates, upstream accounts, groups, credentials, and per-template format/model allowlists |
| **Admin console** | React web UI embedded in the binary (`/app`), plus a full OpenAPI-defined admin API (`/api/admin`) |
| **Billing & usage** | Per-user balance with pre-check deductions, FEFO temporary quotas, per-model pricing synced from litellm, daily-partitioned usage logs and statistics — billing is enabled by default (`config.example.toml` billing.enabled=true) |
| **Rules engine** | Customizable routing, rate limiting, and 429/error backoff rules with a built-in scheduler |
| **Multi-instance ready** | PostgreSQL-based state, `NOTIFY`-based cross-instance invalidation, Redis-heartbeat instance discovery (cluster size auto-detected) — zero-config horizontal scaling |
| **Single binary** | Go binary with embedded frontend, non-root Docker image, drop-in deployment |

### Request formats

| Format | Endpoint | Upstream |
|---|---|---|
| OpenAI Responses API | `POST /v1/responses` | OpenAI Responses (REST) |
| OpenAI Responses API — WebSocket | `WS /v1/responses` (GET with upgrade header) | OpenAI Responses WebSocket (e.g. Codex client) |
| Anthropic Messages API | `POST /v1/messages` | Anthropic Messages (REST + SSE) |
| OpenAI Chat Completions API | `POST /v1/chat/completions` | OpenAI Chat Completions (REST + SSE) |
| OpenAI Images API | `POST /v1/images/generations` / `POST /v1/images/edits` | OpenAI Images (JSON + multipart, REST + SSE) |
| Codex web search | `POST /v1/alpha/search` | Codex SDK Search (Codex client only) |
| OpenAI model list | `GET /v1/models` | In-memory scheduler snapshot (zero DB) |

## Quick Start

### Docker Compose (recommended)

```bash
cp .env.example .env
# Set AUTH_JWT_SECRET, POSTGRES_PASSWORD, and ADMIN_TOKEN in .env.
docker compose up -d --build
```

This builds the image locally and starts PostgreSQL, Redis, and the C4 gateway together. To use a published image instead, remove the `build:` block from `compose.yml`, then run `docker compose pull` followed by `docker compose up -d`.

The gateway listens on `http://127.0.0.1:18080` — admin console at `/app`, user console at `/user`, liveness at `/healthz`, and readiness (snapshots plus a configured proxy route) at `/readyz`.

**Initial administration** — Compose requires a static `ADMIN_TOKEN` and binds the gateway to localhost by default. Open `/user/login`, choose **Admin token**, and paste the generated value; the console validates it with a read-only admin request before opening `/app`. Public registrations are regular `user` accounts; place the gateway behind an HTTPS reverse proxy when exposing it to other machines.

Prebuilt images are published to GHCR (`ghcr.io/baimao5299-ship-it/c4`): `:beta` tracks the latest beta release, and version-pinned tags are also available. Pull standalone with `docker pull ghcr.io/baimao5299-ship-it/c4:beta`. The legacy `c3api` image tags are published in parallel for existing deployments.

### Local development

```bash
# 0. Inject local dev secrets once (config.toml keeps empty values; placeholder
#    values like change-me are rejected by config.Load)
export C3API_ADMIN_TOKEN=local-admin-token
export C3API_AUTH_JWT_SECRET=$(openssl rand -hex 16)

# 1. Start the gateway (default :18080)
go run ./cmd/server -config config.toml

# 2. Start the frontend dev server (:5173, proxies /api to 18080)
cd web && pnpm install && pnpm run dev
```

Point any OpenAI/Anthropic-compatible SDK at the gateway URL — the request format is selected by path, so a single base URL serves all six request formats.

## Architecture

```
                    ┌───────────────────────────────┐
 OpenAI SDK / curl ─▶│   C4 gateway (1 binary)       │
 Anthropic SDK ─────▶│  ┌─────────────────────────┐  │
 Codex client ──────▶│  │ chi router              │  │──▶ OpenAI upstream (REST + SSE)
   Browser (SPA) ───▶│  │ /healthz /readyz /api/admin /api/user + SPA / /user /app │  │──▶ Anthropic upstream (REST + SSE)
                    │  │ /v1/*                    │  │──▶ Responses / resp-ws upstream
                    │  │ proxy: auth → gate →     │  │
                    │  │         route → forward  │  │
                    │  └──────────┬──────────────┘  │
                    │   workers: billing / usage /  │
                    │   errlog / scheduler / notify │
                    │   retention / stats-agg /     │
                    │   pricing-sync / rule-engine  │
                    │   auth-sync / invalidate /    │
                    │   discovery                   │
                    └───────┼───────────────┼──────┘
                            ▼               ▼
              PostgreSQL 18 (state + NOTIFY) │
                                             ▼
                        Redis 8 (ephemeral: coordination +
                        short-lived verification codes)
```

- **Single binary**: the frontend is built and embedded via `go:embed`, so the runtime is one `server` process plus a mounted config file.
- **Stateless gateway, stateful DB**: all shared state lives in PostgreSQL; instances coordinate through `NOTIFY` on the `c3api_invalidate` channel. The cluster instance count for multi-instance budget sharing is auto-discovered via Redis heartbeats — scale horizontally by just adding instances (no manual setting).
- **Persistent workers**: billing deduction, usage/statistics flushing, error-log auditing, partition retention, offline stats aggregation, price sync, and the rule scheduler run as long-lived workers with graceful shutdown draining.

## Performance

A 30 s CPU profile of the gateway (`go tool pprof -http=:8081 <profile.pprof>`):

![CPU flame graph](docs/images/pprof-flamegraph.png)

The hot spots are I/O and garbage collection, not request-path logic:

- **~23%** `syscall` — network reads/writes (upstream connections, pgx)
- **~15%** GC — allocation-heavy paths (span class sizing + object/span scanning)
- **~4%** `selectgo` — concurrency wait; **~1%** `memmove` — data copies

The request path itself (JSON parsing, routing, quota/billing) stays a single-digit share of total CPU, leaving headroom for higher throughput. GC tuning hooks (`GOGC` / `GOMEMLIMIT`) are wired through the compose stack — see `.env.example`.

## Configuration

The gateway loads `config.toml` (see `config.example.toml`), overlaid by `C3API_`-prefixed environment variables (the prefix must be **uppercase**):

| Variable | Description |
|---|---|
| `C3API_ADMIN_TOKEN` | Admin API token (required by the Compose template; direct deployments may leave it empty only when a trusted `platform_admin` bootstrap path is explicitly configured) |
| `C3API_AUTH_JWT_SECRET` | JWT signing secret for user auth (required; stable across restarts and instances) |
| `C3API_AUTH_ALLOW_FIRST_USER_ADMIN` | Allow the first public registration to become `platform_admin`; default `false` for new deployments |
| `C3API_AUTH_LOGIN_RATE_PER_MINUTE` | Per-instance login requests per source IP (default `20`) |
| `C3API_AUTH_REGISTER_RATE_PER_MINUTE` | Per-instance registration requests per source IP (default `5`) |
| `C3API_AUTH_CODE_RATE_PER_MINUTE` | Per-instance verification-code requests per source IP (default `3`) |
| `C3API_AUTH_RESET_RATE_PER_MINUTE` | Per-instance password-reset requests per source IP (default `5`) |
| `C3API_DB_DSN` | PostgreSQL DSN |
| `C3API_REDIS_ADDR` | Redis address (required; e.g. `127.0.0.1:6379` — instance discovery, short-lived verification codes and other ephemeral state) |
| `C3API_SERVER_TIME_ZONE` | Deployment timezone for pricing time/day-of-week conditions (IANA name, e.g. `Asia/Shanghai`; empty = process local) |

See `config.example.toml` for the full schema (server, log, admin, auth, db, redis, proxy, upstream, limit, scheduler, usage, billing). Set `upstream.proxy_url` at startup when needed (`http://`, `https://`, or `socks5h://`); empty means direct and system `HTTP_PROXY` is ignored. While running, use Settings -> Network proxy and update `upstream_proxy_url`: `inherit` keeps the startup value, `direct` bypasses it, and a new URL is connectivity-checked before an atomic switch. In-flight requests finish on the old route, so changing a local proxy does not require a restart.

- **Relay balance checks**: configure a JSON balance endpoint under `billing.balance_adapters."template-id"` (`provider`, `endpoint`, `auth`, and `balance_path`). The accounts page reads balances on demand and caches them; unconfigured, unavailable, stale, and real values remain distinct, so a failed probe is never shown as zero. The “Refresh balances” action explicitly clears the selected cache entries before querying upstream; normal reads remain cached.
- **Relay pool import**: the accounts page accepts newline-delimited `URL | key | name | weight | max concurrency` or JSON/JSONL input, previews duplicates/invalid/over-limit rows, and never silently skips an invalid row.

- **Fresh setup for beta upgrades** — schemas and configuration may change between releases, so deploy a new instance and re-check the configuration before upgrading (see [Status: Beta](#status-beta)).
- **Env-only deployments** (e.g. K8s): pass `-config ""` to skip the config file entirely — the flag defaults to `config.toml`, and a missing file is a startup error.
- **Most config is read once at startup**; the upstream proxy can be switched from the admin settings without a restart.
- **Invalid config fails fast at startup** with the offending key: non-positive durations/intervals, unknown keys (typos, removed legacy keys), missing required secrets, and placeholder values (`change-me`, `dev-admin-token`, …) are all rejected.
- **Public authentication guard**: login, registration, verification-code, and reset endpoints use bounded per-instance source-IP limits. When running multiple replicas, enforce an equivalent limit at the trusted edge as well.

## Deployment

- `scripts/deploy.ps1` — PowerShell 7 (`pwsh`) remote deploy helper. It previews by default; `-Apply` uploads the current committed version into an isolated directory, fills missing first-deploy secrets and layout values, starts Compose, and checks `/readyz`. Existing instances update only `app` with `--no-deps` after PostgreSQL/Redis readiness checks, so dependency containers are not recreated during a release switch; failures pin the old app image and roll back without rebuilding over the network. It validates Compose before moving `current`, stops on port conflicts or a non-C4 service using the port, and never overwrites another project. Existing `.env` files must contain exactly one non-empty `POSTGRES_PASSWORD` and `AUTH_JWT_SECRET`; beta database reuse requires the explicit `-ReuseDatabase` switch. For provider-verified SSH host keys, pass `-KnownHostsFile` to enable strict checking; without it, first use is `accept-new` and a changed key still fails closed. Successful upgrades retain three releases by default; use `-KeepReleases N` (2–20) to adjust retention.
- `compose.yml` — production stack: one `db` (postgres:18-alpine, bind-mounted data under `deploy/data/pg`) + one `redis` (redis:8-alpine, ephemeral coordination + short-lived verification codes — no persistence) + one `app` container (non-root, read-only config mount from `deploy/config.toml`, healthcheck). The published app image defaults to the pinned `v0.0.1-beta.18` tag; set `IMAGE` explicitly for another release or the moving `:beta` preview. Set `CONFIG_FILE` only when a deployment needs an explicit machine-local override.
- `Dockerfile` — three-stage build (node → go → alpine), producing a single static binary with the UI embedded.
- Public entry guidance is in [`deploy/PUBLIC_DEPLOYMENT.md`](./deploy/PUBLIC_DEPLOYMENT.md) and [`deploy/Caddyfile.example`](./deploy/Caddyfile.example): keep C4 on localhost and let Caddy terminate HTTPS; do not expose port 18080 directly.
- **Dual required dependencies**: PostgreSQL 18 (all durable state, source of record) + Redis 8 (ephemeral coordination + short-lived email verification codes — instance discovery heartbeats and verification codes; never a cache layer). Both are startup-mandatory. Do not set an eviction policy (`allkeys-lru` etc.) for this instance: an evicted code is benign (the user just re-requests one), but keep it out of the configuration.

### GC tuning (optional, default off)

Under high concurrency (25k+ concurrent streams) the default Go GC becomes a measurable cost. A/B-tested at 50k concurrency on a 24-core box, `GOGC=off` + `GOMEMLIMIT=17179869184` (16 GiB — plain bytes, the env var rejects unit suffixes like `16G`) measured:

- Throughput **+14.5%** (25.1k → 28.75k req/s)
- Per-request CPU **-27.7%** (331 → 239 µs)
- First-byte latency **-21%** (1103 → 873 ms); p99 first-byte **-36%** (5785 → 3705 ms)

Trade-off: the heap grows to the limit (~13 GiB with a 16 GiB cap) and GC fires roughly every ~10 s instead of every ~2-3 s — fine on a 64 GiB box; raise `GOMEMLIMIT` to cut the frequency further at the cost of memory. Set both in `.env` (empty values keep Go defaults):

```env
GOGC=off
GOMEMLIMIT=17179869184
```

## License

C4 is open source under the **GNU AGPL v3.0-or-later** (`LICENSE`) — you may use, modify, and deploy it, including for commercial and hosted services, as long as you follow the license terms for the version you run. **No purchase is required for an AGPL-compliant deployment.**

Need **closed-source deployment** (exempt from the AGPL obligations on your deployment)? A commercial license (`LICENSE.commercial`) is available — it waives the AGPL obligations for your deployment.

External code contributions require a CLA (contributor license agreement) so that contributed code can be merged under this dual-license scheme — see [CLA.md](./CLA.md) and [CONTRIBUTING.md](./CONTRIBUTING.md).

## Contact

- GitHub: [baimao5299-ship-it/c4](https://github.com/baimao5299-ship-it/c4)
- Issues: open a GitHub issue for bugs, questions, or feature requests
- Security: report vulnerabilities via [SECURITY.md](./SECURITY.md) (private report)
- Changelog: [CHANGELOG.md](./CHANGELOG.md)
