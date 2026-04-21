# Managing Releases

This guide describes how to create a new release of Batch Gateway and manage releases.

## Overview

- **Release workflow** (`.github/workflows/create-release.yml`): Runs when you push a tag matching `v*.*.*` (e.g. `v1.0.0`). It **only proceeds if that tag points at a commit on `main` or on a `release-vX.Y.Z` branch** (e.g. `release-v1.0.0`; reachable from `origin/main` or from such a branch on `origin`), then builds Linux binaries (amd64, arm64), packages them as `**.tar.gz**` (so execute permission survives browser download), writes `**SHA256SUMS**`, pins image tags in the Helm chart `values.yaml` to the release version, packages and publishes the Helm chart to the OCI registry (GHCR), creates a GitHub Release with notes generated automatically, uploads those assets, and marks the release as **Latest**.
- **Docker workflow** (`.github/workflows/ci-release.yaml`): Builds and pushes container images to GHCR. It runs on the following triggers:
  - **Push to `main`**: Images are tagged `latest` and with the commit SHA.
  - **Push of version tag** (`v*.*.*`): Images are tagged with the version (e.g. `v1.0.0`) and with the commit SHA.
- **Release notes config** (`.github/release.yml`): Defines how PRs are grouped in release notes generated automatically (e.g. Features, Bug fixes, Documentation).
- **Release template** (`.github/RELEASE_TEMPLATE.md`): Optional template you can copy into a release description (e.g. Docker image names, upgrade notes).

## Tagging policy (main or release-vX.Y.Z branches)

**The tagged commit must be reachable from `origin/main` or from an `origin/release-vX.Y.Z` branch** (branch name must match `release-v` plus the same semver-like pattern as a version tag). The workflow enforces this so release tags are not cut from arbitrary feature branches.

- **Normal releases:** merge to `main`, then tag from `main` (default), e.g. `./scripts/generate-release.sh 1.0.0` or `make generate-release REL_VERSION=1.0.0`.
- **Hotfixes on a release line:** use the corresponding `release-vX.Y.Z` branch (e.g. `release-v0.1.0`), merge or cherry-pick the fix there, then tag from that branch: `./scripts/generate-release.sh 0.1.1 release-v0.1.0` or `make generate-release REL_VERSION=0.1.1 REL_BRANCH=release-v0.1.0`.
- **Version must match the release line:** when tagging from a `release-vX.Y.Z` branch, the version tag must share the same major and minor (`vX.Y.*`). For example, `release-v0.1.0` only accepts tags like `v0.1.1` or `v0.1.2-rc1`; a tag like `v0.3.0` will be rejected. This is enforced both by `generate-release.sh` locally and by the `create-release.yml` workflow in CI.
- **Don't:** push a release tag that points only at a commit that is not on `main` and not on any allowed `release-vX.Y.Z` branch the workflow can see.

Pushing `v*.*.*` **always** triggers the workflow if the check passes.

## Creating a release

1. **Ensure the target branch is in a good state**
  For tags from `main`, CI and tests should be passing on `main`. For hotfixes, validate the relevant `release-vX.Y.Z` branch.
2. **Create and push a version tag** (semantic version with optional `v` prefix; script normalizes to `v*.*.*`):

   From **main** (default):

   ```bash
   ./scripts/generate-release.sh 1.0.0
   ```

   From a **release branch** (e.g. patch after `v0.1.0`):

   ```bash
   ./scripts/generate-release.sh 0.1.1 release-v0.1.0
   ```

   Or using the Makefile (for admins):

  ```bash
  make generate-release REL_VERSION=1.0.0
  make generate-release REL_VERSION=0.1.1 REL_BRANCH=release-v0.1.0
  ```

3. **Let automation run**
  - **create-release.yml**: Packages binaries as `.tar.gz`, pins Helm chart image tags to the release version, publishes the Helm chart to `oci://ghcr.io/llm-d-incubation/charts/batch-gateway`, creates the GitHub Release with generated notes, attaches binaries, chart `.tgz`, and `SHA256SUMS`.
  - **ci-release.yaml**: Builds and pushes images for that tag to GHCR.
4. **Optional: edit the release**
  - In GitHub: **Releases** → open the new release → **Edit**.
  - You can paste content from `.github/RELEASE_TEMPLATE.md` (Docker image section, upgrade notes) and adjust the generated notes if needed.

## Release notes

Release notes are generated from merged PRs and grouped by labels. See `.github/release.yml` for exclusions and categories. Assign appropriate labels to PRs so they appear in the correct section.

## Verifying checksums

Each release includes `**SHA256SUMS`** for every binary `**.tar.gz**` and the Helm chart `**.tgz**`. After downloading into one directory:

```bash
sha256sum -c SHA256SUMS
```

Extract a binary (execute bit preserved):

```bash
tar xzf batch-gateway-apiserver-linux-amd64.tar.gz
```

## Release template

`.github/RELEASE_TEMPLATE.md` is for human use when drafting or editing a release. It reminds you to mention:

- Docker image names and tag
- Helm chart OCI URL and install command
- Upgrade or migration notes
- That Linux binaries are attached as `.tar.gz`, the Helm chart as `.tgz`, with `SHA256SUMS` covering those files

The workflow does **not** automatically inject this file into the release body; it only uses GitHub's generated notes. Paste the template content manually if you want it in the description.

## Testing the release workflow

To verify the release workflow without affecting a real version, use the same `generate-release` script with a test tag (e.g. `v0.0.0-test`):

1. **Create a test tag** on `main` or a `release-vX.Y.Z` branch (the same reachability rules as a real release apply):
  ```bash
   ./scripts/generate-release.sh v0.0.0-test
   # or from a release branch:
   ./scripts/generate-release.sh v0.0.0-test release-v0.0.1
  ```

   Equivalent with Make: `make generate-release REL_VERSION=v0.0.0-test REL_BRANCH=release-v0.0.1`

2. **Check that workflows run** in the **Actions** tab: **Release** and **CI Release** should run for that tag. When they finish, a new release and new image tags will exist.
3. **Important:** Running a failed workflow again uses the workflow file from the original trigger commit. To run with updated workflow code (e.g. after fixing ci-release.yaml), push the fix and then push the tag again from the new commit so a fresh run is triggered.
4. **Clean up when done** — see [Manually deleting a release](#manually-deleting-a-release).

## Manually deleting a release

To remove a release and its tag (e.g. after a test release):

1. **Delete the GitHub Release first**
  - On GitHub: **Releases** → open the release → **Delete this release**
  - Or with [GitHub CLI](https://cli.github.com/): `gh release delete <tag> --yes`
2. **Delete the tag** locally and remotely:
  ```bash
   git tag -d <tag>
   git push origin --delete <tag>
  ```
3. **Docker images and Helm charts** already pushed to GHCR for that tag are **not** removed. Delete them in the **Packages** area of the repo if needed.
