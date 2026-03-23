<!--
  Optional template for release descriptions.
  The Release workflow uses auto-generated notes from .github/release.yml.
  You can paste the sections below when drafting or editing a release, or
  use them as a checklist for release communication.
-->

## Docker images

Images are published to GitHub Container Registry. For this release, use the tag `$TAG` (e.g. `v1.0.0`).

## Upgrade notes

_Add any migration or upgrade instructions here._

## Binaries

Pre-built binaries for Linux (amd64, arm64) are attached as **`.tar.gz`** archives (preserves execute bit on extract). **`SHA256SUMS`** lists digests for those archives — verify with `sha256sum -c SHA256SUMS`, then e.g. `tar xzf batch-gateway-apiserver-linux-amd64.tar.gz`.
