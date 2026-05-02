# anchord integration test launcher (Windows / PowerShell).
#
# Builds the runner image, then exec's `go test -tags=integration` against
# the real kernel netlink stack inside a privileged-enough container.
#
# Why a separate launcher per OS: docker-cli's bind-mount path syntax
# differs between PowerShell ($PWD), bash ($(pwd)), and Git-Bash (which
# mangles paths via MSYS conversion). One thin wrapper per shell keeps
# every layer below it identical.

$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path "$PSScriptRoot/../..").Path

Write-Host "[runner] building anchord-integration image"
docker build -q -t anchord-integration -f "$PSScriptRoot/Dockerfile" $PSScriptRoot | Out-Null
if ($LASTEXITCODE -ne 0) { throw "image build failed" }

Write-Host "[runner] running integration tests"
$dockerArgs = @(
    'run', '--rm',
    '--cap-add=NET_ADMIN',
    '-v', "${repoRoot}:/repo",
    '-v', 'anchord-go-cache:/go',
    'anchord-integration'
) + $args

& docker @dockerArgs
exit $LASTEXITCODE
