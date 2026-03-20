#!/bin/bash
# Creates a test release tag to verify the release workflow.
# Uses the same flow as a real release; the main-only check still applies.
#
# Usage: ./scripts/test/test-generate-release.sh [tag]
# Example: ./scripts/test/test-generate-release.sh           # uses v0.0.0-test
#          ./scripts/test/test-generate-release.sh v0.0.1-test
#
# The tag must match v*.*.* to trigger the workflows.

set -euo pipefail

TAG="${1:-v0.0.0-test}"

# Add 'v' prefix if not present
if [[ ! "$TAG" =~ ^v ]]; then
    TAG="v${TAG}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

echo "Creating test release tag ${TAG}..."
"${REPO_ROOT}/scripts/generate-release.sh" "${TAG}"

echo ""
echo "Test tag ${TAG} pushed. Check the Actions tab for Release and CI Release workflows."
echo "When done, clean up with: ./scripts/test/test-delete-release.sh ${TAG}"
