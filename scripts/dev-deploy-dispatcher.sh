#!/bin/bash
# This script can be run standalone or sourced from dev-deploy.sh.
# When sourced, SCRIPT_DIR, REPO_ROOT, and dev-common.sh are already set.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -euo pipefail
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
    source "${SCRIPT_DIR}/dev-common.sh"
fi

# ── Configuration ────────────────────────────────────────────────────────────
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-batch-gateway-dev}"
DISPATCHER_RELEASE="${DISPATCHER_RELEASE:-dispatcher}"
DISPATCHER_VERSION="${DISPATCHER_VERSION:-v0.7.4}"
DISPATCHER_IMAGE="${DISPATCHER_IMAGE:-ghcr.io/llm-d/llm-d-async:${DISPATCHER_VERSION}}"
DISPATCHER_CHART="${DISPATCHER_CHART:-oci://ghcr.io/llm-d/charts/async-processor}"
DISPATCHER_CHART_VERSION="${DISPATCHER_CHART_VERSION:-0.7.4}"
DISPATCHER_REDIS_PORT="${DISPATCHER_REDIS_PORT:-6399}"
PID_FILE="${REPO_ROOT}/.dispatcher-port-forward.pid"
# Set DISPATCHER_SOURCE to a local llm-d-async checkout to build from source
# instead of pulling a released image. The local chart is used automatically.
# Example: DISPATCHER_SOURCE=~/src/llm-d-async ENABLE_DISPATCHER=true make dev-deploy
DISPATCHER_SOURCE="${DISPATCHER_SOURCE:-}"

# ── Prerequisites (standalone only — dev-deploy.sh already checks these) ─────
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    for cmd in kubectl helm kind jq nc; do
        command -v "$cmd" &>/dev/null || die "Missing required tool: $cmd"
    done

    if [[ -n "${CONTAINER_TOOL:-}" ]]; then
        : # caller specified
    elif command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
        CONTAINER_TOOL="docker"
    elif command -v podman &>/dev/null; then
        CONTAINER_TOOL="podman"
    else
        die "Neither docker (running) nor podman found. Please install one."
    fi

    if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
        die "Kind cluster '${KIND_CLUSTER_NAME}' not found. Run 'make dev-deploy' first."
    fi
fi

# ── Build or pull dispatcher image ────────────────────────────────────────────
if [[ -n "${DISPATCHER_SOURCE}" ]]; then
    if [[ ! -d "${DISPATCHER_SOURCE}" ]]; then
        die "DISPATCHER_SOURCE directory not found: ${DISPATCHER_SOURCE}"
    fi
    DISPATCHER_IMAGE="ghcr.io/llm-d/llm-d-async:dev-local"
    DISPATCHER_CHART="${DISPATCHER_SOURCE}/charts/async-processor"
    unset DISPATCHER_CHART_VERSION
    step "Building async-processor image from ${DISPATCHER_SOURCE}..."
    ${CONTAINER_TOOL} build -t "${DISPATCHER_IMAGE}" "${DISPATCHER_SOURCE}"
else
    if ${CONTAINER_TOOL} image exists "${DISPATCHER_IMAGE}" 2>/dev/null || \
       ${CONTAINER_TOOL} inspect "${DISPATCHER_IMAGE}" &>/dev/null; then
        step "Using local dispatcher image ${DISPATCHER_IMAGE}"
    else
        step "Pulling dispatcher image ${DISPATCHER_IMAGE}..."
        ${CONTAINER_TOOL} pull "${DISPATCHER_IMAGE}"
    fi
fi

step "Loading dispatcher image into Kind cluster '${KIND_CLUSTER_NAME}'..."
if docker exec "${KIND_CLUSTER_NAME}-control-plane" ctr --namespace=k8s.io images list -q 2>/dev/null | grep -q "^${DISPATCHER_IMAGE}$"; then
    log "Image already present in Kind node, skipping load"
