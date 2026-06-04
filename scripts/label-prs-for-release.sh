#!/usr/bin/env bash
# Bulk-label merged PRs since v0.1.0 based on conventional commit title prefixes.
# Maps: feat/feature → feature, refactor → enhancement, fix → bug,
#       docs/doc → documentation, chore/ci/test → release-note-none.
#
# Usage:
#   DRY_RUN=true  ./scripts/label-prs-for-release.sh   # preview only
#   DRY_RUN=false ./scripts/label-prs-for-release.sh   # apply labels
#
# Requires: gh CLI authenticated with write access to the upstream repo.

set -euo pipefail

REPO="llm-d-incubation/batch-gateway"
SINCE="2025-04-09"  # v0.1.0 release date
DRY_RUN="${DRY_RUN:-true}"

prs_json=$(gh pr list --repo "$REPO" --state merged --search "merged:>=$SINCE" \
  --limit 200 --json number,title,labels)

count=$(echo "$prs_json" | jq 'length')
echo "Found $count merged PRs since $SINCE"
echo "DRY_RUN=$DRY_RUN"
echo "---"

echo "$prs_json" | jq -c '.[]' | while IFS= read -r entry; do
  number=$(echo "$entry" | jq -r '.number')
  title=$(echo "$entry" | jq -r '.title')
  existing=$(echo "$entry" | jq -r '[.labels[].name] | join(",")')

  # Determine label from title prefix
  label=""
  lower_title=$(echo "$title" | tr '[:upper:]' '[:lower:]')
  case "$lower_title" in
    feat:*|feat\(*|feature:*|feature\(*)
      label="feature" ;;
    refactor:*|refactor\(*)
      label="enhancement" ;;
    fix:*|fix\(*)
      label="bug" ;;
    docs:*|docs\(*|doc:*|doc\(*)
      label="documentation" ;;
    deps:*|deps\(*)
      label="dependencies" ;;
    chore:*|chore\(*|ci:*|ci\(*)
      label="release-note-none" ;;
    test:*|test\(*)
      label="release-note-none" ;;
    *)
      echo "SKIP  #$number — no prefix match: $title"
      continue ;;
  esac

  # Skip if any release-relevant label is already present
  for existing_label in $(echo "$existing" | tr ',' '\n'); do
    case "$existing_label" in
      enhancement|feature|bug|bugfix|documentation|docs|dependencies|breaking-change|semver-major|semver-minor|semver-patch|skip-changelog|release-note-none)
        echo "OK    #$number — already has '$existing_label': $title"
        continue 2 ;;
    esac
  done

  if [ "$DRY_RUN" = "true" ]; then
    echo "WOULD #$number — add '$label': $title  (current: $existing)"
  else
    gh pr edit "$number" --repo "$REPO" --add-label "$label"
    echo "ADDED #$number — '$label': $title"
  fi
done

echo "---"
echo "Done. $([ "$DRY_RUN" = "true" ] && echo 'Re-run with DRY_RUN=false to apply.' || echo 'Labels applied.')"
