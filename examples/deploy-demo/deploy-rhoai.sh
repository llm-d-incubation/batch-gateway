#!/usr/bin/env bash
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
# Ref: https://docs.redhat.com/en/documentation/red_hat_connectivity_link

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

source "${SCRIPT_DIR}/common.sh"

# ── Configuration ────────────────────────────────────────────────────────────
LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"
KUADRANT_NAMESPACE="${KUADRANT_NAMESPACE:-kuadrant-system}"

OPERATOR_TYPE="${OPERATOR_TYPE:-rhoai}"    # rhoai or odh
CUSTOM_CATALOG="${CUSTOM_CATALOG:-}"       # custom catalog image (e.g. quay.io/rhoai/rhoai-fbc-fragment:...)
RHOAI_CHANNEL="${RHOAI_CHANNEL:-stable-3.x}"
ODH_CHANNEL="${ODH_CHANNEL:-fast-3}"
GATEWAY_CLASS_NAME="${GATEWAY_CLASS_NAME:-openshift-default}"
GATEWAY_NAME="${GATEWAY_NAME:-openshift-ai-inference}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
BATCH_INTERNAL_GATEWAY_NAME="${BATCH_INTERNAL_GATEWAY_NAME:-batch-internal-gateway}"
BATCH_INTERNAL_GATEWAY_NAMESPACE="${BATCH_INTERNAL_GATEWAY_NAMESPACE:-${GATEWAY_NAMESPACE}}"

# LLMInferenceService configuration
MODEL_NAME="${MODEL_NAME:-facebook/opt-125m}"
MODEL_URI="${MODEL_URI:-hf://facebook/opt-125m}"
MODEL_REPLICAS="${MODEL_REPLICAS:-1}"
ISVC_NAME="${ISVC_NAME:-$(echo "${MODEL_NAME}" | tr '/' '-' | tr '[:upper:]' '[:lower:]')}"
SIM_IMAGE="${SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:v0.7.1}"

# Flow control: GIE priority-based dispatch (interactive > batch).
# When enabled, EPP is configured with flow control plugins and InferenceObjective
# CRDs are created so batch requests are sheddable (priority -1) while interactive
# requests get priority 100.
ENABLE_FLOW_CONTROL="${ENABLE_FLOW_CONTROL:-true}"
INTERACTIVE_FLOW_CONTROL_OBJECTIVE="${INTERACTIVE_FLOW_CONTROL_OBJECTIVE:-interactive-default}"
BATCH_FLOW_CONTROL_OBJECTIVE="${BATCH_FLOW_CONTROL_OBJECTIVE:-batch-sheddable}"

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

    if kubectl get leaderworkersetoperator cluster \
        -n openshift-lws-operator &>/dev/null 2>&1; then
        log "LWS CR already installed. Skipping."
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
create_inference_external_gateway() {
    step "Creating OpenShift GatewayClass and Gateway..."

    # GatewayClass
    # During the creation of the GatewayClass resource, the Ingress Operator(in the openshift-ingress-operator namespace) installs a lightweight version of Red Hat OpenShift Service Mesh, an Istio custom resource, and a new deployment in the openshift-ingress namespace
    if kubectl get gatewayclass "${GATEWAY_CLASS_NAME}" &>/dev/null; then
        log "GatewayClass '${GATEWAY_CLASS_NAME}' already exists. Skipping."
    else
        kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: ${GATEWAY_CLASS_NAME}
spec:
  controllerName: openshift.io/gateway-controller/v1
EOF
    fi

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
  gatewayClassName: ${GATEWAY_CLASS_NAME}
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
    wait_for_deployment "${GATEWAY_NAME}-${GATEWAY_CLASS_NAME}" "${GATEWAY_NAMESPACE}"

    log "OpenShift Gateway created."
}

# ── 4. Red Hat Connectivity Link (Kuadrant) ──────────────────────────────────
# Ref: https://docs.redhat.com/en/documentation/red_hat_connectivity_link

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

    step "Waiting for Authorino deployment to be ready..."
    wait_for_deployment "authorino" "${ns}" 180s

    # If RHOAI was already installed before Connectivity Link, restart controllers
    if kubectl get deployment odh-model-controller -n redhat-ods-applications &>/dev/null; then
        step "Restarting RHOAI controllers to pick up Connectivity Link..."
        kubectl delete pod -n redhat-ods-applications -l app=odh-model-controller 2>/dev/null || true
        kubectl delete pod -n redhat-ods-applications -l control-plane=kserve-controller-manager 2>/dev/null || true
    fi

    log "Red Hat Connectivity Link installed with Authorino SSL."
}

