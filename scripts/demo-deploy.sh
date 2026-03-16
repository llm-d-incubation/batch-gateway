#!/bin/bash
set -euo pipefail

# ── Deploy batch-gateway with Istio + GAIE (no Kuadrant/auth/rate-limiting) ──
# Deploys:
#   - Istio (Gateway API + GAIE support)
#   - GAIE InferencePools + EPP
#   - vLLM simulator
#   - Redis (database for batch-gateway)
#   - batch-gateway (apiserver + processor) via helm chart
#
# No Kuadrant, AuthPolicy, or RateLimitPolicy is configured.
# For auth/rate-limiting integration, use deploy-with-kuadrant-*.sh scripts.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/common.sh"

# ── Batch Gateway Configuration ──────────────────────────────────────────────
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
DEV_VERSION="${DEV_VERSION:-latest}"
# Override common.sh defaults: real batch-gateway service instead of nginx stub
BATCH_INFERENCE_SERVICE="${HELM_RELEASE}-apiserver"
BATCH_INFERENCE_PORT=8000
APP_SECRET_NAME="${APP_SECRET_NAME:-${HELM_RELEASE}-secrets}"
FILES_PVC_NAME="${FILES_PVC_NAME:-${HELM_RELEASE}-files}"
DB_TYPE="${DB_TYPE:-postgresql}"
REDIS_RELEASE="${REDIS_RELEASE:-redis}"
POSTGRESQL_RELEASE="${POSTGRESQL_RELEASE:-postgresql}"
# WARNING: Default passwords are for demo only. For production, override via env vars or use K8s secrets.
POSTGRESQL_PASSWORD="${POSTGRESQL_PASSWORD:-postgres}"
# Storage backend: "fs" (PVC) or "s3" (MinIO)
STORAGE_TYPE="${STORAGE_TYPE:-fs}"
MINIO_RELEASE="${MINIO_RELEASE:-minio}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_BUCKET="${MINIO_BUCKET:-batch-gateway}"
TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-selfsigned-issuer}"
GATEWAY_URL="https://localhost:${LOCAL_PORT}"

# ── PVC ──────────────────────────────────────────────────────────────────────

