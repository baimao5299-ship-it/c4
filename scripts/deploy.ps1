# C4 远程部署助手（PowerShell 7+ / Windows OpenSSH）
#
# 默认只检查并打印部署计划；加 -Apply 才会上传并启动。脚本不会停止或
# 覆盖服务器上的其他 Compose 项目，也不会把密钥放在命令行参数中。
#
# 预览：
#   .\scripts\deploy.ps1 -Server 203.0.113.10 -User root -Domain api.example.com
# 执行：
#   .\scripts\deploy.ps1 -Server 203.0.113.10 -User root -Domain api.example.com -Apply

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)]
  [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9.-]*$')]
  [string]$Server,

  [Parameter(Mandatory = $true)]
  [ValidatePattern('^[A-Za-z_][A-Za-z0-9_.-]*$')]
  [string]$User,

  [ValidateRange(1, 65535)]
  [int]$SshPort = 22,

  [ValidatePattern('^/[A-Za-z0-9._/-]+$')]
  [string]$RemoteDir = '/opt/c4',

  [ValidateRange(1024, 65535)]
  [int]$AppPort = 18080,

  [ValidatePattern('^[A-Za-z0-9.-]+$')]
  [string]$Domain = '',

  [string]$CardStoreUrl = '',

  [string]$AppName = '',

  [switch]$Apply
)

$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$sha = (& git -C $projectRoot rev-parse --verify HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $sha.Length -ne 40) { throw '无法读取当前 Git 提交。' }

$dirty = @(& git -C $projectRoot status --porcelain)

foreach ($tool in @('ssh', 'scp', 'tar')) {
  if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
    throw "本机缺少 $tool；请安装 Windows OpenSSH 与 bsdtar 后重试。"
  }
}

$bundleDir = Join-Path $projectRoot 'artifacts\deploy'
New-Item -ItemType Directory -Force -Path $bundleDir | Out-Null
$bundle = Join-Path $bundleDir "c4-$sha.tar.gz"

Write-Host "提交:  $sha"
Write-Host "目标:  $User@$Server`:$SshPort"
Write-Host "目录:  $RemoteDir"
Write-Host "端口:  127.0.0.1`:$AppPort"
if ($Domain) { Write-Host "域名:  $Domain (仅生成 Caddy 片段，不自动改 DNS)" }
if ($CardStoreUrl -and ($CardStoreUrl -match "['`r`n]" -or $CardStoreUrl -notmatch '^https?://')) {
  throw 'CardStoreUrl 必须是 http(s) URL，且不能包含单引号或换行。'
}
if ($AppName -and $AppName -match "['`r`n]") { throw 'AppName 不能包含单引号或换行。' }
$caddyPath = $null
if ($Domain) {
  $caddyPath = Join-Path $bundleDir "Caddyfile-$sha"
  @(
    "# Generated for $Domain; review before installing on the server.",
    "$Domain {",
    '    encode zstd gzip',
    "    reverse_proxy 127.0.0.1:$AppPort",
    '}'
  ) | Set-Content -Path $caddyPath -Encoding utf8
  Write-Host "Caddy: $caddyPath"
}
if (-not $Apply) {
  if ($dirty.Count -gt 0) { Write-Warning '当前工作树有未提交改动；预览可继续，但执行部署前必须先提交。' }
  Write-Host '预览模式：未上传、未启动。加 -Apply 执行部署。'
  exit 0
}
if ($dirty.Count -gt 0) {
  throw '工作树不是干净提交；请先提交并验证 C4，再部署精确提交。'
}

# git archive 只打包已提交文件，避免 node_modules、数据库和本地 .env 进入服务器。
& git -C $projectRoot archive --format=tar.gz --output=$bundle HEAD
if ($LASTEXITCODE -ne 0 -or -not (Test-Path $bundle)) { throw '生成部署包失败。' }

