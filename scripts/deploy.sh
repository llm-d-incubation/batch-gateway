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
error(){ echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── Configuration (override via env vars) ─────────────────────────────────────
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-batch-gateway-dev}"
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
NAMESPACE="${NAMESPACE:-default}"
DEV_VERSION="${DEV_VERSION:-0.0.1}"
REDIS_RELEASE="redis"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OS="$(uname -s)"
ARCH="$(uname -m)"

CONTAINER_TOOL=""
USE_KIND=false
KIND_CLUSTER=""

# ── Prerequisites ─────────────────────────────────────────────────────────────

detect_container_tool() {
    if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
        echo "docker"
    elif command -v podman &>/dev/null; then
        echo "podman"
    else
        error "Neither docker (running) nor podman found. Please install one."
    fi
}

check_prerequisites() {
    step "Checking prerequisites..."
    local missing=()
    for cmd in kubectl helm kind make; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing required tools: ${missing[*]}. Please install them first."
    fi
    CONTAINER_TOOL="$(detect_container_tool)"
    log "Container tool : ${CONTAINER_TOOL}"
    log "OS / Arch      : ${OS} / ${ARCH}"
}

# ── Cluster ───────────────────────────────────────────────────────────────────

is_openshift() {
    kubectl api-resources 2>/dev/null | grep -q "route.openshift.io"
}

ensure_cluster() {
    step "Checking for an existing Kubernetes / OpenShift cluster..."

    if kubectl cluster-info &>/dev/null 2>&1; then
        local ctx
        ctx="$(kubectl config current-context 2>/dev/null || echo "<unknown>")"

        if is_openshift; then
            log "OpenShift cluster detected (context: ${ctx}). Using it."
        else
            log "Kubernetes cluster detected (context: ${ctx}). Using it."
        fi

        # Track if the active context is already a kind cluster
        if [[ "${ctx}" == kind-* ]]; then
            USE_KIND=true
            KIND_CLUSTER="${ctx#kind-}"
            log "Active context is a kind cluster: ${KIND_CLUSTER}"
        fi
        return
    fi

    log "No reachable cluster found. Setting up kind cluster '${KIND_CLUSTER_NAME}'..."

    if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        log "Kind cluster '${KIND_CLUSTER_NAME}' already exists. Switching context..."
        kubectl config use-context "kind-${KIND_CLUSTER_NAME}"
    else
        kind create cluster --name "${KIND_CLUSTER_NAME}"
        kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}"
    fi

    USE_KIND=true
    KIND_CLUSTER="${KIND_CLUSTER_NAME}"
    log "Using kind cluster '${KIND_CLUSTER}'."
}

# ── Redis ─────────────────────────────────────────────────────────────────────

install_redis() {
    step "Installing Redis..."

    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
    fi
    helm repo update || warn "Some Helm repo updates failed; continuing."

    if helm status "${REDIS_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Redis release '${REDIS_RELEASE}' is already installed. Skipping."
        return
    fi

    helm install "${REDIS_RELEASE}" bitnami/redis \
        --namespace "${NAMESPACE}" \
        --set auth.enabled=false \
        --set replica.replicaCount=0 \
        --set master.persistence.enabled=false \
        --wait --timeout 120s

    log "Redis installed successfully."
}

# ── Images ────────────────────────────────────────────────────────────────────

get_target_arch() {
    case "${ARCH}" in
        arm64|aarch64) echo "arm64" ;;
        x86_64|amd64)  echo "amd64" ;;
        *)
            warn "Unknown arch '${ARCH}'; defaulting to amd64."
            echo "amd64"
            ;;
    esac
}

