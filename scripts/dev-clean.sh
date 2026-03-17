#!/bin/bash
set -euo pipefail

# Source common functions and configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/dev-common.sh"

# ── Cleanup ───────────────────────────────────────────────────────────────────

cleanup_kubernetes_resources() {
    step "Cleaning up Kubernetes resources in namespace '${NAMESPACE}'..."

    # Uninstall helm releases
    if helm status "${HELM_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Uninstalling helm release '${HELM_RELEASE}'..."
        helm uninstall "${HELM_RELEASE}" -n "${NAMESPACE}"
    else
        log "Helm release '${HELM_RELEASE}' not found, skipping."
    fi

    if helm status "${REDIS_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Uninstalling helm release '${REDIS_RELEASE}'..."
        helm uninstall "${REDIS_RELEASE}" -n "${NAMESPACE}"
    else
        log "Helm release '${REDIS_RELEASE}' not found, skipping."
    fi

    if helm status "${POSTGRESQL_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Uninstalling helm release '${POSTGRESQL_RELEASE}'..."
        helm uninstall "${POSTGRESQL_RELEASE}" -n "${NAMESPACE}"
    else
        log "Helm release '${POSTGRESQL_RELEASE}' not found, skipping."
    fi

    # Delete deployments and services
    if kubectl get deployment "${JAEGER_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Deleting Jaeger deployment and service..."
        kubectl delete deployment,svc "${JAEGER_NAME}" -n "${NAMESPACE}"
    else
        log "Jaeger not found, skipping."
    fi

    if kubectl get deployment "${VLLM_SIM_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Deleting vLLM simulator '${VLLM_SIM_NAME}'..."
        kubectl delete deployment,svc "${VLLM_SIM_NAME}" -n "${NAMESPACE}"
    else
        log "vLLM simulator '${VLLM_SIM_NAME}' not found, skipping."
    fi

    if kubectl get deployment "${VLLM_SIM_B_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Deleting vLLM simulator '${VLLM_SIM_B_NAME}'..."
        kubectl delete deployment,svc "${VLLM_SIM_B_NAME}" -n "${NAMESPACE}"
    else
        log "vLLM simulator '${VLLM_SIM_B_NAME}' not found, skipping."
    fi

    # Delete secrets
    for secret in "${APP_SECRET_NAME}" "${TLS_SECRET_NAME}"; do
        if kubectl get secret "${secret}" -n "${NAMESPACE}" &>/dev/null; then
            log "Deleting secret '${secret}'..."
            kubectl delete secret "${secret}" -n "${NAMESPACE}"
        else
            log "Secret '${secret}' not found, skipping."
        fi
    done

    # Delete PVC
    if kubectl get pvc "${FILES_PVC_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Deleting PVC '${FILES_PVC_NAME}'..."
        kubectl delete pvc "${FILES_PVC_NAME}" -n "${NAMESPACE}"
    else
        log "PVC '${FILES_PVC_NAME}' not found, skipping."
    fi

    log "Kubernetes resources cleaned up."
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║   Batch Gateway Cleanup Script       ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""

    # Check if kubectl is available
    if ! command -v kubectl &>/dev/null; then
        die "kubectl not found. Please install it first."
    fi

    step "Cleaning all batch-gateway resources from namespace '${NAMESPACE}'..."

    step "Killing port-forward processes..."
    kill_port_forwards

    cleanup_kubernetes_resources

    log ""
    log "Cleanup complete!"
    log ""
}

main "$@"
