#!/bin/bash
set -euo pipefail

# ── Install RHOAI (Red Hat OpenShift AI) platform ────────────────────────────
#
# Installs the prerequisites for running ${GATEWAY_NAME} on OpenShift:
#   1. cert-manager operator (OLM)
#   2. LeaderWorkerSet operator (OLM)
#   3. GatewayClass + Gateway (OpenShift default, auto-installs Service Mesh)
#   4. Red Hat Connectivity Link (productized Kuadrant, OLM) [optional]
#   5. RHOAI operator (OLM) + DSCInitialization + DataScienceCluster
#   6. LLMInferenceService (CPU simulator)
#   7. Batch Gateway (apiserver + processor)
#
# Ref: https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/installing_and_uninstalling_openshift_ai_self-managed/index
# Ref: https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/ingress_and_load_balancing/configuring-ingress-cluster-traffic#ingress-gateway-api
# Ref: https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/deploying_models/index
# Ref: https://github.com/red-hat-data-services/kserve/tree/rhoai-3.4/docs/samples/llmisvc
# Ref: https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

source "${SCRIPT_DIR}/common.sh"

# ── Configuration (set before sourcing common.sh to override its defaults) ──
LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"
KUADRANT_NAMESPACE="${KUADRANT_NAMESPACE:-kuadrant-system}"

OPERATOR_TYPE="${OPERATOR_TYPE:-rhoai}"    # rhoai or odh
RHOAI_VERSION="${RHOAI_VERSION:-3.4}"
RHOAI_CHANNEL="${RHOAI_CHANNEL:-stable-${RHOAI_VERSION}}"
ODH_CHANNEL="${ODH_CHANNEL:-fast-3}"
GATEWAY_NAME="${GATEWAY_NAME:-openshift-ai-inference}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"

# LLMInferenceService configuration
MODEL_NAME="${MODEL_NAME:-facebook/opt-125m}"
MODEL_URI="${MODEL_URI:-hf://sshleifer/tiny-gpt2}"
MODEL_REPLICAS="${MODEL_REPLICAS:-2}"
SIM_IMAGE="${SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:v0.7.1}"

# ── 1. cert-manager ─────────────────────────────────────────────────────────

install_cert_manager_operator() {
    step "Installing cert-manager operator (OLM)..."

    if kubectl get subscription.operators.coreos.com openshift-cert-manager-operator \
        -n cert-manager-operator &>/dev/null 2>&1; then
        log "cert-manager operator already installed. Skipping."
        return
    fi

    kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: cert-manager-operator
  namespace: cert-manager-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-cert-manager-operator
  namespace: cert-manager-operator
spec:
  channel: stable-v1
  installPlanApproval: Automatic
  name: openshift-cert-manager-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

    wait_for_subscription "cert-manager-operator" "openshift-cert-manager-operator"

    # Wait for webhook to be ready before creating ClusterIssuers
    wait_for_deployment "cert-manager-webhook" "cert-manager" 180s
}

# ── 2. LeaderWorkerSet ───────────────────────────────────────────────────────

install_lws_operator() {
    step "Installing LeaderWorkerSet operator (OLM)..."

    if kubectl get subscription.operators.coreos.com leader-worker-set \
        -n openshift-lws-operator &>/dev/null 2>&1; then
        log "LWS operator already installed. Skipping."
        return
    fi

    kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-lws-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  targetNamespaces:
  - openshift-lws-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  channel: stable-v1.0
  installPlanApproval: Automatic
  name: leader-worker-set
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

    wait_for_subscription "openshift-lws-operator" "leader-worker-set"

    step "Creating LeaderWorkerSetOperator CR..."
    kubectl apply -f - <<'EOF'
apiVersion: operator.openshift.io/v1
kind: LeaderWorkerSetOperator
metadata:
  name: cluster
  namespace: openshift-lws-operator
spec:
  managementState: Managed
EOF
    log "LWS operator installed."
}

# ── 3. GatewayClass + Gateway ───────────────────────────────────────────────