install_connectivity_link_v1_3() {
    local ns="${KUADRANT_NAMESPACE}"
    local rhcl_csv="rhcl-operator.v1.3.5"

    step "Installing Red Hat Connectivity Link 1.3.5 (pinned) in namespace '${ns}'..."

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
  installPlanApproval: Manual
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  startingCSV: ${rhcl_csv}
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant
  namespace: ${ns}
spec:
  upgradeStrategy: Default
EOF

        # Approve install plans as they appear (RHCL + sub-operators)
        step "Approving install plans for RHCL 1.3.x..."
        local approved=0
        for i in $(seq 1 30); do
            local plans
            plans=$(kubectl get installplan -n "${ns}" -o jsonpath='{range .items[?(@.spec.approved==false)]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
            if [ -n "${plans}" ]; then
                for plan in ${plans}; do
                    local csv_list
                    csv_list=$(kubectl get installplan "${plan}" -n "${ns}" -o jsonpath='{.spec.clusterServiceVersionNames[*]}' 2>/dev/null)
                    # Only approve 1.3.x plans
                    if ! echo "${csv_list}" | grep -qE 'rhcl-operator\.v1\.3'; then
                        log "Skipping install plan ${plan} (not RHCL 1.3.x: ${csv_list})"
                        continue
                    fi
                    log "Approving install plan ${plan} (${csv_list})"
                    kubectl patch installplan "${plan}" -n "${ns}" --type=merge -p '{"spec":{"approved":true}}'
                    approved=$((approved + 1))
                done
            fi
            # Check if RHCL CSV is installed
            local phase
            phase=$(kubectl get csv "${rhcl_csv}" -n "${ns}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [ "${phase}" = "Succeeded" ]; then
                log "RHCL ${rhcl_csv} installed (approved ${approved} install plans)."
                break
            fi
            sleep 10
        done

        local phase
        phase=$(kubectl get csv "${rhcl_csv}" -n "${ns}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        if [ "${phase}" != "Succeeded" ]; then
            die "RHCL ${rhcl_csv} not installed after 300s. Phase: ${phase}"
        fi

        step "Waiting for Connectivity Link sub-operators..."
        for deploy in authorino-operator \
                      limitador-operator-controller-manager \
                      dns-operator-controller-manager; do
            wait_for_deployment "$deploy" "${ns}" 180s
        done
    fi

    # Create Kuadrant CR with retry (same logic as install_connectivity_link)
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

    step "Waiting for Authorino deployment to be ready..."
    wait_for_deployment "authorino" "${ns}" 180s

    if kubectl get deployment odh-model-controller -n redhat-ods-applications &>/dev/null; then
        step "Restarting RHOAI controllers to pick up Connectivity Link..."
        kubectl delete pod -n redhat-ods-applications -l app=odh-model-controller 2>/dev/null || true
        kubectl delete pod -n redhat-ods-applications -l control-plane=kserve-controller-manager 2>/dev/null || true
    fi

    log "Red Hat Connectivity Link 1.3.5 installed (pinned) with Authorino SSL."
}

# ── 5. RHOAI / ODH operator ─────────────────────────────────────────────────

create_custom_catalogsource() {
    local name="$1"
    local namespace="$2"
    local image="$3"
    local timeout="${4:-120}"

    step "Creating custom CatalogSource '${name}' from image: ${image}"

    # Delete existing CatalogSource to force image refresh
    if kubectl get catalogsource "${name}" -n "${namespace}" &>/dev/null; then
        log "CatalogSource '${name}' already exists. Updating..."
        kubectl delete catalogsource "${name}" -n "${namespace}" --ignore-not-found
    fi

    kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  sourceType: grpc
  image: ${image}
  displayName: "Custom ${name} Catalog"
  publisher: "Custom"
  updateStrategy:
    registryPoll:
      interval: 10m
EOF

    step "Waiting for CatalogSource '${name}' to be ready..."
    if ! kubectl wait catalogsource "${name}" -n "${namespace}" \
        --for=jsonpath='{.status.connectionState.lastObservedState}'=READY \
        --timeout="${timeout}s" 2>/dev/null; then
        local state
        state=$(kubectl get catalogsource "${name}" -n "${namespace}" \
            -o jsonpath='{.status.connectionState.lastObservedState}' 2>/dev/null || echo "unknown")
        die "CatalogSource '${name}' not ready after ${timeout}s (state: ${state})"
    fi
    log "CatalogSource '${name}' is ready."
}

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

    # install operator
    if kubectl get subscription.operators.coreos.com "${operator_name}" \
        -n "${namespace}" &>/dev/null 2>&1; then
        log "${OPERATOR_TYPE} operator already installed. Skipping."
    else
        # Custom catalog: create CatalogSource first so PackageManifest is
        # available for version/channel auto-detection
        if [ -n "${CUSTOM_CATALOG}" ]; then
            local custom_catalog_name="${OPERATOR_TYPE}-custom-catalog"
            log "Using custom catalog: ${CUSTOM_CATALOG}"
            create_custom_catalogsource "${custom_catalog_name}" "openshift-marketplace" "${CUSTOM_CATALOG}"
            catalog="${custom_catalog_name}"
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
    local dsc_name="default-dsc"
    if kubectl get datasciencecluster ${dsc_name} &>/dev/null; then
        log "DataScienceCluster already exists. Skipping."
    else
        step "Creating DataScienceCluster..."
        kubectl apply -f - <<EOF
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: ${dsc_name}
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
        phase=$(kubectl get datasciencecluster ${dsc_name} \
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
    local isvc_name="${ISVC_NAME}"

    step "Deploying LLMInferenceService '${isvc_name}' (simulator) in namespace '${LLM_NAMESPACE}'..."

    kubectl get namespace "${LLM_NAMESPACE}" &>/dev/null || kubectl create namespace "${LLM_NAMESPACE}"
    kubectl label namespace "${LLM_NAMESPACE}" llm-d.ai/gateway-route=true --overwrite

    if kubectl get llminferenceservice "${isvc_name}" -n "${LLM_NAMESPACE}" &>/dev/null; then
        log "LLMInferenceService '${isvc_name}' already exists. Skipping."
        return
    fi

    wait_for_crd "llminferenceservices.serving.kserve.io"

    # Build scheduler config: with or without flow control
    local scheduler_yaml
    if [ "${ENABLE_FLOW_CONTROL}" = "true" ]; then
        log "Flow control enabled: EPP will use EndpointPickerConfig with flowControl feature gate"
        scheduler_yaml=$(cat <<'SCHEDULER_EOF'
    scheduler:
      config:
        inline:
          apiVersion: inference.networking.x-k8s.io/v1alpha1
          kind: EndpointPickerConfig
          featureGates:
            - "flowControl"
          plugins:
            - type: round-robin-fairness-policy
            - type: global-strict-fairness-policy
            - type: slo-deadline-ordering-policy
            - type: utilization-detector
              parameters:
                queueDepthThreshold: 5
                kvCacheUtilThreshold: 0.8
          schedulingProfiles: []
          saturationDetector:
            pluginRef: utilization-detector
          flowControl:
            maxBytes: 4294967296
            defaultRequestTTL: 30s
            priorityBands:
              - priority: 100
                maxBytes: 1073741824
                fairnessPolicyRef: round-robin-fairness-policy
                orderingPolicyRef: fcfs-ordering-policy
              - priority: -1
                maxBytes: 3221225472
                fairnessPolicyRef: global-strict-fairness-policy
                orderingPolicyRef: slo-deadline-ordering-policy
            defaultPriorityBand:
              maxBytes: 536870912
              fairnessPolicyRef: global-strict-fairness-policy
              orderingPolicyRef: fcfs-ordering-policy
SCHEDULER_EOF
        )
    else
        scheduler_yaml="    scheduler: {}"
    fi

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
${scheduler_yaml}
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

# ── 6b. InferencePool discovery helper ──────────────────────────────────────

discover_inference_pool() {
    local isvc_name="${ISVC_NAME}"
    local pool_name
    pool_name=$(kubectl get inferencepool -n "${LLM_NAMESPACE}" -o json | \
        jq -r --arg owner "${isvc_name}" \
        '.items[] | select(.metadata.ownerReferences[]?.name == $owner) | .metadata.name' \
        2>/dev/null | head -1)
    [ -z "${pool_name}" ] && die "No InferencePool owned by LLMInferenceService '${isvc_name}' found in namespace '${LLM_NAMESPACE}'."
    echo "${pool_name}"
}

# ── 6c. InferenceObjective CRDs for flow control ──────────────────────────────

create_inference_objectives() {
    local isvc_name="${ISVC_NAME}"

    step "Discovering InferencePool for InferenceObjective CRDs..."
    local pool_name
    pool_name=$(discover_inference_pool)
    log "InferencePool: ${pool_name} (owned by ${isvc_name})"

    step "Creating InferenceObjective CRDs..."
    kubectl apply -f - <<EOF
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: ${INTERACTIVE_FLOW_CONTROL_OBJECTIVE}
  namespace: ${LLM_NAMESPACE}
spec:
  priority: 100
  poolRef:
    group: inference.networking.k8s.io
    name: ${pool_name}
---
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: ${BATCH_FLOW_CONTROL_OBJECTIVE}
  namespace: ${LLM_NAMESPACE}
spec:
  priority: -1
  poolRef:
    group: inference.networking.k8s.io
    name: ${pool_name}
EOF
    log "InferenceObjectives created (${INTERACTIVE_FLOW_CONTROL_OBJECTIVE}: priority 100, ${BATCH_FLOW_CONTROL_OBJECTIVE}: priority -1)."
}

# ── 6d. Batch LLM HTTPRoute on Internal Gateway ─────────────────────────────
# Routes batch processor inference traffic through the Internal Gateway (ClusterIP)
# to the same InferencePool, preserving EPP but bypassing TokenRateLimitPolicy.

create_batch_llm_httproute() {
    local isvc_name="${ISVC_NAME}"

    step "Discovering InferencePool for LLMInferenceService '${isvc_name}'..."
    local pool_name
    pool_name=$(discover_inference_pool)
    log "InferencePool: ${pool_name} (owned by ${isvc_name})"

    step "Creating batch-llm-route on Internal Gateway..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-llm-route
  namespace: ${LLM_NAMESPACE}
spec:
  parentRefs:
  - name: ${BATCH_INTERNAL_GATEWAY_NAME}
    namespace: ${BATCH_INTERNAL_GATEWAY_NAMESPACE}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NAMESPACE}/${isvc_name}/v1/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/completions
    backendRefs:
    - group: inference.networking.x-k8s.io
      kind: InferencePool
      name: ${pool_name}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NAMESPACE}/${isvc_name}/v1/chat/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/chat/completions
    backendRefs:
    - group: inference.networking.x-k8s.io
      kind: InferencePool
      name: ${pool_name}
EOF

    log "batch-llm-route created: /${LLM_NAMESPACE}/${isvc_name}/* -> InferencePool/${pool_name} (via Internal Gateway)"
}

# ── 6e. AuthPolicy for batch LLM route (Internal Gateway) ──────────────────
# Same authentication + model-level authorization as the external LLM route,
# but NO TokenRateLimitPolicy — batch requests are exempt from token rate limits.

apply_batch_llm_auth_policy() {
    step "Creating batch-llm-route AuthPolicy (authentication + model authorization)..."
    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-llm-route-auth
  namespace: ${LLM_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-llm-route
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
              value: serving.kserve.io
            resource:
              value: llminferenceservices
            namespace:
              expression: request.path.split("/")[1]
            name:
              expression: request.path.split("/")[2]
            verb:
              value: get
EOF
    log "Batch LLM AuthPolicy applied."
}

# ── 6f. TokenRateLimitPolicy for inference ───────────────────────────────────

apply_llm_token_rate_limit() {
    local isvc_name="${ISVC_NAME}"

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

# ── 6g. AuthPolicy for batch route ────────────────────────────────────────────
# The batch API paths (/v1/batches, /v1/files) don't match the /{ns}/{model}
# pattern used for model-level authorization. Authentication only here;
# model-level authorization happens when the batch processor forwards requests
# to the Internal Gateway's batch-llm-route (which has its own AuthPolicy).

apply_batch_auth_policy() {
    step "Creating batch-route AuthPolicy (authentication only)..."
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

# ── 6h. RateLimitPolicy for batch route ───────────────────────────────────────

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

# ── 6i. Flow control verification ────────────────────────────────────────────

verify_flow_control_config() {
    banner "Verifying Flow Control configuration"

    local isvc_name="${ISVC_NAME}"
    local errors=0

    # 1. EPP scheduler pod config
    step "Checking EPP scheduler pod config..."
    local epp_pod
    epp_pod=$(kubectl get pod -n "${LLM_NAMESPACE}" \
        -l "app.kubernetes.io/name=${isvc_name},app.kubernetes.io/component=llminferenceservice-router-scheduler" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [ -z "${epp_pod}" ]; then
        warn "No EPP scheduler pod found for '${isvc_name}'. Cannot verify config."
        errors=$((errors + 1))
    else
        local pod_args
        pod_args=$(kubectl get pod -n "${LLM_NAMESPACE}" "${epp_pod}" \
            -o jsonpath='{.spec.containers[0].args}' 2>/dev/null || echo "")
        if echo "${pod_args}" | grep -q "flowControl"; then
            log "EPP scheduler is configured with flowControl."
        else
            warn "EPP scheduler does not appear to have flow control config."
            errors=$((errors + 1))
        fi
    fi

    # 2. InferenceObjective CRDs
    step "Checking InferenceObjective CRDs..."
    for obj in "${INTERACTIVE_FLOW_CONTROL_OBJECTIVE}" "${BATCH_FLOW_CONTROL_OBJECTIVE}"; do
        if kubectl get inferenceobjective "${obj}" -n "${LLM_NAMESPACE}" &>/dev/null; then
            log "InferenceObjective '${obj}' exists."
        else
            warn "InferenceObjective '${obj}' not found."
            errors=$((errors + 1))
        fi
    done

    # 3. Batch processor inferenceObjective config
    step "Checking batch processor config..."
    if kubectl get configmap "${BATCH_INSTANCE_NAME}-processor-config" -n "${BATCH_NAMESPACE}" \
        -o jsonpath='{.data}' 2>/dev/null | grep "inference_objective" | grep -q "${BATCH_FLOW_CONTROL_OBJECTIVE}"; then
        log "Processor configured with inferenceObjective: ${BATCH_FLOW_CONTROL_OBJECTIVE}"
    else
        warn "Processor configmap does not contain inference_objective: ${BATCH_FLOW_CONTROL_OBJECTIVE}"
        errors=$((errors + 1))
    fi

    if [ "${errors}" -gt 0 ]; then
        die "Flow control verification failed with ${errors} error(s). Review output above."
    fi
    log "Flow control verification passed."
}

verify_flow_control_runtime() {
    banner "Verifying Flow Control runtime (metrics)"

    local isvc_name="${ISVC_NAME}"
    local errors=0

    step "Fetching EPP flow control metrics..."

    # Try to find a metrics-reader SA token secret (name varies across RHOAI versions).
    local metrics_token=""
    local metrics_secret
    metrics_secret=$(kubectl get secret -n "${LLM_NAMESPACE}" -o json \
        | jq -r '.items[] | select(.type=="kubernetes.io/service-account-token")
        | select(.metadata.name | test("metrics-reader"))
        | .metadata.name' 2>/dev/null | head -1)
    if [ -n "${metrics_secret}" ]; then
        metrics_token=$(kubectl get secret "${metrics_secret}" -n "${LLM_NAMESPACE}" \
            -o jsonpath='{.data.token}' 2>/dev/null | base64 -d) || true
        log "Using metrics token from secret '${metrics_secret}'."
    else
        log "No metrics-reader secret found. Trying unauthenticated access."
    fi

    local epp_pod
    epp_pod=$(kubectl get pod -n "${LLM_NAMESPACE}" \
        -l "app.kubernetes.io/name=${isvc_name},app.kubernetes.io/component=llminferenceservice-router-scheduler" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [ -z "${epp_pod}" ]; then
        die "No EPP scheduler pod found for '${isvc_name}'."
    fi

    local curl_args=(-sk)
    if [ -n "${metrics_token}" ]; then
        curl_args+=(-H "Authorization: Bearer ${metrics_token}")
    fi

    local metrics_response
    metrics_response=$(kubectl exec -n "${LLM_NAMESPACE}" "${epp_pod}" -c main -- \
        curl "${curl_args[@]}" -w "\n%{http_code}" http://localhost:9090/metrics)

    local metrics_http_code metrics_body
    metrics_http_code=$(echo "${metrics_response}" | tail -1)
    metrics_body=$(echo "${metrics_response}" | sed '$d')
    if [ "${metrics_http_code}" != "200" ]; then
        die "EPP metrics endpoint returned HTTP ${metrics_http_code}."
    fi
    if [ -z "${metrics_body}" ]; then
        die "EPP metrics response body is empty."
    fi

    echo "${metrics_body}" | grep 'inference_extension_flow_control_request_queue_duration_seconds_count'

    # 1. Interactive requests dispatched (priority 0)
    step "Checking flow control metrics for interactive requests (priority 0)..."
    local interactive_count
    interactive_count=$(echo "${metrics_body}" | grep 'inference_extension_flow_control_request_queue_duration_seconds_count' \
        | grep 'priority="0"' | grep 'outcome="Dispatched"' | grep -oE '[0-9]+$' || echo "0")
    if [ "${interactive_count}" -gt 0 ] 2>/dev/null; then
        log "Flow control dispatched ${interactive_count} interactive request(s) (priority 0)."
    else
        warn "No interactive requests (priority 0) found in flow control metrics."
        errors=$((errors + 1))
    fi

    # 2. Batch requests dispatched (priority -1)
    step "Checking flow control metrics for batch requests (priority -1)..."
    local batch_count
    batch_count=$(echo "${metrics_body}" | grep 'inference_extension_flow_control_request_queue_duration_seconds_count' \
        | grep 'priority="-1"' | grep 'outcome="Dispatched"' | grep -oE '[0-9]+$' || echo "0")
    if [ "${batch_count}" -gt 0 ] 2>/dev/null; then
        log "Flow control dispatched ${batch_count} batch request(s) (priority -1)."
    else
        warn "No batch requests (priority -1) found in flow control metrics."
        errors=$((errors + 1))
    fi

    # 3. Pool saturation metric exists
    step "Checking pool saturation metric..."
    if [[ "${metrics_body}" == *"inference_extension_flow_control_pool_saturation"* ]]; then
        local saturation
        saturation=$(echo "${metrics_body}" | grep 'inference_extension_flow_control_pool_saturation{' \
            | grep -oE '[0-9.]+$' | head -1)
        log "Pool saturation: ${saturation}"
    else
        # saturation metric may not be exposed in EPP
        warn "Pool saturation metric not found."
    fi

    if [ "${errors}" -gt 0 ]; then
        die "Flow control runtime verification failed with ${errors} error(s). Review output above."
    fi
    log "Flow control runtime verification passed."
}

# ── 7. Batch Gateway ─────────────────────────────────────────────────────────

deploy_batch_gateway_rhoai() {
    banner "Installing Batch Gateway"
    do_deploy_batch_gateway_dsc "${ISVC_NAME}" "Authorization"
}

check_prerequisites() {
    step "Checking prerequisites..."
    local missing=()
    for cmd in oc kubectl helm jq curl; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    [ ${#missing[@]} -gt 0 ] && die "Missing required tools: ${missing[*]}"

    oc whoami &>/dev/null || die "Not logged in to OpenShift. Run 'oc login' first."
    is_openshift || die "This script requires OpenShift. Use deploy-k8s.sh for vanilla Kubernetes."
    log "Connected to: $(oc whoami --show-server)"
}

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    banner "RHOAI(${OPERATOR_TYPE}) + Batch Gateway Setup"

    check_prerequisites

    # cert manager
    install_cert_manager_operator
    create_selfsigned_issuer

    # lws: https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/ai_workloads/leader-worker-set-operator
    install_lws_operator

    create_inference_external_gateway

    # rhcl: https://docs.redhat.com/en/documentation/red_hat_connectivity_link
    # TODO
    # Pin RHCL to 1.3.x to work around wasm plugin incompatibility with Service Mesh 3.x.
    # RHCL 1.4.x wasm plugin fails with 'allow_on_headers_stop_iteration' unknown field
    # and causes OOM in gateway pods. Uses Manual approval to prevent auto-upgrade.
    # Tracked by: CONNLINK-1130, https://access.redhat.com/solutions/7144055
    # install_connectivity_link
    install_connectivity_link_v1_3

    # rhoai: https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed
    install_rhoai_operator
    apply_dsci_and_dsc

    # llm-d: https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.5/html/deploy_models_using_distributed_inference_with_llm-d
    deploy_llm_inference_service
    if [ "${ENABLE_FLOW_CONTROL}" = "true" ]; then
        create_inference_objectives
    fi
    apply_llm_token_rate_limit

    # batch gateway
    create_batch_internal_gateway
    create_batch_llm_httproute
    apply_batch_llm_auth_policy
    deploy_batch_gateway_rhoai
    apply_batch_auth_policy
    apply_batch_request_rate_limit

    # verify
    if [ "${ENABLE_FLOW_CONTROL}" = "true" ]; then
        verify_flow_control_config
    fi

    echo ""
    log "Setup complete."
    log "  Operator: ${OPERATOR_TYPE}"
    [ -n "${CUSTOM_CATALOG}" ] && log "  Catalog:  ${CUSTOM_CATALOG} (custom)"
    log "  Model: ${MODEL_NAME} (simulator, ${MODEL_REPLICAS} replicas, no GPU)"
    log "  Flow Control: ${ENABLE_FLOW_CONTROL}"
    log "  Batch Gateway: ${BATCH_INSTANCE_NAME} (${BATCH_NAMESPACE}, LLMBatchGateway CR)"
    log ""
    log "Run '$0 test' to verify."
}


# ── Test ──────────────────────────────────────────────────────────────────────

cmd_test() {
    banner "Testing: RHOAI + Batch Gateway"

    set_gateway_url

    local isvc_name="${ISVC_NAME}"

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

    local test_failures=0
    run_tests "${llm_url}" "${GATEWAY_URL}" "${MODEL_NAME}" \
        "Authorization: Bearer ${token}" \
        "Authorization: Bearer ${unauth_token}" \
        "${inference_payload}" \
        || test_failures=$?

    if [ "${ENABLE_FLOW_CONTROL}" = "true" ]; then
        verify_flow_control_runtime
    fi

    return "${test_failures}"
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
    kubectl delete llmbatchgateway "${BATCH_INSTANCE_NAME}" -n "${BATCH_NAMESPACE}" --timeout=60s 2>/dev/null || true
    helm uninstall "${BATCH_INSTANCE_NAME}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    helm uninstall "${BATCH_POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || true
    kubectl delete deployment,svc -l app="${BATCH_MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    kubectl delete pvc "${BATCH_FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true
    force_delete_namespace "${BATCH_NAMESPACE}"

    # DestinationRule (in GATEWAY_NAMESPACE, not deleted with batch namespace)
    kubectl delete destinationrule "${BATCH_INSTANCE_NAME}-backend-tls" -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    # InferenceObjective CRDs (flow control)
    step "Removing InferenceObjective resources..."
    kubectl delete inferenceobjective --all -n "${LLM_NAMESPACE}" 2>/dev/null || true

    # Internal Gateway resources (batch-llm-route)
    step "Removing Internal Gateway resources..."
    kubectl delete authpolicy batch-llm-route-auth -n "${LLM_NAMESPACE}" 2>/dev/null || true
    kubectl delete httproute batch-llm-route -n "${LLM_NAMESPACE}" 2>/dev/null || true

    # TokenRateLimitPolicy
    step "Removing TokenRateLimitPolicy..."
    kubectl delete tokenratelimitpolicy inference-token-limit -n "${GATEWAY_NAMESPACE}" 2>/dev/null || true

    # Named Gateways only (never delete all Gateways in a shared ingress namespace).
    step "Removing Gateways..."
    kubectl delete gateway "${BATCH_INTERNAL_GATEWAY_NAME}" -n "${BATCH_INTERNAL_GATEWAY_NAMESPACE}" 2>/dev/null || true
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

        # Remove custom CatalogSource if it exists
        kubectl delete catalogsource "${OPERATOR_TYPE}-custom-catalog" -n openshift-marketplace 2>/dev/null || true

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

        step "Removing GatewayClass ${GATEWAY_CLASS_NAME}..."
        kubectl delete gatewayclass "${GATEWAY_CLASS_NAME}" 2>/dev/null || true

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
    echo "  CUSTOM_CATALOG    Custom catalog image for operator (creates CatalogSource)"
    echo "  RHOAI_CHANNEL    RHOAI OLM channel"
    echo "  MODEL_NAME       Model name for simulator (default: facebook/opt-125m)"
    echo "  MODEL_REPLICAS   Number of replicas (default: 1)"
    echo "  SIM_IMAGE        Simulator image (default: ghcr.io/llm-d/llm-d-inference-sim:v0.7.1)"
    echo "  BATCH_DB_TYPE          Database: postgresql or redis (default: postgresql)"
    echo "  BATCH_STORAGE_TYPE     File storage: fs or s3 (default: s3)"
    echo "  ENABLE_FLOW_CONTROL   Enable GIE flow control (default: true)"
    echo "  BATCH_FLOW_CONTROL_OBJECTIVE InferenceObjective name for batch (default: batch-sheddable)"
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
