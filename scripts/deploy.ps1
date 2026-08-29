# C4 远程部署助手（PowerShell 7+ / Windows OpenSSH）
#
# 默认只检查并打印部署计划；加 -Apply 才会上传并启动。脚本不会停止或
# 覆盖服务器上的其他 Compose 项目，也不会把密钥放在命令行参数中。
#
# 预览：
#   pwsh -NoProfile -File .\scripts\deploy.ps1 -Server server.example.com -User deploy -IdentityFile .\keys\c4-server-ed25519 -RemoteDir /srv/c4 -Domain app.example.com
# 执行：
#   pwsh -NoProfile -File .\scripts\deploy.ps1 -Server server.example.com -User deploy -IdentityFile .\keys\c4-server-ed25519 -RemoteDir /srv/c4 -Domain app.example.com -Apply
# Beta 版本默认要求新目录/新数据库；仅在已核对 schema 兼容性后显式传
# -ReuseDatabase 复用已有数据库。

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

  [string]$IdentityFile = '',

  [ValidatePattern('^/[A-Za-z0-9._/-]+$')]
  [string]$RemoteDir = '/srv/c4',

  [ValidateRange(1024, 65535)]
  [int]$AppPort = 18080,

  [ValidatePattern('^[A-Za-z0-9.-]+$')]
  [string]$Domain = '',

  [string]$CardStoreUrl = '',

  [string]$AppName = '',

  [switch]$ReuseDatabase,

  [switch]$Apply
)

$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

$identityPath = ''
if ($IdentityFile) {
  $identityPath = (Resolve-Path -LiteralPath $IdentityFile -ErrorAction Stop).Path
  if (-not (Test-Path -LiteralPath $identityPath -PathType Leaf)) {
    throw "IdentityFile 不是文件：$IdentityFile"
  }
}

# RemoteDir is interpolated into the quoted remote shell script below.  The
# character allow-list prevents shell metacharacters, but it still permits
# traversal segments such as `/srv/c4/../shared`; reject those explicitly so a
# typo cannot deploy outside the operator-selected release root.  Empty
# segments and a trailing slash are rejected as well, keeping all generated
# paths canonical and making the path checks deterministic on POSIX hosts.
if ($RemoteDir -eq '/' -or $RemoteDir -match '//|/$') {
  throw 'RemoteDir must be a canonical absolute path without the root, duplicate slashes, or a trailing slash.'
}
foreach ($segment in $RemoteDir.Substring(1).Split('/')) {
  if ([string]::IsNullOrEmpty($segment) -or $segment -eq '.' -or $segment -eq '..') {
    throw 'RemoteDir must not contain dot, dot-dot, or empty path segments.'
  }
}
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
if ($identityPath) { Write-Host "密钥:  $identityPath" }
if ($ReuseDatabase) { Write-Warning '已显式允许复用已有数据库；仅用于已核对 schema 兼容性的版本。' }
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
    "`tencode zstd gzip",
    "`treverse_proxy 127.0.0.1:$AppPort {",
	"`t`theader_up X-Forwarded-For {http.request.remote.host}",
    "`t`theader_up X-Real-IP {http.request.remote.host}",
    "`t}",
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
$pgDataDir = "$RemoteDir/deploy/data/pg"
$reuseDatabaseValue = if ($ReuseDatabase) { '1' } else { '0' }
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
# A prior failed first deployment may have left an empty .env or release
# directory. Only a current symlink or an initialized PG_VERSION means that
# this directory already owns durable state and needs an explicit reuse opt-in.
durable_state=0
if [ -e '__REMOTE_DIR__/current' ] || [ -L '__REMOTE_DIR__/current' ] || [ -e '__PG_DATA_DIR__/PG_VERSION' ] || [ -e '__PG_DATA_DIR__/data/PG_VERSION' ]; then durable_state=1; fi
if [ ! -s '__REMOTE_DIR__/.env' ]; then
  # An existing current release or PG directory without .env is ambiguous:
  # never generate replacement secrets and accidentally attach to old data.
  if [ "$durable_state" -eq 1 ]; then
    echo '检测到已有 release 或数据库目录但缺少 .env；请先恢复配置，停止部署' >&2
    exit 5
  fi
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
for secret in POSTGRES_PASSWORD AUTH_JWT_SECRET; do
  secret_count=$(grep -c "^${secret}=" '__REMOTE_DIR__/.env' || true)
  secret_value=$(sed -n "s/^${secret}=//p" '__REMOTE_DIR__/.env' | head -n 1)
  if [ "$secret_count" -ne 1 ] || [ -z "$secret_value" ]; then
    echo "$secret 缺失、为空或重复；请先补齐 .env，停止部署" >&2
    exit 5
  fi
