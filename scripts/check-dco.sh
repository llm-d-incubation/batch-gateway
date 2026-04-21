#!/usr/bin/env bash
# Check that all commits in a range have a Signed-off-by trailer (DCO).
# Usage:
#   scripts/check-dco.sh [base] [head]
#
# Defaults: base = merge-base of main and HEAD, head = HEAD
# In CI, pass the PR base and head SHAs explicitly.

set -euo pipefail

base="${1:-${DCO_BASE:-$(git merge-base main HEAD)}}"
head="${2:-${DCO_HEAD:-HEAD}}"

failed=0
for sha in $(git rev-list "$base".."$head"); do
  # Skip merge commits (structural, not contributions).
  if [ "$(git rev-list --parents -1 "$sha" | wc -w)" -gt 2 ]; then
    continue
  fi
  if ! git log -1 --format='%B' "$sha" | grep -qE '^Signed-off-by: .+ <.+>'; then
    echo "ERROR: Commit $sha is missing Signed-off-by. Use 'git commit -s' or see PR_SIGNOFF.md."
    failed=1
  fi
done

if [ "$failed" -eq 1 ]; then
  echo ""
  echo "One or more commits are missing the DCO sign-off trailer."
  echo "Add it with: git commit -s --amend (for the last commit)"
  echo "Or for multiple commits: git rebase HEAD~N --signoff"
  echo "See PR_SIGNOFF.md for details."
  exit 1
fi

echo "All commits have DCO sign-off."
