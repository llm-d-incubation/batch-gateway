#!/usr/bin/env bash
set -euo pipefail

# Benchmark environment setup.
# Deploys the full stack for a single scenario.
#
# Usage:
#   KUBE_CONTEXT=my-ctx SCENARIO=2 ./benchmarks/setup.sh
#
# Required env vars:
#   KUBE_CONTEXT       — kubectl context (e.g. coreweave-waldorf)
#   SCENARIO           — scenario number (0-5)
#
# Optional:
#   MODE               — "sim" to use inference-sim instead of real vLLM (default: gpu)
#   LLM_D_REPO         — path to llm-d checkout (overrides downloading from LLM_D_TAG)
#   ROUTER_REPO        — path to llm-d-router checkout (overrides OCI chart)
#   ROUTER_CHART_VERSION — OCI chart version for llm-d-router (default: 0.9.2)
#   LLM_D_TAG          — git tag for llm-d guide values (default: v0.7.0)
#   NAMESPACE          — override auto-generated namespace (default: batch-bench-s${SCENARIO})
#   MODEL              — model to serve (default: Qwen/Qwen3-8B)
#   GUIDE_NAME         — inference pool name (default: optimized-baseline)
#   MODEL_REVISION     — HuggingFace model revision/commit-sha to pin (default: unset, uses latest)
#   SIM_IMAGE          — inference-sim container image (default: ghcr.io/llm-d/llm-d-inference-sim:latest)
#   SIM_TTFT           — simulated time-to-first-token (default: 50ms)
#   SIM_ITL            — simulated inter-token-latency (default: 20ms)
#   BG_IMAGE_REPO      — batch-gateway image repo override
#   BG_IMAGE_TAG       — batch-gateway image tag override

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Defaults
MODE="${MODE:-gpu}"
if [ "${MODE}" = "sim" ]; then
    MODEL="${MODEL:-sim-model}"
else
    MODEL="${MODEL:-Qwen/Qwen3-8B}"
fi
GUIDE_NAME="${GUIDE_NAME:-optimized-baseline}"
NAMESPACE="${NAMESPACE:-batch-bench-s${SCENARIO}}"
ROUTER_CHART_VERSION="${ROUTER_CHART_VERSION:-0.9.2}"
LLM_D_TAG="${LLM_D_TAG:-v0.7.0}"
SIM_IMAGE="${SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:latest}"
SIM_TTFT="${SIM_TTFT:-50ms}"
SIM_ITL="${SIM_ITL:-20ms}"

# Validate required vars
for var in KUBE_CONTEXT SCENARIO; do
    if [ -z "${!var:-}" ]; then
        echo "ERROR: $var is not set" >&2
        exit 1
    fi
done

if [ "${SCENARIO}" -lt 0 ] || [ "${SCENARIO}" -gt 5 ]; then
    echo "ERROR: SCENARIO must be 0-5, got: ${SCENARIO}" >&2
    exit 1
fi

K="kubectl --context=${KUBE_CONTEXT}"
H="helm --kube-context=${KUBE_CONTEXT}"

log() { echo "[$(date +%H:%M:%S)] $*"; }

# Determine which Helm values file to use for the processor
values_file_for_scenario() {
    case "${SCENARIO}" in
        0|1) echo "" ;;  # No batch-gateway deployed
        2)   echo "${SCRIPT_DIR}/helm-values/scenario-2-ungated.yaml" ;;
        3)   echo "${SCRIPT_DIR}/helm-values/scenario-3-aimd.yaml" ;;
        4)   echo "${SCRIPT_DIR}/helm-values/scenario-4-aimd-flow-control.yaml" ;;
        5)   echo "${SCRIPT_DIR}/helm-values/scenario-5-async.yaml" ;;
    esac
}

log "=== Setting up scenario ${SCENARIO} in namespace ${NAMESPACE} ==="

# Create namespace
${K} create namespace "${NAMESPACE}" 2>/dev/null || true

# --- Redis ---
log "Installing Redis"
${H} install redis oci://registry-1.docker.io/bitnamicharts/redis \
    -n "${NAMESPACE}" \
    --set auth.enabled=false \
    --set master.persistence.size=1Gi \
    --set replica.replicaCount=0 \
    --wait --timeout 120s >/dev/null

# --- PostgreSQL ---
log "Installing PostgreSQL"
${H} install postgresql oci://registry-1.docker.io/bitnamicharts/postgresql \
    -n "${NAMESPACE}" \
    --set auth.database=batchgateway \
    --set auth.password=benchmarkpw \
    --set primary.persistence.size=5Gi \
    --wait --timeout 120s >/dev/null

# --- Secrets ---
log "Creating secrets"
${K} -n "${NAMESPACE}" create secret generic batch-gateway-secrets \
    --from-literal=redis-url="redis://redis-master.${NAMESPACE}.svc.cluster.local:6379" \
    --from-literal=postgresql-url="postgresql://postgres:benchmarkpw@postgresql.${NAMESPACE}.svc.cluster.local:5432/batchgateway?sslmode=disable" \
    --from-literal=inference-api-key="" \
    --from-literal=s3-secret-access-key="" \
    2>/dev/null || true

