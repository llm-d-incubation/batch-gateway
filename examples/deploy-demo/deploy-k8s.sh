#!/bin/bash
set -euo pipefail

# ── Deploy batch-gateway on Kubernetes / OpenShift ─────────────────────────────
#
# Works on both vanilla Kubernetes and OpenShift. All components are installed
# from open-source Helm charts (not OLM / OperatorHub), making this script
# platform-agnostic.
#
# Installs llm-d stack (Istio + GAIE InferencePool + vllm-sim) and batch-gateway.
#   - Batch Gateway exposed via istio-ingress (HTTPS:443)
#   - Kuadrant for auth (kubernetesTokenReview) and rate limiting
#   - cert-manager (Helm) + Redis/PostgreSQL + batch-gateway helm chart
#
# Single simulated model for demo/testing.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

source "${SCRIPT_DIR}/common.sh"

# ── Configuration ─────────────────────────────────────────────────────────────
GATEWAY_NAME="${GATEWAY_NAME:-istio-gateway}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-istio-ingress}"

LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"

KUADRANT_NAMESPACE="${KUADRANT_NAMESPACE:-kuadrant-system}"
KUADRANT_VERSION="${KUADRANT_VERSION:-1.3.1}"
KUADRANT_RELEASE="${KUADRANT_RELEASE:-kuadrant-operator}"

CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.15.3}"

GATEWAY_LOCAL_PORT="${GATEWAY_LOCAL_PORT:-8080}"

LLMD_VERSION="${LLMD_VERSION:-main}"
LLMD_GIT_DIR="/tmp/llm-d-${LLMD_VERSION}"
LLMD_RELEASE_POSTFIX="${LLMD_RELEASE_POSTFIX:-llmd}"

# Model name matches the simulated-accelerators helmfile default ("random")
MODEL_NAME="${MODEL_NAME:-random}"
LLMD_POOL_NAME="gaie-${LLMD_RELEASE_POSTFIX}"

MODEL_ROUTES=(
    "${MODEL_NAME}:${LLMD_POOL_NAME}"
)

# ── Infrastructure ────────────────────────────────────────────────────────────

install_cert_manager() {
    step "Installing cert-manager ${CERT_MANAGER_VERSION}..."

    if helm status cert-manager -n cert-manager &>/dev/null; then
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
        wait_for_deployment "$deploy" cert-manager 180s
    done

    log "cert-manager installed successfully."
}