create_openshift_gateway() {
    step "Creating OpenShift GatewayClass and Gateway..."

    # GatewayClass
    # During the creation of the GatewayClass resource, the Ingress Operator(in the openshift-ingress-operator namespace) installs a lightweight version of Red Hat OpenShift Service Mesh, an Istio custom resource, and a new deployment in the openshift-ingress namespace
    kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: openshift.io/gateway-controller/v1
EOF

    # Get cluster domain
    local domain
    domain=$(oc get ingresses.config/cluster -o jsonpath='{.spec.domain}')
    local hostname="llm-inference.${domain}"
    log "Cluster domain: ${domain}, Gateway hostname: ${hostname}"

    # Gateway CR
    # KServe does NOT create the Gateway CR itself — it only creates HTTPRoutes
    # that reference this Gateway. The Gateway must be pre-provisioned.
    if kubectl get gateway ${GATEWAY_NAME} -n "${GATEWAY_NAMESPACE}" &>/dev/null; then
        log "Gateway already exists. Skipping."
    else
        step "Creating Gateway CR..."
        kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${GATEWAY_NAME}
  namespace: ${GATEWAY_NAMESPACE}
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: http
    hostname: "${hostname}"
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            llm-d.ai/gateway-route: "true"
  - name: https
    hostname: "${hostname}"
    port: 443
    protocol: HTTPS
    tls:
      mode: Terminate
      certificateRefs:
      - name: router-certs-default
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            llm-d.ai/gateway-route: "true"
EOF
    fi

    step "Waiting for Istio control plane (istiod) to be ready..."
    wait_for_deployment "istiod-openshift-gateway" "openshift-ingress"

    log "OpenShift Gateway created."
}

# ── 4. Red Hat Connectivity Link (Kuadrant) ──────────────────────────────────
# Ref: https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.3

install_connectivity_link() {
    local ns="${KUADRANT_NAMESPACE}"

    step "Installing Red Hat Connectivity Link (Kuadrant) in namespace '${ns}'..."

    if kubectl get subscription.operators.coreos.com rhcl-operator \
        -n "${ns}" &>/dev/null 2>&1; then
        log "Connectivity Link operator already installed. Skipping."
    else
        kubectl create namespace "${ns}" 2>/dev/null || true

        kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: rhcl-operator
  namespace: ${ns}
spec:
  channel: stable
  installPlanApproval: Automatic
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant
  namespace: ${ns}
spec:
  upgradeStrategy: Default
EOF

        wait_for_subscription "${ns}" "rhcl-operator"

        # RHCL operator installs sub-operators (authorino, limitador, dns).
        # Wait for them before creating the Kuadrant CR.
        step "Waiting for Connectivity Link sub-operators..."
        for deploy in authorino-operator \
                      limitador-operator-controller-manager \
                      dns-operator-controller-manager; do
            wait_for_deployment "$deploy" "${ns}" 180s
        done
    fi

    # Create Kuadrant CR with retry.
    # The operator may not detect sub-operators immediately after they become ready,
    # so we retry by restarting the operator pod if the CR fails to become Ready.
    local kuadrant_ready=false
    for attempt in 1 2 3; do
        log "Waiting 30s for Kuadrant operator to register sub-operators..."
        sleep 30

        step "Creating Kuadrant CR (attempt ${attempt}/3)..."
        kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: ${ns}
spec: {}
EOF

        if kubectl wait kuadrant/kuadrant --for="condition=Ready=true" \
            -n "${ns}" --timeout=180s 2>/dev/null; then
            kuadrant_ready=true
            break
        fi

        warn "Kuadrant CR not ready, force-restarting operator pod..."
        kubectl delete kuadrant/kuadrant -n "${ns}" --ignore-not-found 2>/dev/null || true
        # rollout restart does not always recreate the pod; delete it directly
        # so the operator re-checks Authorino CRD at startup.
        kubectl delete pod -n "${ns}" -l control-plane=controller-manager,app=kuadrant --force 2>/dev/null || true
        wait_for_deployment "kuadrant-operator-controller-manager" "${ns}" 120s
    done

    if [ "${kuadrant_ready}" != "true" ]; then
        die "Kuadrant CR did not become ready after 3 attempts"
    fi

    # Configure Authorino for authentication (SSL with OpenShift serving certs)
    step "Configuring Authorino SSL..."
    oc annotate svc/authorino-authorino-authorization \
        service.beta.openshift.io/serving-cert-secret-name=authorino-server-cert \
        -n "${ns}" --overwrite
    sleep 2

    kubectl apply -f - <<EOF
apiVersion: operator.authorino.kuadrant.io/v1beta1
kind: Authorino
metadata:
  name: authorino
  namespace: ${ns}
spec:
  replicas: 1
  clusterWide: true
  listener:
    tls:
      enabled: true
      certSecretRef:
        name: authorino-server-cert
  oidcServer:
    tls:
      enabled: false
EOF

    step "Waiting for Authorino pods to be ready..."
    kubectl wait --for=condition=ready pod -l authorino-resource=authorino \
        -n "${ns}" --timeout=180s

    # If RHOAI was already installed before Connectivity Link, restart controllers
    if kubectl get deployment odh-model-controller -n redhat-ods-applications &>/dev/null; then
        step "Restarting RHOAI controllers to pick up Connectivity Link..."
        kubectl delete pod -n redhat-ods-applications -l app=odh-model-controller 2>/dev/null || true
        kubectl delete pod -n redhat-ods-applications -l control-plane=kserve-controller-manager 2>/dev/null || true
    fi

    log "Red Hat Connectivity Link installed with Authorino SSL."
}

