# 公网部署

C4 的 Compose 默认只绑定 `127.0.0.1`。公网用户应通过 HTTPS 反向代理访问，
不要把 `18080` 直接开放到公网。

## 自动部署

在本机 PowerShell 预览：

```powershell
.\scripts\deploy.ps1 -Server 203.0.113.10 -User root -Domain api.example.com -AppName 晴雨天
```

确认服务器地址、SSH 用户、端口和本机 SSH 登录无误后执行：

```powershell
.\scripts\deploy.ps1 -Server 203.0.113.10 -User root -Domain api.example.com -AppName 晴雨天 -Apply
```

脚本会：

1. 要求工作树干净，按当前提交生成部署包。
2. 先检查服务器 Docker、Compose、`flock` 及基础 Unix 工具和应用端口；端口被占用就停止，不碰已有服务。
3. 将版本放到独立的 `releases/<commit>` 目录，`current` 只指向本次版本；部署锁阻止并发升级。
4. 首次部署自动生成 `ADMIN_TOKEN`、`POSTGRES_PASSWORD`、`AUTH_JWT_SECRET`，并将 `.env` 权限设为仅所有者可读写。
5. 首次部署把 PostgreSQL 数据固定在 `<RemoteDir>/data/pg`（默认 `/opt/c4/data/pg`），并固定 Compose 项目名 `c4`；后续 release 切换不会换库或创建第二套项目。
6. 已有 `.env` 若缺少 `PG_DATA_DIR` 或 `COMPOSE_PROJECT_NAME=c4` 会停止并要求先完成迁移，避免误用新库或接管其他 Compose 项目。
7. 启动 Compose 后轮询 `/healthz`；新版本启动或健康检查失败会自动切回上一版并再次检查，首次部署则保留现场并报告容器状态。`-AppName` 和 `-CardStoreUrl` 只在首次生成 `.env` 时写入，后续升级会复用原值。

脚本不会自动修改 DNS、停止其他项目或把密钥打印到终端。SSH 连接带超时和保活参数；升级时使用新提交重新运行，
切换前先确认健康检查通过。

## 域名与 HTTPS

1. 在域名服务商添加 `A`（IPv4）或 `AAAA`（IPv6）记录，指向服务器公网地址。
2. 安装 Caddy，将 [Caddyfile.example](./Caddyfile.example) 复制为 `Caddyfile`，替换域名。
3. 确保防火墙只放行 `80/tcp` 和 `443/tcp`；`18080/tcp` 保持关闭。
4. Caddy 启动后，用户访问 `https://你的域名/user/login`；客户端 API Base URL 使用
   `https://你的域名/v1`。

## 开放用户前检查

- 管理员先登录 `/app`，创建模板/上游、读取模型并完成一次 `hi` 测试。
- 创建至少一个公开分组，并确认“渠道监控”能看到样本。
- 用普通用户完成注册、创建 Key、复制端点、发送一次 Chat Completions 和一次 Responses 请求。
- 在“消费日志”核对 Token、费用、延迟与错误原因；再用无效 Key 确认返回 401。
- 配置备份：定期备份服务器 `current/.env`（离线保管）和 `<RemoteDir>/data/pg`（默认 `/opt/c4/data/pg`），不要把它们提交到仓库。
- 生产环境关闭公开注册或启用邮箱验证，并在反向代理配置登录/注册限流。
