<#
.SYNOPSIS
  Windows entry point for scripts/update-test-report.sh.

.DESCRIPTION
  The actual logic lives in update-test-report.sh and runs inside an
  alpine + bash + git + docker-cli container, so it's identical
  across hosts. This wrapper exists because PowerShell on Windows
  routes `bash` to WSL, which on a default Docker Desktop install
  has no bash available — so we bypass the host shell entirely and
  jump straight into Docker.

  On Linux / macOS / CI use the .sh directly; it self-execs into the
  same container.

.NOTES
  E2E_BRIDGE_FLOOD_FIX env var (default 1) is forwarded to the harness.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$floodFix = if ($env:E2E_BRIDGE_FLOOD_FIX) { $env:E2E_BRIDGE_FLOOD_FIX } else { '1' }

& docker run --rm `
    -v "//var/run/docker.sock:/var/run/docker.sock" `
    -v "${repoRoot}:/repo" `
    -w /repo `
    -e IN_DOCKER_RUNNER=1 `
    -e "HOST_REPO_PATH=${repoRoot}" `
    -e "E2E_BRIDGE_FLOOD_FIX=${floodFix}" `
    alpine:3.19 `
    sh -c "apk add -q --no-cache bash git docker-cli docker-cli-compose >/dev/null && exec bash scripts/update-test-report.sh"
exit $LASTEXITCODE