$target = "$User@$Server"
$cardStoreLine = if ($CardStoreUrl) { "if ! grep -q '^VITE_CARD_STORE_URL=' '$RemoteDir/.env'; then printf 'VITE_CARD_STORE_URL=%s\n' '$CardStoreUrl' >> '$RemoteDir/.env'; fi" } else { ':' }
$appNameLine = if ($AppName) { "if ! grep -q '^VITE_APP_NAME=' '$RemoteDir/.env'; then printf 'VITE_APP_NAME=%s\n' '$AppName' >> '$RemoteDir/.env'; fi" } else { ':' }
$pgDataDir = "$RemoteDir/data/pg"
$remote = @'
set -eu
command -v docker >/dev/null || { echo '服务器缺少 docker' >&2; exit 2; }
if docker compose version >/dev/null 2>&1; then compose() { docker compose "$@"; }
elif command -v docker-compose >/dev/null 2>&1; then compose() { docker-compose "$@"; }
else echo '服务器缺少 Docker Compose' >&2; exit 2; fi
for tool in flock tar grep sed head od tr seq ln readlink; do command -v "$tool" >/dev/null || { echo "服务器缺少 $tool" >&2; exit 3; }; done
exec 9>'__REMOTE_DIR__/.deploy.lock'
flock -n 9 || { echo '已有另一个 C4 部署正在进行' >&2; exit 8; }
existing=0
if [ -f '__REMOTE_DIR__/current/compose.yml' ] && (cd '__REMOTE_DIR__/current' && compose -p c4 ps -q app 2>/dev/null | grep -q .); then existing=1; fi
port_in_use() {
  if command -v ss >/dev/null; then ss -ltn 2>/dev/null | grep -Eq '[:.]__APP_PORT__([[:space:]]|$)'; return $?; fi
  if command -v netstat >/dev/null; then netstat -ltn 2>/dev/null | grep -Eq '[:.]__APP_PORT__([[:space:]]|$)'; return $?; fi
  echo '服务器缺少 ss/netstat，无法安全检查端口' >&2; exit 3
}
if [ "$existing" -eq 0 ] && port_in_use; then echo '端口 __APP_PORT__ 已被占用，停止部署' >&2; exit 3; fi
mkdir -p '__REMOTE_DIR__/releases/__SHA__'
if [ ! -s '__REMOTE_DIR__/.env' ]; then
  umask 077
  rand() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
  printf 'ADMIN_TOKEN=%s\nPOSTGRES_PASSWORD=%s\nAUTH_JWT_SECRET=%s\nBIND_ADDRESS=127.0.0.1\nPORT=__APP_PORT__\nPG_DATA_DIR=__PG_DATA_DIR__\nCOMPOSE_PROJECT_NAME=c4\n' "$(rand)" "$(rand)" "$(rand)" > '__REMOTE_DIR__/.env'
