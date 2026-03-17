# Releasing

This guide describes how to create a new release of Batch Gateway using the release workflow and release notes configuration.

## Overview

- **Release workflow** (`.github/workflows/release.yml`): Runs when you push a tag matching `v*.*.*` (e.g. `v1.0.0`). It builds Linux binaries (amd64, arm64), creates a GitHub Release with auto-generated notes, and uploads the binaries as assets.
- **Docker workflow** (`.github/workflows/docker.yml`): Also runs on the same tag and builds/pushes container images to GHCR. No extra step needed.
- **Release notes config** (`.github/release.yml`): Defines how PRs are grouped in auto-generated release notes (e.g. Features, Bug fixes, Documentation).
- **Release template** (`.github/RELEASE_TEMPLATE.md`): Optional template you can copy into a release description (e.g. Docker image names, upgrade notes).

## How to cut a release

1. **Ensure `main` is in a good state**
   CI and tests should be passing.

2. **Create and push a version tag** (semantic version with `v` prefix):

   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

3. **Let automation run**
   - **release.yml**: Builds binaries, creates the GitHub Release with generated notes, attaches the binaries.
   - **docker.yml**: Builds and pushes `batch-gateway-apiserver` and `batch-gateway-processor` images for that tag to `ghcr.io/llm-d-incubation/...`.

4. **Optional: edit the release**
   - In GitHub: **Releases** → open the new release → **Edit**.
   - You can paste content from `.github/RELEASE_TEMPLATE.md` (Docker image section, upgrade notes) and adjust the generated notes if needed.

## Release notes (auto-generated)

Release notes are generated from merged PRs and grouped by labels. Configuration is in `.github/release.yml`:

- **Excluded**: PRs with labels `release-note-none` or `skip-changelog`, and PRs by `dependabot` / `dependabot[bot]`.
- **Categories**: Breaking changes, Features, Bug fixes, Documentation, Dependencies, Other changes. Assign the right labels to PRs so they appear in the correct section.

To get consistent notes, label PRs with at least one of: `enhancement`, `feature`, `bug`, `bugfix`, `documentation`, `docs`, `dependencies`, or use `*` (Other changes) by default.

## Release template

`.github/RELEASE_TEMPLATE.md` is for human use when drafting or editing a release. It reminds you to mention:

- Docker image names and tag
- Upgrade or migration notes
- That Linux binaries are attached

The workflow does **not** automatically inject this file into the release body; it only uses GitHub’s auto-generated notes. Paste the template content manually if you want it in the description.

## Testing the release workflow

To verify the release workflow without affecting a real version:

1. **Use a test tag** that matches `v*.*.*` but is clearly not a real release, for example:
   ```bash
   git tag v0.0.0-test
   git push origin v0.0.0-test
   ```
   Or use something like `v99.99.99` if you prefer.

2. **Check that workflows run** in the **Actions** tab: **Release** and **Docker Build and Push** should run for that tag. When they finish, a new release and new image tags will exist.

3. **Important:** Re-running a failed workflow uses the workflow file from the **original trigger commit**. To run with updated workflow code (e.g. after fixing docker.yml), you must push the fix and then **re-push the tag** from the new commit so a fresh run is triggered.

4. **Clean up when done.** You can delete the tag and the release:

   - **Delete the GitHub Release** (required before deleting the tag if the release exists):
     - In the repo: **Releases** → open the test release → **Delete this release**.
     - Or with GitHub CLI: `gh release delete v0.0.0-test --yes`
   - **Delete the tag** (local and remote):
     ```bash
     git tag -d v0.0.0-test
     git push origin --delete v0.0.0-test
     ```

   **Note:** Deleting the release and tag does **not** remove Docker images already pushed to GHCR for that tag. You can delete those in the **Packages** area of the repo (or leave them; they are just another tag in the package).