elif [[ "${CONTAINER_TOOL}" == "docker" ]]; then
    docker save "${DISPATCHER_IMAGE}" | docker exec -i "${KIND_CLUSTER_NAME}-control-plane" \
        ctr --namespace=k8s.io images import --snapshotter=overlayfs -
else
    ${CONTAINER_TOOL} save "${DISPATCHER_IMAGE}" | kind load image-archive /dev/stdin --name "${KIND_CLUSTER_NAME}"
fi

# ── Deploy async-processor via Helm ──────────────────────────────────────────
HELM_VALUES="${REPO_ROOT}/test/e2e/dispatcher/helm-values.yaml"

if [[ ! -f "${HELM_VALUES}" ]]; then
    die "Helm values file not found: ${HELM_VALUES}"
fi

DISPATCHER_SCRAPE_RELEASE="${DISPATCHER_SCRAPE_RELEASE:-dispatcher-scrape}"
HELM_VALUES_SCRAPE="${REPO_ROOT}/test/e2e/dispatcher/helm-values-scrape.yaml"
IMAGE_REPO="$(echo "${DISPATCHER_IMAGE}" | cut -d: -f1)"
IMAGE_TAG="$(echo "${DISPATCHER_IMAGE}" | cut -d: -f2)"

HELM_VERSION_FLAG=()
if [[ -n "${DISPATCHER_CHART_VERSION:-}" ]]; then
    HELM_VERSION_FLAG=(--version "${DISPATCHER_CHART_VERSION}")
fi

step "Deploying async-processor with redis gate (release: ${DISPATCHER_RELEASE})..."
helm upgrade --install "${DISPATCHER_RELEASE}" "${DISPATCHER_CHART}" \
    "${HELM_VERSION_FLAG[@]}" \
    --namespace "${NAMESPACE}" \
    --values "${HELM_VALUES}" \
    --set "ap.image.repository=${IMAGE_REPO}" \
    --set "ap.image.tag=${IMAGE_TAG}" \
    --wait --timeout=120s

step "Deploying async-processor with endpoint-scrape gate (release: ${DISPATCHER_SCRAPE_RELEASE})..."
helm upgrade --install "${DISPATCHER_SCRAPE_RELEASE}" "${DISPATCHER_CHART}" \
    "${HELM_VERSION_FLAG[@]}" \
    --namespace "${NAMESPACE}" \
    --values "${HELM_VALUES_SCRAPE}" \
    --set "ap.image.repository=${IMAGE_REPO}" \
    --set "ap.image.tag=${IMAGE_TAG}" \
    --wait --timeout=120s

DISPATCHER_PROM_RELEASE="${DISPATCHER_PROM_RELEASE:-dispatcher-prom}"
HELM_VALUES_PROM="${REPO_ROOT}/test/e2e/dispatcher/helm-values-prometheus.yaml"

step "Deploying async-processor with prometheus-query gate (release: ${DISPATCHER_PROM_RELEASE})..."
helm upgrade --install "${DISPATCHER_PROM_RELEASE}" "${DISPATCHER_CHART}" \
    "${HELM_VERSION_FLAG[@]}" \
    --namespace "${NAMESPACE}" \
    --values "${HELM_VALUES_PROM}" \
    --set "ap.image.repository=${IMAGE_REPO}" \
    --set "ap.image.tag=${IMAGE_TAG}" \
    --wait --timeout=120s

log "Dispatchers deployed."

# ── Verify dispatchers ───────────────────────────────────────────────────────
step "Waiting for dispatcher pods to be ready..."
kubectl wait --for=condition=available deployment/"${DISPATCHER_RELEASE}-async-processor" \
    --namespace "${NAMESPACE}" --timeout=60s
kubectl wait --for=condition=available deployment/"${DISPATCHER_SCRAPE_RELEASE}-async-processor" \
    --namespace "${NAMESPACE}" --timeout=60s
