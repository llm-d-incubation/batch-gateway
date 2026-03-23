#!/bin/bash
# Generates a release by creating and pushing a version tag from main.
# This triggers the create-release and ci-release workflows.
#
# Usage: ./scripts/generate-release.sh <version>
# Example: ./scripts/generate-release.sh 1.0.0
#          ./scripts/generate-release.sh v1.0.0
#          ./scripts/generate-release.sh v0.0.0-test   # for testing the workflow
#
# The version must match v*.*.* (e.g. v1.0.0) or v*.*.*-suffix (e.g. v0.0.0-test). The 'v' prefix is added if omitted.

set -euo pipefail

usage() {
    echo "Usage: $0 <version>"
    echo ""
    echo "Creates and pushes a release tag from main. Triggers create-release and ci-release workflows."
    echo ""
    echo "Examples:"
    echo "  $0 1.0.0         # tags as v1.0.0"
    echo "  $0 v1.0.0        # tags as v1.0.0"
    echo "  $0 v0.0.0-test   # tags as v0.0.0-test (for testing the release workflow)"
    echo ""
    echo "The tagged commit must already be on main (merge your changes first)."
    exit 1
}

if [ $# -lt 1 ]; then
    usage
fi

VERSION="$1"
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

echo "Generating release ${VERSION}..."
echo "  - Checking out main and pulling latest"
git checkout main
git pull origin main

echo "  - Creating tag ${VERSION}"
git tag "${VERSION}"

echo "  - Pushing tag ${VERSION} to origin"
git push origin "${VERSION}"

echo ""
echo "Release tag ${VERSION} pushed. Workflows (create-release, ci-release) will run automatically."
echo "Monitor progress in the Actions tab of your repository."