fi
admin_count=$(grep -c '^ADMIN_TOKEN=' '__REMOTE_DIR__/.env' || true)
admin_value=$(sed -n 's/^ADMIN_TOKEN=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$admin_count" -ne 1 ]; then echo 'ADMIN_TOKEN 必须唯一，停止部署' >&2; exit 5; fi
if [ -z "$admin_value" ]; then
  rand() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
  sed -i "s/^ADMIN_TOKEN=.*/ADMIN_TOKEN=$(rand)/" '__REMOTE_DIR__/.env'
fi
port_count=$(grep -c '^PORT=' '__REMOTE_DIR__/.env' || true)
configured_port=$(sed -n 's/^PORT=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$port_count" -ne 1 ] || [ "$configured_port" != '__APP_PORT__' ]; then echo '现有 C4 配置端口缺失、重复或与本次参数不同，停止部署' >&2; exit 5; fi
bind_count=$(grep -c '^BIND_ADDRESS=' '__REMOTE_DIR__/.env' || true)
configured_bind=$(sed -n 's/^BIND_ADDRESS=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$bind_count" -ne 1 ] || [ "$configured_bind" != '127.0.0.1' ]; then echo '现有 C4 配置绑定地址缺失、重复或不是本机绑定，停止部署' >&2; exit 5; fi
pg_data_count=$(grep -c '^PG_DATA_DIR=' '__REMOTE_DIR__/.env' || true)
configured_pg_data=$(sed -n 's/^PG_DATA_DIR=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$pg_data_count" -ne 1 ] || [ "$configured_pg_data" != '__PG_DATA_DIR__' ]; then echo 'PG_DATA_DIR 必须唯一且固定在共享数据目录，停止部署' >&2; exit 6; fi
project_count=$(grep -c '^COMPOSE_PROJECT_NAME=' '__REMOTE_DIR__/.env' || true)
if [ "$project_count" -ne 1 ] || ! grep -q '^COMPOSE_PROJECT_NAME=c4$' '__REMOTE_DIR__/.env'; then echo 'COMPOSE_PROJECT_NAME 必须唯一且为 c4，停止部署' >&2; exit 7; fi
__CARD_STORE_LINE__
__APP_NAME_LINE__
chmod 600 '__REMOTE_DIR__/.env'
mkdir -p '__PG_DATA_DIR__'
previous=$(readlink -f '__REMOTE_DIR__/current' 2>/dev/null || true)
if [ -e '__REMOTE_DIR__/current' ] && [ ! -L '__REMOTE_DIR__/current' ]; then echo 'current 必须是 release 符号链接，停止部署' >&2; exit 9; fi
tar -xzf '__REMOTE_DIR__/c4-__SHA__.tar.gz' -C '__REMOTE_DIR__/releases/__SHA__'
ln -sfn '__REMOTE_DIR__/releases/__SHA__' '__REMOTE_DIR__/current'
ln -sfn '__REMOTE_DIR__/.env' '__REMOTE_DIR__/current/.env'
cd '__REMOTE_DIR__/current'
compose -p c4 config >/dev/null
deploy_ok=1
if ! compose -p c4 up -d --build; then deploy_ok=0; fi
health_ok() {
  if command -v curl >/dev/null; then curl -fsS --max-time 3 'http://127.0.0.1:__APP_PORT__/healthz' >/dev/null; return $?; fi
  compose -p c4 exec -T app wget -q -T 3 -O /dev/null 'http://127.0.0.1:18080/healthz'
}
if [ "$deploy_ok" -eq 1 ]; then
  for i in $(seq 1 30); do if health_ok; then exit 0; fi; sleep 2; done
  deploy_ok=0
fi
echo 'C4 新版本启动或健康检查失败' >&2
if [ -n "$previous" ] && [ -f "$previous/compose.yml" ]; then
  echo "正在回滚到 $previous" >&2
  ln -sfn "$previous" '__REMOTE_DIR__/current'
  ln -sfn '__REMOTE_DIR__/.env' '__REMOTE_DIR__/current/.env'
  cd '__REMOTE_DIR__/current'
  if compose -p c4 up -d --build && health_ok; then
    echo '已恢复上一版本；新版本保留在 releases 目录供排查' >&2
  else
    echo '回滚健康检查也失败，请保留现场并检查 docker compose ps/logs' >&2
  fi
else
  echo '这是首次部署，没有可回滚版本' >&2
fi
compose -p c4 ps
exit 4
'@
$remote = $remote.Replace('__APP_PORT__', $AppPort.ToString()).Replace('__REMOTE_DIR__', $RemoteDir).Replace('__SHA__', $sha).Replace('__PG_DATA_DIR__', $pgDataDir).Replace('__CARD_STORE_LINE__', $cardStoreLine).Replace('__APP_NAME_LINE__', $appNameLine)

& ssh -p $SshPort -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 $target "mkdir -p '$RemoteDir'"
if ($LASTEXITCODE -ne 0) { throw 'SSH 连接或远端目录创建失败。' }
& scp -P $SshPort -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 $bundle "$target`:$RemoteDir/c4-$sha.tar.gz"
if ($LASTEXITCODE -ne 0) { throw '上传部署包失败。' }
& ssh -p $SshPort -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 $target $remote
if ($LASTEXITCODE -ne 0) { throw '远程部署或健康检查失败；脚本已尝试回滚到上一版。' }

Write-Host "C4 已启动：$target`:$RemoteDir/current，健康检查通过。"
Write-Host '下一步：将 deploy/Caddyfile.example 复制到服务器并填入域名，再让 DNS 指向服务器。'
