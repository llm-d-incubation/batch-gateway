#!/bin/bash
# Deletes a test release tag and its GitHub Release.
# Use after testing the release workflow.
#
# Usage: ./scripts/test/test-delete-release.sh [tag]
# Example: ./scripts/test/test-delete-release.sh           # deletes v0.0.0-test
#          ./scripts/test/test-delete-release.sh v0.0.0-test
#
# Note: This does NOT remove Docker images already pushed to GHCR for that tag.
# Delete those in the Packages area of the repo if needed.

set -euo pipefail

TAG="${1:-v0.0.0-test}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

echo "Deleting test release ${TAG}..."

# Delete GitHub Release first (required before deleting the tag)
if command -v gh &>/dev/null; then
    echo "  - Deleting GitHub Release: ${TAG}"
    gh release delete "${TAG}" --yes 2>/dev/null || echo "  - No release found or already deleted"
else
    echo "  - GitHub CLI (gh) not found. Delete the release manually:"
    echo "    Releases → open ${TAG} → Delete this release"
    echo "  - Or install gh: https://cli.github.com/"
    echo "  Then run: git tag -d ${TAG} && git push origin --delete ${TAG}"
    exit 1
fi

# Delete tag (local and remote)
echo "  - Deleting local tag: ${TAG}"
git tag -d "${TAG}" 2>/dev/null || true
echo "  - Deleting remote tag: ${TAG}"
git push origin --delete "${TAG}" 2>/dev/null || echo "  - Remote tag not found or already deleted"

echo ""
echo "Tag ${TAG} deleted. Docker images in GHCR for this tag are unchanged."