# ── 5. RHOAI / ODH operator ─────────────────────────────────────────────────

install_rhoai_operator() {
    step "Installing ${OPERATOR_TYPE} operator (OLM)..."

    local operator_name namespace catalog channel
    case "${OPERATOR_TYPE}" in
        rhoai)
            operator_name="rhods-operator"
            namespace="redhat-ods-operator"
            catalog="redhat-operators"
            channel="${RHOAI_CHANNEL}"
            ;;
        odh)
            operator_name="opendatahub-operator"
            namespace="opendatahub"
            catalog="community-operators"
            channel="${ODH_CHANNEL}"
            ;;
        *)
            die "Unknown OPERATOR_TYPE: ${OPERATOR_TYPE}. Use rhoai or odh."
            ;;
    esac

    if kubectl get subscription.operators.coreos.com "${operator_name}" \
        -n "${namespace}" &>/dev/null 2>&1; then
        log "${OPERATOR_TYPE} operator already installed. Skipping."
    else
        # Validate channel exists in catalog
        local available_channels
        available_channels=$(kubectl get packagemanifest "${operator_name}" \
            -n openshift-marketplace -o jsonpath='{.status.channels[*].name}' 2>/dev/null || echo "")
        if [ -z "${available_channels}" ]; then
            die "PackageManifest '${operator_name}' not found in catalog. Is the catalog source available?"
        fi
        if ! echo " ${available_channels} " | grep -q " ${channel} "; then
            die "Channel '${channel}' not found for '${operator_name}'. Available: ${available_channels}"
        fi

        kubectl create namespace "${namespace}" 2>/dev/null || true

        kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: ${operator_name}
  namespace: ${namespace}
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: ${operator_name}
  namespace: ${namespace}
spec:
  channel: ${channel}
  installPlanApproval: Automatic
  name: ${operator_name}
  source: ${catalog}
  sourceNamespace: openshift-marketplace
EOF

        wait_for_subscription "${namespace}" "${operator_name}"
    fi

    # Verify installed version matches expected version
    if [ "${OPERATOR_TYPE}" = "rhoai" ]; then
        local installed_csv
        installed_csv=$(kubectl get subscription.operators.coreos.com "${operator_name}" \
            -n "${namespace}" -o jsonpath='{.status.installedCSV}' 2>/dev/null || echo "")
        if [ -z "${installed_csv}" ]; then
            die "No installedCSV found for subscription '${operator_name}'."
        fi
        if [[ "${installed_csv}" != rhods-operator.${RHOAI_VERSION}.* ]]; then
            die "Expected RHOAI ${RHOAI_VERSION} but installedCSV is '${installed_csv}'."
        fi
        log "RHOAI installedCSV: ${installed_csv}"
    fi
}