kubectl wait --for=condition=available deployment/"${DISPATCHER_PROM_RELEASE}-async-processor" \
    --namespace "${NAMESPACE}" --timeout=60s

# ── Add vllm-sim to Prometheus scrape targets ────────────────────────────────
step "Adding vllm-sim to Prometheus scrape config..."
PROM_CM="${PROMETHEUS_NAME:-prometheus}-config"
CURRENT_PROM_CONFIG=$(kubectl get configmap "${PROM_CM}" --namespace "${NAMESPACE}" -o jsonpath='{.data.prometheus\.yml}' 2>/dev/null || true)

if echo "${CURRENT_PROM_CONFIG}" | grep -q "vllm-sim"; then
    log "vllm-sim already in Prometheus scrape config, skipping"
else
    # Append vllm-sim scrape job (indentation must match existing scrape_configs entries)
    read -r -d '' VLLM_SIM_SCRAPE <<-SCRAPE || true
- job_name: 'vllm-sim'
  metrics_path: /metrics
  scrape_interval: 5s
  static_configs:
  - targets: ['${VLLM_SIM_NAME}.${NAMESPACE}.svc.cluster.local:8000']
    labels:
      component: vllm-sim
SCRAPE
    UPDATED_PROM_CONFIG="${CURRENT_PROM_CONFIG}
${VLLM_SIM_SCRAPE}"

    kubectl create configmap "${PROM_CM}" \
        --namespace "${NAMESPACE}" \
        --from-literal="prometheus.yml=${UPDATED_PROM_CONFIG}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Reload Prometheus config
    kubectl rollout restart deployment/"${PROMETHEUS_NAME:-prometheus}" --namespace "${NAMESPACE}"
    kubectl rollout status deployment/"${PROMETHEUS_NAME:-prometheus}" --namespace "${NAMESPACE}" --timeout=60s
    log "Prometheus scrape config updated with vllm-sim target"
fi

# ── Enable fake metrics on vllm-sim ──────────────────────────────────────────
step "Patching vllm-sim to enable --fake-metrics..."
# --fake-metrics requires a JSON argument with initial metric values.
# Replace the full args array to avoid duplicate appends on re-runs.
CURRENT_ARGS=$(kubectl get deployment "${VLLM_SIM_NAME}" --namespace "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[0].args}')

if echo "${CURRENT_ARGS}" | grep -q "fake-metrics"; then
    log "vllm-sim already has --fake-metrics, skipping patch"
else
    kubectl patch deployment "${VLLM_SIM_NAME}" --namespace "${NAMESPACE}" --type=json \
        -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--fake-metrics"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"{\"kv-cache-usage\": 0, \"waiting-requests\": 0, \"running-requests\": 0}"}]'
    kubectl rollout status deployment/"${VLLM_SIM_NAME}" --namespace "${NAMESPACE}" --timeout=120s
fi

# ── Reconfigure processor for async dispatch ─────────────────────────────────
PROCESSOR_ASYNC_VALUES="${REPO_ROOT}/test/e2e/dispatcher/processor-async-values.yaml"

step "Reconfiguring batch-gateway processor for async dispatch..."

# --reuse-values deep-merges maps, so stale sync-mode models from the initial
# deploy would persist and crash the processor (missing inferencePoolName).
# Instead, export current values, strip modelGateways, and pass as a file.
REUSED_VALUES=$(mktemp)
helm get values "${HELM_RELEASE}" -n "${NAMESPACE}" -o json | \
    jq 'del(.processor.config.modelGateways)' > "${REUSED_VALUES}"

helm upgrade "${HELM_RELEASE}" "${REPO_ROOT}/charts/batch-gateway" \
    --namespace "${NAMESPACE}" \
    --reset-values \
    --values "${REUSED_VALUES}" \
    --values "${PROCESSOR_ASYNC_VALUES}" \
    --wait --timeout=120s
rm -f "${REUSED_VALUES}"