# --- PVCs ---
log "Creating PVCs"
${K} -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: batch-gateway-files
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
EOF
${K} -n "${NAMESPACE}" apply -f "${SCRIPT_DIR}/manifests/results-pvc.yaml"

# --- Inference backend ---
if [ "${MODE}" = "sim" ]; then
    # Sim mode: deploy inference-sim (no GPU, no router, no Istio)
    log "Deploying inference-sim (MODE=sim, model: ${MODEL})"
    ${K} -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inference-sim
  labels:
    llm-d.ai/role: decode
spec:
  replicas: 1
  selector:
    matchLabels:
      llm-d.ai/role: decode
  template:
    metadata:
      labels:
        llm-d.ai/role: decode
    spec:
      containers:
        - name: vllm-sim
          image: ${SIM_IMAGE}
          args:
            - --model
            - "${MODEL}"
            - --port
            - "8000"
            - --time-to-first-token=${SIM_TTFT}
            - --inter-token-latency=${SIM_ITL}
          ports:
            - containerPort: 8000
              name: modelserver
              protocol: TCP
          readinessProbe:
            httpGet:
              path: /v1/models
              port: 8000
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /health
              port: 8000
            initialDelaySeconds: 10
            periodSeconds: 10
---
apiVersion: v1
kind: Service
metadata:
  name: inference-sim
spec:
  selector:
    llm-d.ai/role: decode
  ports:
    - port: 8000
      targetPort: 8000
      name: http
EOF
else
    # GPU mode: deploy real vLLM + llm-d Router + Istio Gateway

    # --- llm-d Router (EPP) ---
    log "Installing llm-d Router (${GUIDE_NAME})"

    # Scenario 4: include flow-control overlay for priority-based scheduling
    FLOW_CONTROL_OVERLAY=""
    if [ "${SCENARIO}" = "4" ]; then
        FLOW_CONTROL_OVERLAY="-f ${SCRIPT_DIR}/helm-values/scenario-4-flow-control-overlay.yaml"
        log "  Flow control: enabling EPP priority bands (interactive=100, batch=-1)"
    fi

    if [ -n "${ROUTER_REPO:-}" ] && [ -n "${LLM_D_REPO:-}" ]; then
        # Local repo mode (development override)
        log "  Using local repos: ROUTER_REPO=${ROUTER_REPO}, LLM_D_REPO=${LLM_D_REPO}"
        if [ ! -f "${ROUTER_REPO}/config/charts/llm-d-router-gateway/charts/router-0.0.0.tgz" ]; then
            (cd "${ROUTER_REPO}/config/charts/llm-d-router-gateway" && helm dependency build >/dev/null 2>&1)
        fi
        ${H} install "${GUIDE_NAME}" \
            "${ROUTER_REPO}/config/charts/llm-d-router-gateway/" \
            -n "${NAMESPACE}" \
            -f "${LLM_D_REPO}/guides/recipes/router/base.values.yaml" \
            -f "${LLM_D_REPO}/guides/${GUIDE_NAME}/router/${GUIDE_NAME}.values.yaml" \
            -f "${LLM_D_REPO}/guides/recipes/router/features/monitoring.values.yaml" \
            ${FLOW_CONTROL_OVERLAY} \
            --set provider.name=istio \
            --set httpRoute.create=true \
            --set httpRoute.inferenceGatewayName=llm-d-inference-gateway >/dev/null
    else
        # OCI mode (default — reproducible, pinned versions)
        log "  Using OCI chart: ghcr.io/llm-d/llm-d-router-gateway:${ROUTER_CHART_VERSION}"
        log "  Using llm-d guide values from tag: ${LLM_D_TAG}"

        # Download guide values from pinned llm-d tag
        LLM_D_VALUES_DIR=$(mktemp -d)
        trap "rm -rf ${LLM_D_VALUES_DIR}" EXIT
        local_base="https://raw.githubusercontent.com/llm-d/llm-d/${LLM_D_TAG}"
        curl -sL "${local_base}/guides/recipes/router/base.values.yaml" -o "${LLM_D_VALUES_DIR}/base.values.yaml"
        curl -sL "${local_base}/guides/${GUIDE_NAME}/router/${GUIDE_NAME}.values.yaml" -o "${LLM_D_VALUES_DIR}/guide.values.yaml"
        curl -sL "${local_base}/guides/recipes/router/features/monitoring.values.yaml" -o "${LLM_D_VALUES_DIR}/monitoring.values.yaml"

        ${H} install "${GUIDE_NAME}" \
            oci://ghcr.io/llm-d/llm-d-router-gateway \
            --version "${ROUTER_CHART_VERSION}" \
            -n "${NAMESPACE}" \
            -f "${LLM_D_VALUES_DIR}/base.values.yaml" \
            -f "${LLM_D_VALUES_DIR}/guide.values.yaml" \
            -f "${LLM_D_VALUES_DIR}/monitoring.values.yaml" \
            ${FLOW_CONTROL_OVERLAY} \
            --set provider.name=istio \
            --set httpRoute.create=true \
            --set httpRoute.inferenceGatewayName=llm-d-inference-gateway >/dev/null
    fi

    # --- Istio Gateway ---
    log "Creating Istio Gateway"
    ${K} -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: llm-d-inference-gateway
  annotations:
    networking.istio.io/service-type: ClusterIP
