#!/bin/bash
# Generates a release by creating and pushing a version tag from main or a release-* branch.
# This triggers the create-release and ci-release workflows.
#
# Usage: ./scripts/generate-release.sh <version> [branch]
#   branch defaults to main; must be main or release-* (e.g. release-v0.1.0 for hotfixes).
#
# Example: ./scripts/generate-release.sh 1.0.0
#          ./scripts/generate-release.sh 0.1.1 release-v0.1.0
#          ./scripts/generate-release.sh v1.0.0
#          ./scripts/generate-release.sh v0.0.0-test              # test tag from main
#          ./scripts/generate-release.sh v0.0.0-test release-v0.0.1   # test tag from a release branch
#
# The version must match v*.*.* (e.g. v1.0.0) or v*.*.*-suffix (e.g. v0.0.0-test). The 'v' prefix is added if omitted.

set -euo pipefail

usage() {
    echo "Usage: $0 <version> [branch]"
    echo ""
    echo "Creates and pushes a release tag from the given branch (default: main)."
    echo "Branch must be main or release-*. Triggers create-release and ci-release workflows."
    echo ""
    echo "Examples:"
    echo "  $0 1.0.0                    # tags v1.0.0 from main"
    echo "  $0 0.1.1 release-v0.1.0     # hotfix tag from release branch"
    echo "  $0 v1.0.0                   # tags as v1.0.0 from main"
    echo "  $0 v0.0.0-test              # test tag from main"
    echo "  $0 v0.0.0-test release-v0.0.1   # test tag from a release branch"
    echo ""
    echo "The tagged commit must already be on the branch you select (merge or push there first)."
    exit 1
}

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
    usage
fi

VERSION="$1"
BRANCH="${2:-main}"

if [[ "$BRANCH" != "main" && ! "$BRANCH" =~ ^release- ]]; then
    echo "Error: branch must be 'main' or match 'release-*' (got: ${BRANCH})" >&2
    exit 1
fi
# Add 'v' prefix if not present
if [[ ! "$VERSION" =~ ^v ]]; then
    VERSION="v${VERSION}"
fi

# Validate semver-like format (v*.*.* or v*.*.*-suffix for test tags)
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
    echo "Error: version must match v*.*.* (e.g. v1.0.0) or v*.*.*-suffix (e.g. v0.0.0-test)" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

echo "Generating release ${VERSION} from branch ${BRANCH}..."
echo "  - Checking out ${BRANCH} and pulling latest"
git checkout "${BRANCH}"
git pull origin "${BRANCH}"

echo "  - Creating tag ${VERSION}"
git tag "${VERSION}"

echo "  - Pushing tag ${VERSION} to origin"
git push origin "${VERSION}"

echo ""
echo "Release tag ${VERSION} pushed. Workflows (create-release, ci-release) will run automatically."
echo "Monitor progress in the Actions tab of your repository."
