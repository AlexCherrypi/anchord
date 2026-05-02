#!/usr/bin/env bash
# Pipeline gate: compares the code hash recorded in README.md's
# auto-generated TEST-REPORT block against the current source tree.
# Exits 0 if they match (i.e. README reflects a green run for the
# code being released), non-zero otherwise.
#
# Used by .github/workflows/release-gate.yml on tag push. Can also be
# run locally before tagging:
#
#   scripts/verify-test-report.sh
#
# To regenerate the report after code changes:
#
#   scripts/update-test-report.sh
set -euo pipefail
cd "$(dirname "$0")/.."

current=$(scripts/code-hash.sh)
# Extract "sha256:<hex>" from the auto-generated TEST-REPORT block.
# The hash is rendered as `- **Code hash:** \`sha256:...\`` in the
# tabular markdown report; we tolerate optional formatting
# decoration around it.
recorded=$(grep -oE 'sha256:[0-9a-f]{64}' README.md 2>/dev/null | head -n1 || true)

if [ "$current" = "$recorded" ]; then
    echo "Test report is current ($current)"
    exit 0
fi

cat >&2 <<MSG
Test report is stale.
  current source hash:        $current
  README-recorded hash:       ${recorded:-<none>}

The recorded hash represents the source tree that last passed the
full test suite. To regenerate:

  scripts/update-test-report.sh
  git add README.md && git commit

If you arrived here from CI: the tag you pushed predates a green run.
Re-run the test suite locally, commit the updated README, and re-tag.
MSG
exit 1
