<#
.SYNOPSIS
  Host-side launcher (Windows PowerShell) for the anchord e2e harness.

.DESCRIPTION
  Builds the e2e runner image, then execs it against the host's Docker
  daemon with /var/run/docker.sock + repo bind-mounted. Forwards
  arguments to test/e2e/run.sh inside the runner — i.e., all assertion
  logic happens inside Docker so the harness behaves identically on
  Linux, macOS and Windows.

.EXAMPLE
  ./test/e2e/run.ps1                 # all four scenarios
  ./test/e2e/run.ps1 v4-only         # just one
  $env:NO_TEARDOWN='1'; ./test/e2e/run.ps1   # leave stack up after tests

.NOTES
  Equivalent of test/e2e/up.sh. Both invoke the same containerized
  bash test runner — pick whichever shell is convenient.
#>

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Scenarios = @()
)

$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$repoRoot = Resolve-Path (Join-Path $here '../..')

Write-Host '[run.ps1] building anchord-e2e-runner image' -ForegroundColor Cyan
& docker build -q -t anchord-e2e-runner (Join-Path $here 'images/runner') | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'runner image build failed' }

Write-Host '[run.ps1] starting runner' -ForegroundColor Cyan
$dockerArgs = @(
    'run', '--rm',
    '-v', '/var/run/docker.sock:/var/run/docker.sock',
    '-v', "${repoRoot}:/repo",
    '-e', 'REPO_ROOT=/repo',
    '-e', "WAIT_SECONDS=$($env:WAIT_SECONDS ?? '15')",
    '-e', "NO_TEARDOWN=$($env:NO_TEARDOWN ?? '0')",
    'anchord-e2e-runner',
    'test/e2e/run.sh'
) + $Scenarios

& docker @dockerArgs
exit $LASTEXITCODE