spec:
  gatewayClassName: istio
  listeners:
  - name: default
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Same
EOF

    # --- vLLM ---
    log "Deploying vLLM (${MODEL})"
    ${K} -n "${NAMESPACE}" apply -k "${SCRIPT_DIR}/manifests/vllm/"

    # Pin model revision if specified
    if [ -n "${MODEL_REVISION:-}" ]; then
        log "  Pinning model revision: ${MODEL_REVISION}"
        ${K} -n "${NAMESPACE}" patch deploy/decode --type=json \
            -p "[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--revision=${MODEL_REVISION}\"}]" >/dev/null
    fi
fi

# --- Wait for inference backend ---
if [ "${MODE}" = "sim" ]; then
    log "Waiting for inference-sim to be ready..."
    ${K} -n "${NAMESPACE}" wait pod -l llm-d.ai/role=decode \
        --for=condition=Ready --timeout=120s >/dev/null
else
    log "Waiting for vLLM to be ready..."
    ${K} -n "${NAMESPACE}" wait pod -l llm-d.ai/role=decode \
        --for=condition=Ready --timeout=300s >/dev/null
fi

# --- Batch Gateway (scenarios 2-5 only) ---
VALUES_FILE=$(values_file_for_scenario)
if [ -n "${VALUES_FILE}" ]; then
    log "Installing batch-gateway (scenario ${SCENARIO})"
    BG_EXTRA_ARGS=()
    if [ -n "${BG_IMAGE_REPO:-}" ]; then
        BG_EXTRA_ARGS+=(
            --set "apiserver.image.repository=${BG_IMAGE_REPO}-apiserver"
            --set "processor.image.repository=${BG_IMAGE_REPO}-processor"
        )
    fi
    if [ -n "${BG_IMAGE_TAG:-}" ]; then
        BG_EXTRA_ARGS+=(
            --set-string "apiserver.image.tag=${BG_IMAGE_TAG}"
            --set-string "processor.image.tag=${BG_IMAGE_TAG}"
        )
    fi
    if [ -n "${BG_PULL_POLICY:-}" ]; then
        BG_EXTRA_ARGS+=(
            --set "apiserver.image.pullPolicy=${BG_PULL_POLICY}"
            --set "processor.image.pullPolicy=${BG_PULL_POLICY}"
        )
    fi

    # In sim mode, replace all model gateways with a single entry pointing to inference-sim
    if [ "${MODE}" = "sim" ]; then
        BG_EXTRA_ARGS+=(
            --set-json "processor.config.modelGateways={\"${MODEL}\":{\"url\":\"http://inference-sim.${NAMESPACE}.svc.cluster.local:8000\",\"requestTimeout\":\"5m\",\"maxRetries\":3,\"initialBackoff\":\"1s\",\"maxBackoff\":\"60s\"}}"
        )
    fi

    ${H} install batch-gateway \
        "${REPO_ROOT}/charts/batch-gateway/" \
        -n "${NAMESPACE}" \
        -f "${VALUES_FILE}" \
        --set global.secretName=batch-gateway-secrets \
        --set global.fileClient.type=fs \
        --set global.fileClient.fs.pvcName=batch-gateway-files \
        --set gc.enabled=false \
        "${BG_EXTRA_ARGS[@]+"${BG_EXTRA_ARGS[@]}"}" >/dev/null

    # TMPDIR fix for large file uploads
    ${K} -n "${NAMESPACE}" set env deploy/batch-gateway-apiserver TMPDIR=/tmp/batch-gateway >/dev/null
else
    log "Skipping batch-gateway (not needed for scenario ${SCENARIO})"
fi

# --- Scenario 4: Flow control InferenceObjective CRDs ---
if [ "${SCENARIO}" = "4" ]; then
    log "Deploying InferenceObjective CRDs for flow control"
    ${K} -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: interactive-default
spec:
  priority: 100
  poolRef:
    group: inference.networking.k8s.io
    name: ${GUIDE_NAME}
---
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: batch-sheddable
spec:
  priority: -1
  poolRef:
    group: inference.networking.k8s.io
    name: ${GUIDE_NAME}
EOF
    log "  Created InferenceObjective: interactive-default (priority 100)"
    log "  Created InferenceObjective: batch-sheddable (priority -1)"
fi

# --- Scenario 5: Async processor ---
if [ "${SCENARIO}" = "5" ]; then
    log "ERROR: Scenario 5 (async) is blocked on async-processor integration"
    log "  Skipping async-processor deployment"
fi

if [ -n "${VALUES_FILE}" ]; then
    ${K} -n "${NAMESPACE}" rollout status deploy/batch-gateway-apiserver --timeout=60s >/dev/null
    ${K} -n "${NAMESPACE}" rollout status deploy/batch-gateway-processor --timeout=60s >/dev/null
fi

log "=== Scenario ${SCENARIO} ready in namespace ${NAMESPACE} ==="
