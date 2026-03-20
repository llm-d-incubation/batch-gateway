#!/bin/bash
set -euo pipefail

# ── Deploy batch-gateway on MaaS platform ────────────────────────────────────
#
# Deploys batch-gateway integrated with MaaS (Models-as-a-Service) platform.
# MaaS provides: Gateway, Istio, Kuadrant, cert-manager, AuthPolicy, TokenRateLimitPolicy.
# This script only deploys: MaaS platform + sample model + batch-gateway + HTTPRoute.
#
# Prerequisites:
#   - OpenShift cluster (self-managed, not ROSA/HyperShift)
#   - oc, helm, kustomize, jq, htpasswd CLI tools
#   - Cluster admin access
#
# MaaS repo: https://github.com/opendatahub-io/models-as-a-service

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Override common.sh defaults to match MaaS conventions BEFORE sourcing
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
INGRESS_NAMESPACE="${INGRESS_NAMESPACE:-openshift-ingress}"

source "${SCRIPT_DIR}/common.sh"

# ── MaaS-specific Configuration ─────────────────────────────────────────────
MAAS_REPO="${MAAS_REPO:-https://github.com/opendatahub-io/models-as-a-service.git}"
MAAS_REF="${MAAS_REF:-main}"
MAAS_DIR="${MAAS_DIR:-/tmp/maas}"
MAAS_NAMESPACE="${MAAS_NAMESPACE:-opendatahub}"
# maas-controller only watches this namespace for MaaSAuthPolicy and MaaSSubscription CRs.
# CRs created in other namespaces (e.g. opendatahub) will be ignored by the controller.
MAAS_POLICY_NAMESPACE="${MAAS_POLICY_NAMESPACE:-models-as-a-service}"

# MaaS test user
MAAS_TEST_USER="${MAAS_TEST_USER:-testuser}"
MAAS_TEST_PASS="${MAAS_TEST_PASS:-testpass}"
MAAS_TEST_GROUP="${MAAS_TEST_GROUP:-tier-free-users}"

# Model served by MaaS simulator sample
MAAS_MODEL_NAME="${MAAS_MODEL_NAME:-facebook/opt-125m}"

# ── Install MaaS Platform ───────────────────────────────────────────────────

