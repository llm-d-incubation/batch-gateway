#!/bin/bash
set -euo pipefail

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
step() { echo -e "${BLUE}[STEP]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── Configuration (must match dev-deploy.sh defaults) ────────────────────────
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-batch-gateway-dev}"
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
NAMESPACE="${NAMESPACE:-default}"
REDIS_RELEASE="redis"
POSTGRESQL_RELEASE="${POSTGRESQL_RELEASE:-postgresql}"
TLS_SECRET_NAME="${TLS_SECRET_NAME:-${HELM_RELEASE}-tls}"
APP_SECRET_NAME="${APP_SECRET_NAME:-${HELM_RELEASE}-secrets}"
FILES_PVC_NAME="${FILES_PVC_NAME:-${HELM_RELEASE}-files}"
JAEGER_NAME="${JAEGER_NAME:-jaeger}"
VLLM_SIM_NAME="${VLLM_SIM_NAME:-vllm-sim}"
VLLM_SIM_B_NAME="${VLLM_SIM_B_NAME:-vllm-sim-b}"
USE_KIND="${USE_KIND:-true}"

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

cleanup_kind_cluster() {
    if [ "${USE_KIND}" != "true" ]; then
        log "Not using kind cluster (USE_KIND=${USE_KIND}), skipping cluster deletion."
        return
    fi

    step "Deleting kind cluster '${KIND_CLUSTER_NAME}'..."

    if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        kind delete cluster --name "${KIND_CLUSTER_NAME}"
        log "Kind cluster '${KIND_CLUSTER_NAME}' deleted."
    else
        log "Kind cluster '${KIND_CLUSTER_NAME}' not found, skipping."
    fi
}

kill_port_forwards() {
    step "Killing port-forward processes..."

    local ports=(8000 8081 9090 16686)
    for port in "${ports[@]}"; do
        local pids=$(lsof -ti tcp:${port} 2>/dev/null || true)
        if [ -n "${pids}" ]; then
            log "Killing processes on port ${port}: ${pids}"
            kill ${pids} 2>/dev/null || true
        fi
    done

    log "Port-forwards cleaned up."
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

    # Confirm deletion if using kind
    if [ "${USE_KIND}" = "true" ]; then
        warn "This will delete the kind cluster '${KIND_CLUSTER_NAME}' and all resources."
        read -p "Are you sure? (yes/no): " -r
        echo
        if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
            log "Cleanup cancelled."
            exit 0
        fi
    else
        warn "This will delete all batch-gateway resources from namespace '${NAMESPACE}'."
        read -p "Are you sure? (yes/no): " -r
        echo
        if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
            log "Cleanup cancelled."
            exit 0
        fi
    fi

    kill_port_forwards
    cleanup_kubernetes_resources
    cleanup_kind_cluster

    log ""
    log "Cleanup complete!"
    log ""
}

main "$@"