apply_dsci_and_dsc() {
    local apps_namespace
    case "${OPERATOR_TYPE}" in
        rhoai) apps_namespace="redhat-ods-applications" ;;
        odh)   apps_namespace="opendatahub" ;;
    esac

    # Wait for CRDs
    wait_for_crd "datascienceclusters.datasciencecluster.opendatahub.io"

    # DSCInitialization — the RHOAI operator auto-creates this after subscription.
    # Wait for it to appear rather than creating our own (webhook only allows one).
    step "Waiting for DSCInitialization..."
    local i=0
    while ! kubectl get dscinitializations -o name 2>/dev/null | grep -q .; do
        i=$((i + 1))
        [ "${i}" -gt 60 ] && die "DSCInitialization not created after 60s"
        sleep 2
    done
    log "DSCInitialization exists."

    # DataScienceCluster
    if kubectl get datasciencecluster default-dsc &>/dev/null; then
        log "DataScienceCluster already exists. Skipping."
    else
        step "Creating DataScienceCluster..."
        kubectl apply -f - <<EOF
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed
      modelsAsService:
        managementState: Removed
    dashboard:
      managementState: Removed
EOF
    fi

    # Wait for DSC ready
    step "Waiting for DataScienceCluster to be ready..."
    local i=0
    while [ "${i}" -lt 60 ]; do
        local phase
        phase=$(kubectl get datasciencecluster default-dsc \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        if [ "${phase}" = "Ready" ]; then
            log "DataScienceCluster is ready."
            return
        fi
        echo "  Status: ${phase} (${i}/60)"
        i=$((i + 1))
        sleep 10
    done
    die "DataScienceCluster not ready after 600s. Check operator logs."
}

# ── 6. LLMInferenceService ────────────────────────────────────────────────────

