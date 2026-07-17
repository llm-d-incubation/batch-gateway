#!/usr/bin/env bash
set -uo pipefail

# Benchmark environment teardown.
# Removes all resources for a single scenario namespace.
#
# Usage:
#   KUBE_CONTEXT=my-ctx SCENARIO=2 ./benchmarks/teardown.sh
#   KUBE_CONTEXT=my-ctx SCENARIO=all ./benchmarks/teardown.sh
#
# Required env vars:
#   KUBE_CONTEXT       — kubectl context
#   SCENARIO           — scenario number (0-6), or "all" to tear down all namespaces
#
# Optional:
#   NAMESPACE          — override auto-generated namespace
#   GUIDE_NAME         — inference pool name (default: optimized-baseline)
#   KEEP_NAMESPACE     — if set, don't delete the namespace (preserves RBAC on GPU clusters)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GUIDE_NAME="${GUIDE_NAME:-optimized-baseline}"

if [ -z "${KUBE_CONTEXT:-}" ]; then
    echo "ERROR: KUBE_CONTEXT is not set" >&2
    exit 1
fi

K="kubectl --context=${KUBE_CONTEXT}"
H="helm --kube-context=${KUBE_CONTEXT}"

log() { echo "[$(date +%H:%M:%S)] $*"; }

teardown_namespace() {
    local NS="$1"

    if ! ${K} get ns "${NS}" >/dev/null 2>&1; then
        log "  ${NS}: namespace does not exist, skipping"
        return
    fi

    log "--- Tearing down ${NS} ---"

    # Delete benchmark jobs and configmaps
    ${K} -n "${NS}" delete job --all --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete configmap -l batch-benchmark=true --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete pod -l batch-benchmark=true --ignore-not-found 2>/dev/null || true

    # Helm releases
    for release in batch-gateway async-processor "${GUIDE_NAME}" epp-bench redis postgresql; do
        ${H} uninstall "${release}" -n "${NS}" 2>/dev/null || true
    done

    # Non-helm resources
    ${K} -n "${NS}" delete -k "${SCRIPT_DIR}/manifests/vllm/" --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete gateway llm-d-inference-gateway --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete svc vllm-metrics --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete secret batch-gateway-secrets --ignore-not-found 2>/dev/null || true
    ${K} -n "${NS}" delete pvc --all --ignore-not-found 2>/dev/null || true

    log "  ${NS}: resources deleted"

    # Delete namespace (skip if KEEP_NAMESPACE is set — needed for GPU clusters
    # where ArgoCD-managed RoleBindings take minutes to restore)
    if [ -z "${KEEP_NAMESPACE:-}" ]; then
        ${K} delete ns "${NS}" --ignore-not-found 2>/dev/null || true
        log "  ${NS}: namespace deleted"
    else
        log "  ${NS}: namespace preserved (KEEP_NAMESPACE=1)"
    fi
}

# Validate SCENARIO
if [ -z "${SCENARIO:-}" ]; then
    echo "ERROR: SCENARIO is not set (use 0-6 or 'all')" >&2
    exit 1
fi

# Handle SCENARIO=all
if [ "${SCENARIO}" = "all" ]; then
    log "=== Tearing down ALL scenario namespaces ==="
    for i in 0 1 2 3 4 5 6; do
        teardown_namespace "batch-bench-s${i}"
    done
    log "=== Teardown complete ==="
    exit 0
fi

NAMESPACE="${NAMESPACE:-batch-bench-s${SCENARIO}}"

log "=== Tearing down scenario ${SCENARIO} (namespace: ${NAMESPACE}) ==="
teardown_namespace "${NAMESPACE}"
log "=== Teardown complete ==="
