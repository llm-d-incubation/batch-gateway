#!/bin/bash
# Common functions shared by deploy-with-kuadrant-apikey.sh, deploy-with-kuadrant-satoken.sh, and deploy-with-kuadrant-usertoken.sh

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

# ── Configuration (override via env vars) ─────────────────────────────────────
BATCH_NAMESPACE="${BATCH_NAMESPACE:-batch-api}"
LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"
INGRESS_NAMESPACE="${INGRESS_NAMESPACE:-istio-ingress}"
KUADRANT_NAMESPACE="${KUADRANT_NAMESPACE:-kuadrant-system}"
KUADRANT_RELEASE="${KUADRANT_RELEASE:-kuadrant-operator}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.15.3}"
KUADRANT_VERSION="${KUADRANT_VERSION:-1.3.1}"
ISTIO_VERSION="${ISTIO_VERSION:-v1.28.0}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.1.0}"
GAIE_VERSION="${GAIE_VERSION:-v1.3.1}"
GAIE_REPO=/tmp/gateway-api-inference-extension-${GAIE_VERSION#v}
# TODO: Upgrade to GAIE v1.4.0+ when released — adds EPP request logging (logger.V(1).Info("EPP received request"))
VLLM_SIM_IMAGE="${VLLM_SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:latest}"
GATEWAY_NAME="${GATEWAY_NAME:-istio-gateway}"
LOCAL_PORT="${LOCAL_PORT:-8080}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-selfsigned-issuer}"
GATEWAY_URL="https://localhost:${LOCAL_PORT}"

# Batch Gateway configuration
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
DEV_VERSION="${DEV_VERSION:-latest}"
BATCH_INFERENCE_SERVICE="${BATCH_INFERENCE_SERVICE:-${HELM_RELEASE}-apiserver}"
BATCH_INFERENCE_PORT="${BATCH_INFERENCE_PORT:-8000}"
APP_SECRET_NAME="${APP_SECRET_NAME:-${HELM_RELEASE}-secrets}"
FILES_PVC_NAME="${FILES_PVC_NAME:-${HELM_RELEASE}-files}"
DB_TYPE="${DB_TYPE:-postgresql}"
REDIS_RELEASE="${REDIS_RELEASE:-redis}"
POSTGRESQL_RELEASE="${POSTGRESQL_RELEASE:-postgresql}"
# WARNING: Default passwords are for demo only. For production, override via env vars or use K8s secrets.
POSTGRESQL_PASSWORD="${POSTGRESQL_PASSWORD:-postgres}"
STORAGE_TYPE="${STORAGE_TYPE:-fs}"
MINIO_RELEASE="${MINIO_RELEASE:-minio}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_BUCKET="${MINIO_BUCKET:-batch-gateway}"

FREE_MODEL="${FREE_MODEL:-free-model}"
GOLD_MODEL="${GOLD_MODEL:-gold-model}"
E2E_MODEL="${FREE_MODEL}"  # E2E path uses free-model (accessible by both tiers)

# Model -> InferencePool mapping for llm-route
MODEL_ROUTES=(
    "${FREE_MODEL}:${FREE_MODEL}"
    "${GOLD_MODEL}:${GOLD_MODEL}"
)

# ── Helper Functions ──────────────────────────────────────────────────────────

is_openshift() {
    kubectl api-resources --api-group=route.openshift.io &>/dev/null 2>&1
}

gen_id() {
    uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "${RANDOM}-${RANDOM}-$$"
}

wait_for_deployment() {
    local name="$1"
    local ns="$2"
    local timeout="${3:-120s}"
    local retries=5

    step "Waiting for deployment '${name}' to be ready..."
    for i in $(seq 1 "${retries}"); do
        if kubectl rollout status deployment/"${name}" \
            -n "${ns}" --timeout="${timeout}"; then
            log "Deployment '${name}' is ready."
            return 0
        fi
        [ "${i}" -eq "${retries}" ] && die "Deployment '${name}' did not become ready"
        warn "Deployment not yet visible, retrying in 2s... (${i}/${retries})"
        sleep 2
    done
}

start_gateway_port_forward() {
    local gateway_svc
    gateway_svc="${GATEWAY_NAME}-istio"

    if ! kubectl get svc "${gateway_svc}" -n "${INGRESS_NAMESPACE}" &>/dev/null; then
        gateway_svc="${GATEWAY_NAME}"
        if ! kubectl get svc "${gateway_svc}" -n "${INGRESS_NAMESPACE}" &>/dev/null; then
            warn "Gateway service not found. Skipping port-forward."
            return
        fi
    fi

    step "Starting port-forward: ${gateway_svc} ${LOCAL_PORT}:443 -n ${INGRESS_NAMESPACE}..."

    kubectl port-forward "svc/${gateway_svc}" "${LOCAL_PORT}:443" -n "${INGRESS_NAMESPACE}" &
    local pf_pid=$!
    disown "${pf_pid}"

    log "Port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"
    log "Gateway available at https://localhost:${LOCAL_PORT}"
}

timeout_delete() {
    local timeout="$1"
    shift

    if kubectl delete --timeout="${timeout}" "$@" 2>/dev/null; then
        return 0
    fi

    warn "Delete timed out, removing finalizers..."
    local resource_list
    resource_list=$(kubectl get "$@" -o jsonpath='{range .items[*]}{.kind}/{.metadata.name}{" "}{end}' 2>/dev/null) \
        || resource_list=$(kubectl get "$@" -o jsonpath='{.kind}/{.metadata.name}' 2>/dev/null) || true
    for res in $resource_list; do
        kubectl patch "$res" "${@: -2}" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    done

    warn "Force deleting..."
    kubectl delete --wait=false --force --grace-period=0 "$@" 2>/dev/null || true
}

force_delete_crds() {
    local pattern="$1"
    local crds
    crds=$(kubectl get crds 2>/dev/null | grep -E "$pattern" | awk '{print $1}')
    if [ -z "$crds" ]; then
        log "No CRDs matching '$pattern' found."
        return 0
    fi
    for crd in $crds; do
        # Skip CRDs that still have instances (someone else might be using them)
        local count
        count=$(kubectl get "$crd" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "$count" -gt 0 ]; then
            warn "CRD $crd still has $count instance(s), skipping deletion."
            continue
        fi
        timeout_delete 15s crd "$crd" || warn "Could not delete CRD: $crd"
    done
}

force_delete_namespace() {
    local ns="$1"
    if ! kubectl get namespace "$ns" &>/dev/null; then
        return 0
    fi
    step "Deleting namespace '$ns'..."
    if kubectl delete namespace "$ns" --timeout=60s 2>/dev/null; then
        return 0
    fi
    warn "Namespace '$ns' stuck in Terminating. Removing finalizers..."
    kubectl get namespace "$ns" -o json \
        | jq '.spec.finalizers = []' \
        | kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null \
        || warn "Could not remove finalizers for '$ns'"
}

# ── Infrastructure Install Functions ──────────────────────────────────────────

check_prerequisites() {
    step "Checking prerequisites..."

    local missing=()
    for cmd in kubectl helm; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        die "Missing required tools: ${missing[*]}. Please install them first."
    fi

    if ! kubectl cluster-info --request-timeout=10s &>/dev/null; then
        die "Cannot connect to Kubernetes cluster. Please ensure you're logged in."
    fi
    log "Connected to cluster: $(kubectl config current-context)"
}

install_crds() {
    step "Installing Gateway API CRDs..."
    if kubectl get crd gateways.gateway.networking.k8s.io &>/dev/null; then
        log "Gateway API CRDs already installed. Skipping."
    else
        kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
        log "Gateway API CRDs installed."
    fi

    step "Installing Inference Extension CRDs..."
    if kubectl get crd inferencepools.inference.networking.k8s.io &>/dev/null; then
        log "GAIE CRDs already installed. Skipping."
    else
        kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${GAIE_VERSION}/manifests.yaml"
        log "GAIE CRDs installed."
    fi
}

install_cert_manager() {
    step "Installing cert-manager ${CERT_MANAGER_VERSION}..."

    if kubectl get deployment cert-manager -n cert-manager &>/dev/null; then
        log "cert-manager is already installed. Skipping."
        return
    fi

    helm repo add jetstack https://charts.jetstack.io --force-update
    helm install cert-manager jetstack/cert-manager \
        --namespace cert-manager \
        --create-namespace \
        --version "${CERT_MANAGER_VERSION}" \
        --set crds.enabled=true

    for deploy in cert-manager cert-manager-webhook cert-manager-cainjector; do
        wait_for_deployment "$deploy" cert-manager 120s
    done

    log "cert-manager installed successfully."
}

install_istio() {
    step "Installing Istio ${ISTIO_VERSION} via istioctl..."

    if kubectl get deployment istiod -n istio-system &>/dev/null; then
        local current_version
        current_version=$(kubectl exec -n istio-system deploy/istiod -- pilot-discovery version 2>/dev/null | grep -oP 'Version:"[^"]*"' | grep -oP '"[^"]*"' | tr -d '"') || true
        if [ "${current_version}" = "${ISTIO_VERSION#v}" ]; then
            log "Istio ${ISTIO_VERSION} already installed. Skipping."
            return
        fi
        warn "Istio ${current_version:-unknown} found, upgrading to ${ISTIO_VERSION}..."
    fi

    local istio_dir="/tmp/istio-${ISTIO_VERSION#v}"
    rm -rf "${istio_dir}"
    step "Downloading Istio ${ISTIO_VERSION}..."
    (cd "/tmp" && curl -sL https://istio.io/downloadIstio | ISTIO_VERSION="${ISTIO_VERSION#v}" sh -)

    step "Installing Istio with GAIE support..."
    local istioctl_args=(
        --set components.ingressGateways[0].enabled=false
        --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true
        --set values.pilot.autoscaleEnabled=false
    )
    if is_openshift; then
        log "OpenShift detected, adding platform=openshift"
        istioctl_args+=(--set values.global.platform=openshift)
    fi
    "${istio_dir}/bin/istioctl" install -y "${istioctl_args[@]}"

    wait_for_deployment istiod istio-system 300s
    log "Istio ${ISTIO_VERSION} installed with GAIE support."
}

create_selfsigned_issuer() {
    step "Creating self-signed ClusterIssuer '${TLS_ISSUER_NAME}'..."
    if kubectl get clusterissuer "${TLS_ISSUER_NAME}" &>/dev/null; then
        log "ClusterIssuer '${TLS_ISSUER_NAME}' already exists. Skipping."
        return
    fi
    kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${TLS_ISSUER_NAME}
spec:
  selfSigned: {}
EOF
    log "ClusterIssuer '${TLS_ISSUER_NAME}' created."
}

create_gateway_cr() {
    step "Creating TLS certificate for Gateway..."
    kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${GATEWAY_NAME}-tls
  namespace: ${INGRESS_NAMESPACE}
spec:
  secretName: ${GATEWAY_NAME}-tls
  issuerRef:
    name: ${TLS_ISSUER_NAME}
    kind: ClusterIssuer
  dnsNames:
  - "*.${INGRESS_NAMESPACE}.svc.cluster.local"
  - localhost
EOF
    kubectl wait --for=condition=Ready --timeout=60s \
        -n "${INGRESS_NAMESPACE}" certificate/${GATEWAY_NAME}-tls || warn "Certificate not ready yet"
    log "Gateway TLS certificate created."

    step "Creating Istio Gateway (HTTPS)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${GATEWAY_NAME}
  namespace: ${INGRESS_NAMESPACE}
  labels:
    kuadrant.io/gateway: "true"
spec:
  gatewayClassName: istio
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
    tls:
      mode: Terminate
      certificateRefs:
      - name: ${GATEWAY_NAME}-tls
    allowedRoutes:
      namespaces:
        from: All
EOF

    wait_for_deployment "${GATEWAY_NAME}-istio" "${INGRESS_NAMESPACE}" 300s

    step "Waiting for Gateway to be programmed..."
    kubectl wait --for=condition=Programmed \
        --timeout=300s \
        -n "${INGRESS_NAMESPACE}" \
        gateway/${GATEWAY_NAME} || warn "Gateway not ready yet"

    log "Gateway created (HTTPS on port 443)."
}

create_batch_destinationrule() {
    step "Creating DestinationRule for backend TLS (Gateway -> apiserver)..."
    kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: ${HELM_RELEASE}-backend-tls
  namespace: ${INGRESS_NAMESPACE}
spec:
  host: ${HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local
  trafficPolicy:
    portLevelSettings:
    - port:
        number: ${BATCH_INFERENCE_PORT}
      tls:
        mode: SIMPLE
        insecureSkipVerify: true
EOF
    log "DestinationRule created (Gateway -> apiserver: TLS re-encrypt)."
}

install_kuadrant() {
    step "Installing Kuadrant Operator ${KUADRANT_VERSION}..."

    if helm status "${KUADRANT_RELEASE}" -n "${KUADRANT_NAMESPACE}" &>/dev/null; then
        log "Kuadrant operator '${KUADRANT_RELEASE}' is already installed. Skipping."
    else
        helm repo add kuadrant https://kuadrant.io/helm-charts/ --force-update
        helm install "${KUADRANT_RELEASE}" kuadrant/kuadrant-operator \
            --version "${KUADRANT_VERSION}" \
            --create-namespace \
            --namespace "${KUADRANT_NAMESPACE}"

        step "Waiting for Kuadrant operator deployments..."
        sleep 30
        for deploy in authorino-operator \
                      kuadrant-operator-controller-manager \
                      limitador-operator-controller-manager; do
            wait_for_deployment "$deploy" "${KUADRANT_NAMESPACE}" 120s
        done
        log "Kuadrant operator installed successfully."
    fi

    # Create Kuadrant instance
    if kubectl get kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" &>/dev/null; then
        if kubectl get deployment authorino -n "${KUADRANT_NAMESPACE}" &>/dev/null \
            && kubectl get deployment limitador-limitador -n "${KUADRANT_NAMESPACE}" &>/dev/null; then
            log "Kuadrant instance already exists with authorino + limitador. Skipping."
            return
        fi
        warn "Kuadrant CR exists but authorino/limitador missing. Recreating..."
        kubectl patch kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        kubectl delete kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" --wait=false 2>/dev/null || true
        sleep 5
    fi

    step "Creating Kuadrant instance..."
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: ${KUADRANT_NAMESPACE}
spec: {}
EOF

    step "Waiting for Kuadrant instance to be ready..."
    for deploy in authorino limitador-limitador; do
        wait_for_deployment "$deploy" "${KUADRANT_NAMESPACE}" 300s
    done
    kubectl wait --timeout=300s -n "${KUADRANT_NAMESPACE}" kuadrant kuadrant --for=condition=Ready=True
    kubectl get kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" -o=jsonpath='{.status.conditions[?(@.type=="Ready")].message}{"\n"}'
    log "Kuadrant instance is ready."
}

# ── Application Install Functions ─────────────────────────────────────────────

# deploy_vllm_sim <deploy-name> <model-name>
# Deploys a vllm-sim (llm-d-inference-sim) instance for demo purposes.
# For production, replace with actual vLLM (e.g. vllm/vllm-openai:latest).
deploy_vllm_sim() {
    local deploy_name="$1"
    local model_name="$2"

    step "Installing vLLM simulator '${deploy_name}' (model: ${model_name}) in namespace '${LLM_NAMESPACE}'..."

    if kubectl get deployment "${deploy_name}" -n "${LLM_NAMESPACE}" &>/dev/null; then
        log "vLLM simulator '${deploy_name}' already exists. Skipping."
        return
    fi

    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${deploy_name}
  namespace: ${LLM_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${deploy_name}
  template:
    metadata:
      labels:
        app: ${deploy_name}
    spec:
      containers:
      - name: vllm-sim
        image: ${VLLM_SIM_IMAGE}
        imagePullPolicy: IfNotPresent
        args:
        - --model
        - ${model_name}
        - --port
        - "8000"
        - --v=5
        ports:
        - containerPort: 8000
          name: http
          protocol: TCP
        resources:
          requests:
            cpu: 10m
---
apiVersion: v1
kind: Service
metadata:
  name: ${deploy_name}
  namespace: ${LLM_NAMESPACE}
  labels:
    app: ${deploy_name}
spec:
  selector:
    app: ${deploy_name}
  ports:
  - name: http
    protocol: TCP
    port: 8000
    targetPort: 8000
  type: ClusterIP
EOF

    wait_for_deployment "${deploy_name}" "${LLM_NAMESPACE}" 120s
    log "vLLM simulator '${deploy_name}' installed (model: ${model_name})."
}

# deploy_inferencepool <release-name> <app-label>
# Deploys a GAIE InferencePool + EPP targeting pods with the given app label
deploy_inferencepool() {
    local release_name="$1"
    local app_label="$2"

    if helm status "${release_name}" -n "${LLM_NAMESPACE}" &>/dev/null; then
        log "GAIE InferencePool '${release_name}' already installed. Skipping."
        return
    fi

    step "Downloading GAIE ${GAIE_VERSION} from GitHub..."
    rm -rf "${GAIE_REPO}"
    curl -sL "https://github.com/kubernetes-sigs/gateway-api-inference-extension/archive/refs/tags/${GAIE_VERSION}.tar.gz" \
        | tar -xz -C /tmp
    log "GAIE ${GAIE_VERSION} downloaded to ${GAIE_REPO}"

    step "Installing GAIE InferencePool '${release_name}' in namespace '${LLM_NAMESPACE}'..."
    helm install "${release_name}" \
        --namespace "${LLM_NAMESPACE}" \
        --dependency-update \
        --set inferencePool.modelServers.matchLabels.app="${app_label}" \
        --set inferencePool.modelServerType=vllm \
        --set provider.name=istio \
        --set experimentalHttpRoute.enabled=false \
        --set inferenceExtension.resources.requests.cpu=100m \
        --set inferenceExtension.resources.requests.memory=128Mi \
        --set inferenceExtension.resources.limits.cpu=500m \
        --set inferenceExtension.resources.limits.memory=512Mi \
        "${GAIE_REPO}/config/charts/inferencepool"

    wait_for_deployment "${release_name}-epp" "${LLM_NAMESPACE}" 300s
    log "GAIE InferencePool '${release_name}' installed (targeting app=${app_label})."
}

create_batch_httproute() {
    step "Creating HTTPRoutes..."

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-route
  namespace: ${BATCH_NAMESPACE}
spec:
  parentRefs:
  - name: ${GATEWAY_NAME}
    namespace: ${INGRESS_NAMESPACE}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /v1/batches
    - path:
        type: PathPrefix
        value: /v1/files
    backendRefs:
    - name: ${BATCH_INFERENCE_SERVICE}
      port: ${BATCH_INFERENCE_PORT}
EOF

    # llm-route is created by each script via create_llm_route
    log "batch-route created (${BATCH_NAMESPACE}): /v1/batches, /v1/files -> ${BATCH_INFERENCE_SERVICE}"
}

# Deploy two vllm-sim instances + two InferencePools
deploy_llm() {
    deploy_vllm_sim "vllm-${FREE_MODEL}" "${FREE_MODEL}"
    deploy_vllm_sim "vllm-${GOLD_MODEL}" "${GOLD_MODEL}"
    deploy_inferencepool "${FREE_MODEL}" "vllm-${FREE_MODEL}"
    deploy_inferencepool "${GOLD_MODEL}" "vllm-${GOLD_MODEL}"
}

# create_model_route <model-name> <inferencepool-name>
# Adds a rule to llm-route for a specific model -> InferencePool mapping
# Call create_llm_route first, then add model routes
create_llm_route() {
    step "Creating llm-route..."

    # Build rules from MODEL_ROUTES array (set by each script)
    # Format: "model-name:inferencepool-name"
    local rules=""
    for entry in "${MODEL_ROUTES[@]}"; do
        local model_name="${entry%%:*}"
        local pool_name="${entry##*:}"
        rules="${rules}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NAMESPACE}/${model_name}/v1/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${pool_name}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NAMESPACE}/${model_name}/v1/chat/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/chat/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${pool_name}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NAMESPACE}/${model_name}
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${pool_name}"
        log "  llm-route rule: /${LLM_NAMESPACE}/${model_name}/* -> InferencePool/${pool_name}"
    done

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: ${LLM_NAMESPACE}
spec:
  parentRefs:
  - name: ${GATEWAY_NAME}
    namespace: ${INGRESS_NAMESPACE}
  rules:${rules}
EOF

    log "llm-route created."
}

# ── Rate Limit Policies (shared by all solutions) ─────────────────────────────

apply_batch_ratelimit_policy() {
    step "Applying batch RateLimitPolicy (request-based, per tier)..."

    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: RateLimitPolicy
metadata:
  name: batch-ratelimit
  namespace: ${BATCH_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-route
  limits:
    "gold-limit":
      rates:
      - limit: 100
        window: 60s
      when:
      - predicate: "auth.identity.tier == 'gold'"
    "free-limit":
      rates:
      - limit: 5
        window: 10s
      when:
      - predicate: "auth.identity.tier == 'free'"
EOF

    log "batch-ratelimit applied (gold: 100 req/min, free: 5 req/10s)."
}

apply_llm_token_ratelimit_policy() {
    step "Applying LLM TokenRateLimitPolicy (token-based, per tier)..."

    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: llm-token-ratelimit
  namespace: ${LLM_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  limits:
    gold:
      rates:
      - limit: 2000
        window: 1m
      when:
      - predicate: "auth.identity.tier == 'gold'"
    free:
      rates:
      - limit: 150
        window: 1m
      when:
      - predicate: "auth.identity.tier == 'free'"
EOF

    log "llm-token-ratelimit applied (gold: 2000 tokens/min, free: 150 tokens/min)."
}

# ── Database / Storage Functions ──────────────────────────────────────────────

install_batch_redis() {
    step "Installing Redis..."
    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
    fi
    helm repo update || warn "Some Helm repo updates failed; continuing."
    if helm status "${REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Redis release '${REDIS_RELEASE}' is already installed. Skipping."
        return
    fi
    helm install "${REDIS_RELEASE}" bitnami/redis \
        --namespace "${BATCH_NAMESPACE}" \
        --set auth.enabled=false \
        --set replica.replicaCount=0 \
        --set master.persistence.enabled=false \
        --wait --timeout 120s
    log "Redis installed successfully."
}

install_batch_postgresql() {
    step "Installing PostgreSQL..."
    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
    fi
    helm repo update || warn "Some Helm repo updates failed; continuing."
    if helm status "${POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "PostgreSQL release '${POSTGRESQL_RELEASE}' is already installed. Skipping."
        return
    fi
    helm install "${POSTGRESQL_RELEASE}" bitnami/postgresql \
        --namespace "${BATCH_NAMESPACE}" \
        --set auth.postgresPassword="${POSTGRESQL_PASSWORD}" \
        --set primary.persistence.enabled=false \
        --wait --timeout 120s
    log "PostgreSQL installed successfully."
}

install_batch_minio() {
    if [ "${STORAGE_TYPE}" != "s3" ]; then return; fi
    step "Installing MinIO (S3-compatible object storage)..."
    if kubectl get deployment "${MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "MinIO '${MINIO_RELEASE}' already exists. Skipping."
        return
    fi
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${MINIO_RELEASE}
  namespace: ${BATCH_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${MINIO_RELEASE}
  template:
    metadata:
      labels:
        app: ${MINIO_RELEASE}
    spec:
      containers:
      - name: minio
        image: quay.io/minio/minio:latest
        args: ["server", "/data", "--console-address", ":9001"]
        env:
        - name: MINIO_ROOT_USER
          value: "${MINIO_ROOT_USER}"
        - name: MINIO_ROOT_PASSWORD
          value: "${MINIO_ROOT_PASSWORD}"
        ports:
        - containerPort: 9000
          name: api
        - containerPort: 9001
          name: console
        resources:
          requests:
            cpu: 10m
---
apiVersion: v1
kind: Service
metadata:
  name: ${MINIO_RELEASE}
  namespace: ${BATCH_NAMESPACE}
spec:
  selector:
    app: ${MINIO_RELEASE}
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
EOF
    wait_for_deployment "${MINIO_RELEASE}" "${BATCH_NAMESPACE}" 120s
    step "Creating MinIO bucket '${MINIO_BUCKET}'..."
    kubectl exec -n "${BATCH_NAMESPACE}" deploy/${MINIO_RELEASE} -- \
        mc alias set local http://localhost:9000 "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}" > /dev/null 2>&1
    kubectl exec -n "${BATCH_NAMESPACE}" deploy/${MINIO_RELEASE} -- \
        mc mb --ignore-existing "local/${MINIO_BUCKET}" > /dev/null 2>&1
    log "MinIO installed successfully."
}

create_batch_pvc() {
    if [ "${STORAGE_TYPE}" != "fs" ]; then return; fi
    step "Creating PVC '${FILES_PVC_NAME}' for file storage..."
    if kubectl get pvc "${FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "PVC '${FILES_PVC_NAME}' already exists. Skipping."
        return
    fi
    local default_sc
    default_sc=$(kubectl get sc -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{end}' 2>/dev/null)
    if [ -z "$default_sc" ]; then
        die "No default StorageClass found."
    fi
    log "Using default StorageClass: ${default_sc}"
    kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${FILES_PVC_NAME}
  namespace: ${BATCH_NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
EOF
    log "PVC '${FILES_PVC_NAME}' created."
}

create_batch_secret() {
    step "Creating app secret '${APP_SECRET_NAME}'..."
    local redis_url="redis://${REDIS_RELEASE}-master.${BATCH_NAMESPACE}.svc.cluster.local:6379/0"
    local postgresql_url="postgresql://postgres:${POSTGRESQL_PASSWORD}@${POSTGRESQL_RELEASE}.${BATCH_NAMESPACE}.svc.cluster.local:5432/postgres"
    local secret_args=(
        --namespace "${BATCH_NAMESPACE}"
        --from-literal=redis-url="${redis_url}"
        --from-literal=postgresql-url="${postgresql_url}"
    )
    if [ "${STORAGE_TYPE}" = "s3" ]; then
        secret_args+=(--from-literal=s3-secret-access-key="${MINIO_ROOT_PASSWORD}")
    fi
    kubectl create secret generic "${APP_SECRET_NAME}" \
        "${secret_args[@]}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log "Secret '${APP_SECRET_NAME}' applied."
}

# install_batch_gateway
# Installs batch-gateway via helm chart. Processor routes inference through Gateway.
install_batch_gateway() {
    step "Installing batch-gateway via Helm..."

    local gw_base="https://${GATEWAY_NAME}-istio.${INGRESS_NAMESPACE}.svc.cluster.local/${LLM_NAMESPACE}"

    local helm_args=(
        --namespace "${BATCH_NAMESPACE}"
        --set "apiserver.image.tag=${DEV_VERSION}"
        --set "processor.image.tag=${DEV_VERSION}"
        --set "global.secretName=${APP_SECRET_NAME}"
        --set "global.databaseType=${DB_TYPE}"
        --set "global.fileClient.type=${STORAGE_TYPE}"
        --set "processor.config.modelGateways.${FREE_MODEL}.url=${gw_base}/${FREE_MODEL}"
        --set "processor.config.modelGateways.${FREE_MODEL}.tlsInsecureSkipVerify=true"
        --set "processor.config.modelGateways.${FREE_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${FREE_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${FREE_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${FREE_MODEL}.maxBackoff=60s"
        --set "processor.config.modelGateways.${GOLD_MODEL}.url=${gw_base}/${GOLD_MODEL}"
        --set "processor.config.modelGateways.${GOLD_MODEL}.tlsInsecureSkipVerify=true"
        --set "processor.config.modelGateways.${GOLD_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${GOLD_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${GOLD_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${GOLD_MODEL}.maxBackoff=60s"
        --set "apiserver.config.batchAPI.passThroughHeaders={Authorization}"
        --set "apiserver.tls.enabled=true"
        --set "apiserver.tls.certManager.enabled=true"
        --set "apiserver.tls.certManager.issuerName=${TLS_ISSUER_NAME}"
        --set "apiserver.tls.certManager.issuerKind=ClusterIssuer"
        --set "apiserver.tls.certManager.dnsNames={${HELM_RELEASE}-apiserver,${HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local,localhost}"
    )

    # Storage-specific helm args
    if [ "${STORAGE_TYPE}" = "s3" ]; then
        local minio_endpoint="http://${MINIO_RELEASE}.${BATCH_NAMESPACE}.svc.cluster.local:9000"
        helm_args+=(
            --set "global.fileClient.s3.endpoint=${minio_endpoint}"
            --set "global.fileClient.s3.region=us-east-1"
            --set "global.fileClient.s3.accessKeyId=${MINIO_ROOT_USER}"
            --set "global.fileClient.s3.prefix=${MINIO_BUCKET}"
            --set "global.fileClient.s3.usePathStyle=true"
        )
    else
        helm_args+=(
            --set "global.fileClient.fs.basePath=/tmp/batch-gateway"
            --set "global.fileClient.fs.pvcName=${FILES_PVC_NAME}"
        )
    fi

    # OpenShift: clear fixed UIDs, let SCC assign them
    if is_openshift; then
        log "OpenShift detected, clearing podSecurityContext for SCC compatibility"
        helm_args+=(
            --set "apiserver.podSecurityContext=null"
            --set "processor.podSecurityContext=null"
        )
    fi

    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

    if helm status "${HELM_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Release '${HELM_RELEASE}' already exists. Upgrading..."
        helm upgrade "${HELM_RELEASE}" "${repo_root}/charts/batch-gateway" "${helm_args[@]}"
    else
        helm install "${HELM_RELEASE}" "${repo_root}/charts/batch-gateway" "${helm_args[@]}"
    fi

    wait_for_deployment "${HELM_RELEASE}-apiserver" "${BATCH_NAMESPACE}" 120s
    wait_for_deployment "${HELM_RELEASE}-processor" "${BATCH_NAMESPACE}" 120s

    log "batch-gateway installed (apiserver + processor)."
}

# deploy_batch_gateway
# Full batch-gateway deployment: databases, storage, helm chart, TLS, and routing.
deploy_batch_gateway() {
    install_batch_redis
    install_batch_postgresql
    install_batch_minio
    create_batch_secret
    create_batch_pvc
    install_batch_gateway
    create_batch_destinationrule
    create_batch_httproute
}

# ── Common Install / Uninstall ────────────────────────────────────────────────

install_with_kuadrant() {
    check_prerequisites

    for ns in "${BATCH_NAMESPACE}" "${LLM_NAMESPACE}" "${INGRESS_NAMESPACE}"; do
        if ! kubectl get namespace "${ns}" &>/dev/null; then
            kubectl create namespace "${ns}"
            log "Created namespace '${ns}'."
        fi
    done

    install_crds
    install_cert_manager
    create_selfsigned_issuer
    install_istio
    install_kuadrant

    create_gateway_cr

    deploy_llm
    create_llm_route
    apply_llm_token_ratelimit_policy

    deploy_batch_gateway
    apply_batch_ratelimit_policy
}

wait_for_auth_policies() {
    step "Waiting for auth policies to propagate..."
    sleep 30

    local batch_auth_ok llm_auth_ok
    batch_auth_ok=$(kubectl get authpolicy batch-auth -n "${BATCH_NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null)
    llm_auth_ok=$(kubectl get authpolicy llm-auth -n "${LLM_NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null)

    if [ "$batch_auth_ok" = "True" ] && [ "$llm_auth_ok" = "True" ]; then
        log "All auth policies enforced."
    else
        warn "Some policies not enforced yet (batch-auth=${batch_auth_ok:-?}, llm-auth=${llm_auth_ok:-?}). Waiting more..."
        sleep 30
    fi
}

cmd_uninstall() {
    # Disable exit-on-error for uninstall — individual cleanup failures should not abort
    set +e

    echo ""
    echo "  ╔══════════════════════════════════╗"
    echo "  ║   Uninstalling All Components    ║"
    echo "  ╚══════════════════════════════════╝"
    echo ""

    step "Stopping port-forward processes..."
    pkill -f "kubectl port-forward.*${GATEWAY_NAME}" 2>/dev/null || true

    step "Removing TLS resources..."
    kubectl delete clusterissuer "${TLS_ISSUER_NAME}" 2>/dev/null || true
    kubectl delete destinationrule "${HELM_RELEASE}-backend-tls" -n "${INGRESS_NAMESPACE}" 2>/dev/null || true

    step "Removing gateway resources (${INGRESS_NAMESPACE})..."
    timeout_delete 30s gateway --all -n "${INGRESS_NAMESPACE}" \
        || warn "No gateway to delete in ${INGRESS_NAMESPACE}"

    step "Removing application resources (${BATCH_NAMESPACE})..."
    timeout_delete 30s httproute,authpolicy,ratelimitpolicy --all -n "${BATCH_NAMESPACE}" \
        || warn "No resources to delete in ${BATCH_NAMESPACE}"

    step "Uninstalling batch-gateway..."
    helm uninstall "${HELM_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true

    step "Uninstalling Redis..."
    helm uninstall "${REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true

    step "Uninstalling PostgreSQL..."
    helm uninstall "${POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true

    step "Uninstalling MinIO..."
    kubectl delete deployment,svc -l app="${MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true

    step "Deleting PVC..."
    kubectl delete pvc "${FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true

    step "Removing LLM resources (${LLM_NAMESPACE})..."
    timeout_delete 30s httproute,authpolicy,tokenratelimitpolicy --all -n "${LLM_NAMESPACE}" \
        || warn "No policies to delete in ${LLM_NAMESPACE}"
    step "Uninstalling GAIE InferencePools..."
    for release in $(helm list -n "${LLM_NAMESPACE}" -q 2>/dev/null); do
        helm uninstall "${release}" -n "${LLM_NAMESPACE}" --timeout 60s 2>/dev/null || warn "Failed to uninstall ${release}"
    done
    timeout_delete 30s inferencepool --all -n "${LLM_NAMESPACE}" || warn "No InferencePool to delete"
    timeout_delete 30s deployment,svc --all -n "${LLM_NAMESPACE}" \
        || warn "No resources to delete in ${LLM_NAMESPACE}"

    # Auth-mode specific cleanup (override in each script if needed)
    cleanup_auth_resources 2>/dev/null || true

    step "Uninstalling Kuadrant..."
    timeout_delete 30s kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" || warn "Kuadrant instance not found"
    helm uninstall "${KUADRANT_RELEASE}" -n "${KUADRANT_NAMESPACE}" --timeout 60s 2>/dev/null || warn "Kuadrant not installed"
    force_delete_namespace "${KUADRANT_NAMESPACE}"

    step "Cleaning up Kuadrant CRDs..."
    force_delete_crds 'kuadrant|authorino|limitador'

    step "Uninstalling Istio..."
    local istio_dir="/tmp/istio-${ISTIO_VERSION#v}"
    if [ -d "${istio_dir}" ]; then
        "${istio_dir}/bin/istioctl" uninstall --purge -y 2>/dev/null || warn "istioctl uninstall failed"
    else
        kubectl delete deploy,svc --all -n istio-system 2>/dev/null || true
    fi

    step "Cleaning up GAIE CRDs..."
    force_delete_crds 'inference\.networking'

    step "Cleaning up Istio CRDs..."
    force_delete_crds 'istio\.io|sail'
    force_delete_namespace "istio-system"

    step "Uninstalling cert-manager..."
    helm uninstall cert-manager -n cert-manager --timeout 60s 2>/dev/null || warn "cert-manager not installed"
    kubectl delete -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" --timeout=30s 2>/dev/null || true
    force_delete_crds 'cert-manager'
    force_delete_namespace "cert-manager"

    for ns in "${BATCH_NAMESPACE}" "${LLM_NAMESPACE}" "${INGRESS_NAMESPACE}"; do
        if [ "${ns}" != "default" ]; then
            force_delete_namespace "${ns}"
        fi
    done

    step "Removing Gateway API CRDs..."
    kubectl delete -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" --timeout=30s 2>/dev/null \
        || warn "Gateway API CRDs not found"

    echo ""
    log "Uninstallation complete!"

    # Re-enable exit-on-error
    set -e
}

# ── Test Helpers ──────────────────────────────────────────────────────────────

init_test() {
    local test_title="$1"
    local header="  Testing: ${test_title}  "
    local width=${#header}
    local border=""
    for ((i=0; i<width; i++)); do border+="═"; done
    echo ""
    echo "  ╔${border}╗"
    echo "  ║${header}║"
    echo "  ╚${border}╝"
    echo ""

    if ! kubectl get gateway "${GATEWAY_NAME}" -n "${INGRESS_NAMESPACE}" &>/dev/null; then
        die "Gateway '${GATEWAY_NAME}' not found in namespace '${INGRESS_NAMESPACE}'. Run '$0 install' first."
    fi

    if ! pgrep -f "kubectl port-forward.*${GATEWAY_NAME}" >/dev/null; then
        start_gateway_port_forward
        sleep 3
    else
        log "Port-forward already running."
    fi

    step "Waiting for gateway to be accessible..."
    local base_url="${GATEWAY_URL:-https://localhost:${LOCAL_PORT}}"
    local retries=30
    for i in $(seq 1 "${retries}"); do
        if curl -sk -o /dev/null -w "%{http_code}" "${base_url}/${LLM_NAMESPACE}/${E2E_MODEL}/v1/chat/completions" &>/dev/null; then
            log "Gateway is accessible."
            break
        fi
        if [ "$i" -eq "${retries}" ]; then
            warn "Gateway not accessible after ${retries} attempts. Tests may fail."
        fi
        sleep 2
    done
}

print_test_summary() {
    local test_total="$1"
    local test_passed="$2"
    local test_failed="$3"
    local failed_tests="$4"

    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Test Summary"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""
    if [ "$test_failed" -eq 0 ]; then
        log "All ${test_total} tests passed!"
    else
        warn "${test_failed}/${test_total} tests failed:"
        echo -e "${failed_tests}"
        echo ""
    fi
    echo "  Passed: ${test_passed}  Failed: ${test_failed}  Total: ${test_total}"
    echo ""
}

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Commands:"
    echo "  install    Deploy all components (infrastructure + application + policies)"
    echo "  test       Run integration tests against the deployed environment"
    echo "  uninstall  Remove all deployed components"
    echo "  help       Show this help message"
    exit "${1:-0}"
}
