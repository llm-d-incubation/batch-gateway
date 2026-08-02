#!/bin/bash
# Creates a version tag on GitHub, and for final releases also creates a release branch.
# All operations use the gh CLI against the GitHub API — no local git changes are made.
# Always targets llm-d/llm-d-batch-gateway regardless of local remotes.
#
# Usage: ./scripts/generate-release.sh <commit-sha> <version>
#   commit-sha  Commit SHA (full or abbreviated) to release from.
#               Obtain from GitHub (UI, gh CLI) — no local clone needed:
#                 gh api repos/{owner}/{repo}/commits/main --jq '.sha'
#                 gh api repos/{owner}/{repo}/commits/<branch> --jq '.sha'
#                 gh pr view <number> --json mergeCommit --jq '.mergeCommit.oid'
#   version     Semver version: vX.Y.Z for a final release, or vX.Y.Z-<pre-release>
#               for a pre-release (e.g. v0.4.0-rc1, v1.0.0-alpha.1).
#               The 'v' prefix is added automatically if omitted.
#
# Behavior:
#   Final release (e.g. v0.1.9):
#     - Creates branch release-v0.1.9 from <commit-sha>
#     - Creates tag v0.1.9 on <commit-sha>
#   Pre-release (e.g. v0.1.9-rc1):
#     - Creates tag v0.1.9-rc1 on <commit-sha> (expected to be on main)
#     - No branch is created
#
# Pushing the tag triggers the Release workflow (create-release.yml).
#
# Examples:
#   ./scripts/generate-release.sh abc1234 0.4.0       # final release
#   ./scripts/generate-release.sh abc1234 v0.4.0-rc1  # pre-release

set -euo pipefail

usage() {
    echo "Usage: $0 <commit-sha> <version>"
    echo ""
    echo "Creates a version tag on GitHub. For final releases (vX.Y.Z), also creates"
    echo "a release-vX.Y.Z branch. Pre-releases (vX.Y.Z-<suffix>) only create a tag."
    echo ""
    echo "Arguments:"
    echo "  commit-sha  Commit SHA (full or abbreviated) to release from."
    echo "              Get it from GitHub without a local clone:"
    echo "                gh api repos/{owner}/{repo}/commits/main --jq '.sha'"
    echo "                gh pr view <number> --json mergeCommit --jq '.mergeCommit.oid'"
    echo "  version     vX.Y.Z (final) or vX.Y.Z-<pre-release> (e.g. v0.4.0-rc1, v1.0.0-alpha.1)."
    echo "              The 'v' prefix is added automatically if omitted."
    echo ""
    echo "Examples:"
    echo "  $0 abc1234 0.4.0        # final: creates release-v0.4.0 branch + tag"
    echo "  $0 abc1234 v0.4.0-rc1   # pre-release: tag only, no branch"
    exit 1
}

if [ $# -ne 2 ]; then
    usage
fi

COMMIT_SHA="$1"
VERSION="$2"

# Validate commit SHA (7-40 hex chars, either case)
if [[ ! "$COMMIT_SHA" =~ ^[0-9a-fA-F]{7,40}$ ]]; then
    echo "Error: commit-sha must be a hex SHA of 7-40 characters (got: ${COMMIT_SHA})" >&2
    exit 1
fi

# Normalize version (add v prefix if missing)
if [[ ! "$VERSION" =~ ^v ]]; then
    VERSION="v${VERSION}"
fi

# Validate semver-like format (vX.Y.Z or vX.Y.Z-<pre-release> e.g. v0.4.0-rc1, v1.0.0-alpha.1)
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
    echo "Error: version must be vX.Y.Z or vX.Y.Z-<pre-release> (e.g. v0.4.0, v0.4.0-rc1, v1.0.0-alpha.1) (got: ${VERSION})" >&2
    exit 1
fi

# Determine if this is a final release or a pre-release
IS_PRERELEASE=false
if [[ "$VERSION" == *-* ]]; then
    IS_PRERELEASE=true
fi

REPO="llm-d/llm-d-batch-gateway"

command -v gh >/dev/null 2>&1 || {
    echo "Error: gh CLI is required (https://cli.github.com/)" >&2
    exit 1
}
gh auth status >/dev/null 2>&1 || {
    echo "Error: gh CLI is not authenticated. Run: gh auth login" >&2
    exit 1
}

echo "Repository : ${REPO}"
echo "Commit SHA : ${COMMIT_SHA}"
echo "Tag        : ${VERSION}"
if [ "$IS_PRERELEASE" = true ]; then
    echo "Type       : pre-release (tag only, no branch)"
else
    echo "Type       : final release (branch + tag)"
    echo "Branch     : release-${VERSION}"
fi
echo ""

echo "Verifying commit exists..."
# Resolve to the full 40-char SHA: the create-ref API operates on raw objects
# and rejects abbreviated SHAs with a 422, whereas commits/{sha} resolves ref-ish values.
RESOLVED_SHA="$(gh api "repos/${REPO}/commits/${COMMIT_SHA}" --jq '.sha' 2>/dev/null)" || {
    echo "Error: commit ${COMMIT_SHA} not found in ${REPO}" >&2
    exit 1
}
echo "Commit verified (${RESOLVED_SHA})."
echo ""

# create_ref idempotently points a ref at RESOLVED_SHA. If the ref already
# exists at the same SHA it is left as-is, so a re-run after a partial failure
# (e.g. the branch was created but the tag POST failed) is safe. If it exists
# at a different SHA it is an error rather than a silent move.
create_ref() {
    local ref="$1"  # e.g. refs/heads/release-v1.2.3
    local existing
    if existing="$(gh api "repos/${REPO}/git/ref/${ref#refs/}" --jq '.object.sha' 2>/dev/null)"; then
        if [ "$existing" = "$RESOLVED_SHA" ]; then
            echo "  ${ref} already at ${RESOLVED_SHA}, skipping."
            return 0
        fi
        echo "Error: ${ref} already exists at ${existing} (want ${RESOLVED_SHA}); delete it or re-run with the matching SHA" >&2
        return 1
    fi
    gh api "repos/${REPO}/git/refs" \
        --method POST \
        -f "ref=${ref}" \
        -f "sha=${RESOLVED_SHA}"
}

if [ "$IS_PRERELEASE" = false ]; then
    echo "Creating branch release-${VERSION}..."
    create_ref "refs/heads/release-${VERSION}"
    echo "Branch release-${VERSION} ready."
fi

echo "Creating tag ${VERSION}..."
create_ref "refs/tags/${VERSION}"
echo "Tag ${VERSION} ready."

echo ""
echo "Done. The Release workflow will run automatically."
echo "Monitor progress at: https://github.com/${REPO}/actions"
