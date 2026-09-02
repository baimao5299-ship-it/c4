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

  # Optional pre-provisioned known_hosts file. When supplied, SSH uses strict
  # host-key checking; the default keeps the existing accept-new behavior for
  # first-time setup while still failing closed on a changed key.
  [string]$KnownHostsFile = '',

  [ValidatePattern('^/[A-Za-z0-9._/-]+$')]
  [string]$RemoteDir = '/srv/c4',

  [ValidateRange(1024, 65535)]
  [int]$AppPort = 18080,

  [ValidatePattern('^[A-Za-z0-9.-]+$')]
  [string]$Domain = '',

  [string]$CardStoreUrl = '',

  [string]$AppName = '',

  # Keep enough previous releases for automatic rollback while bounding disk
  # usage. The current and immediately previous release are always retained.
  [ValidateRange(2, 20)]
  [int]$KeepReleases = 3,

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
$knownHostsPath = ''
if ($KnownHostsFile) {
  $knownHostsPath = (Resolve-Path -LiteralPath $KnownHostsFile -ErrorAction Stop).Path
  if (-not (Test-Path -LiteralPath $knownHostsPath -PathType Leaf)) {
    throw "KnownHostsFile 不是文件：$KnownHostsFile"
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
if ($knownHostsPath) { Write-Host "主机指纹:  $knownHostsPath (严格校验)" }
if ($ReuseDatabase) { Write-Warning '已显式允许复用已有数据库；仅用于已核对 schema 兼容性的版本。' }
Write-Host "保留 release:  最少 $KeepReleases 个（含当前版）"
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
	"`t`theader_up CF-Connecting-IP {http.request.remote.host}",
	"`t`theader_up True-Client-IP {http.request.remote.host}",
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
for tool in flock tar grep sed head od tr seq ln readlink find ls mv rm; do command -v "$tool" >/dev/null || { echo "服务器缺少 $tool" >&2; exit 3; }; done
exec 9>'__REMOTE_DIR__/.deploy.lock'
flock -n 9 || { echo '已有另一个 C4 部署正在进行' >&2; exit 8; }
current_link='__REMOTE_DIR__/current'
remote_root='__REMOTE_DIR__'
release_root='__REMOTE_DIR__/releases'
# current is a control-plane pointer, not an operator-controlled path. Resolve
# it before using it for Compose or rollback, and fail closed if it escapes the
# release root (including broken links and traversal segments).
previous=''
if [ -e "$current_link" ] && [ ! -L "$current_link" ]; then
  echo 'current 必须是 release 符号链接，停止部署' >&2
  exit 9
fi
if [ -L "$current_link" ]; then
  previous=$(readlink -f "$current_link" 2>/dev/null || true)
  if [ -z "$previous" ]; then
    echo 'current 是无效或断裂的符号链接，停止部署' >&2
    exit 9
  fi
  previous_id=${previous##*/}
  case "$previous" in
    "$release_root"/*) ;;
    *) echo 'current 指向 RemoteDir 以外的路径，停止部署' >&2; exit 9 ;;
  esac
  if [ "$previous" != "$release_root/$previous_id" ] || ! printf '%s' "$previous_id" | grep -Eq '^[0-9a-f]{40}$' || [ ! -f "$previous/compose.yml" ]; then
    echo 'current 不是有效的 C4 release 目录，停止部署' >&2
    exit 9
  fi
fi
existing=0
# A valid current release means this RemoteDir already owns the Compose
# project, even if its app container is stopped or missing. Treat that as an
# upgrade path so a failed app repair can never recreate PostgreSQL/Redis.
if [ -n "$previous" ]; then existing=1; fi
port_in_use() {
  if command -v ss >/dev/null; then ss -ltn 2>/dev/null | grep -Eq '[:.]__APP_PORT__([[:space:]]|$)'; return $?; fi
  if command -v netstat >/dev/null; then netstat -ltn 2>/dev/null | grep -Eq '[:.]__APP_PORT__([[:space:]]|$)'; return $?; fi
  echo '服务器缺少 ss/netstat，无法安全检查端口' >&2; exit 3
}
port_owned_by_c4_app() {
  # During an upgrade the current app may legitimately own AppPort. Only
  # permit that exact running Compose container; a stopped/missing container
  # or an unrelated listener must fail closed before Compose can replace it.
  app_container=$(docker ps -aq --filter 'label=com.docker.compose.project=c4' --filter 'label=com.docker.compose.service=app' | head -n 1)
  [ -n "$app_container" ] || return 1
  app_state=$(docker inspect -f '{{.State.Status}}' "$app_container" 2>/dev/null || true)
  [ "$app_state" = 'running' ] || return 1
  docker port "$app_container" 18080/tcp 2>/dev/null | grep -Eq '(^|:)__APP_PORT__([[:space:]]|$)'
}
if port_in_use; then
  if [ "$existing" -eq 1 ] && port_owned_by_c4_app; then
    :
  else
    echo '端口 __APP_PORT__ 已被非当前 C4 app 占用，停止部署' >&2
    exit 3
  fi
fi
# A prior failed first deployment may have left an empty .env or release
# directory. Only a validated current release or an initialized PG_VERSION
# means that this directory already owns durable state and needs an explicit
# reuse opt-in.
durable_state=0
pg_version_path=''
if [ -d '__PG_DATA_DIR__' ]; then
  # PostgreSQL 18 stores PG_VERSION below its versioned data directory (the
  # older root/data checks miss this and can accidentally treat a live DB as
  # fresh). Bound the walk to the configured data mount and do not cross a
  # filesystem mount into unrelated host paths.
  pg_version_path=$(find '__PG_DATA_DIR__' -xdev -maxdepth 5 -type f -name PG_VERSION -print -quit 2>/dev/null || true)
fi
if [ -n "$previous" ] || [ -n "$pg_version_path" ]; then durable_state=1; fi
rand() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
if [ ! -s '__REMOTE_DIR__/.env' ]; then
  # An existing current release or PG directory without .env is ambiguous:
  # never generate replacement secrets and accidentally attach to old data.
  if [ "$durable_state" -eq 1 ]; then
    echo '检测到已有 release 或数据库目录但缺少 .env；请先恢复配置，停止部署' >&2
    exit 5
  fi
  umask 077
  printf 'ADMIN_TOKEN=%s\nPOSTGRES_PASSWORD=%s\nAUTH_JWT_SECRET=%s\nBIND_ADDRESS=127.0.0.1\nPORT=__APP_PORT__\nPG_DATA_DIR=__PG_DATA_DIR__\nCOMPOSE_PROJECT_NAME=c4\n' "$(rand)" "$(rand)" "$(rand)" > '__REMOTE_DIR__/.env'
fi
admin_count=$(grep -c '^ADMIN_TOKEN=' '__REMOTE_DIR__/.env' || true)
admin_value=$(sed -n 's/^ADMIN_TOKEN=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$admin_count" -gt 1 ]; then echo 'ADMIN_TOKEN 不能重复，停止部署' >&2; exit 5; fi
if [ "$admin_count" -eq 0 ]; then
  if [ "$durable_state" -eq 1 ]; then echo '已有数据库或 release 但缺少 ADMIN_TOKEN，停止部署' >&2; exit 5; fi
  printf 'ADMIN_TOKEN=%s\n' "$(rand)" >> '__REMOTE_DIR__/.env'
elif [ -z "$admin_value" ]; then
  if [ "$durable_state" -eq 1 ]; then echo '已有数据库或 release 的 ADMIN_TOKEN 为空，停止部署' >&2; exit 5; fi
  sed -i "s/^ADMIN_TOKEN=.*/ADMIN_TOKEN=$(rand)/" '__REMOTE_DIR__/.env'
fi
port_count=$(grep -c '^PORT=' '__REMOTE_DIR__/.env' || true)
configured_port=$(sed -n 's/^PORT=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$port_count" -eq 0 ] && [ "$durable_state" -eq 0 ]; then
  printf 'PORT=__APP_PORT__\n' >> '__REMOTE_DIR__/.env'
  port_count=1
  configured_port='__APP_PORT__'
fi
if [ "$port_count" -ne 1 ] || [ "$configured_port" != '__APP_PORT__' ]; then echo '现有 C4 配置端口缺失、重复或与本次参数不同，停止部署' >&2; exit 5; fi
bind_count=$(grep -c '^BIND_ADDRESS=' '__REMOTE_DIR__/.env' || true)
configured_bind=$(sed -n 's/^BIND_ADDRESS=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$bind_count" -eq 0 ] && [ "$durable_state" -eq 0 ]; then
  printf 'BIND_ADDRESS=127.0.0.1\n' >> '__REMOTE_DIR__/.env'
  bind_count=1
  configured_bind='127.0.0.1'
fi
if [ "$bind_count" -ne 1 ] || [ "$configured_bind" != '127.0.0.1' ]; then echo '现有 C4 配置绑定地址缺失、重复或不是本机绑定，停止部署' >&2; exit 5; fi
pg_data_count=$(grep -c '^PG_DATA_DIR=' '__REMOTE_DIR__/.env' || true)
configured_pg_data=$(sed -n 's/^PG_DATA_DIR=//p' '__REMOTE_DIR__/.env' | head -n 1)
if [ "$pg_data_count" -eq 0 ] && [ "$durable_state" -eq 0 ]; then
  printf 'PG_DATA_DIR=%s\n' '__PG_DATA_DIR__' >> '__REMOTE_DIR__/.env'
  pg_data_count=1
  configured_pg_data='__PG_DATA_DIR__'
fi
if [ "$pg_data_count" -ne 1 ] || [ "$configured_pg_data" != '__PG_DATA_DIR__' ]; then echo 'PG_DATA_DIR 必须唯一且固定在共享数据目录，停止部署' >&2; exit 6; fi
project_count=$(grep -c '^COMPOSE_PROJECT_NAME=' '__REMOTE_DIR__/.env' || true)
if [ "$project_count" -eq 0 ] && [ "$durable_state" -eq 0 ]; then
  printf 'COMPOSE_PROJECT_NAME=c4\n' >> '__REMOTE_DIR__/.env'
  project_count=1
fi
if [ "$project_count" -ne 1 ] || ! grep -q '^COMPOSE_PROJECT_NAME=c4$' '__REMOTE_DIR__/.env'; then echo 'COMPOSE_PROJECT_NAME 必须唯一且为 c4，停止部署' >&2; exit 7; fi
for secret in POSTGRES_PASSWORD AUTH_JWT_SECRET; do
  secret_count=$(grep -c "^${secret}=" '__REMOTE_DIR__/.env' || true)
  secret_value=$(sed -n "s/^${secret}=//p" '__REMOTE_DIR__/.env' | head -n 1)
  if [ "$secret_count" -gt 1 ]; then
    echo "$secret 重复，停止部署" >&2
    exit 5
  fi
  if [ "$secret_count" -eq 0 ]; then
    if [ "$durable_state" -eq 1 ]; then echo "$secret 缺失，已有数据库或 release；停止部署" >&2; exit 5; fi
    printf '%s=%s\n' "$secret" "$(rand)" >> '__REMOTE_DIR__/.env'
  elif [ -z "$secret_value" ]; then
    if [ "$durable_state" -eq 1 ]; then echo "$secret 为空，已有数据库或 release；停止部署" >&2; exit 5; fi
    sed -i "s/^${secret}=.*/${secret}=$(rand)/" '__REMOTE_DIR__/.env'
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
mkdir -p '__REMOTE_DIR__/releases/__SHA__'
tar -xzf '__REMOTE_DIR__/c4-__SHA__.tar.gz' -C '__REMOTE_DIR__/releases/__SHA__'
new_release='__REMOTE_DIR__/releases/__SHA__'
ln -sfn '__REMOTE_DIR__/.env' "$new_release/.env"
cd "$new_release"
# Validate the exact release before starting it. A malformed compose file or
# missing mount must leave the live symlink untouched and therefore keep the
# last-known-good release reachable.
if ! compose -p c4 config >/dev/null; then
  echo 'C4 新版本 Compose 配置无效，保留当前版本，停止部署' >&2
  exit 4
fi
deploy_ok=1
# Pass the exact release id to the Compose build without persisting it beside
# operator-managed secrets. Dockerfile uses this value for `-version` output.
export C3API_VERSION='__SHA__'
ensure_dependency() {
  service="$1"
  container_id=$(docker ps -aq --filter 'label=com.docker.compose.project=c4' --filter "label=com.docker.compose.service=$service" | head -n 1)
  if [ -z "$container_id" ]; then
    echo "已有 C4 实例缺少 $service 容器；为避免重建依赖，停止部署" >&2
    return 1
  fi
  container_state=$(docker inspect -f '{{.State.Status}}' "$container_id" 2>/dev/null || true)
  if [ "$container_state" != 'running' ]; then
    echo "已有 C4 $service 容器未运行（状态：$container_state）；为避免中断，停止部署" >&2
    return 1
  fi
  container_health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || true)
  case "$container_health" in
    healthy|none) ;;
    *) echo "已有 C4 $service 容器不健康（状态：$container_health）；为避免中断，停止部署" >&2; return 1 ;;
  esac
}
# Preserve the exact image behind the currently serving app before the
# candidate is built. A release may use a floating image tag (for example
# :beta); tagging the image by ID gives rollback an immutable, local target even
# when the candidate build replaces that tag.
rollback_image_tag=''
capture_previous_image() {
  if [ "$existing" -eq 0 ]; then return 0; fi
  previous_container=$(docker ps -aq --filter 'label=com.docker.compose.project=c4' --filter 'label=com.docker.compose.service=app' | head -n 1)
  if [ -z "$previous_container" ]; then
    echo '已有 C4 app 容器缺失，无法建立回滚镜像基准；停止部署' >&2
    return 1
  fi
  previous_image_id=$(docker inspect -f '{{.Image}}' "$previous_container" 2>/dev/null || true)
  if [ -z "$previous_image_id" ] || ! docker image inspect "$previous_image_id" >/dev/null 2>&1; then
    echo '无法读取当前 C4 app 镜像；停止部署以保留回滚能力' >&2
    return 1
  fi
  rollback_image_tag="c4-rollback-${previous_id}"
  docker tag "$previous_image_id" "$rollback_image_tag"
}
cleanup_rollback_image() {
  if [ -n "$rollback_image_tag" ]; then
    docker image rm "$rollback_image_tag" >/dev/null 2>&1 || true
    rollback_image_tag=''
  fi
}
start_app() {
  if [ "$existing" -eq 1 ]; then
    # Updating only app keeps PostgreSQL/Redis containers, networks, volumes,
    # and in-flight database connections untouched across release switches.
    ensure_dependency db || return 1
    ensure_dependency redis || return 1
    compose -p c4 up -d --build --no-deps app
  else
    compose -p c4 up -d --build
  fi
}
health_ok() {
  # The host may export HTTP(S)_PROXY for outbound traffic. A local readiness
  # probe must never leave the machine or depend on that proxy route.
  if command -v curl >/dev/null; then curl --noproxy '*' -fsS --max-time 3 'http://127.0.0.1:__APP_PORT__/readyz' >/dev/null; return $?; fi
  # AppPort is the host-side mapping; the application always listens on 18080
  # inside the container.
  compose -p c4 exec -T app wget -q -T 3 -O /dev/null 'http://127.0.0.1:18080/readyz'
}
wait_for_health() {
  for i in $(seq 1 30); do
    if health_ok; then return 0; fi
    if [ "$i" -lt 30 ]; then sleep 2; fi
  done
  return 1
}
set_current() {
  target_release="$1"
  next_link="${current_link}.next.$$"
  rm -f -- "$next_link" || return 1
  ln -s "$target_release" "$next_link" || return 1
  # Both links live in RemoteDir, so rename is atomic. An interrupted deploy
  # therefore leaves current at either the old healthy release or the fully
  # validated candidate, never at a partially committed path.
  if mv -Tf -- "$next_link" "$current_link"; then return 0; fi
  rm -f -- "$next_link" || true
  return 1
}
restore_previous_release() {
  echo "正在回滚到 $previous" >&2
  rollback_pointer_ok=1
  if ! set_current "$previous"; then rollback_pointer_ok=0; fi
  if ! ln -sfn '__REMOTE_DIR__/.env' "$previous/.env"; then
    echo '上一版本 .env 链接恢复失败' >&2
    return 1
  fi
  if ! cd "$previous"; then
    echo '无法进入上一版本 release 目录' >&2
    return 1
  fi
  export C3API_VERSION="$(basename "$previous")"
  # Rollback must be local and build-free. The immutable tag captured before
  # candidate startup prevents a floating IMAGE value from selecting the new
  # image, while --no-deps leaves PostgreSQL/Redis untouched.
  if ensure_dependency db && ensure_dependency redis; then
    if [ -n "$rollback_image_tag" ]; then
      IMAGE="$rollback_image_tag" compose -p c4 up -d --no-build --no-deps app
    else
      compose -p c4 up -d --no-build --no-deps app
    fi
  else
    return 1
  fi
  if wait_for_health; then
    if [ "$rollback_pointer_ok" -eq 1 ] || set_current "$previous"; then
      echo '已恢复上一版本；新版本保留在 releases 目录供排查' >&2
      cleanup_rollback_image
      return 0
    fi
    echo '上一版本已恢复运行，但 current 指针恢复失败；请保留现场并检查目录权限' >&2
    return 1
  fi
  echo '回滚健康检查也失败，请保留现场并检查 docker compose ps/logs' >&2
  return 1
}
stop_uncommitted_first_deploy() {
  echo '首次部署未通过验收；正在停止未验收 APP，保留依赖和 release 供排查' >&2
  if ! cd "$new_release"; then return 1; fi
  if compose -p c4 stop app; then return 0; fi
  echo '停止未验收 APP 失败，请立即检查 docker compose ps/logs' >&2
  return 1
}
# BEGIN C4 deployment transaction guards
rollback_uncommitted() {
  if [ "$candidate_attempted" -ne 1 ] || [ "$deploy_committed" -eq 1 ] || [ "$cleanup_running" -eq 1 ]; then return 0; fi
  cleanup_running=1
  rollback_result=0
  # previous was resolved and validated before any candidate work began.
  if [ -n "$previous" ]; then
    if ! restore_previous_release; then
      rollback_result=1
      # A failed restore must not leave the unverified candidate serving. The
      # project name is stable, so the previous Compose file addresses the same
      # app container even if recreation failed midway.
      cd "$previous" 2>/dev/null || true
      compose -p c4 stop app >/dev/null 2>&1 || true
    fi
  else
    if ! stop_uncommitted_first_deploy; then rollback_result=1; fi
  fi
  compose -p c4 ps || true
  candidate_attempted=0
  cleanup_running=0
  return "$rollback_result"
}
transaction_exit() {
  transaction_status="$1"
  trap - EXIT HUP INT TERM
  set +e
  rollback_uncommitted
  exit "$transaction_status"
}
commit_candidate() {
  if ! set_current "$new_release"; then return 1; fi
  deploy_committed=1
  candidate_attempted=0
  trap - EXIT HUP INT TERM
  return 0
}
# END C4 deployment transaction guards
cleanup_releases() {
  # Keep the current and previous release regardless of mtime. Only remove
  # directories whose names are full Git SHAs beneath the release root; this
  # prevents a malformed operator-created entry from expanding the rm path.
  kept=0
  for release in $(ls -1dt "$release_root"/* 2>/dev/null || true); do
    [ -d "$release" ] || continue
    release_id=${release##*/}
    [ "$release" = "$release_root/$release_id" ] || continue
    printf '%s' "$release_id" | grep -Eq '^[0-9a-f]{40}$' || continue
    if [ "$release" = "$new_release" ] || [ "$release" = "$previous" ]; then
      kept=$((kept + 1))
      continue
    fi
    kept=$((kept + 1))
    if [ "$kept" -gt '__KEEP_RELEASES__' ]; then
      rm -rf -- "$release" || return 1
      rm -f -- "$remote_root/c4-$release_id.tar.gz" || return 1
    fi
  done
  rm -f -- "$remote_root/c4-__SHA__.tar.gz" || return 1
}
candidate_attempted=0
deploy_committed=0
cleanup_running=0
trap 'transaction_exit "$?"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
if ! capture_previous_image; then
  exit 5
fi
candidate_attempted=1
if ! start_app; then deploy_ok=0; fi
if [ "$deploy_ok" -eq 1 ] && wait_for_health; then
  # Compose is already running from new_release. Commit the control-plane
  # pointer only after the candidate has passed the full readiness window.
  if commit_candidate; then
    if ! cleanup_releases; then echo '警告：release 清理失败，但当前版本已就绪；请检查磁盘权限' >&2; fi
    cleanup_rollback_image
    exit 0
  fi
  echo 'C4 新版本已就绪，但 current 原子切换失败；开始恢复上一版本' >&2
fi
deploy_ok=0
echo 'C4 新版本启动或健康检查失败' >&2
# EXIT cleanup restores the resolved previous release, or stops an unverified
# first-deploy app. It preserves this failure status after cleanup completes.
exit 4
'@
$remote = $remote.Replace('__APP_PORT__', $AppPort.ToString()).Replace('__REMOTE_DIR__', $RemoteDir).Replace('__SHA__', $sha).Replace('__PG_DATA_DIR__', $pgDataDir).Replace('__CARD_STORE_LINE__', $cardStoreLine).Replace('__APP_NAME_LINE__', $appNameLine).Replace('__REUSE_DATABASE__', $reuseDatabaseValue).Replace('__KEEP_RELEASES__', $KeepReleases.ToString())

$sshOptions = @(
  '-o', 'BatchMode=yes',
  '-o', 'ConnectTimeout=10',
  '-o', 'ServerAliveInterval=15',
  '-o', 'ServerAliveCountMax=3'
)
if ($knownHostsPath) {
  # Operators can export a provider-verified key with ssh-keyscan and review
  # it out of band before using this mode. Never silently replace this file.
  $sshOptions += @('-o', "UserKnownHostsFile=$knownHostsPath", '-o', 'StrictHostKeyChecking=yes')
} else {
  # Accept a host key only on first use; a changed key still fails closed.
  $sshOptions += @('-o', 'StrictHostKeyChecking=accept-new')
}
if ($identityPath) { $sshOptions = @('-i', $identityPath) + $sshOptions }
& ssh -p $SshPort @sshOptions $target "mkdir -p '$RemoteDir'"
if ($LASTEXITCODE -ne 0) { throw 'SSH 连接或远端目录创建失败。' }
& scp -P $SshPort @sshOptions $bundle "$target`:$RemoteDir/c4-$sha.tar.gz"
if ($LASTEXITCODE -ne 0) { throw '上传部署包失败。' }
& ssh -p $SshPort @sshOptions $target $remote
if ($LASTEXITCODE -ne 0) { throw '远程部署或健康检查失败；脚本已尝试回滚到上一版。' }

Write-Host "C4 已启动：$target`:$RemoteDir/current，就绪检查通过。"
Write-Host '下一步：将 deploy/Caddyfile.example 复制到服务器并填入域名，再让 DNS 指向服务器。'
