#!/bin/bash
set -euo pipefail

# Source common functions and configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/dev-common.sh"

# ── Cleanup ───────────────────────────────────────────────────────────────────

cleanup_kubernetes_resources() {
    step "Cleaning up Kubernetes resources in namespace '${NAMESPACE}'..."

    # Uninstall helm releases (--ignore-not-found available in Helm 3.13+, use || true as fallback)
    log "Uninstalling helm releases..."
    helm uninstall "${HELM_RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
    helm uninstall "${REDIS_RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true
    helm uninstall "${POSTGRESQL_RELEASE}" -n "${NAMESPACE}" 2>/dev/null || true

    # Delete NodePort services (created outside of Helm)
    log "Deleting NodePort services..."
    kubectl delete svc "${HELM_RELEASE}-apiserver-nodeport" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete svc "${HELM_RELEASE}-processor-nodeport" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete svc "${PROMETHEUS_NAME}-nodeport" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete svc "${GRAFANA_NAME}-nodeport" -n "${NAMESPACE}" --ignore-not-found=true

    # Delete deployments and services
    log "Deleting deployments and services..."
    kubectl delete deployment,svc "${JAEGER_NAME}" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete deployment,svc "${GRAFANA_NAME}" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete configmap "${GRAFANA_NAME}-provisioning-datasources" "${GRAFANA_NAME}-provisioning-dashboards" "${GRAFANA_NAME}-dashboards" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete deployment,svc,configmap,sa "${PROMETHEUS_NAME}" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete configmap "${PROMETHEUS_NAME}-config" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete clusterrole,clusterrolebinding "${PROMETHEUS_NAME}" --ignore-not-found=true
    kubectl delete deployment,svc "${VLLM_SIM_NAME}" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete deployment,svc "${VLLM_SIM_B_NAME}" -n "${NAMESPACE}" --ignore-not-found=true
    kubectl delete deployment,svc "${MINIO_NAME}" -n "${NAMESPACE}" --ignore-not-found=true

    # Delete secrets
    log "Deleting secrets..."
    kubectl delete secret "${APP_SECRET_NAME}" "${TLS_SECRET_NAME}" -n "${NAMESPACE}" --ignore-not-found=true

    # Delete PVC
    log "Deleting PVC..."
    kubectl delete pvc "${FILES_PVC_NAME}" -n "${NAMESPACE}" --ignore-not-found=true

    log "Kubernetes resources cleaned up."
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║   Batch Gateway Cleanup Script       ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""

    # Check if required tools are available
    if ! command -v kubectl &>/dev/null; then
        die "kubectl not found. Please install it first."
    fi

    if ! command -v helm &>/dev/null; then
        die "helm not found. Please install it first."
    fi

    step "Cleaning all batch-gateway resources from namespace '${NAMESPACE}'..."

    cleanup_kubernetes_resources

    log ""
    log "Cleanup complete!"
    log ""
}

main "$@"