create_pvc() {
    if [ "${STORAGE_TYPE}" != "fs" ]; then
        return
    fi

    step "Creating PVC '${FILES_PVC_NAME}' for file storage..."

    if kubectl get pvc "${FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "PVC '${FILES_PVC_NAME}' already exists. Skipping."
        return
    fi

    # Check for default StorageClass
    local default_sc
    default_sc=$(kubectl get sc -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{end}' 2>/dev/null)
    if [ -z "$default_sc" ]; then
        die "No default StorageClass found. Set one with: kubectl annotate sc <name> storageclass.kubernetes.io/is-default-class=true"
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
    log "PVC '${FILES_PVC_NAME}' created (using default StorageClass)."
}

# ── Redis ────────────────────────────────────────────────────────────────────

install_redis() {
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

install_postgresql() {
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

install_minio() {
    if [ "${STORAGE_TYPE}" != "s3" ]; then
        return
    fi

    step "Installing MinIO (S3-compatible object storage)..."

    if helm status "${MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "MinIO release '${MINIO_RELEASE}' is already installed. Skipping."
        return
    fi

    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
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

    # Create bucket
    step "Creating MinIO bucket '${MINIO_BUCKET}'..."
    kubectl exec -n "${BATCH_NAMESPACE}" deploy/${MINIO_RELEASE} -- \
        mc alias set local http://localhost:9000 "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}" > /dev/null 2>&1
    kubectl exec -n "${BATCH_NAMESPACE}" deploy/${MINIO_RELEASE} -- \
        mc mb --ignore-existing "local/${MINIO_BUCKET}" > /dev/null 2>&1

    log "MinIO installed successfully."
}

create_app_secret() {
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

# ── TLS (cert-manager) ───────────────────────────────────────────────────────

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

create_gateway_tls_cert() {
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

    # Wait for certificate to be ready
    kubectl wait --for=condition=Ready \
        --timeout=60s \
        -n "${INGRESS_NAMESPACE}" \
        certificate/${GATEWAY_NAME}-tls || warn "Certificate not ready yet"

    log "Gateway TLS certificate created."
}

# Override common.sh create_gateway to use HTTPS
create_gateway() {
    step "Creating Istio Gateway (HTTPS)..."

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${GATEWAY_NAME}
  namespace: ${INGRESS_NAMESPACE}
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

create_backend_tls_destinationrule() {
    step "Creating DestinationRule for backend TLS (Gateway → apiserver)..."

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

    log "DestinationRule created (Gateway → apiserver: TLS re-encrypt)."
}

# ── Batch Gateway (helm chart) ───────────────────────────────────────────────

install_batch_gateway() {
    step "Installing batch-gateway via Helm..."

    # Processor sends inference requests through the Gateway → llm-route → InferencePool → EPP → vLLM
    local gw_base="https://${GATEWAY_NAME}-istio.${INGRESS_NAMESPACE}.svc.cluster.local/${LLM_NAMESPACE}"

    local helm_args=(
        --namespace "${BATCH_NAMESPACE}"
        --set "apiserver.image.tag=${DEV_VERSION}"
        --set "processor.image.tag=${DEV_VERSION}"
        --set "global.appSecretName=${APP_SECRET_NAME}"
        --set "global.databaseType=${DB_TYPE}"
        --set "global.fileClient.type=${STORAGE_TYPE}"
        --set "processor.config.modelGateways.${FREE_MODEL}.url=${gw_base}/${FREE_MODEL}"
        --set "processor.config.modelGateways.${FREE_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${FREE_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${FREE_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${FREE_MODEL}.maxBackoff=60s"
        --set "processor.config.modelGateways.${FREE_MODEL}.tlsInsecureSkipVerify=true"
        --set "processor.config.modelGateways.${GOLD_MODEL}.url=${gw_base}/${GOLD_MODEL}"
        --set "processor.config.modelGateways.${GOLD_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${GOLD_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${GOLD_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${GOLD_MODEL}.maxBackoff=60s"
        --set "processor.config.modelGateways.${GOLD_MODEL}.tlsInsecureSkipVerify=true"
        --set "apiserver.tls.enabled=true"
        --set "apiserver.tls.certManager.enabled=true"
        --set "apiserver.tls.certManager.issuerName=${TLS_ISSUER_NAME}"
        --set "apiserver.tls.certManager.issuerKind=ClusterIssuer"
        --set "apiserver.tls.certManager.dnsNames={${HELM_RELEASE}-apiserver,${HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local,localhost}"
    )

    # OpenShift: clear fixed UIDs, let SCC assign them
    if is_openshift; then
        log "OpenShift detected, clearing podSecurityContext for SCC compatibility"
        helm_args+=(
            --set "apiserver.podSecurityContext=null"
            --set "processor.podSecurityContext=null"
        )
    fi

    # Storage-specific helm args
    if [ "${STORAGE_TYPE}" = "s3" ]; then
        local minio_endpoint="http://${MINIO_RELEASE}.${BATCH_NAMESPACE}.svc.cluster.local:9000"
        helm_args+=(
            --set "global.fileClient.s3.endpoint=${minio_endpoint}"
            --set "global.fileClient.s3.region=us-east-1"
            --set "global.fileClient.s3.accessKeyID=${MINIO_ROOT_USER}"
            --set "global.fileClient.s3.prefix=${MINIO_BUCKET}"
            --set "global.fileClient.s3.usePathStyle=true"
        )
    else
        helm_args+=(
            --set "global.fileClient.fs.basePath=/tmp/batch-gateway"
            --set "global.fileClient.fs.pvcName=${FILES_PVC_NAME}"
        )
    fi

    if helm status "${HELM_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Release '${HELM_RELEASE}' already exists. Upgrading..."
        helm upgrade "${HELM_RELEASE}" "${REPO_ROOT}/charts/batch-gateway" "${helm_args[@]}"
    else
        helm install "${HELM_RELEASE}" "${REPO_ROOT}/charts/batch-gateway" "${helm_args[@]}"
    fi

    wait_for_deployment "${HELM_RELEASE}-apiserver" "${BATCH_NAMESPACE}" 120s
    wait_for_deployment "${HELM_RELEASE}-processor" "${BATCH_NAMESPACE}" 120s

    log "batch-gateway installed (apiserver + processor)."
}

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   Istio + GAIE + Batch Gateway Setup                  ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

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

    create_gateway_tls_cert
    create_gateway

    deploy_llm
    create_llm_route

    install_redis
    install_postgresql
    install_minio
    create_app_secret
    create_pvc

    install_batch_gateway
    create_backend_tls_destinationrule
    create_batch_httproute

    # Port-forward HTTPS (443) instead of HTTP (80)
    step "Starting port-forward: ${GATEWAY_NAME}-istio ${LOCAL_PORT}:443 -n ${INGRESS_NAMESPACE}..."
    kubectl port-forward "svc/${GATEWAY_NAME}-istio" "${LOCAL_PORT}:443" -n "${INGRESS_NAMESPACE}" &
    local pf_pid=$!
    disown "${pf_pid}"
    log "Port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"
    log "Gateway available at https://localhost:${LOCAL_PORT}"

    log "Deployment complete! Run '$0 test' to verify."
}

# ── Test ─────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "Istio + GAIE + Batch Gateway"

    local base_url="https://localhost:${LOCAL_PORT}"
    local test_payload='{"model":"'"${FREE_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local test_total=0
    local test_passed=0
    local test_failed=0
    local failed_tests=""

    pass_test() { test_total=$((test_total + 1)); test_passed=$((test_passed + 1)); log "PASSED: $*"; }
    fail_test() { test_total=$((test_total + 1)); test_failed=$((test_failed + 1)); failed_tests="${failed_tests}\n  - $*"; warn "FAILED: $*"; }

    local response http_code body

    # Helper: pretty-print JSON or print raw
    pretty_print() { echo "$1" | jq . 2>/dev/null || echo "$1"; }

    # ── Automation Test (Go E2E) ──────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Automation Test (Go E2E)"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""

    local obs_port=8081
    if ! curl -sf "http://localhost:${obs_port}/ready" &>/dev/null; then
        step "Starting port-forward for apiserver observability (${obs_port})..."
        kubectl port-forward -n "${BATCH_NAMESPACE}" "svc/${BATCH_INFERENCE_SERVICE}" "${obs_port}:8081" &
        disown
        sleep 3
    fi

    step "Running Go E2E tests..."
    (cd "${REPO_ROOT}/test/e2e" && \
        TEST_APISERVER_URL="https://localhost:${LOCAL_PORT}" \
        TEST_APISERVER_OBS_URL="http://localhost:${obs_port}" \
        TEST_NAMESPACE="${BATCH_NAMESPACE}" \
        TEST_HELM_RELEASE="${HELM_RELEASE}" \
        TEST_MODEL="${FREE_MODEL}" \
        go test -v -count=1 -timeout=300s -run '^TestE2E$/^(Files|Batches)$/^Lifecycle$' . 2>&1)
    local e2e_exit=$?

    if [ "$e2e_exit" -eq 0 ]; then
        log "Go E2E tests passed!"
    else
        warn "Go E2E tests failed (exit code: $e2e_exit)"
    fi

    # ── Manual Test (curl) ────────────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Manual Test (curl)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 1: LLM direct inference via llm-route
    echo ""
    echo "── Test 1: LLM direct inference via llm-route ──"
    echo "  \$ curl -sk -X POST ${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions -H 'Content-Type: application/json' -d '${test_payload}'"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    pretty_print "$body"
    if [ "$http_code" = "200" ]; then
        pass_test "Test 1: 200 OK (llm-route → InferencePool → vLLM)"
    else
        fail_test "Test 1: Expected 200, got $http_code"
    fi

    # Verify EPP was called via Envoy ext-proc debug log
    echo ""
    echo "── Test 1b: Verify EPP (ext-proc) was invoked ──"
    local istio_dir="/tmp/istio-${ISTIO_VERSION#v}"
    "${istio_dir}/bin/istioctl" proxy-config log -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --level ext_proc:debug > /dev/null 2>&1
    sleep 3
    curl -sk -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}" > /dev/null 2>&1
    sleep 3
    local envoy_log
    envoy_log=$(kubectl logs -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --since=10s 2>/dev/null)
    "${istio_dir}/bin/istioctl" proxy-config log -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --level ext_proc:warning > /dev/null 2>&1

    local epp_ok=true
    if echo "$envoy_log" | grep -q "Opening gRPC stream to external processor"; then
        echo "  Envoy → EPP gRPC stream: opened ✓"
    else
        echo "  Envoy → EPP gRPC stream: not found ✗"
        epp_ok=false
    fi
    if echo "$envoy_log" | grep -q "Received request headers response"; then
        echo "  EPP response received: ✓"
    else
        echo "  EPP response received: not found ✗"
        epp_ok=false
    fi

    # Verify via Istio metrics: requests routed through InferencePool
    local istio_stats
    istio_stats=$(kubectl exec -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio -- pilot-agent request GET /stats 2>/dev/null \
        | grep "istio_requests_total.*${FREE_MODEL}.*response_code.200" | head -1)
    if [ -n "$istio_stats" ]; then
        local req_count
        req_count=$(echo "$istio_stats" | grep -o '[0-9]*$')
        echo "  Istio metrics: ${req_count} requests routed via InferencePool ✓"
    else
        echo "  Istio metrics: no InferencePool requests found ✗"
        epp_ok=false
    fi

    if [ "$epp_ok" = true ]; then
        pass_test "Test 1b: EPP (ext-proc) verified working"
    else
        fail_test "Test 1b: EPP verification failed"
    fi

    sleep 1

    # Test 1c: Multi-model routing (gold-model)
    echo ""
    echo "── Test 1c: Multi-model routing (${GOLD_MODEL}) ──"
    local gold_payload='{"model":"'"${GOLD_MODEL}"'","messages":[{"role":"user","content":"Hello gold"}]}'
    echo "  \$ curl -sk -X POST ${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    local resp_model
    resp_model=$(echo "$body" | jq -r '.model' 2>/dev/null)
    pretty_print "$body"
    if [ "$http_code" = "200" ] && [ "$resp_model" = "${GOLD_MODEL}" ]; then
        pass_test "Test 1c: 200 OK (${GOLD_MODEL} routed correctly, response model=${resp_model})"
    elif [ "$http_code" = "200" ]; then
        fail_test "Test 1c: 200 but wrong model (expected ${GOLD_MODEL}, got ${resp_model})"
    else
        fail_test "Test 1c: Expected 200, got $http_code"
    fi

    sleep 1

    # Test 2: Upload input file
    echo ""
    echo "── Test 2: Upload batch input file ──"
    local input_file="/tmp/batch-test-input-$$.jsonl"
    cat > "${input_file}" <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${FREE_MODEL}","messages":[{"role":"user","content":"Hello"}]}}
{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"${FREE_MODEL}","messages":[{"role":"user","content":"Tell me a joke"}]}}
JSONL
    echo "  File contents:"
    sed 's/^/    /' "${input_file}"
    echo "  \$ curl -sk -X POST ${base_url}/v1/files -F purpose=batch -F file=@${input_file}"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/v1/files" \
        -F "purpose=batch" \
        -F "file=@${input_file}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    rm -f "${input_file}"
    local file_id=""
    pretty_print "$body"
    if [ "$http_code" = "200" ]; then
        file_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
        pass_test "Test 2: File uploaded (id: ${file_id})"
    else
        fail_test "Test 2: File upload failed (HTTP $http_code)"
    fi

    sleep 1

    # Test 3: Create batch
    echo ""
    echo "── Test 3: Create batch job ──"
    local batch_create_body="{\"input_file_id\":\"${file_id}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}"
    if [ -n "$file_id" ]; then
        echo "  \$ curl -sk -X POST ${base_url}/v1/batches -H 'Content-Type: application/json' -d '${batch_create_body}'"
        response=$(curl -sk -w "\n%{http_code}" \
            -X POST "${base_url}/v1/batches" \
            -H 'Content-Type: application/json' \
            -d "${batch_create_body}")
        http_code=$(echo "$response" | sed -n '$p')
        body=$(echo "$response" | sed '$d')
        local batch_id=""
        pretty_print "$body"
        if [ "$http_code" = "200" ]; then
            batch_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
            pass_test "Test 3: Batch created (id: ${batch_id})"
        else
            fail_test "Test 3: Batch creation failed (HTTP $http_code)"
        fi
    else
        fail_test "Test 3: Skipped (no file_id from Test 2)"
    fi

    sleep 1

    # Test 4: List batches
    echo ""
    echo "── Test 4: List batches ──"
    echo "  \$ curl -sk -X GET ${base_url}/v1/batches"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    local batch_count
    batch_count=$(echo "$body" | jq '.data | length' 2>/dev/null || echo "?")
    if [ "$http_code" = "200" ]; then
        pass_test "Test 4: 200 OK (${batch_count} batches)"
    else
        fail_test "Test 4: Expected 200, got $http_code"
    fi

    sleep 1

    # Test 5: Poll batch status until terminal
    echo ""
    echo "── Test 5: Poll batch status until completion ──"
    if [ -n "$batch_id" ]; then
        echo "  \$ curl -sk ${base_url}/v1/batches/${batch_id}  (polling every 5s)"
        local status="unknown"
        local poll_count=0
        local max_polls=60
        while [ "$poll_count" -lt "$max_polls" ]; do
            response=$(curl -sk "${base_url}/v1/batches/${batch_id}")
            status=$(echo "$response" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
            echo "  Poll $((poll_count+1)): status=${status}"
            if [ "$status" = "completed" ] || [ "$status" = "failed" ] || [ "$status" = "expired" ] || [ "$status" = "cancelled" ]; then
                break
            fi
            poll_count=$((poll_count + 1))
            sleep 5
        done

        pretty_print "$response"
        if [ "$status" = "completed" ]; then
            local completed=$(echo "$response" | grep -o '"completed":[0-9]*' | head -1 | cut -d: -f2)
            local failed_count=$(echo "$response" | grep -o '"failed":[0-9]*' | head -1 | cut -d: -f2)
            pass_test "Test 5: Batch completed (completed=${completed}, failed=${failed_count})"
        else
            fail_test "Test 5: Batch ended with status=${status} (expected completed)"
        fi
    else
        fail_test "Test 5: Skipped (no batch_id from Test 3)"
    fi

    sleep 1

    # Test 6: Download output file
    echo ""
    echo "── Test 6: Download batch output file ──"
    local output_file_id=""
    if [ -n "$batch_id" ]; then
        output_file_id=$(echo "$response" | grep -o '"output_file_id":"[^"]*"' | head -1 | cut -d'"' -f4)
    fi
    if [ -n "$output_file_id" ]; then
        echo "  \$ curl -sk ${base_url}/v1/files/${output_file_id}/content"
        local output_content
        output_content=$(curl -sk "${base_url}/v1/files/${output_file_id}/content")
        # Pretty-print each JSONL line
        echo "$output_content" | while IFS= read -r line; do
            [ -n "$line" ] && pretty_print "$line"
        done
        if [ -n "$output_content" ]; then
            pass_test "Test 6: Output file downloaded (id: ${output_file_id})"
        else
            fail_test "Test 6: Output file is empty"
        fi
    else
        fail_test "Test 6: Skipped (no output_file_id from Test 5)"
    fi

    print_test_summary "$test_total" "$test_passed" "$test_failed" "$failed_tests"
}

# ── Uninstall ────────────────────────────────────────────────────────────────

# No auth resources in this script
cleanup_auth_resources() { true; }

cmd_uninstall_custom() {
    set +e

    echo ""
    echo "  ╔══════════════════════════════════════════════════════════╗"
    echo "  ║   Uninstalling Istio + GAIE + Batch Gateway              ║"
    echo "  ╚══════════════════════════════════════════════════════════╝"
    echo ""

    step "Stopping port-forward processes..."
    pkill -f "kubectl port-forward.*${GATEWAY_NAME}" 2>/dev/null || true

    step "Uninstalling batch-gateway..."
    helm uninstall "${HELM_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || warn "batch-gateway not installed"
    kubectl delete destinationrule "${HELM_RELEASE}-backend-tls" -n "${INGRESS_NAMESPACE}" 2>/dev/null || true

    step "Uninstalling Redis..."
    helm uninstall "${REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || warn "Redis not installed"

    step "Uninstalling PostgreSQL..."
    helm uninstall "${POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout 60s 2>/dev/null || warn "PostgreSQL not installed"

    step "Uninstalling MinIO..."
    kubectl delete deployment,svc -l app="${MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true

    step "Deleting PVC..."
    kubectl delete pvc "${FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" 2>/dev/null || true

    step "Removing gateway resources..."
    timeout_delete 30s gateway --all -n "${INGRESS_NAMESPACE}" || true

    step "Removing routes..."
    timeout_delete 30s httproute --all -n "${BATCH_NAMESPACE}" || true
    timeout_delete 30s httproute --all -n "${LLM_NAMESPACE}" || true

    step "Uninstalling GAIE InferencePools..."
    for release in $(helm list -n "${LLM_NAMESPACE}" -q 2>/dev/null); do
        helm uninstall "${release}" -n "${LLM_NAMESPACE}" --timeout 60s 2>/dev/null || warn "Failed to uninstall ${release}"
    done
    timeout_delete 30s inferencepool --all -n "${LLM_NAMESPACE}" || true
    timeout_delete 30s deployment,svc --all -n "${LLM_NAMESPACE}" || true

    step "Uninstalling Istio..."
    local istio_dir="/tmp/istio-${ISTIO_VERSION#v}"
    if [ -d "${istio_dir}" ]; then
        "${istio_dir}/bin/istioctl" uninstall --purge -y 2>/dev/null || warn "istioctl uninstall failed"
    else
        kubectl delete deploy,svc --all -n istio-system 2>/dev/null || true
    fi

    step "Cleaning up CRDs..."
    force_delete_crds 'inference\.networking'
    force_delete_crds 'istio\.io|sail'
    force_delete_namespace "istio-system"

    step "Uninstalling cert-manager..."
    kubectl delete clusterissuer "${TLS_ISSUER_NAME}" 2>/dev/null || true
    helm uninstall cert-manager -n cert-manager --timeout 60s 2>/dev/null || warn "cert-manager not installed"
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
    set -e
}

# ── Usage (override common.sh) ────────────────────────────────────────────────

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Commands:"
    echo "  install    Deploy Istio, GAIE, cert-manager, databases, and batch-gateway"
    echo "  test       Run automated (Go E2E) and manual (curl) tests"
    echo "  uninstall  Remove all deployed components"
    echo "  help       Show this help message"
    echo ""
    echo "Environment Variables:"
    echo ""
    echo "  Batch Gateway:"
    echo "    HELM_RELEASE          Helm release name (default: batch-gateway)"
    echo "    DEV_VERSION           Image tag for apiserver/processor (default: latest)"
    echo "    BATCH_NAMESPACE       Namespace for batch-gateway (default: batch-api)"
    echo "    LLM_NAMESPACE         Namespace for model servers (default: llm)"
    echo "    LOCAL_PORT            Local port for Gateway port-forward (default: 8080)"
    echo ""
    echo "  Database:"
    echo "    DB_TYPE               Database type: postgresql or redis (default: postgresql)"
    echo "    POSTGRESQL_PASSWORD   PostgreSQL password (default: postgres)"
    echo ""
    echo "  Storage:"
    echo "    STORAGE_TYPE          File storage backend: fs or s3 (default: fs)"
    echo "                          fs  — PersistentVolumeClaim (requires default StorageClass)"
    echo "                          s3  — MinIO S3-compatible object storage"
    echo "    MINIO_ROOT_USER       MinIO root user (default: minioadmin)"
    echo "    MINIO_ROOT_PASSWORD   MinIO root password (default: minioadmin)"
    echo "    MINIO_BUCKET          MinIO bucket name (default: batch-gateway)"
    echo ""
    echo "  Models:"
    echo "    FREE_MODEL            Free tier model name (default: free-model)"
    echo "    GOLD_MODEL            Gold tier model name (default: gold-model)"
    echo ""
    echo "Examples:"
    echo "  $0 install                          # Default: fs storage, postgresql"
    echo "  STORAGE_TYPE=s3 $0 install          # S3 (MinIO) storage"
    echo "  DEV_VERSION=v1.0.0 $0 install       # Specific image version"
    exit "${1:-0}"
}

# ── Main ─────────────────────────────────────────────────────────────────────

if [ $# -eq 0 ]; then
    usage 0
fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    test)      shift; cmd_test "$@" ;;
    uninstall) shift; cmd_uninstall_custom "$@" ;;
    help|-h|--help) usage 0 ;;
    *) echo "Error: Unknown command '$1'"; echo ""; usage 1 ;;
esac