deploy_llm_inference_service() {
    # https://github.com/red-hat-data-services/kserve/tree/main/docs/samples/llmisvc
    local isvc_name
    isvc_name=$(echo "${MODEL_NAME}" | tr '/' '-' | tr '[:upper:]' '[:lower:]')

    step "Deploying LLMInferenceService '${isvc_name}' (simulator) in namespace '${LLM_NAMESPACE}'..."

    kubectl get namespace "${LLM_NAMESPACE}" &>/dev/null || kubectl create namespace "${LLM_NAMESPACE}"
    kubectl label namespace "${LLM_NAMESPACE}" llm-d.ai/gateway-route=true --overwrite

    if kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" &>/dev/null; then
        log "LLMInferenceService '${isvc_name}' already exists. Skipping."
        return
    fi

    wait_for_crd "llminferenceservices.serving.kserve.io"

    kubectl apply -f - <<EOF
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: ${isvc_name}
  namespace: ${LLM_NAMESPACE}
  annotations:
    # Enables Gateway-level AuthPolicy (SubjectAccessReview on LLMInferenceService)
    security.opendatahub.io/enable-auth: "true"
spec:
  model:
    uri: ${MODEL_URI}
    name: ${MODEL_NAME}
  replicas: ${MODEL_REPLICAS}
  router:
    route: {}
    #gateway:
    #  refs:
    #    - name: ${GATEWAY_NAME}
    #      namespace: ${GATEWAY_NAMESPACE}
    # No gateway.refs needed: KServe uses the default gateway configured in
    # inferenceservice-config ConfigMap (kserveIngressGateway: openshift-ingress/openshift-ai-inference).
    scheduler: {}
  template:
    containers:
      - name: main
        image: "${SIM_IMAGE}"
        imagePullPolicy: Always
        command: ["/app/llm-d-inference-sim"]
        args:
        - --port
        - "8000"
        - --model
        - ${MODEL_NAME}
        - --mode
        - random
        - --ssl-certfile
        - /var/run/kserve/tls/tls.crt
        - --ssl-keyfile
        - /var/run/kserve/tls/tls.key
        env:
          - name: POD_NAME
            valueFrom:
              fieldRef:
                apiVersion: v1
                fieldPath: metadata.name
          - name: POD_NAMESPACE
            valueFrom:
              fieldRef:
                apiVersion: v1
                fieldPath: metadata.namespace
        ports:
          - name: https
            containerPort: 8000
            protocol: TCP
        livenessProbe:
          httpGet:
            path: /health
            port: https
            scheme: HTTPS
        readinessProbe:
          httpGet:
            path: /ready
            port: https
            scheme: HTTPS
        resources:
          requests:
            cpu: 100m
            memory: 256Mi
          limits:
            cpu: 500m
            memory: 512Mi
EOF

    step "Waiting for LLMInferenceService '${isvc_name}' pods to start"
    for deploy in ${isvc_name}-kserve \
                  ${isvc_name}-kserve-router-scheduler; do
        wait_for_deployment "$deploy" "${LLM_NAMESPACE}" 180s
    done

    step "Waiting for LLMInferenceService '${isvc_name}' to be ready..."
    local i=0
    while [ "${i}" -lt 60 ]; do
        local ready
        ready=$(kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        if [ "${ready}" = "True" ]; then
            log "LLMInferenceService '${isvc_name}' is ready."
            return
        fi
        local phase
        phase=$(kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        echo "  Status: ${phase} (${i}/60)"
        i=$((i + 1))
        sleep 10
    done
    die "LLMInferenceService '${isvc_name}' not ready after 600s. Check operator logs."
}

# ── 6b. TokenRateLimitPolicy for inference ───────────────────────────────────

apply_llm_token_rate_limit() {
    local isvc_name
    isvc_name=$(echo "${MODEL_NAME}" | tr '/' '-' | tr '[:upper:]' '[:lower:]')

    # Target Gateway (not HTTPRoute) because the inference HTTPRoute name is
    # dynamically generated by LLMInferenceService controller.
    step "Creating TokenRateLimitPolicy for inference (500 tokens/1m per user)..."
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: inference-token-limit
  namespace: ${GATEWAY_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: ${GATEWAY_NAME}
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
        -n "${GATEWAY_NAMESPACE}" --timeout=180s 2>/dev/null \
        || die "TokenRateLimitPolicy not enforced after 180s."

    log "TokenRateLimitPolicy applied."
}

# ── 6c. AuthPolicy for batch route ────────────────────────────────────────────
# The Gateway-level AuthPolicy checks LLMInferenceService RBAC via path-based
# namespace/name extraction, which doesn't apply to batch API paths
# (/v1/batches, /v1/files). Override with token auth only (no authorization).

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

# ── 6d. RateLimitPolicy for batch route ───────────────────────────────────────

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

# ── 7. Batch Gateway ─────────────────────────────────────────────────────────

deploy_batch_gateway_rhoai() {
    banner "Installing Batch Gateway"

    # Get model URL from LLMInferenceService
    local isvc_name
    isvc_name=$(echo "${MODEL_NAME}" | tr '/' '-' | tr '[:upper:]' '[:lower:]')
    local model_url
    model_url=$(kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" \
        -o jsonpath='{.status.url}' 2>/dev/null || echo "")
    [ -z "${model_url}" ] && die "LLMInferenceService '${isvc_name}' has no URL. Is it ready?"
    log "Model URL: ${model_url}"

    # Escape dots in model name for helm --set path
    local model_key="${MODEL_NAME//./\\.}"

    local helm_args=(
        --set "processor.config.modelGateways.${model_key}.url=${model_url}"
        --set "processor.config.modelGateways.${model_key}.requestTimeout=${GW_REQUEST_TIMEOUT}"
        --set "processor.config.modelGateways.${model_key}.maxRetries=${GW_MAX_RETRIES}"
        --set "processor.config.modelGateways.${model_key}.initialBackoff=${GW_INITIAL_BACKOFF}"
        --set "processor.config.modelGateways.${model_key}.maxBackoff=${GW_MAX_BACKOFF}"
        --set "apiserver.config.batchAPI.passThroughHeaders={Authorization}"
    )
    if [[ "${DEMO_TLS_INSECURE_SKIP_VERIFY}" == "1" ]]; then
        helm_args+=(--set "processor.config.modelGateways.${model_key}.tlsInsecureSkipVerify=true")
        warn "DEMO_TLS_INSECURE_SKIP_VERIFY=1: processor TLS verify disabled for model gateway (demo/lab only)."
    fi

    do_deploy_batch_gateway "${helm_args[@]}"
}

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    banner "RHOAI Platform + Batch Gateway Setup (${OPERATOR_TYPE})"

    # Prerequisites
    local missing=()
    for cmd in oc kubectl helm jq curl; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    [ ${#missing[@]} -gt 0 ] && die "Missing required tools: ${missing[*]}"
    is_openshift || die "This script requires OpenShift. Use deploy-k8s.sh for vanilla Kubernetes."
    oc whoami &>/dev/null || die "Not logged in to OpenShift. Run 'oc login' first."
    log "Connected to: $(oc whoami --show-server)"

    install_cert_manager_operator
    create_selfsigned_issuer

    install_lws_operator
    create_openshift_gateway
    install_connectivity_link
    install_rhoai_operator
    apply_dsci_and_dsc

    deploy_llm_inference_service
    apply_llm_token_rate_limit

    deploy_batch_gateway_rhoai
    apply_batch_auth_policy
    apply_batch_request_rate_limit

    echo ""
    log "Setup complete."
    log "  Operator: ${OPERATOR_TYPE}"
    log "  Model: ${MODEL_NAME} (simulator, ${MODEL_REPLICAS} replicas, no GPU)"
    log "  Batch Gateway: ${BATCH_HELM_RELEASE} (${BATCH_NAMESPACE})"
    if [ -n "${BATCH_RELEASE_VERSION}" ]; then
        log "  Batch Gateway version: ${BATCH_RELEASE_VERSION} (OCI chart)"
    elif [ "${BATCH_DEV_VERSION}" != "local" ]; then
        log "  Batch Gateway image tag: ${BATCH_DEV_VERSION} (commit chart)"
    else
        log "  Batch Gateway image tag: latest (local chart)"
    fi
    log ""
    log "Run '$0 test' to verify."
}

# ── Test ──────────────────────────────────────────────────────────────────────

cmd_test() {
    banner "Testing: RHOAI + Batch Gateway"

    set_gateway_url

    local isvc_name
    isvc_name=$(echo "${MODEL_NAME}" | tr '/' '-' | tr '[:upper:]' '[:lower:]')

    log "Gateway:   ${GATEWAY_URL}"
    log "Inference: ${GATEWAY_URL}/${LLM_NAMESPACE}/${isvc_name}"
    log "Batch API: ${GATEWAY_URL}"

    # Auth setup: create SA + token + RBAC
    local sa_name="test-authorized-sa"
    log "Creating ServiceAccount '${sa_name}' for testing..."
    kubectl create serviceaccount "${sa_name}" -n "${LLM_NAMESPACE}" 2>/dev/null || true

    # Grant permission to get the specific LLMInferenceService
    kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${sa_name}-llm-reader
  namespace: ${LLM_NAMESPACE}
rules:
- apiGroups: ["serving.kserve.io"]
  resources: ["llminferenceservices"]
  resourceNames: ["${isvc_name}"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${sa_name}-llm-reader
  namespace: ${LLM_NAMESPACE}
subjects:
- kind: ServiceAccount
  name: ${sa_name}
  namespace: ${LLM_NAMESPACE}
roleRef:
  kind: Role
  name: ${sa_name}-llm-reader
  apiGroup: rbac.authorization.k8s.io
EOF

    local token
    token=$(oc create token "${sa_name}" -n "${LLM_NAMESPACE}" \
        --audience=https://kubernetes.default.svc --duration=10m) \
        || die "Failed to create token for SA '${sa_name}'"
    [[ "${token}" == ey* ]] || die "Token for SA '${sa_name}' doesn't look like a valid JWT"

    # Create unauthorized SA (no RBAC bindings)
    local unauth_sa="test-unauthorized-sa"
    kubectl create serviceaccount "${unauth_sa}" -n "${LLM_NAMESPACE}" 2>/dev/null || true
    sleep 2
    local unauth_token
    unauth_token=$(oc create token "${unauth_sa}" -n "${LLM_NAMESPACE}" \
        --audience=https://kubernetes.default.svc --duration=10m) \
        || die "Failed to create token for SA '${unauth_sa}'"
    [[ "${unauth_token}" == ey* ]] || die "Token for SA '${unauth_sa}' doesn't look like a valid JWT"

    local llm_url="${GATEWAY_URL}/${LLM_NAMESPACE}/${isvc_name}/v1/chat/completions"
    local inference_payload="{\"model\":\"${MODEL_NAME}\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}],\"max_tokens\":10}"

    run_tests "${llm_url}" "${GATEWAY_URL}" "${MODEL_NAME}" \
        "Authorization: Bearer ${token}" \
        "Authorization: Bearer ${unauth_token}" \
        "${inference_payload}"

}

# ── Uninstall ────────────────────────────────────────────────────────────────

cmd_uninstall() {
    set +e

    banner "Uninstalling RHOAI Platform + Batch Gateway"

    step "Removing test resources..."
    kubectl delete role test-authorized-sa-llm-reader -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete rolebinding test-authorized-sa-llm-reader -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete serviceaccount test-authorized-sa -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete serviceaccount test-unauthorized-sa -n "${LLM_NAMESPACE}" 2>/dev/null || true

    # Batch Gateway
    step "Removing batch-gateway..."
    kubectl delete ratelimitpolicy batch-ratelimit -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete authpolicy batch-route-auth -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete httproute batch-route -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    helm uninstall "${BATCH_HELM_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    kubectl delete deployment,svc -l app="${BATCH_MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete pvc "${BATCH_FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    force_delete_namespace "${BATCH_NAMESPACE}"

    # DestinationRule (in GATEWAY_NAMESPACE, not deleted with batch namespace)
    kubectl delete destinationrule "${BATCH_HELM_RELEASE}-backend-tls" -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    # TokenRateLimitPolicy
    step "Removing TokenRateLimitPolicy..."
    kubectl delete tokenratelimitpolicy inference-token-limit -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    # Named Gateway only (never delete all Gateways in a shared ingress namespace).
    step "Removing Gateway ${GATEWAY_NAME}..."
    kubectl delete gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    if is_demo_uninstall_all; then
        # LLMInferenceService
        step "Removing LLMInferenceService..."
        kubectl delete llminferenceservice --all -n "${LLM_NAMESPACE}" --timeout=180s 2>/dev/null || true

        # DSC + DSCI
        step "Removing DataScienceCluster and DSCInitialization..."
        kubectl delete datasciencecluster --all --timeout=180s 2>/dev/null || true
        kubectl delete dscinitializations --all --timeout=180s 2>/dev/null || true

        # RHOAI/ODH operator
        local operator_name namespace
        case "${OPERATOR_TYPE}" in
            rhoai) operator_name="rhods-operator"; namespace="redhat-ods-operator" ;;
            odh)   operator_name="opendatahub-operator"; namespace="opendatahub" ;;
        esac
        step "Removing ${OPERATOR_TYPE} operator..."
        kubectl delete subscription.operators.coreos.com "${operator_name}" -n "${namespace}" 2>/dev/null || true
        local csv
        csv=$(kubectl get csv -n "${namespace}" --no-headers 2>/dev/null | grep "${operator_name}" | awk '{print $1}')
        [ -n "${csv}" ] && kubectl delete csv "${csv}" -n "${namespace}" 2>/dev/null || true

        # Red Hat Connectivity Link (Kuadrant)
        step "Removing Connectivity Link..."
        kubectl delete kuadrant kuadrant -n "${KUADRANT_NAMESPACE}" 2>/dev/null || true
        kubectl delete subscription.operators.coreos.com rhcl-operator -n "${KUADRANT_NAMESPACE}" 2>/dev/null || true
        csv=$(kubectl get csv -n "${KUADRANT_NAMESPACE}" --no-headers 2>/dev/null | grep "rhcl-operator" | awk '{print $1}')
        [ -n "${csv}" ] && kubectl delete csv "${csv}" -n "${KUADRANT_NAMESPACE}" 2>/dev/null || true
        kubectl delete namespace "${KUADRANT_NAMESPACE}" --timeout=60s 2>/dev/null || true
        kubectl get crd -o name 2>/dev/null | grep -E 'kuadrant|authorino|limitador' | xargs -r kubectl delete 2>/dev/null || true
        kubectl get clusterrole -o name 2>/dev/null | grep -E 'kuadrant|authorino|limitador|^clusterrole.*/dns-operator-' | xargs -r kubectl delete 2>/dev/null || true
        kubectl get clusterrolebinding -o name 2>/dev/null | grep -E 'kuadrant|authorino|limitador|^clusterrolebinding.*/dns-operator-' | xargs -r kubectl delete 2>/dev/null || true

        step "Removing GatewayClass openshift-default..."
        kubectl delete gatewayclass openshift-default 2>/dev/null || true

        # LWS
        step "Removing LWS operator..."
        kubectl delete leaderworkersetoperator cluster -n openshift-lws-operator 2>/dev/null || true
        kubectl delete subscription.operators.coreos.com leader-worker-set -n openshift-lws-operator 2>/dev/null || true
        csv=$(kubectl get csv -n openshift-lws-operator --no-headers 2>/dev/null | grep "leader-worker" | awk '{print $1}')
        [ -n "${csv}" ] && kubectl delete csv "${csv}" -n openshift-lws-operator 2>/dev/null || true
        kubectl delete namespace openshift-lws-operator --timeout=60s 2>/dev/null || true

        # cert-manager (OLM operator lives in cert-manager-operator, workloads in cert-manager)
        step "Removing cert-manager operator..."
        kubectl delete subscription.operators.coreos.com openshift-cert-manager-operator -n cert-manager-operator 2>/dev/null || true
        csv=$(kubectl get csv -n cert-manager-operator --no-headers 2>/dev/null | grep "cert-manager" | awk '{print $1}')
        [ -n "${csv}" ] && kubectl delete csv "${csv}" -n cert-manager-operator 2>/dev/null || true
        kubectl delete namespace cert-manager-operator --timeout=60s 2>/dev/null || true
        kubectl delete namespace cert-manager --timeout=60s 2>/dev/null || true
        kubectl get crd -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete 2>/dev/null || true
        kubectl get clusterrole -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete 2>/dev/null || true
        kubectl get clusterrolebinding -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete 2>/dev/null || true
        kubectl get validatingwebhookconfiguration -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete 2>/dev/null || true
        kubectl get mutatingwebhookconfiguration -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete 2>/dev/null || true
        kubectl get role -n kube-system -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete -n kube-system 2>/dev/null || true
        kubectl get rolebinding -n kube-system -o name 2>/dev/null | grep cert-manager | xargs -r kubectl delete -n kube-system 2>/dev/null || true

        # LLM namespace
        force_delete_namespace "${LLM_NAMESPACE}"
    else
        warn "Skipping LLMInferenceService, OpenShift AI operators, Kuadrant, GatewayClass, LWS, cert-manager, and '${LLM_NAMESPACE}' namespace delete (shared-cluster safety)."
        warn "For full teardown on an ephemeral cluster only: UNINSTALL_ALL=1 $0 uninstall"
    fi

    echo ""
    log "RHOAI platform + batch gateway uninstalled."

    set -e
}

# ── Usage ────────────────────────────────────────────────────────────────────

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Install RHOAI platform + batch-gateway on OpenShift."
    echo ""
    echo "Commands:"
    echo "  install    Install RHOAI platform, LLMInferenceService, and batch-gateway"
    echo "  test       Run inference + batch lifecycle tests"
    echo "  uninstall  Remove demo resources (use UNINSTALL_ALL=1 for full platform teardown)"
    echo "  help       Show this help"
    echo ""
    echo "Environment Variables:"
    echo "  OPERATOR_TYPE    rhoai or odh (default: rhoai)"
    echo "  RHOAI_VERSION    RHOAI version (default: 3.4)"
    echo "  RHOAI_CHANNEL    OLM channel (default: stable-\${RHOAI_VERSION})"
    echo "  MODEL_NAME       Model name for simulator (default: facebook/opt-125m)"
    echo "  MODEL_REPLICAS   Number of replicas (default: 2)"
    echo "  SIM_IMAGE        Simulator image (default: ghcr.io/llm-d/llm-d-inference-sim:v0.7.1)"
    echo "  BATCH_DEV_VERSION      Batch gateway image tag / commit SHA (default: local)"
    echo "  BATCH_RELEASE_VERSION  Install released OCI chart (e.g. v1.0.0)"
    echo "  BATCH_DB_TYPE          Database: postgresql or redis (default: postgresql)"
    echo "  BATCH_STORAGE_TYPE     File storage: fs or s3 (default: s3)"
    echo "  UNINSTALL_ALL            Set to 1 to remove RHOAI operators, Kuadrant, cert-manager, etc. (ephemeral clusters only)"
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