done
if [ "$durable_state" -eq 1 ] && [ '__REUSE_DATABASE__' != '1' ]; then
  echo 'Beta 部署默认不复用已有 .env/数据库；请使用新 RemoteDir，或显式传 -ReuseDatabase 并确认 schema 兼容' >&2
  exit 10
fi
__CARD_STORE_LINE__
__APP_NAME_LINE__
chmod 600 '__REMOTE_DIR__/.env'
mkdir -p '__PG_DATA_DIR__'
previous=$(readlink -f '__REMOTE_DIR__/current' 2>/dev/null || true)
if [ -e '__REMOTE_DIR__/current' ] && [ ! -L '__REMOTE_DIR__/current' ]; then echo 'current 必须是 release 符号链接，停止部署' >&2; exit 9; fi
mkdir -p '__REMOTE_DIR__/releases/__SHA__'
tar -xzf '__REMOTE_DIR__/c4-__SHA__.tar.gz' -C '__REMOTE_DIR__/releases/__SHA__'
new_release='__REMOTE_DIR__/releases/__SHA__'
ln -sfn '__REMOTE_DIR__/.env' "$new_release/.env"
cd "$new_release"
# Validate the exact release before moving current. A malformed compose file or
# missing mount must leave the live symlink untouched and therefore keep the
# running version reachable.
if ! compose -p c4 config >/dev/null; then
  echo 'C4 新版本 Compose 配置无效，保留当前版本，停止部署' >&2
  exit 4
fi
ln -sfn "$new_release" '__REMOTE_DIR__/current'
deploy_ok=1
# Pass the exact release id to the Compose build without persisting it beside
# operator-managed secrets. Dockerfile uses this value for `-version` output.
export C3API_VERSION='__SHA__'
if ! compose -p c4 up -d --build; then deploy_ok=0; fi
health_ok() {
  # The host may export HTTP(S)_PROXY for outbound traffic. A local readiness
  # probe must never leave the machine or depend on that proxy route.
  if command -v curl >/dev/null; then curl --noproxy '*' -fsS --max-time 3 'http://127.0.0.1:__APP_PORT__/readyz' >/dev/null; return $?; fi
  # AppPort is the host-side mapping; the application always listens on 18080
  # inside the container.
  compose -p c4 exec -T app wget -q -T 3 -O /dev/null 'http://127.0.0.1:18080/readyz'
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
  export C3API_VERSION="$(basename "$previous")"
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
$remote = $remote.Replace('__APP_PORT__', $AppPort.ToString()).Replace('__REMOTE_DIR__', $RemoteDir).Replace('__SHA__', $sha).Replace('__PG_DATA_DIR__', $pgDataDir).Replace('__CARD_STORE_LINE__', $cardStoreLine).Replace('__APP_NAME_LINE__', $appNameLine).Replace('__REUSE_DATABASE__', $reuseDatabaseValue)

$sshOptions = @(
  '-o', 'BatchMode=yes',
  '-o', 'ConnectTimeout=10',
  '-o', 'ServerAliveInterval=15',
  '-o', 'ServerAliveCountMax=3',
  # Accept a host key only on first use; a changed key still fails closed.
  '-o', 'StrictHostKeyChecking=accept-new'
)
if ($identityPath) { $sshOptions = @('-i', $identityPath) + $sshOptions }
& ssh -p $SshPort @sshOptions $target "mkdir -p '$RemoteDir'"
if ($LASTEXITCODE -ne 0) { throw 'SSH 连接或远端目录创建失败。' }
& scp -P $SshPort @sshOptions $bundle "$target`:$RemoteDir/c4-$sha.tar.gz"
if ($LASTEXITCODE -ne 0) { throw '上传部署包失败。' }
& ssh -p $SshPort @sshOptions $target $remote
if ($LASTEXITCODE -ne 0) { throw '远程部署或健康检查失败；脚本已尝试回滚到上一版。' }

Write-Host "C4 已启动：$target`:$RemoteDir/current，就绪检查通过。"
Write-Host '下一步：将 deploy/Caddyfile.example 复制到服务器并填入域名，再让 DNS 指向服务器。'
