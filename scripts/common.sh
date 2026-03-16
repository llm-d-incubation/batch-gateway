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
BATCH_INFERENCE_SERVICE="${BATCH_INFERENCE_SERVICE:-batch-inference}"
BATCH_INFERENCE_PORT="${BATCH_INFERENCE_PORT:-80}"
GATEWAY_NAME="${GATEWAY_NAME:-istio-gateway}"
LOCAL_PORT="${LOCAL_PORT:-8080}"

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

    step "Starting port-forward: ${gateway_svc} ${LOCAL_PORT}:80 -n ${INGRESS_NAMESPACE}..."

    kubectl port-forward "svc/${gateway_svc}" "${LOCAL_PORT}:80" -n "${INGRESS_NAMESPACE}" &
    local pf_pid=$!
    disown "${pf_pid}"

    log "Port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"
    log "Gateway available at http://localhost:${LOCAL_PORT}"
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

create_gateway() {
    step "Creating Istio Gateway..."

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
  - name: http
    protocol: HTTP
    port: 80
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

    kubectl get gateway "${GATEWAY_NAME}" -n "${INGRESS_NAMESPACE}" \
        -o=jsonpath='{.status.conditions[?(@.type=="Accepted")].message}{"\n"}{.status.conditions[?(@.type=="Programmed")].message}{"\n"}'

    log "Gateway created."
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


# NOTE: This deploys a simplified nginx stub for demo purposes only.
# It proxies batch requests to the LLM route to demonstrate Kuadrant integration
# (auth, rate limiting, routing). For production, replace with the actual batch-gateway service.
install_batch_inference() {
    step "Installing batch inference service '${BATCH_INFERENCE_SERVICE}' (demo stub)..."

    if kubectl get deployment "${BATCH_INFERENCE_SERVICE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Batch inference service '${BATCH_INFERENCE_SERVICE}' already exists. Skipping."
        return
    fi

    local gateway_upstream="http://${GATEWAY_NAME}-istio.${INGRESS_NAMESPACE}.svc.cluster.local"

    local dns_resolver
    if kubectl get svc dns-default -n openshift-dns &>/dev/null; then
        dns_resolver="dns-default.openshift-dns.svc.cluster.local"
    else
        dns_resolver="kube-dns.kube-system.svc.cluster.local"
    fi
    log "Using DNS resolver: ${dns_resolver}"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${BATCH_INFERENCE_SERVICE}-config
  namespace: ${BATCH_NAMESPACE}
data:
  nginx.conf: |
    pid /tmp/nginx.pid;

    events {
      worker_connections 1024;
    }

    http {
      client_body_temp_path /tmp/client_temp;
      proxy_temp_path /tmp/proxy_temp;
      fastcgi_temp_path /tmp/fastcgi_temp;
      uwsgi_temp_path /tmp/uwsgi_temp;
      scgi_temp_path /tmp/scgi_temp;

      log_format detailed '\$remote_addr - \$remote_user [\$time_local] '
                         '"\$request" \$status \$body_bytes_sent '
                         'tier:\$http_x_tier user:\$http_x_username group:\$http_x_group '
                         'body:\$request_body';

      access_log /dev/stdout detailed;
      error_log /dev/stderr info;

      server {
        listen 8000;

        resolver ${dns_resolver} valid=10s;

        set \$upstream ${gateway_upstream};

        # POST /v1/batches -> forward to GAIE for LLM inference
        location /v1/batches {
          # GET -> return dummy response
          if (\$request_method = GET) {
            add_header Content-Type application/json;
            return 200 '{"status":"ok","message":"batch list (dummy)"}';
          }

          client_body_buffer_size 128k;
          client_max_body_size 10m;

          rewrite ^/v1/batches(.*)\$ /${LLM_NAMESPACE}/${E2E_MODEL}/v1/chat/completions break;
          proxy_pass \$upstream;
          proxy_http_version 1.1;
          proxy_set_header Host \$host;
          proxy_set_header X-Real-IP \$remote_addr;
          proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
          proxy_set_header X-Forwarded-Proto \$scheme;
          proxy_pass_request_headers on;
        }

        # Default: pass through to gateway
        location / {
          client_body_buffer_size 128k;
          client_max_body_size 10m;

          proxy_pass \$upstream;
          proxy_http_version 1.1;
          proxy_set_header Host \$host;
          proxy_set_header X-Real-IP \$remote_addr;
          proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
          proxy_set_header X-Forwarded-Proto \$scheme;
          proxy_pass_request_headers on;
        }
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${BATCH_INFERENCE_SERVICE}
  namespace: ${BATCH_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${BATCH_INFERENCE_SERVICE}
  template:
    metadata:
      labels:
        app: ${BATCH_INFERENCE_SERVICE}
    spec:
      containers:
      - name: nginx
        image: nginxinc/nginx-unprivileged:alpine
        ports:
        - containerPort: 8000
          name: http
          protocol: TCP
        volumeMounts:
        - name: config
          mountPath: /etc/nginx/nginx.conf
          subPath: nginx.conf
        resources:
          requests:
            cpu: 10m
      volumes:
      - name: config
        configMap:
          name: ${BATCH_INFERENCE_SERVICE}-config
---
apiVersion: v1
kind: Service
metadata:
  name: ${BATCH_INFERENCE_SERVICE}
  namespace: ${BATCH_NAMESPACE}
  labels:
    app: ${BATCH_INFERENCE_SERVICE}
spec:
  selector:
    app: ${BATCH_INFERENCE_SERVICE}
  ports:
  - name: http
    protocol: TCP
    port: 80
    targetPort: 8000
  type: ClusterIP
EOF

    wait_for_deployment "${BATCH_INFERENCE_SERVICE}" "${BATCH_NAMESPACE}" 120s
    log "Batch inference service installed (upstream: gateway llm-route)."
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
    install_istio
    install_kuadrant

    create_gateway

    deploy_llm
    create_llm_route
    apply_llm_token_ratelimit_policy

    install_batch_inference
    create_batch_httproute
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

    step "Removing gateway resources (${INGRESS_NAMESPACE})..."
    timeout_delete 30s gateway --all -n "${INGRESS_NAMESPACE}" \
        || warn "No gateway to delete in ${INGRESS_NAMESPACE}"

    step "Removing application resources (${BATCH_NAMESPACE})..."
    timeout_delete 30s httproute,authpolicy,ratelimitpolicy --all -n "${BATCH_NAMESPACE}" \
        || warn "No resources to delete in ${BATCH_NAMESPACE}"
    timeout_delete 30s deployment,svc,configmap -l app="${BATCH_INFERENCE_SERVICE}" -n "${BATCH_NAMESPACE}" \
        || warn "No batch-inference resources to delete"

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
    local base_url="${GATEWAY_URL:-http://localhost:${LOCAL_PORT}}"
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

# verify_nginx_headers <expect_tier> <expect_username> [expect_group]
# Checks nginx logs for injected identity headers.
# expect_group is optional; if omitted, only checks presence (non-empty).
# Sets HEADER_CHECK_OK=true/false for the caller to use.
verify_nginx_headers() {
    local expect_tier="$1"
    local expect_username="$2"
    local expect_group="${3:-}"

    sleep 1
    local nginx_log
    nginx_log=$(kubectl logs -n "${BATCH_NAMESPACE}" -l app="${BATCH_INFERENCE_SERVICE}" --tail=5 2>/dev/null)
    echo "  Checking nginx logs for headers..."
    HEADER_CHECK_OK=true

    if echo "$nginx_log" | grep -q "tier:${expect_tier}"; then
        echo "  x-tier: ${expect_tier} ✓"
    else
        echo "  x-tier: ${expect_tier} ✗ (not found)"
        HEADER_CHECK_OK=false
    fi

    if echo "$nginx_log" | grep -q "user:.*${expect_username}"; then
        echo "  x-username: contains ${expect_username} ✓"
    else
        echo "  x-username: ${expect_username} ✗ (not found)"
        HEADER_CHECK_OK=false
    fi

    if [ -n "$expect_group" ]; then
        if echo "$nginx_log" | grep -q "group:${expect_group}"; then
            echo "  x-group: ${expect_group} ✓"
        else
            echo "  x-group: ${expect_group} ✗ (not found)"
            HEADER_CHECK_OK=false
        fi
    else
        if echo "$nginx_log" | grep -q "group:.\+"; then
            echo "  x-group: present ✓"
        else
            echo "  x-group: ✗ (not found)"
            HEADER_CHECK_OK=false
        fi
    fi
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
