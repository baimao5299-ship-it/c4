# 公网部署

C4 的 Compose 默认只绑定 `127.0.0.1`。公网用户应通过 HTTPS 反向代理访问，
不要把 `18080` 直接开放到公网。随附的生产配置已开启 `proxy.behind_cdn`，
因此必须让 Caddy 覆盖客户端来源头；否则客户端可伪造限流和审计使用的来源 IP。

## 自动部署

在本机 PowerShell 7（命令名 `pwsh`）中预览：

```powershell
pwsh -NoProfile -File .\scripts\deploy.ps1 -Server server.example.com -User deploy -IdentityFile .\keys\c4-server-ed25519 -RemoteDir /srv/c4 -Domain app.example.com -AppName C4
```

确认服务器地址、SSH 用户、端口和本机 SSH 登录无误后执行：

```powershell
pwsh -NoProfile -File .\scripts\deploy.ps1 -Server server.example.com -User deploy -IdentityFile .\keys\c4-server-ed25519 -RemoteDir /srv/c4 -Domain app.example.com -AppName C4 -Apply
```

如果已从服务器提供商处取得并审核过主机公钥，将其放入本机专用
`known_hosts` 文件，并在两条命令后追加
`-KnownHostsFile .\keys\c4-known-hosts`。脚本会改用严格主机密钥校验；不传该参数时，
首次连接接受新指纹，之后指纹变化会失败并停止部署。

脚本会：

1. 要求工作树干净，按当前提交生成部署包。
2. 先检查服务器 Docker、Compose（支持 `docker compose` 或 `docker-compose`）、`flock` 及基础 Unix 工具和应用端口；端口被占用就停止，不碰已有服务。
3. 将版本放到独立的 `releases/<commit>` 目录，`current` 只指向本次版本；部署锁阻止并发升级。
4. 首次部署自动生成 `ADMIN_TOKEN`、`POSTGRES_PASSWORD`、`AUTH_JWT_SECRET`；如果从 `.env.example` 复制出空值，脚本也会在全新目标补齐随机密钥。已有数据库或 release 时不会生成或替换密钥，并将 `.env` 权限设为仅所有者可读写。
5. 首次部署把 PostgreSQL 数据固定在 `<RemoteDir>/deploy/data/pg`，并固定 Compose 项目名 `c4`。新目标若省略 `BIND_ADDRESS`、`PORT`、`PG_DATA_DIR` 或 `COMPOSE_PROJECT_NAME`，脚本会补齐安全默认值；同一目录内的 release 切换会复用这套数据库。由于 beta 版本不提供迁移保证，beta 升级应改用新的 `RemoteDir` 和数据库。
6. 已有 `.env` 的 `PG_DATA_DIR`、`COMPOSE_PROJECT_NAME`、端口和绑定地址必须唯一且与当前目标一致，避免误用新库或接管其他 Compose 项目。PostgreSQL 18 的版本化 `PG_VERSION` 路径也会被识别为已有持久化状态。
7. 已有实例升级时，脚本只更新 `app`（`--no-deps`），先确认 PostgreSQL 和 Redis 容器仍在运行且健康，不会因为新 release 的工作目录或配置哈希重建依赖服务。首次部署才会启动完整栈。启动 Compose 前先校验新 release 的 Compose 配置，成功后才切换 `current`；启动或 `/readyz` 就绪检查失败会自动切回上一版并再次检查，回滚同样只更新 `app`。`/healthz` 只表示进程存活，`/readyz` 还要求启动快照和已配置代理链路就绪。已有 `.env` 必须包含唯一且非空的 `POSTGRES_PASSWORD`、`AUTH_JWT_SECRET`；脚本默认拒绝 beta 数据库复用，已核对兼容性后可显式传 `-ReuseDatabase`。`-AppName` 和 `-CardStoreUrl` 只在首次写入 `.env`，后续升级会复用原值。
8. 健康升级成功后默认只保留最近 3 个 release（当前版和上一版始终保留），并删除对应部署包；用 `-KeepReleases N` 调整为 2–20 个。清理失败只打印警告，不影响已就绪的当前版本；失败部署的包和新目录会保留供排查。

脚本不会自动修改 DNS、停止其他项目或把密钥打印到终端。SSH 连接带超时和保活参数；升级时使用新提交重新运行，
切换前先确认 `/readyz` 就绪检查通过。

Compose 默认给应用、PostgreSQL 和 Redis 设置内存/CPU 上限，并为每个服务启用
`json-file` 日志轮转（10 MiB × 5）。按服务器规格在 `.env` 中调整
`APP_MEMORY_LIMIT`、`APP_CPU_LIMIT`、`DB_MEMORY_LIMIT`、`DB_CPU_LIMIT`、
`REDIS_MEMORY_LIMIT`、`REDIS_CPU_LIMIT`、`LOG_MAX_SIZE` 和 `LOG_MAX_FILES`；这些值只设上限，
不会预留全部资源。

## 域名与 HTTPS

1. 在域名服务商添加 `A`（IPv4）或 `AAAA`（IPv6）记录，指向服务器公网地址。
2. 安装 Caddy，将 [Caddyfile.example](./Caddyfile.example) 复制为 `Caddyfile`，替换域名。
3. 确保防火墙只放行 `80/tcp` 和 `443/tcp`；`18080/tcp` 保持关闭。
4. Caddy 启动后，用户访问 `https://你的域名/user/login`；客户端 API Base URL 使用
   `https://你的域名/v1`。

## 开放用户前检查

- 管理员打开 `/user/login`，切换到“管理员令牌”并粘贴部署时生成的 `ADMIN_TOKEN`；页面会先调用一个只读管理接口验证令牌，再进入 `/app`。
- 管理员登录后创建模板/上游、读取模型并完成一次 `hi` 测试。
- 创建至少一个公开分组，并确认“渠道监控”能看到样本。
- 用普通用户完成注册、创建 Key、复制端点、发送一次 Chat Completions 和一次 Responses 请求。
- 在“消费日志”核对 Token、费用、延迟与错误原因；再用无效 Key 确认返回 401。
- 配置备份：定期备份服务器 `current/.env`（离线保管）和 `<RemoteDir>/deploy/data/pg`，不要把它们提交到仓库。
- 生产环境关闭公开注册或启用邮箱验证，并在反向代理配置登录/注册限流。
