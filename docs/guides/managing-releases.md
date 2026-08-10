# Managing Releases

This guide describes how to create a new release of Batch Gateway and manage releases.

## Overview

- **Release workflow** (`.github/workflows/create-release.yml`): Runs when a tag matching `v*.*.*` is pushed. It **only proceeds if that tag points at a commit on `main` or on a `release-vX.Y.Z` branch** (reachable from `origin/main` or from such a branch on `origin`), then runs these steps in order:
  1. **Build binaries** (parallel with images): Linux amd64 and arm64, packaged as **`.tar.gz`** with **`SHA256SUMS`**.
  2. **Build and push Docker images** (parallel with binaries): all three images tagged with the version (e.g. `v0.4.0`), pushed to GHCR.
  3. **Publish Helm chart**: pins image tags in `values.yaml` to the release version, packages the chart, publishes it to `oci://ghcr.io/llm-d/charts/batch-gateway`, appends chart digest to `SHA256SUMS`.
  4. **Create GitHub Release**: generated release notes, all `.tar.gz` and `.tgz` assets, `SHA256SUMS`.
  Docker images are guaranteed to exist before the Helm chart is published.
  Tags with a `-` (e.g. `v0.3.0-RC1`) are marked **Pre-release**; tags without one (e.g. `v0.3.0`) are full releases marked **Latest**.
- **CI Build workflow** (`.github/workflows/ci-build.yaml`): Builds and pushes container images on every merge to `main`. Images are tagged `latest` and with the commit SHA. Does **not** run on version tags — those are handled entirely by `create-release.yml`.
- **Release notes config** (`.github/release.yml`): Defines how PRs are grouped in auto-generated release notes (e.g. Features, Bug fixes, Documentation).
- **Release template** (`.github/RELEASE_TEMPLATE.md`): **Not used by any workflow.** Manual reference only — paste it into the release description on GitHub if you want the boilerplate (Docker image names, Helm install command, binary instructions) to appear above the auto-generated changelog.

## Creating a release

1. **Ensure the target commit is in a good state** — CI and tests should be passing on the commit you want to release from.

2. **Find the commit SHA** you want to release from — no local clone needed:
   ```bash
   # tip of main
   gh api repos/{owner}/{repo}/commits/main --jq '.sha'
   # tip of any branch
   gh api repos/{owner}/{repo}/commits/<branch> --jq '.sha'
   # merge commit of a specific PR
   gh pr view <number> --json mergeCommit --jq '.mergeCommit.oid'
   ```

3. **Create the release branch and tag** via the GitHub API (no local git changes):
   ```bash
   ./scripts/generate-release.sh <commit-sha> 0.4.0
   ```
   Or using the Makefile:
   ```bash
   make generate-release REL_SHA=<commit-sha> REL_VERSION=0.4.0
   ```
   This creates `release-v0.4.0` branch and `v0.4.0` tag on that commit directly on the remote.

4. **Let automation run** — the Release workflow runs automatically on the tag push:
   - Binaries and Docker images build in parallel.
   - The Helm chart is published after images are confirmed pushed.
   - The GitHub Release is created with notes, assets, and `SHA256SUMS`.

5. **Optional: edit the release** — GitHub **Releases** → open the new release → **Edit**. Paste content from `.github/RELEASE_TEMPLATE.md` (Docker image section, Helm install command, upgrade notes) and adjust generated notes if needed.

## Tagging policy

**The tagged commit must be reachable from `origin/main` or from an `origin/release-vX.Y.Z` branch.** The workflow enforces this.

The script handles two cases:

- **Pre-release** (e.g. `v0.4.0-rc1`, `v0.4.0-alpha.1`): tag only, no branch created. The commit is expected to be on `main` — the verify check passes because it is reachable from `origin/main`.
- **Final release** (e.g. `v0.4.0`): creates `release-v0.4.0` from the given commit and tags it. The verify check passes because the commit is the tip of the release branch.

## Release notes

Release notes are generated from merged PRs grouped by labels. See `.github/release.yml` for exclusions and categories. Assign appropriate labels to PRs so they appear in the correct section.

## Verifying checksums

Each release includes `SHA256SUMS` for every binary `.tar.gz` and the Helm chart `.tgz`. After downloading into one directory:

```bash
sha256sum -c SHA256SUMS
```

Extract a binary (execute bit preserved):

```bash
tar xzf batch-gateway-apiserver-linux-amd64.tar.gz
```

## Testing the release workflow

To verify the release workflow without affecting a real version, use a test tag (e.g. `v0.0.0-test`):

1. **Create a test tag** from any commit on `main` or a `release-vX.Y.Z` branch:
   ```bash
   ./scripts/generate-release.sh $(git rev-parse HEAD) v0.0.0-test
   # or via Make:
   make generate-release REL_SHA=$(git rev-parse HEAD) REL_VERSION=v0.0.0-test
   ```

2. **Check that the workflow runs** in the **Actions** tab. When it finishes, a new release and new image tags will exist.

3. **Important:** Running a failed workflow again uses the workflow file from the original trigger commit. To run with updated workflow code, push the fix and then re-create the tag from the new commit so a fresh run is triggered.

4. **Clean up when done** — see [Manually deleting a release](#manually-deleting-a-release).

## Manually deleting a release

To remove a release and its tag (e.g. after a test release):

1. **Delete the GitHub Release first**
   - On GitHub: **Releases** → open the release → **Delete this release**
   - Or with [GitHub CLI](https://cli.github.com/): `gh release delete <tag> --yes`
2. **Delete the tag** via the GitHub API:
   ```bash
   gh api repos/{owner}/{repo}/git/refs/tags/<tag> --method DELETE
   ```
3. **Delete the release branch** (final releases only) if no longer needed:
   ```bash
   gh api repos/{owner}/{repo}/git/refs/heads/release-<tag> --method DELETE
   ```
4. **Docker images and Helm charts** already pushed to GHCR for that tag are **not** removed. Delete them in the **Packages** area of the repo if needed.