step "Restarting processor to pick up new config..."
kubectl rollout restart deployment/"${HELM_RELEASE}-processor" --namespace "${NAMESPACE}"
kubectl rollout status deployment/"${HELM_RELEASE}-processor" --namespace "${NAMESPACE}" --timeout=60s

log "Processor reconfigured for async dispatch."

# ── Port-forward Redis to host ───────────────────────────────────────────────
# Kind only exposes ports declared in extraPortMappings at cluster creation.
# Use kubectl port-forward to make Redis accessible from the host.
step "Setting up Redis port-forward on localhost:${DISPATCHER_REDIS_PORT}..."

# Kill any previous port-forward
if [[ -f "${PID_FILE}" ]]; then
    old_pid=$(cat "${PID_FILE}")
    kill "${old_pid}" 2>/dev/null || true
    rm -f "${PID_FILE}"
fi

# Detect Redis service name
redis_svc="${REDIS_RELEASE}-master"
if [[ "${EXCHANGE_CLIENT_TYPE}" == "valkey" ]]; then
    redis_svc="${REDIS_RELEASE}-valkey-primary"
fi

kubectl port-forward "svc/${redis_svc}" "${DISPATCHER_REDIS_PORT}:6379" \
    --namespace "${NAMESPACE}" &
PORT_FORWARD_PID=$!
echo "${PORT_FORWARD_PID}" > "${PID_FILE}"

# Wait for the port-forward to be ready
for i in $(seq 1 10); do
    if nc -z localhost "${DISPATCHER_REDIS_PORT}" 2>/dev/null; then
        break
    fi
    sleep 0.5
done

if ! nc -z localhost "${DISPATCHER_REDIS_PORT}" 2>/dev/null; then
    die "Port-forward to Redis failed to start"
fi

log "Redis accessible at localhost:${DISPATCHER_REDIS_PORT}"

# ── Port-forward vLLM sim to host ────────────────────────────────────────────
DISPATCHER_SIM_PORT="${DISPATCHER_SIM_PORT:-8099}"
SIM_PID_FILE="${REPO_ROOT}/.dispatcher-sim-port-forward.pid"

step "Setting up vLLM sim port-forward on localhost:${DISPATCHER_SIM_PORT}..."

if [[ -f "${SIM_PID_FILE}" ]]; then
    old_pid=$(cat "${SIM_PID_FILE}")
    kill "${old_pid}" 2>/dev/null || true
    rm -f "${SIM_PID_FILE}"
fi

kubectl port-forward "svc/${VLLM_SIM_NAME}" "${DISPATCHER_SIM_PORT}:8000" \
    --namespace "${NAMESPACE}" &
SIM_PF_PID=$!
echo "${SIM_PF_PID}" > "${SIM_PID_FILE}"

for i in $(seq 1 10); do
    if nc -z localhost "${DISPATCHER_SIM_PORT}" 2>/dev/null; then
        break
    fi
    sleep 0.5
done

if ! nc -z localhost "${DISPATCHER_SIM_PORT}" 2>/dev/null; then
    die "Port-forward to vLLM sim failed to start"
fi

log "vLLM sim accessible at localhost:${DISPATCHER_SIM_PORT}"

log ""
log "Dispatcher is ready."
log ""
log "Usage:"
log "  ENABLE_DISPATCHER=true make test-e2e"
log "  ENABLE_DISPATCHER=true TEST_REDIS_URL=redis://localhost:${DISPATCHER_REDIS_PORT} go test ./test/e2e/ -run TestDispatcher -v -count=1"
log ""
log "Jaeger UI: http://localhost:${JAEGER_PORT}  (traces from both batch-gateway and async-processor)"
log "To stop port-forwards: make dev-clean"
log ""
if [[ -n "${DISPATCHER_SOURCE}" ]]; then
    log "Built from local source: ${DISPATCHER_SOURCE}"
    log "To rebuild after changes: DISPATCHER_SOURCE=${DISPATCHER_SOURCE} ENABLE_DISPATCHER=true make dev-deploy"
fi
