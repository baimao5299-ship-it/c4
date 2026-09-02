$ErrorActionPreference = 'Stop'

$deployPath = Join-Path $PSScriptRoot 'deploy.ps1'
$source = Get-Content -Raw -LiteralPath $deployPath
$remoteStart = $source.IndexOf("`$remote = @'")
$remoteEnd = $source.IndexOf("'@", $remoteStart + 1)
if ($remoteStart -lt 0 -or $remoteEnd -le $remoteStart) {
  throw 'Could not locate the remote deployment transaction in deploy.ps1.'
}
$remote = $source.Substring($remoteStart, $remoteEnd - $remoteStart)
$guardStartMarker = '# BEGIN C4 deployment transaction guards'
$guardEndMarker = '# END C4 deployment transaction guards'
$guardStart = $remote.IndexOf($guardStartMarker)
$guardEnd = $remote.IndexOf($guardEndMarker, $guardStart + 1)
if ($guardStart -lt 0 -or $guardEnd -le $guardStart) {
  throw 'Could not locate the deployment transaction guard block.'
}
$transactionGuards = $remote.Substring($guardStart, $guardEnd - $guardStart)

$bashCandidates = @(
  (Join-Path $env:ProgramFiles 'Git\bin\bash.exe'),
  (Join-Path $env:ProgramFiles 'Git\usr\bin\bash.exe'),
  (Join-Path $env:LOCALAPPDATA 'Programs\Git\bin\bash.exe')
)
$bashPath = $bashCandidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
if (-not $bashPath -and $IsLinux) {
  $bashPath = (Get-Command bash -ErrorAction SilentlyContinue).Source
}

function Invoke-TransactionHarness {
  param([Parameter(Mandatory = $true)][string]$Body)

  $output = $Body | & $bashPath -s 2>&1
  [pscustomobject]@{
    ExitCode = $LASTEXITCODE
    Output = ($output -join "`n")
  }
}

Describe 'C4 deployment transaction safety' {
  It 'commits current only after the candidate passes bounded readiness' {
    $startApp = $remote.IndexOf('if ! start_app; then deploy_ok=0; fi')
    $waitCandidate = $remote.IndexOf('if [ "$deploy_ok" -eq 1 ] && wait_for_health; then')
    $commitCandidate = $remote.IndexOf('if commit_candidate; then')

    $startApp | Should BeGreaterThan -1
    $waitCandidate | Should BeGreaterThan $startApp
    $commitCandidate | Should BeGreaterThan $waitCandidate
    $remote.Substring(0, $waitCandidate) | Should Not Match 'ln -sfn \"\$new_release\".*current'
    $transactionGuards | Should Match 'commit_candidate\(\)[\s\S]*set_current \"\$new_release\"[\s\S]*deploy_committed=1[\s\S]*trap - EXIT HUP INT TERM'
    $remote | Should Match 'next_link=\"\$\{current_link\}\.next\.\$\$\"'
    $remote | Should Match 'mv -Tf -- \"\$next_link\" \"\$current_link\"'
  }

  It 'uses the same bounded readiness loop for forward deploy and rollback' {
    $remote | Should Match 'wait_for_health\(\) \{[\s\S]*for i in \$\(seq 1 30\); do[\s\S]*sleep 2[\s\S]*return 1[\s\S]*\}'
    $remote | Should Match 'if \[ \"\$deploy_ok\" -eq 1 \] && wait_for_health; then'
    $remote | Should Match 'if ensure_dependency db && ensure_dependency redis; then[\s\S]*if wait_for_health; then'
    $remote | Should Not Match 'if start_app && health_ok; then'
  }

  It 'captures an immutable old image and keeps rollback build-free' {
    $remote | Should Match 'capture_previous_image\(\)[\s\S]*docker inspect -f ''\{\{\.Image\}\}'''
    $remote | Should Match 'rollback_image_tag="c4-rollback-\$\{previous_id\}"'
    $remote | Should Match 'IMAGE="\$rollback_image_tag" compose -p c4 up -d --no-build --no-deps app'
    $remote | Should Match 'compose -p c4 up -d --no-build --no-deps app'
    $remote | Should Match 'if ! capture_previous_image; then'
  }

  It 'rejects a port owned by an unrelated process during an upgrade' {
    $remote | Should Match 'port_owned_by_c4_app\(\)[\s\S]*docker port "\$app_container" 18080/tcp'
    $remote | Should Match 'if port_in_use; then[\s\S]*if \[ "\$existing" -eq 1 \] && port_owned_by_c4_app; then[\s\S]*端口 __APP_PORT__ 已被非当前 C4 app 占用'
    $remote | Should Not Match 'if \[ "\$existing" -eq 0 \] && port_in_use'
  }

  It 'rolls back an attempted candidate when the remote shell receives HUP' -Skip:(-not $bashPath) {
    $harness = @"
set -eu
candidate_attempted=0
deploy_committed=0
cleanup_running=0
previous=/releases/old
new_release=/releases/new
restore_previous_release() { echo ROLLBACK_PREVIOUS; }
stop_uncommitted_first_deploy() { echo STOP_FIRST_DEPLOY; }
compose() { :; }
set_current() { :; }
$transactionGuards
trap 'transaction_exit "`$?"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
candidate_attempted=1
kill -HUP "`$`$"
"@
    $result = Invoke-TransactionHarness -Body $harness

    $result.ExitCode | Should Be 129
    $result.Output | Should Match 'ROLLBACK_PREVIOUS'
    $result.Output | Should Not Match 'STOP_FIRST_DEPLOY'
  }

  It 'stops an uncommitted app when an interrupted first deploy has no previous release' -Skip:(-not $bashPath) {
    $harness = @"
set -eu
candidate_attempted=0
deploy_committed=0
cleanup_running=0
previous=
new_release=/releases/new
restore_previous_release() { echo UNEXPECTED_ROLLBACK; }
stop_uncommitted_first_deploy() { echo STOP_FIRST_DEPLOY; }
compose() { :; }
set_current() { :; }
$transactionGuards
trap 'transaction_exit "`$?"' EXIT
trap 'exit 143' TERM
candidate_attempted=1
kill -TERM "`$`$"
"@
    $result = Invoke-TransactionHarness -Body $harness

    $result.ExitCode | Should Be 143
    $result.Output | Should Match 'STOP_FIRST_DEPLOY'
    $result.Output | Should Not Match 'UNEXPECTED_ROLLBACK'
  }

  It 'disarms rollback after a successful candidate commit' -Skip:(-not $bashPath) {
    $harness = @"
set -eu
candidate_attempted=0
deploy_committed=0
cleanup_running=0
previous=/releases/old
new_release=/releases/new
restore_previous_release() { echo UNEXPECTED_ROLLBACK; }
stop_uncommitted_first_deploy() { echo UNEXPECTED_STOP; }
compose() { :; }
set_current() { echo COMMITTED; }
$transactionGuards
trap 'transaction_exit "`$?"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
candidate_attempted=1
commit_candidate
exit 0
"@
    $result = Invoke-TransactionHarness -Body $harness

    $result.ExitCode | Should Be 0
    $result.Output | Should Match 'COMMITTED'
    $result.Output | Should Not Match 'UNEXPECTED_ROLLBACK|UNEXPECTED_STOP'
  }
}