install_maas() {
    step "Installing MaaS platform..."

    if kubectl get deployment maas-api -n "${MAAS_NAMESPACE}" &>/dev/null; then
        log "MaaS API already deployed in '${MAAS_NAMESPACE}'. Skipping."
        return
    fi

    step "Cloning MaaS repo (${MAAS_REF})..."
    rm -rf "${MAAS_DIR}"
    git clone --depth 1 --branch "${MAAS_REF}" "${MAAS_REPO}" "${MAAS_DIR}" 2>/dev/null \
        || git clone "${MAAS_REPO}" "${MAAS_DIR}"
    if [ "${MAAS_REF}" != "main" ]; then
        (cd "${MAAS_DIR}" && git checkout "${MAAS_REF}")
    fi

    step "Running MaaS deploy script..."
    (cd "${MAAS_DIR}" && MAAS_REF="${MAAS_REF}" ./scripts/deploy.sh)

    # TODO: Remove this RBAC patch once the ODH operator includes these permissions natively.
    # The ODH operator creates the maas-api ServiceAccount and its ClusterRole,
    # but the maas-controller CRDs (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)
    # are installed AFTER the operator finishes. This means the operator's ClusterRole
    # does not include permissions for these CRDs, causing maas-api to crash with:
    #   "maassubscriptions.maas.opendatahub.io is forbidden"
    #   "secrets is forbidden"
    # This patch adds the missing permissions until the operator is updated to include them.
    step "Patching maas-api RBAC for MaaS CRDs..."
    kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: maas-api-extra
rules:
  # maas-api watches MaaSSubscription and MaaSModelRef for subscription selection
  # and model listing. These CRDs come from maas-controller, not the ODH operator,
  # so the operator-managed ClusterRole doesn't include them.
- apiGroups: ["maas.opendatahub.io"]
  resources: ["maassubscriptions", "maasmodelrefs", "maasauthpolicies"]
  verbs: ["get", "list", "watch"]
  # maas-api reads the maas-db-config secret for the PostgreSQL connection URL.
  # The operator creates the SA but doesn't grant secret read access in the namespace.
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
  # maas-api reads the cluster-scoped Auth CR (services.opendatahub.io/v1alpha1)
  # to determine admin groups for API key management authorization.
- apiGroups: ["services.opendatahub.io"]
  resources: ["auths"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: maas-api-extra
subjects:
- kind: ServiceAccount
  name: maas-api
  namespace: ${MAAS_NAMESPACE}
roleRef:
  kind: ClusterRole
  name: maas-api-extra
  apiGroup: rbac.authorization.k8s.io
EOF
    kubectl rollout restart deploy/maas-api -n "${MAAS_NAMESPACE}"
    kubectl rollout status deploy/maas-api -n "${MAAS_NAMESPACE}" --timeout=120s

    # MaaS installs OpenShift cert-manager operator, but does not create a ClusterIssuer.
    # batch-gateway needs one for apiserver TLS certificates.
    if ! kubectl get clusterissuer "${TLS_ISSUER_NAME}" &>/dev/null; then
        step "Creating ClusterIssuer '${TLS_ISSUER_NAME}' for batch-gateway TLS..."
        kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${TLS_ISSUER_NAME}
spec:
  selfSigned: {}
EOF
        log "ClusterIssuer '${TLS_ISSUER_NAME}' created."
    fi

    log "MaaS platform installed."
}

# ── Deploy Sample Model ─────────────────────────────────────────────────────

deploy_sample_model() {
    step "Deploying sample model (simulator) in namespace '${LLM_NAMESPACE}'..."

    # kustomize namePrefix produces "facebook-opt-125m-simulated"
    local isvc_name="facebook-opt-125m-simulated"

    if kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" &>/dev/null; then
        log "Sample model '${isvc_name}' already exists. Skipping."
        return
    fi

    kubectl get namespace "${LLM_NAMESPACE}" &>/dev/null || kubectl create namespace "${LLM_NAMESPACE}"

    local samples_dir="${MAAS_DIR}/docs/samples/models/simulator"
    if [ ! -d "${samples_dir}" ]; then
        die "MaaS samples not found at ${samples_dir}. Run install first."
    fi

    kustomize build "${samples_dir}" | kubectl apply -f -
    wait_for_deployment "${isvc_name}-kserve" "${LLM_NAMESPACE}" 300s

    step "Waiting for LLMInferenceService to be ready..."
    if ! oc wait "llminferenceservice/${isvc_name}" -n "${LLM_NAMESPACE}" \
            --for=condition=Ready --timeout=300s 2>/dev/null; then
        warn "LLMInferenceService not ready after 300s"
        oc get "llminferenceservice/${isvc_name}" -n "${LLM_NAMESPACE}" -o yaml 2>/dev/null || true
        oc get events -n "${LLM_NAMESPACE}" --sort-by='.lastTimestamp' 2>/dev/null | tail -10 || true
        die "Model '${isvc_name}' did not become ready"
    fi
    log "Sample model '${isvc_name}' is ready."
}

# ── Override install_batch_gateway to use MaaS model routing ─────────────────

install_batch_gateway() {
    step "Installing batch-gateway via Helm..."

    # MaaS gateway routes: /<namespace>/<isvc-name>/v1/...
    local gw_base="https://${GATEWAY_NAME}-istio.${INGRESS_NAMESPACE}.svc.cluster.local/${LLM_NAMESPACE}/facebook-opt-125m-simulated"

    local helm_args=(
        --namespace "${BATCH_NAMESPACE}"
        --set "apiserver.image.tag=${DEV_VERSION}"
        --set "processor.image.tag=${DEV_VERSION}"
        --set "global.secretName=${APP_SECRET_NAME}"
        --set "global.databaseType=${DB_TYPE}"
        --set "global.fileClient.type=${STORAGE_TYPE}"
        --set "global.fileClient.fs.basePath=/tmp/batch-gateway"
        --set "global.fileClient.fs.pvcName=${FILES_PVC_NAME}"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.url=${gw_base}"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.tlsInsecureSkipVerify=true"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.requestTimeout=5m"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.maxRetries=3"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.initialBackoff=1s"
        --set "processor.config.modelGateways.${MAAS_MODEL_NAME}.maxBackoff=60s"
        --set "apiserver.config.batchAPI.passThroughHeaders={Authorization,X-MaaS-Subscription}"
        --set "apiserver.tls.enabled=true"
        --set "apiserver.tls.certManager.enabled=true"
        --set "apiserver.tls.certManager.issuerName=${TLS_ISSUER_NAME}"
        --set "apiserver.tls.certManager.issuerKind=ClusterIssuer"
        --set "apiserver.tls.certManager.dnsNames={${HELM_RELEASE}-apiserver,${HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local,localhost}"
        --set "apiserver.podSecurityContext=null"
        --set "processor.podSecurityContext=null"
    )

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
    log "batch-gateway installed."
}

# ── MaaS Model Policies (MaaSModelRef + MaaSAuthPolicy + MaaSSubscription) ───

MAAS_TOKEN_RATE_LIMIT="${MAAS_TOKEN_RATE_LIMIT:-500}"
MAAS_TOKEN_RATE_WINDOW="${MAAS_TOKEN_RATE_WINDOW:-1m}"

create_maas_model_policies() {
    local isvc_name="facebook-opt-125m-simulated"

    kubectl get namespace "${MAAS_POLICY_NAMESPACE}" &>/dev/null || kubectl create namespace "${MAAS_POLICY_NAMESPACE}"

    step "Creating MaaSModelRef for '${isvc_name}'..."
    kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: ${isvc_name}
  namespace: ${LLM_NAMESPACE}
spec:
  modelRef:
    kind: LLMInferenceService
    name: ${isvc_name}
EOF

    step "Creating MaaSAuthPolicy (grant '${MAAS_TEST_GROUP}' access to model)..."
    kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: batch-model-access
  namespace: ${MAAS_POLICY_NAMESPACE}
spec:
  modelRefs:
    - name: ${isvc_name}
      namespace: ${LLM_NAMESPACE}
  subjects:
    groups:
      - name: ${MAAS_TEST_GROUP}
EOF

    step "Creating MaaSSubscription (token rate limit: ${MAAS_TOKEN_RATE_LIMIT} tokens/${MAAS_TOKEN_RATE_WINDOW})..."
    kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: batch-test-subscription
  namespace: ${MAAS_POLICY_NAMESPACE}
spec:
  owner:
    groups:
      - name: ${MAAS_TEST_GROUP}
  modelRefs:
    - name: ${isvc_name}
      namespace: ${LLM_NAMESPACE}
      tokenRateLimits:
        - limit: ${MAAS_TOKEN_RATE_LIMIT}
          window: ${MAAS_TOKEN_RATE_WINDOW}
EOF

    # Wait for MaaSModelRef to be Ready (controller reconciles HTTPRoute + AuthPolicy)
    step "Waiting for MaaSModelRef to be Ready..."
    local retries=30 mr_ready=false
    for i in $(seq 1 "${retries}"); do
        local phase
        phase=$(kubectl get maasmodelref "${isvc_name}" -n "${LLM_NAMESPACE}" \
            -o jsonpath='{.status.phase}' 2>/dev/null)
        if [ "${phase}" = "Ready" ]; then
            mr_ready=true
            break
        fi
        sleep 5
    done
    if ! ${mr_ready}; then
        # Controller can get stuck; bouncing may unstick it (known issue)
        warn "MaaSModelRef not ready after ${retries} retries, bouncing maas-controller..."
        kubectl rollout restart deployment/maas-controller -n "${MAAS_NAMESPACE}" 2>/dev/null || true
        kubectl rollout status deployment/maas-controller -n "${MAAS_NAMESPACE}" --timeout=120s 2>/dev/null || true
        for i in $(seq 1 30); do
            local phase
            phase=$(kubectl get maasmodelref "${isvc_name}" -n "${LLM_NAMESPACE}" \
                -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "${phase}" = "Ready" ]; then mr_ready=true; break; fi
            sleep 5
        done
    fi
    if ${mr_ready}; then
        log "MaaSModelRef ready."
    else
        warn "MaaSModelRef still not ready after bounce, continuing anyway."
    fi

    # Wait for all AuthPolicies to be enforced (Authorino reconcile)
    wait_for_auth_policies_enforced

    # Wait for TokenRateLimitPolicy to be generated from MaaSSubscription
    step "Waiting for TokenRateLimitPolicy..."
    for i in $(seq 1 30); do
        if kubectl get tokenratelimitpolicy -n llm 2>/dev/null | grep -q "${isvc_name}"; then
            log "TokenRateLimitPolicy generated for model."
            return
        fi
        sleep 5
    done
    warn "TokenRateLimitPolicy not found after 30 attempts."
}

# Wait for all Kuadrant AuthPolicies to be enforced across model namespaces
wait_for_auth_policies_enforced() {
    local timeout=180
    step "Waiting for AuthPolicies to be enforced (timeout: ${timeout}s)..."

    local deadline=$((SECONDS + timeout))
    while [ $SECONDS -lt $deadline ]; do
        local all_enforced=true total=0
        while IFS= read -r status; do
            total=$((total + 1))
            if [ "${status}" != "True" ]; then
                all_enforced=false
            fi
        done < <(kubectl get authpolicy -A -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Enforced")].status}{"\n"}{end}' 2>/dev/null)

        if ${all_enforced} && [ $total -gt 0 ]; then
            log "All AuthPolicies enforced (${total} policies)."
            return
        fi
        echo "  Waiting... (${total} policies found, not all enforced yet)"
        sleep 10
    done
    warn "AuthPolicies not all enforced after ${timeout}s, tests may fail."
    kubectl get authpolicy -A -o wide 2>/dev/null || true
}

# ── Batch Policies (MaaS API key auth + request rate limit) ──────────────────

create_batch_policies() {
    local api_key_url="https://maas-api.${MAAS_NAMESPACE}.svc.cluster.local:8443/internal/v1/api-keys/validate"

    step "Creating AuthPolicy for batch-route (MaaS API key validation)..."
    # Uses the same API key HTTP callback as MaaS-generated model AuthPolicies.
    # Authorino calls maas-api to validate the API key and extract user identity.
    # Any user with a valid MaaS API key can access the batch API.
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-auth
  namespace: ${BATCH_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-route
  rules:
    metadata:
      # Validate API key via MaaS API callback, returns {valid, username, groups, keyId}
      apiKeyValidation:
        http:
          url: "${api_key_url}"
          contentType: application/json
          method: POST
          body:
            expression: '{"key": request.headers.authorization.replace("Bearer ", "")}'
        metrics: false
        priority: 0
    authentication:
      # Plain authentication — actual validation is done in the metadata layer above
      api-keys:
        plain:
          selector: request.headers.authorization
        metrics: false
        priority: 0
    authorization:
      # Ensure the API key is valid
      api-key-valid:
        patternMatching:
          patterns:
          - selector: auth.metadata.apiKeyValidation.valid
            operator: eq
            value: "true"
        metrics: false
        priority: 0
    response:
      success:
        filters:
          identity:
            json:
              properties:
                userid:
                  selector: auth.metadata.apiKeyValidation.username
                keyId:
                  selector: auth.metadata.apiKeyValidation.keyId
            metrics: false
            priority: 0
        headers:
          X-MaaS-Username:
            plain:
              selector: auth.metadata.apiKeyValidation.username
            metrics: false
            priority: 0
          X-MaaS-Key-Id:
            plain:
              selector: auth.metadata.apiKeyValidation.keyId
            metrics: false
            priority: 0
      unauthenticated:
        code: 401
        message:
          value: "Authentication required"
      unauthorized:
        code: 403
        message:
          value: "Access denied"
EOF
    log "batch-auth AuthPolicy applied (MaaS API key validation, targets batch-route)."

    step "Creating RateLimitPolicy for batch-route (per-user request count)..."
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
      - expression: auth.identity.userid
EOF
    log "batch-ratelimit applied (20 req/min per user)."
}

# ── Create MaaS Test User ───────────────────────────────────────────────────

create_maas_test_user() {
    step "Creating MaaS test user '${MAAS_TEST_USER}' in group '${MAAS_TEST_GROUP}'..."

    if oc get user "${MAAS_TEST_USER}" &>/dev/null 2>&1; then
        log "User '${MAAS_TEST_USER}' already exists. Skipping user creation."
    else
        htpasswd -cbB /tmp/htpasswd "${MAAS_TEST_USER}" "${MAAS_TEST_PASS}"
        oc create secret generic htpass-secret \
            --from-file=htpasswd=/tmp/htpasswd \
            -n openshift-config \
            --dry-run=client -o yaml | oc apply -f -
        oc patch oauth cluster --type=merge -p "
spec:
  identityProviders:
  - name: htpasswd
    type: HTPasswd
    htpasswd:
      fileData:
        name: htpass-secret"
        log "OAuth htpasswd identity provider configured. Waiting for restart..."
        sleep 30
    fi

    if ! oc get group "${MAAS_TEST_GROUP}" &>/dev/null 2>&1; then
        oc adm groups new "${MAAS_TEST_GROUP}"
    fi
    oc adm groups add-users "${MAAS_TEST_GROUP}" "${MAAS_TEST_USER}" 2>/dev/null || true
    log "User '${MAAS_TEST_USER}' added to group '${MAAS_TEST_GROUP}'."
}

get_maas_gateway_host() {
    local cluster_domain
    cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null) \
        || die "Cannot detect cluster domain. Is this an OpenShift cluster?"
    echo "https://maas.${cluster_domain}"
}

get_maas_api_key() {
    local host="$1"
    local server_url
    server_url=$(oc whoami --show-server)

    # Use a temporary kubeconfig so we don't pollute the admin session
    local temp_kubeconfig
    temp_kubeconfig=$(mktemp)
    trap "rm -f '${temp_kubeconfig}'" RETURN

    # Login as test user to get OpenShift token
    KUBECONFIG="${temp_kubeconfig}" oc login "${server_url}" \
        -u "${MAAS_TEST_USER}" -p "${MAAS_TEST_PASS}" --insecure-skip-tls-verify &>/dev/null \
        || die "Failed to login as ${MAAS_TEST_USER}"
    local user_token
    user_token=$(KUBECONFIG="${temp_kubeconfig}" oc whoami -t) || die "Failed to get user token"

    # Create MaaS API key using OpenShift token
    local key_response
    key_response=$(curl -sSk \
        -H "Authorization: Bearer ${user_token}" \
        -H "Content-Type: application/json" \
        -X POST -d '{"name":"batch-e2e","expiration":"1h"}' \
        "${host}/maas-api/v1/api-keys")
    local api_key
    api_key=$(echo "${key_response}" | jq -r '.key // empty')

    if [ -z "${api_key}" ]; then
        die "Failed to create MaaS API key. Response: ${key_response}"
    fi
    echo "${api_key}"
}

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   MaaS + Batch Gateway Setup                          ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

    step "Checking prerequisites..."
    local missing=()
    for cmd in oc kubectl helm kustomize jq htpasswd; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    [ ${#missing[@]} -gt 0 ] && die "Missing required tools: ${missing[*]}"
    oc whoami &>/dev/null || die "Not logged in to OpenShift. Run 'oc login' first."
    is_openshift || die "This script requires an OpenShift cluster (oc, OAuth, LLMInferenceService)."
    log "Connected to cluster: $(oc whoami --show-server)"

    install_maas
    deploy_sample_model

    create_maas_test_user

    kubectl get namespace "${BATCH_NAMESPACE}" &>/dev/null || kubectl create namespace "${BATCH_NAMESPACE}"
    deploy_batch_gateway
    create_batch_policies
    create_maas_model_policies

    local host
    host=$(get_maas_gateway_host)
    log "Deployment complete!"
    log "  MaaS Gateway: ${host}"
    log "  Batch API:    ${host}/v1/batches"
    log "  Test user:    ${MAAS_TEST_USER} / ${MAAS_TEST_PASS} (group: ${MAAS_TEST_GROUP})"
    log ""
    log "Run '$0 test' to verify."
}

# ── Test ─────────────────────────────────────────────────────────────────────

cmd_test() {
    echo ""
    echo "  ╔═════════════════════════════════════╗"
    echo "  ║  Testing: MaaS + Batch Gateway      ║"
    echo "  ╚═════════════════════════════════════╝"
    echo ""

    local host
    host=$(get_maas_gateway_host)
    log "MaaS Gateway: ${host}"

    local test_total=0 test_passed=0 test_failed=0 failed_tests=""
    pass_test() { test_total=$((test_total + 1)); test_passed=$((test_passed + 1)); log "PASSED: $*"; }
    fail_test() { test_total=$((test_total + 1)); test_failed=$((test_failed + 1)); failed_tests="${failed_tests}\n  - $*"; warn "FAILED: $*"; }

    local response http_code body

    # ── Get MaaS API key ──────────────────────────────────────
    step "Obtaining MaaS API key..."
    local api_key
    api_key=$(get_maas_api_key "${host}")
    log "API key obtained: ${api_key:0:20}..."

    # ── Test 1: Batch API authentication (no key) ────────────
    echo ""
    echo "── Test 1: Batch API - No API key -> 401 ──"
    response=$(curl -sSk -w "\n%{http_code}" -X GET "${host}/v1/batches")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ] || [ "$http_code" = "403" ]; then
        pass_test "Test 1: ${http_code} (unauthenticated rejected)"
    else
        fail_test "Test 1: Expected 401/403, got $http_code"
    fi

    sleep 1

    # ── Test 2: Batch API authentication (with key) ──────────
    echo ""
    echo "── Test 2: Batch API - With API key -> 200 ──"
    response=$(curl -sSk -w "\n%{http_code}" -X GET "${host}/v1/batches" \
        -H "Authorization: Bearer ${api_key}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 2: 200 OK (batch list)"
    else
        fail_test "Test 2: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # ── Test 3: Batch request rate limiting ───────────────────
    echo ""
    echo "── Test 3: Batch API - Rate limiting (20 req/min per user) ──"
    local rl_success=0 rl_limited=0
    for i in $(seq 1 25); do
        http_code=$(curl -sSk -o /dev/null -w "%{http_code}" \
            -X GET "${host}/v1/batches" \
            -H "Authorization: Bearer ${api_key}")
        if [ "$http_code" = "429" ]; then
            rl_limited=$((rl_limited + 1))
            echo "  Request $i: 429 Rate Limited"
        else
            rl_success=$((rl_success + 1))
            [ "$i" -le 3 ] || [ "$http_code" = "429" ] && echo "  Request $i: $http_code"
        fi
        sleep 0.1
    done
    echo "  Result: $rl_success passed, $rl_limited rate-limited"
    if [ "$rl_limited" -ge 3 ]; then
        pass_test "Test 3: Rate limiting is working"
    else
        fail_test "Test 3: Rate limiting not triggered (expected at least 3 x 429)"
    fi

    # Wait for rate limit window to reset before next tests
    step "Waiting 60s for rate limit window to reset..."
    sleep 60

    # ── Test 4: Model token rate limiting ────────────────────
    echo ""
    echo "── Test 4: Model token rate limit (${MAAS_TOKEN_RATE_LIMIT} tokens/${MAAS_TOKEN_RATE_WINDOW}) ──"
    echo "  Goal: Direct model inference hits TokenRateLimitPolicy after token budget exhausted"

    local model_endpoint="${host}/${LLM_NAMESPACE}/facebook-opt-125m-simulated/v1/chat/completions"
    local subscription_name="batch-test-subscription"
    local trl_success=0 trl_limited=0
    for i in $(seq 1 15); do
        http_code=$(curl -sSk -o /dev/null -w "%{http_code}" \
            -H "Authorization: Bearer ${api_key}" \
            -H "X-MaaS-Subscription: ${subscription_name}" \
            -H "Content-Type: application/json" \
            -d "{\"model\":\"${MAAS_MODEL_NAME}\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}],\"max_tokens\":100}" \
            "${model_endpoint}")
        if [ "$http_code" = "429" ]; then
            trl_limited=$((trl_limited + 1))
            echo "  Request $i: 429 Token Rate Limited"
        else
            trl_success=$((trl_success + 1))
            [ "$i" -le 3 ] && echo "  Request $i: $http_code"
        fi
        sleep 0.2
    done
    echo "  Result: $trl_success passed, $trl_limited token-rate-limited"
    if [ "$trl_limited" -ge 1 ]; then
        pass_test "Test 4: Token rate limiting is working on model route"
    else
        fail_test "Test 4: Token rate limit not triggered (sent 15 requests, expected 429)"
    fi

    # Wait for rate limit windows to reset before E2E batch test
    step "Waiting 60s for rate limit windows to reset..."
    sleep 60

    # ── Test 5: Batch E2E lifecycle ──────────────────────────
    echo ""
    echo "── Test 5: E2E - Upload file + create batch ──"
    local input_file="/tmp/batch-test-input-$$.jsonl"
    cat > "${input_file}" <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${MAAS_MODEL_NAME}","messages":[{"role":"user","content":"Hello from MaaS"}],"max_tokens":10}}
JSONL
    response=$(curl -sSk -w "\n%{http_code}" -X POST "${host}/v1/files" \
        -H "Authorization: Bearer ${api_key}" \
        -F "purpose=batch" -F "file=@${input_file}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')
    rm -f "${input_file}"
    local file_id="" batch_id=""
    if [ "$http_code" = "200" ]; then
        file_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
        echo "  File uploaded: ${file_id}"
    else
        fail_test "Test 5: File upload failed (HTTP $http_code)"
        echo "  Response: $body"
    fi

    if [ -n "$file_id" ]; then
        response=$(curl -sSk -w "\n%{http_code}" -X POST "${host}/v1/batches" \
            -H "Authorization: Bearer ${api_key}" \
            -H "X-MaaS-Subscription: batch-test-subscription" \
            -H 'Content-Type: application/json' \
            -d "{\"input_file_id\":\"${file_id}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")
        http_code=$(echo "$response" | tail -n1)
        body=$(echo "$response" | sed '$d')
        if [ "$http_code" = "200" ]; then
            batch_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
            pass_test "Test 5: File uploaded + batch created (batch: ${batch_id})"
        else
            fail_test "Test 5: Batch creation failed (HTTP $http_code)"
            echo "  Response: $body"
        fi
    fi

    # ── Test 6: Batch completion ─────────────────────────────
    echo ""
    echo "── Test 6: E2E - Batch completion (passThroughHeaders -> MaaS gateway -> model) ──"
    echo "  Goal: Verify processor forwards API key through MaaS gateway to model"
    if [ -n "$batch_id" ]; then
        local status="unknown" poll_count=0
        while [ "$poll_count" -lt 60 ]; do
            response=$(curl -sSk "${host}/v1/batches/${batch_id}" \
                -H "Authorization: Bearer ${api_key}")
            status=$(echo "$response" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
            echo "  Poll $((poll_count+1)): status=${status}"
            case "$status" in completed|failed|expired|cancelled) break ;; esac
            poll_count=$((poll_count + 1))
            sleep 5
        done
        if [ "$status" = "completed" ]; then
            pass_test "Test 6: Batch completed (MaaS auth passthrough working)"
        else
            fail_test "Test 6: Batch ended with status=${status} (expected completed)"
        fi
    else
        fail_test "Test 6: Skipped (no batch_id from Test 5)"
    fi

    print_test_summary "$test_total" "$test_passed" "$test_failed" "$failed_tests"
}

# ── Uninstall ────────────────────────────────────────────────────────────────

cleanup_auth_resources() { true; }

cmd_uninstall() {
    set +e

    echo ""
    echo "  ╔══════════════════════════════════════════════════════╗"
    echo "  ║   Uninstalling All (Batch Gateway + MaaS)            ║"
    echo "  ╚══════════════════════════════════════════════════════╝"
    echo ""

    # Batch gateway
    # Batch gateway resources
    step "Removing batch-gateway resources..."
    kubectl delete ratelimitpolicy batch-ratelimit -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete authpolicy batch-auth -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete httproute batch-route -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete destinationrule "${HELM_RELEASE}-backend-tls" -n "${INGRESS_NAMESPACE}" 2>/dev/null || true
    helm uninstall "${HELM_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    kubectl delete pvc "${FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete namespace "${BATCH_NAMESPACE}" --timeout=60s 2>/dev/null || true
    log "Batch gateway uninstalled."

    # RBAC patch and ClusterIssuer
    kubectl delete clusterrolebinding maas-api-extra 2>/dev/null || true
    kubectl delete clusterrole maas-api-extra 2>/dev/null || true
    kubectl delete clusterissuer "${TLS_ISSUER_NAME}" 2>/dev/null || true

    # Test user
    step "Removing test user..."
    oc delete group "${MAAS_TEST_GROUP}" 2>/dev/null || true
    oc delete user "${MAAS_TEST_USER}" 2>/dev/null || true
    oc delete identity "htpasswd:${MAAS_TEST_USER}" 2>/dev/null || true

    # MaaS platform (reuse cleanup-odh.sh from MaaS repo)
    step "Removing MaaS platform..."
    local cleanup_script="${MAAS_DIR}/.github/hack/cleanup-odh.sh"
    if [ -f "${cleanup_script}" ]; then
        log "Using MaaS cleanup script: ${cleanup_script}"
        bash "${cleanup_script}" --include-crds || warn "cleanup-odh.sh returned non-zero"
    else
        warn "MaaS cleanup script not found at ${cleanup_script}, cleaning up manually..."
        kubectl delete datasciencecluster --all -A --timeout=120s 2>/dev/null || true
        kubectl delete dscinitialization --all -A --timeout=120s 2>/dev/null || true
        kubectl delete namespace "${MAAS_NAMESPACE}" --timeout=120s 2>/dev/null || true
        kubectl delete namespace "${MAAS_POLICY_NAMESPACE}" --timeout=60s 2>/dev/null || true
        kubectl delete namespace kuadrant-system --timeout=60s 2>/dev/null || true
        kubectl delete gateway "${GATEWAY_NAME}" -n "${INGRESS_NAMESPACE}" 2>/dev/null || true
    fi

    # Operators not covered by cleanup-odh.sh
    step "Removing cert-manager and LWS operators..."
    local cm_csv
    cm_csv=$(kubectl get csv -n cert-manager-operator -o name 2>/dev/null | grep cert-manager || true)
    if [ -n "${cm_csv}" ]; then
        kubectl delete subscription openshift-cert-manager-operator -n cert-manager-operator 2>/dev/null || true
        kubectl delete "${cm_csv}" -n cert-manager-operator 2>/dev/null || true
    fi
    kubectl delete namespace cert-manager-operator --timeout=60s 2>/dev/null || true

    local lws_csv
    lws_csv=$(kubectl get csv -n openshift-lws-operator -o name 2>/dev/null | grep leader-worker || true)
    if [ -n "${lws_csv}" ]; then
        kubectl delete subscription leader-worker-set -n openshift-lws-operator 2>/dev/null || true
        kubectl delete "${lws_csv}" -n openshift-lws-operator 2>/dev/null || true
    fi
    kubectl delete namespace openshift-lws-operator --timeout=60s 2>/dev/null || true

    echo ""
    log "Uninstallation complete (batch-gateway + MaaS)."

    set -e
}

# ── Usage ────────────────────────────────────────────────────────────────────

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Commands:"
    echo "  install    Install MaaS platform + sample model + batch-gateway"
    echo "  test       Run integration tests (MaaS auth + batch lifecycle)"
    echo "  uninstall  Remove everything (batch-gateway + MaaS + operators)"
    echo "  help       Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  MAAS_REF              MaaS git ref (default: main)"
    echo "  MAAS_TEST_USER        Test username (default: testuser)"
    echo "  MAAS_TEST_GROUP       Test user group (default: tier-free-users)"
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