build_and_load_images() {
    local target_arch
    target_arch="$(get_target_arch)"
    local apiserver_img="ghcr.io/llm-d/batch-gateway-apiserver:${DEV_VERSION}"
    local processor_img="ghcr.io/llm-d/batch-gateway-processor:${DEV_VERSION}"

    step "Building container images (TARGETARCH=${target_arch}, version=${DEV_VERSION})..."
    cd "${REPO_ROOT}"
    CONTAINER_TOOL="${CONTAINER_TOOL}" TARGETARCH="${target_arch}" DEV_VERSION="${DEV_VERSION}" make image-build

    if [ "${USE_KIND}" = true ]; then
        step "Loading images into kind cluster '${KIND_CLUSTER}'..."

        if [ "${CONTAINER_TOOL}" = "docker" ]; then
            kind load docker-image "${apiserver_img}" --name "${KIND_CLUSTER}"
            kind load docker-image "${processor_img}" --name "${KIND_CLUSTER}"
        else
            # Podman: save to tar archives then load into kind
            local tmp_apiserver tmp_processor
            tmp_apiserver="$(mktemp /tmp/apiserver-XXXX.tar)"
            tmp_processor="$(mktemp /tmp/processor-XXXX.tar)"

            log "Saving Podman images to tar archives..."
            podman save -o "${tmp_apiserver}" "${apiserver_img}"
            podman save -o "${tmp_processor}" "${processor_img}"

            kind load image-archive "${tmp_apiserver}" --name "${KIND_CLUSTER}"
            kind load image-archive "${tmp_processor}" --name "${KIND_CLUSTER}"

            rm -f "${tmp_apiserver}" "${tmp_processor}"
        fi
        log "Images loaded into kind."
    else
        warn "Not a kind cluster — skipping image load."
        warn "Ensure '${apiserver_img}' and '${processor_img}' are accessible from the cluster."
    fi
}

# ── Batch Gateway ─────────────────────────────────────────────────────────────

install_batch_gateway() {
    step "Installing batch-gateway via Helm (version=${DEV_VERSION})..."
    cd "${REPO_ROOT}"

    local helm_args=(
        --set apiserver.image.pullPolicy=IfNotPresent
        --set "apiserver.image.tag=${DEV_VERSION}"
        --set processor.enabled=true
        --set processor.image.pullPolicy=IfNotPresent
        --set "processor.image.tag=${DEV_VERSION}"
        --namespace "${NAMESPACE}"
    )

    if helm status "${HELM_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Release '${HELM_RELEASE}' already exists. Upgrading..."
        helm upgrade "${HELM_RELEASE}" ./charts/batch-gateway "${helm_args[@]}"
    else
        helm install "${HELM_RELEASE}" ./charts/batch-gateway "${helm_args[@]}"
    fi

    log "batch-gateway installed."
}

# ── Verify ────────────────────────────────────────────────────────────────────

verify_deployment() {
    step "Verifying deployment..."
    kubectl get pods -l "app.kubernetes.io/instance=${HELM_RELEASE}" -n "${NAMESPACE}"
    kubectl get svc  -l "app.kubernetes.io/instance=${HELM_RELEASE}" -n "${NAMESPACE}"
}

print_usage() {
    local svc="svc/${HELM_RELEASE}-apiserver"
    local port=8000
    local base="http://localhost:${port}"

    echo ""
    echo "  ╔══════════════════════════════════════════════════════════════╗"
    echo "  ║                        Next Steps                           ║"
    echo "  ╚══════════════════════════════════════════════════════════════╝"
    echo ""
    echo "  1. Start port-forward in a separate terminal:"
    echo ""
    echo "       kubectl port-forward ${svc} ${port}:${port} -n ${NAMESPACE}"
    echo ""
    echo "  2. Check apiserver health:"
    echo ""
    echo "       curl -s ${base}/health"
    echo "       curl -s ${base}/ready"
    echo ""
    echo "  3. Upload a batch input file (JSONL):"
    echo ""
    echo "       curl -s -X POST ${base}/v1/files \\"
    echo "         -F 'file=@/path/to/requests.jsonl' \\"
    echo "         -F 'purpose=batch'"
    echo ""
    echo "     Each line in the JSONL file should follow this format:"
    echo '       {"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}}'
    echo ""
    echo "  4. Create a batch (replace FILE_ID with the id from step 3):"
    echo ""
    echo "       curl -s -X POST ${base}/v1/batches \\"
    echo "         -H 'Content-Type: application/json' \\"
    echo "         -d '{\"input_file_id\":\"FILE_ID\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}'"
    echo ""
    echo "  5. Cleanup:"
    echo ""
    echo "       # Uninstall batch-gateway and Redis"
    echo "       helm uninstall ${HELM_RELEASE} -n ${NAMESPACE}"
    echo "       helm uninstall ${REDIS_RELEASE} -n ${NAMESPACE}"
    echo ""
    if [ "${USE_KIND}" = true ]; then
    echo "       # Delete the kind cluster"
    echo "       kind delete cluster --name ${KIND_CLUSTER}"
    echo ""
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║   Batch Gateway Deployment Script    ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""

    check_prerequisites
    ensure_cluster
    install_redis
    build_and_load_images
    install_batch_gateway
    verify_deployment
    print_usage

    log "Deployment complete!"
}

main "$@"
