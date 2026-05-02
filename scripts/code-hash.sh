#!/usr/bin/env bash
# Deterministic SHA-256 over the files that define what anchord ships
# AND how it gets verified:
#
#   - every *.go in the repo (the code)
#   - go.mod / go.sum (dependency pin)
#   - Dockerfile (runtime build recipe)
#   - test/  (the e2e + integration harnesses — define "passes")
#   - scripts/ (this directory — defines the gate itself, included so
#     the gate cannot weaken itself silently)
#
# Excluded by intent:
#   - *.md (README, SPEC, ARCHITECTURE, CONTEXT, CLAUDE.md)
#   - LICENSE, .gitignore
#   - compose.example.yaml (illustration for users, not the SUT)
#   - .github/ (CI plumbing — ortogonal to local verifiability)
#
# Stable across re-runs as long as no in-scope file changes.
# Format: "sha256:<64-hex>". Same shape as Docker image digests so it
# reads naturally in logs and commit messages.
set -euo pipefail
cd "$(dirname "$0")/.."

{
    git ls-files '*.go'
    git ls-files test
    git ls-files scripts
    echo go.mod
    echo go.sum
    echo Dockerfile
} | LC_ALL=C sort -u \
  | xargs sha256sum \
  | sha256sum \
  | awk '{print "sha256:" $1}'
