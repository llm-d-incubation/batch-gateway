#!/usr/bin/env bash
# Patch the Helm chart for a git release tag, package it, append the chart digest
# to SHA256SUMS, and push the chart to GHCR as OCI.
#
# Required environment:
#   VERSION       Git tag (e.g. v1.0.0)
#   GITHUB_TOKEN  Token for helm registry login to ghcr.io
#   GITHUB_ACTOR  Username for registry login (e.g. github.actor in Actions)
#
# Chart is always pushed to oci://ghcr.io/llm-d-incubation/charts (not configurable).
#
# Requires: helm, yq (mikefarah). Run after make package-release so release/ exists.
set -euo pipefail

VERSION="${VERSION:?VERSION is required (e.g. v1.0.0)}"
CHART_VERSION="${VERSION#v}"
export VERSION
export CHART_VERSION

HELM_OCI_REGISTRY='oci://ghcr.io/llm-d-incubation/charts'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

command -v yq >/dev/null 2>&1 || {
  echo "yq is required (https://github.com/mikefarah/yq)" >&2
  exit 1
}
command -v helm >/dev/null 2>&1 || {
  echo "helm is required" >&2
  exit 1
}

yq -i '.apiserver.image.tag = strenv(VERSION) | .processor.image.tag = strenv(VERSION) | .gc.image.tag = strenv(VERSION)' charts/batch-gateway/values.yaml
yq -i '.version = strenv(CHART_VERSION) | .appVersion = strenv(CHART_VERSION)' charts/batch-gateway/Chart.yaml

helm package charts/batch-gateway -d release/

(cd release && sha256sum "batch-gateway-${CHART_VERSION}.tgz" >> SHA256SUMS && cat SHA256SUMS)

echo "${GITHUB_TOKEN}" | helm registry login ghcr.io -u "${GITHUB_ACTOR}" --password-stdin
helm push "release/batch-gateway-${CHART_VERSION}.tgz" "${HELM_OCI_REGISTRY}"

echo "Helm chart published: ${HELM_OCI_REGISTRY}/batch-gateway:${CHART_VERSION}"
