<!--
  NOT used by any workflow. The Release workflow (create-release.yml) generates
  release notes automatically from merged PR labels via .github/release.yml.

  This file is a manual reference only. If you want the boilerplate below
  (Docker image names, Helm install command, binary instructions) to appear
  in the release description, paste it manually when editing the release on
  GitHub, above the auto-generated changelog.
-->

## Docker images

Images are published to GitHub Container Registry. For this release, use the tag `$TAG` (e.g. `v1.0.0`).

## Helm chart (OCI)

The Helm chart is published to GitHub Container Registry for each release. Install using the chart semver from `Chart.yaml` (no `v` prefix — for tag `$TAG` such as `v1.0.0`, use `--version 1.0.0`):

```bash
helm install batch-gateway oci://ghcr.io/llm-d/charts/batch-gateway --version "${TAG#v}"
```

## Upgrade notes

_Add any migration or upgrade instructions here._

## Binaries

Pre-built binaries for Linux (amd64, arm64) are attached as **`.tar.gz`** archives (preserves execute bit on extract). The Helm chart is attached as **`batch-gateway-<semver>.tgz`**. **`SHA256SUMS`** lists digests for those archives and the chart — verify with `sha256sum -c SHA256SUMS`, then e.g. `tar xzf batch-gateway-apiserver-linux-amd64.tar.gz`.