create_k8s_gateway() {
    step "Creating TLS certificate for Gateway..."
    kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${GATEWAY_NAME}-tls
  namespace: ${GATEWAY_NAMESPACE}
spec:
  secretName: ${GATEWAY_NAME}-tls
  issuerRef:
    name: ${TLS_ISSUER_NAME}
    kind: ClusterIssuer
  dnsNames:
  - "*.${GATEWAY_NAMESPACE}.svc.cluster.local"
  - localhost
EOF
    kubectl wait --for=condition=Ready --timeout=60s \
        -n "${GATEWAY_NAMESPACE}" certificate/${GATEWAY_NAME}-tls \
        || die "TLS certificate '${GATEWAY_NAME}-tls' not ready after 60s. Check cert-manager logs."
    log "Gateway TLS certificate created."

    step "Creating Istio Gateway (HTTP + HTTPS)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${GATEWAY_NAME}
  namespace: ${GATEWAY_NAMESPACE}
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
        from: Selector
        selector:
          matchLabels:
            llm-d.ai/gateway-route: "true"
  - name: https
    protocol: HTTPS
    port: 443
    tls:
      mode: Terminate
      certificateRefs:
      - name: ${GATEWAY_NAME}-tls
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            llm-d.ai/gateway-route: "true"
EOF

    wait_for_deployment "${GATEWAY_NAME}-istio" "${GATEWAY_NAMESPACE}" 300s

    step "Waiting for Gateway to be programmed..."
    kubectl wait --for=condition=Programmed \
        --timeout=300s \
        -n "${GATEWAY_NAMESPACE}" \
        gateway/${GATEWAY_NAME} \
        || die "Gateway '${GATEWAY_NAME}' not programmed after 300s. Check Istio logs."

    log "Gateway created (HTTPS on port 443)."
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
            wait_for_deployment "$deploy" "${KUADRANT_NAMESPACE}" 180s
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

create_llm_route() {
    step "Creating llm-route..."

    # Build rules from MODEL_ROUTES array
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
    namespace: ${GATEWAY_NAMESPACE}
  rules:${rules}
EOF

    log "llm-route created."
}

init_test() {
    local test_title="$1"
    banner "Testing: ${test_title}"

    if ! kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" &>/dev/null; then
        die "Gateway '${GATEWAY_NAME}' not found in namespace '${GATEWAY_NAMESPACE}'. Run '$0 install' first."
    fi

    # Resolve gateway URL
    set_gateway_url

    step "Waiting for gateway to be accessible..."
    local retries=30
    for i in $(seq 1 "${retries}"); do
        if curl -sk -o /dev/null -w "%{http_code}" "${GATEWAY_URL}/" &>/dev/null; then
            log "Gateway is accessible."
            break
        fi
        if [ "$i" -eq "${retries}" ]; then
            die "Gateway not accessible after ${retries} attempts."
        fi
        sleep 2
    done
}

# ── llm-d stack: Istio + GAIE + vllm-sim ─────────────────────────────────────

install_llmd_deps() {
    step "Installing llm-d dependencies (CRDs + Istio)..."

    rm -rf "${LLMD_GIT_DIR}"
    git clone --depth 1 --branch "${LLMD_VERSION}" https://github.com/llm-d/llm-d.git "${LLMD_GIT_DIR}"

    local llmd_dir="${LLMD_GIT_DIR}"

    # CRDs (Gateway API + GAIE)
    # On OpenShift, Gateway API CRDs are managed by the Ingress Operator and may
    # reject external modifications. Skip on failure.
    step "Installing CRDs..."
    bash "${llmd_dir}/guides/prereq/gateway-provider/install-gateway-provider-dependencies.sh" \
        || warn "CRD install failed (may already exist on OpenShift). Continuing."

    # Istio (via helmfile)
    step "Installing Istio..."
    helmfile apply -f "${llmd_dir}/guides/prereq/gateway-provider/istio.helmfile.yaml"

    log "llm-d dependencies installed (CRDs + Istio)."
}

deploy_llmd_model() {
    step "Deploying model with llm-d..."

    local llmd_dir="${LLMD_GIT_DIR}"
    local sim_dir="${llmd_dir}/guides/simulated-accelerators"

    # Reduce resource requests — defaults (4 CPU) exceed demo/CI cluster capacity.
    local istio_config="${llmd_dir}/guides/prereq/gateway-provider/common-configurations/istio.yaml"
    # Disable llm-d's own gateway — we use our own istio-gateway with AuthPolicy
    yq -i '.gateway.enabled = false' "${istio_config}"
    yq -i '.inferenceExtension.resources = {"requests": {"cpu": "500m", "memory": "512Mi"}, "limits": {"cpu": "2", "memory": "1Gi"}}' \
        "${sim_dir}/gaie-sim/values.yaml"
    RELEASE_NAME_POSTFIX="${LLMD_RELEASE_POSTFIX}" \
        helmfile apply -f "${sim_dir}/helmfile.yaml.gotmpl" -e istio -n "${LLM_NAMESPACE}"

    # Wait for llm-d deployments
    wait_for_deployment "${LLMD_POOL_NAME}-epp" "${LLM_NAMESPACE}" 300s
    wait_for_deployment "ms-${LLMD_RELEASE_POSTFIX}-llm-d-modelservice-decode" "${LLM_NAMESPACE}" 300s

    log "llm-d model deployed."
}

uninstall_llmd() {
    step "Removing llm-d stack (${LLM_NAMESPACE})..."
    timeout_delete 30s httproute --all -n "${LLM_NAMESPACE}" || true
    local sim_helmfile="${LLMD_GIT_DIR}/guides/simulated-accelerators/helmfile.yaml.gotmpl"
    if [ -f "${sim_helmfile}" ]; then
        RELEASE_NAME_POSTFIX="${LLMD_RELEASE_POSTFIX}" \
            helmfile destroy -f "${sim_helmfile}" -e istio -n "${LLM_NAMESPACE}" 2>/dev/null \
            || warn "helmfile destroy failed"
    else
        helm uninstall "ms-${LLMD_RELEASE_POSTFIX}" -n "${LLM_NAMESPACE}" --timeout 60s 2>/dev/null || true
        helm uninstall "${LLMD_POOL_NAME}" -n "${LLM_NAMESPACE}" --timeout 60s 2>/dev/null || true
        helm uninstall "infra-${LLMD_RELEASE_POSTFIX}" -n "${LLM_NAMESPACE}" --timeout 60s 2>/dev/null || true
    fi
    timeout_delete 30s inferencepool --all -n "${LLM_NAMESPACE}" || true
}

# ── Batch Gateway ─────────────────────────────────────────────────────────────

deploy_batch_gateway_k8s() {
    banner "Installing Batch Gateway"

    # Route through the main istio-gateway (which has AuthPolicy) so that
    # batch processor requests are subject to LLM route authorization.
    local main_gw="https://${GATEWAY_NAME}-istio.${GATEWAY_NAMESPACE}.svc.cluster.local/${LLM_NAMESPACE}"

    local helm_args=(
        --set "processor.config.modelGateways.${MODEL_NAME}.url=${main_gw}/${MODEL_NAME}"
        --set "processor.config.modelGateways.${MODEL_NAME}.requestTimeout=${GW_REQUEST_TIMEOUT}"
        --set "processor.config.modelGateways.${MODEL_NAME}.maxRetries=${GW_MAX_RETRIES}"
        --set "processor.config.modelGateways.${MODEL_NAME}.initialBackoff=${GW_INITIAL_BACKOFF}"
        --set "processor.config.modelGateways.${MODEL_NAME}.maxBackoff=${GW_MAX_BACKOFF}"
        --set "apiserver.config.batchAPI.passThroughHeaders={Authorization}"
    )
    if [[ "${DEMO_TLS_INSECURE_SKIP_VERIFY}" == "1" ]]; then
        helm_args+=(--set "processor.config.modelGateways.${MODEL_NAME}.tlsInsecureSkipVerify=true")
        warn "DEMO_TLS_INSECURE_SKIP_VERIFY=1: processor TLS verify disabled for model gateway (demo/lab only)."
    fi

    do_deploy_batch_gateway "${helm_args[@]}"
}

# ── Auth & Rate Limit Policies ────────────────────────────────────────────────
apply_llm_auth_policy() {
    step "Creating llm-route AuthPolicy (authentication + model-level authorization)..."
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: llm-route-auth
  namespace: ${LLM_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  rules:
    authentication:
      kubernetes-user:
        kubernetesTokenReview:
          audiences:
          - https://kubernetes.default.svc
    authorization:
      model-access:
        kubernetesSubjectAccessReview:
          user:
            expression: auth.identity.user.username
          authorizationGroups:
            expression: auth.identity.user.groups
          resourceAttributes:
            group:
              value: inference.networking.k8s.io
            resource:
              value: inferencepools
            namespace:
              expression: request.path.split("/")[1]
            name:
              expression: request.path.split("/")[2]
            verb:
              value: get
EOF
    log "LLM AuthPolicy applied."
}

apply_llm_token_rate_limit() {
    step "Creating TokenRateLimitPolicy for inference (500 tokens/1m per user)..."
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: inference-token-limit
  namespace: ${LLM_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  limits:
    per-user:
      rates:
      - limit: 500
        window: 1m
      when:
      - predicate: request.path.endsWith("/v1/chat/completions")
      counters:
      - expression: auth.identity.user.username
EOF

    step "Waiting for TokenRateLimitPolicy to be enforced..."
    kubectl wait tokenratelimitpolicy/inference-token-limit \
        --for="condition=Enforced=true" \
        -n "${LLM_NAMESPACE}" --timeout=180s 2>/dev/null \
        || die "TokenRateLimitPolicy not enforced after 180s."

    log "TokenRateLimitPolicy applied."
}

apply_batch_auth_policy() {
    step "Creating batch-route AuthPolicy (authentication only)..."
    # No authorization here; model-level authorization happens when batch processor
    # forwards requests to the LLM route.
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-route-auth
  namespace: ${BATCH_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-route
  rules:
    authentication:
      kubernetes-user:
        kubernetesTokenReview:
          audiences:
          - https://kubernetes.default.svc
EOF
    log "Batch AuthPolicy applied."
}

apply_batch_request_rate_limit() {
    step "Creating batch-route RateLimitPolicy (20 req/1m per user)..."
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
    per-user:
      rates:
      - limit: 20
        window: 1m
      counters:
      - expression: auth.identity.user.username
EOF
    log "RateLimitPolicy applied (20 req/min per user)."
}

# ── Prerequisite checks ─────────────────────────────────────────────────────

check_all_prerequisites() {
    local missing=()
    for cmd in kubectl helm helmfile git curl jq yq; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        die "Missing required tools: ${missing[*]}."
    fi

    if ! helm plugin list 2>/dev/null | grep -q '^diff'; then
        die "Missing helm-diff plugin. Install with: helm plugin install https://github.com/databus23/helm-diff"
    fi

    if ! kubectl cluster-info --request-timeout=10s &>/dev/null; then
        die "Cannot connect to Kubernetes cluster."
    fi
    log "Connected to cluster: $(kubectl config current-context)"
}

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    banner "llm-d + Batch Gateway Setup"

    check_all_prerequisites

    local namespaces=("${BATCH_NAMESPACE}" "${LLM_NAMESPACE}" "${GATEWAY_NAMESPACE}")
    for ns in "${namespaces[@]}"; do
        if ! kubectl get namespace "${ns}" &>/dev/null; then
            kubectl create namespace "${ns}"
            log "Created namespace '${ns}'."
        fi
    done

    for ns in "${BATCH_NAMESPACE}" "${LLM_NAMESPACE}"; do
        kubectl label namespace "${ns}" llm-d.ai/gateway-route=true --overwrite
    done

    install_cert_manager
    create_selfsigned_issuer
    install_llmd_deps
    install_kuadrant

    create_k8s_gateway

    deploy_llmd_model
    create_llm_route
    apply_llm_auth_policy
    apply_llm_token_rate_limit

    deploy_batch_gateway_k8s
    apply_batch_auth_policy
    apply_batch_request_rate_limit

    log "Deployment complete!"
    if [ -n "${BATCH_RELEASE_VERSION}" ]; then
        log "  Batch Gateway version: ${BATCH_RELEASE_VERSION} (OCI chart)"
    elif [ "${BATCH_DEV_VERSION}" != "local" ]; then
        log "  Batch Gateway image tag: ${BATCH_DEV_VERSION} (commit chart)"
    else
        log "  Batch Gateway image tag: latest (local chart)"
    fi
    log "Run '$0 test' to verify."
}

# ── Test ─────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "Batch Gateway (llm-d)"

    # Auth setup: create SA + RBAC + token
    local sa_name="test-authorized-sa"
    log "Creating ServiceAccount '${sa_name}' for testing..."
    kubectl create serviceaccount "${sa_name}" -n "${LLM_NAMESPACE}" 2>/dev/null || true

    # Grant permission to access the model via SubjectAccessReview
    kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: model-access
  namespace: ${LLM_NAMESPACE}
rules:
- apiGroups: ["inference.networking.k8s.io"]
  resources: ["inferencepools"]
  resourceNames: ["${MODEL_NAME}"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: model-access-binding
  namespace: ${LLM_NAMESPACE}
subjects:
- kind: ServiceAccount
  name: ${sa_name}
  namespace: ${LLM_NAMESPACE}
roleRef:
  kind: Role
  name: model-access
  apiGroup: rbac.authorization.k8s.io
EOF
    local token
    token=$(kubectl create token "${sa_name}" -n "${LLM_NAMESPACE}" \
        --audience=https://kubernetes.default.svc --duration=10m) \
        || die "Failed to create token for SA '${sa_name}'"
    [[ "${token}" == ey* ]] || die "Token for SA '${sa_name}' doesn't look like a valid JWT"

    # Create unauthorized SA (no RBAC bindings)
    local unauth_sa="test-unauthorized-sa"
    kubectl create serviceaccount "${unauth_sa}" -n "${LLM_NAMESPACE}" 2>/dev/null || true
    local unauth_token
    unauth_token=$(kubectl create token "${unauth_sa}" -n "${LLM_NAMESPACE}" \
        --audience=https://kubernetes.default.svc --duration=10m) \
        || die "Failed to create token for SA '${unauth_sa}'"
    [[ "${unauth_token}" == ey* ]] || die "Token for SA '${unauth_sa}' doesn't look like a valid JWT"

    local llm_url="${GATEWAY_URL}/${LLM_NAMESPACE}/${MODEL_NAME}/v1/chat/completions"
    local inference_payload="{\"model\":\"${MODEL_NAME}\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}],\"max_tokens\":10}"

    run_tests "${llm_url}" "${GATEWAY_URL}" "${MODEL_NAME}" \
        "Authorization: Bearer ${token}" \
        "Authorization: Bearer ${unauth_token}" \
        "${inference_payload}"

}

# ── Uninstall ────────────────────────────────────────────────────────────────

cmd_uninstall() {
    set +e

    banner "Uninstalling All Components"

    step "Stopping port-forward processes..."
    pkill -f "kubectl port-forward.*${GATEWAY_NAME}" 2>/dev/null || true

    step "Removing test resources..."
    kubectl delete role model-access -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete rolebinding model-access-binding -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete serviceaccount test-authorized-sa -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete serviceaccount test-unauthorized-sa -n "${LLM_NAMESPACE}" 2>/dev/null || true

    step "Removing policies..."
    kubectl delete ratelimitpolicy batch-ratelimit -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete authpolicy batch-route-auth -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete authpolicy llm-route-auth -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete tokenratelimitpolicy inference-token-limit -n "${LLM_NAMESPACE}" 2>/dev/null || true

    step "Removing batch resources (${BATCH_NAMESPACE})..."
    timeout_delete 30s httproute --all -n "${BATCH_NAMESPACE}" || true
    helm uninstall "${BATCH_HELM_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    kubectl delete deployment,svc -l app="${BATCH_MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete pvc "${BATCH_FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true

    step "Removing batch Gateway (${GATEWAY_NAMESPACE})..."
    timeout_delete 30s gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" || true
    kubectl delete destinationrule "${BATCH_HELM_RELEASE}-backend-tls" -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    if is_demo_uninstall_all; then
        step "Removing Kuadrant..."
        timeout_delete 30s kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" || true
        helm uninstall "${KUADRANT_RELEASE}" -n "${KUADRANT_NAMESPACE}" --timeout 60s 2>/dev/null || true
        force_delete_crds 'kuadrant|authorino|limitador'
        force_delete_namespace "${KUADRANT_NAMESPACE}"

        step "Removing llm-d stack (${LLM_NAMESPACE})..."
        uninstall_llmd

        step "Uninstalling Istio..."
        local istio_helmfile="${LLMD_GIT_DIR}/guides/prereq/gateway-provider/istio.helmfile.yaml"
        if [ -f "${istio_helmfile}" ]; then
            helmfile destroy -f "${istio_helmfile}" 2>/dev/null \
                || warn "helmfile destroy failed"
        fi
        helm uninstall istiod -n istio-system --timeout 60s 2>/dev/null || true
        helm uninstall istio-base -n istio-system --timeout 60s 2>/dev/null || true
        force_delete_crds 'istio\.io|sail'
        force_delete_namespace "istio-system"

        step "Removing CRDs..."
        local crd_script="${LLMD_GIT_DIR}/guides/prereq/gateway-provider/install-gateway-provider-dependencies.sh"
        [ -f "${crd_script}" ] && bash "${crd_script}" delete 2>/dev/null || true

        step "Cleaning up cache..."
        rm -rf "${LLMD_GIT_DIR}"

        step "Removing TLS resources..."
        kubectl delete clusterissuer "${TLS_ISSUER_NAME}" 2>/dev/null || true

        step "Uninstalling cert-manager..."
        helm uninstall cert-manager -n cert-manager --timeout 60s 2>/dev/null || true
        force_delete_crds 'cert-manager'
        force_delete_namespace "cert-manager"

        for ns in "${BATCH_NAMESPACE}" "${LLM_NAMESPACE}" "${GATEWAY_NAMESPACE}"; do
            [ "${ns}" != "default" ] && force_delete_namespace "${ns}"
        done
    else
        warn "Skipping Kuadrant, llm-d, Istio, cert-manager, CRD teardown, and deletes for '${LLM_NAMESPACE}' / '${GATEWAY_NAMESPACE}' (shared-cluster safety)."
        warn "For full teardown on an ephemeral demo cluster only: UNINSTALL_ALL=1 $0 uninstall"
        step "Removing batch namespace (${BATCH_NAMESPACE})..."
        force_delete_namespace "${BATCH_NAMESPACE}"
    fi

    echo ""
    log "Uninstallation complete!"

    set -e
}

# ── Usage ────────────────────────────────────────────────────────────────────

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Deploy batch-gateway with llm-d on vanilla Kubernetes."
    echo ""
    echo "Commands:"
    echo "  install    Deploy llm-d stack + Kuadrant + batch-gateway"
    echo "  test       Run auth, batch lifecycle, and rate limit tests"
    echo "  uninstall  Remove demo resources (use UNINSTALL_ALL=1 for full stack teardown)"
    echo "  help       Show this help"
    echo ""
    echo "Environment Variables:"
    echo "  MODEL_NAME             Model name for routing (default: random)"
    echo "  LLMD_VERSION           llm-d git ref (default: main)"
    echo "  LLMD_RELEASE_POSTFIX   Release name postfix (default: llmd)"
    echo "  BATCH_HELM_RELEASE     Helm release name (default: batch-gateway)"
    echo "  GATEWAY_LOCAL_PORT     Port-forward fallback port (default: 8080)"
    echo "  BATCH_DEV_VERSION      Batch gateway image tag / commit SHA (default: local)"
    echo "  BATCH_RELEASE_VERSION  Install released OCI chart (e.g. v1.0.0)"
    echo "  UNINSTALL_ALL          Set to 1 to also remove Kuadrant/Istio/cert-manager and CRDs (ephemeral clusters only)"
    echo ""
    echo "Examples:"
    echo "  $0 install"
    echo "  MODEL_NAME=my-model LLMD_VERSION=v0.5.0 $0 install"
    exit "${1:-0}"
}

# ── Main ─────────────────────────────────────────────────────────────────────

if [ $# -eq 0 ]; then usage 0; fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    test)      shift; cmd_test "$@" ;;
    uninstall) shift; cmd_uninstall "$@" ;;
    help|-h|--help) usage 0 ;;
    *) echo "Error: Unknown command '$1'"; echo ""; usage 1 ;;
esac
