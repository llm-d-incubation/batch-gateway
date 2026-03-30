#!/bin/bash
# Common functions

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
banner() {
    local title="$1"
    local len=${#title}
    local border; border=$(printf '═%.0s' $(seq 1 $((len + 4))))
    echo ""
    echo "  ╔${border}╗"
    printf "  ║  %s  ║\n" "${title}"
    echo "  ╚${border}╝"
    echo ""
}

TLS_ISSUER_NAME="${TLS_ISSUER_NAME:-selfsigned-issuer}"

# Batch Gateway configuration
BATCH_NAMESPACE="${BATCH_NAMESPACE:-batch-api}"
BATCH_HELM_RELEASE="${BATCH_HELM_RELEASE:-batch-gateway}"
BATCH_DEV_VERSION="${BATCH_DEV_VERSION:-latest}"
BATCH_INFERENCE_SERVICE="${BATCH_INFERENCE_SERVICE:-${BATCH_HELM_RELEASE}-apiserver}"
BATCH_INFERENCE_PORT="${BATCH_INFERENCE_PORT:-8000}"
BATCH_APP_SECRET_NAME="${BATCH_APP_SECRET_NAME:-${BATCH_HELM_RELEASE}-secrets}"
BATCH_FILES_PVC_NAME="${BATCH_FILES_PVC_NAME:-${BATCH_HELM_RELEASE}-files}"
BATCH_DB_TYPE="${BATCH_DB_TYPE:-postgresql}"
# Default HTTP settings for model gateway entries.
# Each per-model entry must be fully specified (no inheritance).
GW_REQUEST_TIMEOUT="${GW_REQUEST_TIMEOUT:-5m}"
GW_MAX_RETRIES="${GW_MAX_RETRIES:-3}"
GW_INITIAL_BACKOFF="${GW_INITIAL_BACKOFF:-1s}"
GW_MAX_BACKOFF="${GW_MAX_BACKOFF:-60s}"
BATCH_REDIS_RELEASE="${BATCH_REDIS_RELEASE:-redis}"
BATCH_POSTGRESQL_RELEASE="${BATCH_POSTGRESQL_RELEASE:-postgresql}"
# WARNING: Default passwords are for demo only. For production, override via env vars or use K8s secrets.
BATCH_POSTGRESQL_PASSWORD="${BATCH_POSTGRESQL_PASSWORD:-postgres}"
BATCH_STORAGE_TYPE="${BATCH_STORAGE_TYPE:-fs}"
BATCH_MINIO_RELEASE="${BATCH_MINIO_RELEASE:-minio}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_BUCKET="${MINIO_BUCKET:-batch-gateway}"


# ── Helper Functions ──────────────────────────────────────────────────────────

is_openshift() {
    kubectl api-resources --api-group=route.openshift.io &>/dev/null 2>&1
}

gen_id() {
    uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "${RANDOM}-${RANDOM}-$$"
}

wait_for_deployment() {
    local deploy_name="$1"
    local namespace="$2"
    local timeout="${3:-120s}"

    step "Waiting for deployment '${deploy_name}' to be ready..."

    local retries=0
    while ! kubectl get deploy "${deploy_name}" -n "${namespace}" &>/dev/null; do
        retries=$((retries + 1))
        if [ "$retries" -ge 5 ]; then
            die "Deployment '${deploy_name}' did not become ready"
        fi
        warn "Deployment not yet visible, retrying in 2s... ($retries/5)"
        sleep 2
    done

    kubectl rollout status deploy/"${deploy_name}" -n "${namespace}" --timeout="${timeout}"
    log "Deployment '${deploy_name}' is ready."
}

wait_for_subscription() {
    local namespace="$1"
    local sub_name="$2"

    step "Waiting for subscription '${sub_name}' to be ready..."

    local retries=0
    while true; do
        local csv
        csv=$(kubectl get subscription.operators.coreos.com "${sub_name}" -n "${namespace}" \
            -o jsonpath='{.status.currentCSV}' 2>/dev/null || echo "")
        if [ -n "${csv}" ]; then
            local phase
            phase=$(kubectl get csv "${csv}" -n "${namespace}" \
                -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [ "${phase}" = "Succeeded" ]; then
                log "Subscription '${sub_name}' is ready (CSV: ${csv})."
                return
            fi
        fi
        retries=$((retries + 1))
        if [ "$retries" -ge 60 ]; then
            die "Subscription '${sub_name}' did not become ready after 300s"
        fi
        sleep 5
    done
}

wait_for_crd() {
    local crd="$1"
    local timeout="${2:-120}"

    step "Waiting for CRD '${crd}'..."
    local elapsed=0
    while ! kubectl get crd "${crd}" &>/dev/null; do
        if [ "$elapsed" -ge "$timeout" ]; then
            die "CRD ${crd} not available after ${timeout}s"
        fi
        elapsed=$((elapsed + 5))
        sleep 5
    done
    log "CRD '${crd}' is available."
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

# ── TLS ──────────────────────────────────────────────────────────────────────

create_selfsigned_issuer() {
    step "Creating self-signed ClusterIssuer '${TLS_ISSUER_NAME}'..."
    if kubectl get clusterissuer "${TLS_ISSUER_NAME}" &>/dev/null; then
        log "ClusterIssuer '${TLS_ISSUER_NAME}' already exists. Skipping."
        return
    fi
    # cert-manager webhook may not be fully ready (TLS bootstrap race condition).
    # Retry a few times to allow the webhook certificate to propagate.
    local attempt
    for attempt in 1 2 3 4 5; do
        if kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${TLS_ISSUER_NAME}
spec:
  selfSigned: {}
EOF
        then
            log "ClusterIssuer '${TLS_ISSUER_NAME}' created."
            return
        fi
        warn "Attempt ${attempt}/5 failed. Waiting 10s for webhook TLS to be ready..."
        sleep 10
    done
    die "Failed to create ClusterIssuer after 5 attempts."
}

# ── Gateway URL ──────────────────────────────────────────────────────────────
#
# Resolves the external gateway URL. Priority:
#
#   1. spec.listeners[0].hostname — OpenShift router uses SNI (Server Name
#      Indication) to route requests to the correct backend. The hostname
#      (e.g. llm-inference.apps.xxx) must be present in the TLS ClientHello
#      for the router to match it. Accessing via raw LB address fails because
#      the router cannot determine which route to use without the hostname.
#
#   2. status.addresses[0].value — On vanilla K8s with Istio, the gateway
#      terminates TLS directly and routes by path, not by hostname. Listeners
#      typically have no hostname configured, so the LB address works fine.
#
#   3. port-forward — Fallback for clusters without a LoadBalancer (e.g.
#      kind, minikube).

set_gateway_url() {
    # 1. Prefer spec hostname (required for SNI-based routing, e.g. OpenShift)
    local gw_hostname
    gw_hostname=$(kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" \
        -o jsonpath='{.spec.listeners[0].hostname}' 2>/dev/null || echo "")
    if [[ -n "${gw_hostname}" ]]; then
        log "Gateway hostname: ${gw_hostname}"
        export GATEWAY_URL="https://${gw_hostname}"
        log "Gateway URL: ${GATEWAY_URL}"
        return
    fi

    # 2. Fall back to status address (e.g. LoadBalancer IP/hostname on vanilla K8s)
    local gw_addr
    gw_addr=$(kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" \
        -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || echo "")
    if [[ -n "${gw_addr}" ]]; then
        log "Gateway address: ${gw_addr}"
        export GATEWAY_URL="https://${gw_addr}"
        log "Gateway URL: ${GATEWAY_URL}"
        return
    fi

    # 3. Fall back to port-forward (no external address, e.g. kind/minikube)
    log "No external address found, falling back to port-forward."
    local gateway_svc
    gateway_svc=$(kubectl get svc -n "${GATEWAY_NAMESPACE}" \
        -l "gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    [ -z "${gateway_svc}" ] && die "No service found for gateway '${GATEWAY_NAME}'."
    step "Starting port-forward: ${gateway_svc} ${GATEWAY_LOCAL_PORT}:443 -n ${GATEWAY_NAMESPACE}..."
    kubectl port-forward "svc/${gateway_svc}" "${GATEWAY_LOCAL_PORT}:443" -n "${GATEWAY_NAMESPACE}" &
    disown $!
    log "Port-forward PID: $!"
    export GATEWAY_URL="https://localhost:${GATEWAY_LOCAL_PORT}"
    log "Gateway URL: ${GATEWAY_URL}"
}

# ── Shared Route & DestinationRule Functions ──────────────────────────────────

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
    namespace: ${GATEWAY_NAMESPACE}
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

    log "batch-route created (${BATCH_NAMESPACE}): /v1/batches, /v1/files -> ${BATCH_INFERENCE_SERVICE}"
}

# Tells Istio to use TLS when connecting to the apiserver backend.
# Without this, the Gateway would send plaintext to the HTTPS apiserver port.
create_batch_destinationrule() {
    step "Creating DestinationRule for backend TLS (Gateway -> apiserver)..."
    kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: ${BATCH_HELM_RELEASE}-backend-tls
  namespace: ${GATEWAY_NAMESPACE}
spec:
  host: ${BATCH_HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local
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

# ── Database / Storage Functions ──────────────────────────────────────────────

install_batch_redis() {
    step "Installing Redis..."
    if helm status "${BATCH_REDIS_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Redis release '${BATCH_REDIS_RELEASE}' is already installed. Skipping."
        return
    fi
    helm install "${BATCH_REDIS_RELEASE}" oci://registry-1.docker.io/bitnamicharts/redis \
        --namespace "${BATCH_NAMESPACE}" --create-namespace \
        --set architecture=standalone \
        --set auth.enabled=false
    kubectl rollout status statefulset/"${BATCH_REDIS_RELEASE}-master" -n "${BATCH_NAMESPACE}" --timeout=120s
    log "Redis installed (standalone, no auth)."
}

install_batch_postgresql() {
    step "Installing PostgreSQL..."
    if helm status "${BATCH_POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "PostgreSQL release '${BATCH_POSTGRESQL_RELEASE}' is already installed. Skipping."
        return
    fi
    helm install "${BATCH_POSTGRESQL_RELEASE}" oci://registry-1.docker.io/bitnamicharts/postgresql \
        --namespace "${BATCH_NAMESPACE}" --create-namespace \
        --set auth.postgresPassword="${BATCH_POSTGRESQL_PASSWORD}" \
        --set auth.database=batch

    step "Waiting for PostgreSQL to be ready..."
    kubectl rollout status statefulset/"${BATCH_POSTGRESQL_RELEASE}" -n "${BATCH_NAMESPACE}" --timeout=120s
    log "PostgreSQL installed (database: batch)."
}

install_batch_minio() {
    step "Installing MinIO..."
    if kubectl get deployment "${BATCH_MINIO_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "MinIO deployment '${BATCH_MINIO_RELEASE}' already exists. Skipping."
        return
    fi

    kubectl create namespace "${BATCH_NAMESPACE}" 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${BATCH_MINIO_RELEASE}
  namespace: ${BATCH_NAMESPACE}
  labels:
    app: ${BATCH_MINIO_RELEASE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${BATCH_MINIO_RELEASE}
  template:
    metadata:
      labels:
        app: ${BATCH_MINIO_RELEASE}
    spec:
      containers:
      - name: minio
        image: quay.io/minio/minio:RELEASE.2024-12-18T13-15-44Z
        args:
        - server
        - /data
        - --console-address
        - ":9001"
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
        volumeMounts:
        - name: data
          mountPath: /data
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${BATCH_MINIO_RELEASE}
  namespace: ${BATCH_NAMESPACE}
  labels:
    app: ${BATCH_MINIO_RELEASE}
spec:
  selector:
    app: ${BATCH_MINIO_RELEASE}
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
  type: ClusterIP
EOF

    wait_for_deployment "${BATCH_MINIO_RELEASE}" "${BATCH_NAMESPACE}" 120s

    step "Creating MinIO bucket '${MINIO_BUCKET}'..."
    local minio_pod
    minio_pod=$(kubectl get pod -n "${BATCH_NAMESPACE}" -l app="${BATCH_MINIO_RELEASE}" -o jsonpath='{.items[0].metadata.name}')
    [[ -z "${minio_pod}" ]] && die "No MinIO pod found with label app=${BATCH_MINIO_RELEASE} in namespace ${BATCH_NAMESPACE}"
    kubectl exec -n "${BATCH_NAMESPACE}" "${minio_pod}" -- \
        mc alias set local http://localhost:9000 "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}" 2>/dev/null \
        || die "Failed to configure MinIO client. Check credentials and MinIO readiness."
    kubectl exec -n "${BATCH_NAMESPACE}" "${minio_pod}" -- \
        mc mb "local/${MINIO_BUCKET}" 2>/dev/null || true
    log "MinIO installed (bucket: ${MINIO_BUCKET})."
}

create_batch_pvc() {
    if kubectl get pvc "${BATCH_FILES_PVC_NAME}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "PVC '${BATCH_FILES_PVC_NAME}' already exists. Skipping."
        return
    fi
    step "Creating PVC '${BATCH_FILES_PVC_NAME}'..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${BATCH_FILES_PVC_NAME}
  namespace: ${BATCH_NAMESPACE}
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
EOF
    log "PVC '${BATCH_FILES_PVC_NAME}' created."
}

create_batch_secret() {
    step "Creating app secret '${BATCH_APP_SECRET_NAME}'..."

    local redis_url="redis://${BATCH_REDIS_RELEASE}-master.${BATCH_NAMESPACE}.svc.cluster.local:6379/0"
    local postgresql_url="postgresql://postgres:${BATCH_POSTGRESQL_PASSWORD}@${BATCH_POSTGRESQL_RELEASE}.${BATCH_NAMESPACE}.svc.cluster.local:5432/batch?sslmode=disable"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${BATCH_APP_SECRET_NAME}
  namespace: ${BATCH_NAMESPACE}
stringData:
  redis-url: "${redis_url}"
  postgresql-url: "${postgresql_url}"
  s3-secret-access-key: "${MINIO_ROOT_PASSWORD}"
EOF
    log "Secret '${BATCH_APP_SECRET_NAME}' applied."
}

# install_batch_gateway [helm_args...]
# Installs batch-gateway via helm chart. Callers pass the complete helm args array.
install_batch_gateway() {
    step "Installing batch-gateway via Helm..."

    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

    if helm status "${BATCH_HELM_RELEASE}" -n "${BATCH_NAMESPACE}" &>/dev/null; then
        log "Release '${BATCH_HELM_RELEASE}' already exists. Upgrading..."
        helm upgrade "${BATCH_HELM_RELEASE}" "${repo_root}/charts/batch-gateway" "$@"
    else
        helm install "${BATCH_HELM_RELEASE}" "${repo_root}/charts/batch-gateway" "$@"
    fi

    wait_for_deployment "${BATCH_HELM_RELEASE}-apiserver" "${BATCH_NAMESPACE}" 120s
    wait_for_deployment "${BATCH_HELM_RELEASE}-processor" "${BATCH_NAMESPACE}" 120s

    log "batch-gateway installed (apiserver + processor)."
}

# do_deploy_batch_gateway [extra_helm_args...]
# Full batch-gateway deployment: databases, storage, helm chart and routing.
# Common helm args are built here; callers only pass script-specific overrides
# (e.g. modelGateways, passThroughHeaders).
do_deploy_batch_gateway() {
    kubectl get namespace "${BATCH_NAMESPACE}" &>/dev/null || kubectl create namespace "${BATCH_NAMESPACE}"

    install_batch_redis
    install_batch_postgresql
    install_batch_minio
    create_batch_secret
    create_batch_pvc

    local helm_args=(
        --namespace "${BATCH_NAMESPACE}"
        --set "apiserver.image.tag=${BATCH_DEV_VERSION}"
        --set "processor.image.tag=${BATCH_DEV_VERSION}"
        --set "global.secretName=${BATCH_APP_SECRET_NAME}"
        --set "global.databaseType=${BATCH_DB_TYPE}"
        --set "global.fileClient.type=${BATCH_STORAGE_TYPE}"
        --set "apiserver.tls.enabled=true"
        --set "apiserver.tls.certManager.enabled=true"
        --set "apiserver.tls.certManager.issuerName=${TLS_ISSUER_NAME}"
        --set "apiserver.tls.certManager.issuerKind=ClusterIssuer"
        --set "apiserver.tls.certManager.dnsNames={${BATCH_HELM_RELEASE}-apiserver,${BATCH_HELM_RELEASE}-apiserver.${BATCH_NAMESPACE}.svc.cluster.local,localhost}"
    )

    if [ "${BATCH_STORAGE_TYPE}" = "s3" ]; then
        local minio_endpoint="http://${BATCH_MINIO_RELEASE}.${BATCH_NAMESPACE}.svc.cluster.local:9000"
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
            --set "global.fileClient.fs.pvcName=${BATCH_FILES_PVC_NAME}"
        )
    fi

    if is_openshift; then
        log "OpenShift detected, clearing podSecurityContext for SCC compatibility"
        helm_args+=(
            --set "apiserver.podSecurityContext=null"
            --set "processor.podSecurityContext=null"
        )
    fi

    # Append caller-specific args (modelGateways, passThroughHeaders, etc.)
    install_batch_gateway "${helm_args[@]}" "$@"
    create_batch_httproute
    create_batch_destinationrule
}

# ── Test Framework ────────────────────────────────────────────────────────────
#
# Usage:
#   init_test_framework                          # reset counters
#   test_group_header "LLM Authentication"       # print group header
#   assert_http 401 "No auth -> 401" "${url}"    # simple HTTP code assertion
#   assert_http 200 "Auth -> 200" -H "Authorization: Bearer ${t}" "${url}"
#   run_batch_tests batch_url model authorized_header unauthorized_header [extra_headers...]
#   finish_tests                                 # print summary + wait

# run_tests llm_url batch_url model_name authorized_header unauthorized_header \
#           inference_payload [extra_headers...]
#
# Runs ALL test groups:
#   1. LLM Authentication     (unauthenticated -> 401, authenticated -> 200)
#   2. LLM Authorization      (unauthorized -> 403, authorized -> 200)
#   3. LLM Token Rate Limit   (send requests until 429)
#   4. sleep 60
#   5. Batch Authentication   (unauthenticated -> 401, authenticated -> 200)
#   6. Batch Authorization    (unauthorized batch -> LLM route rejects)
#   7. Batch Lifecycle        (upload + create + poll -> completed)
#   8. Batch Request Rate Limit (rapid requests -> 429)
#
# llm_url:            inference endpoint (e.g. .../v1/chat/completions)
# batch_url:          batch base URL (e.g. https://host)
# authorized_header:        authorized user header (e.g. "Authorization: Bearer xxx")
# unauthorized_header:      unauthorized user header
# inference_payload:  JSON body for inference requests
# extra_headers:      optional headers for token rate limit and batch creation
#                     (e.g. "X-MaaS-Subscription: xxx")
run_tests() {
    local llm_url="$1"
    local batch_url="$2"
    local model_name="$3"
    local authorized_header="$4"
    local unauthorized_header="$5"
    local inference_payload="$6"
    shift 6
    local extra_headers=("$@")

    # ── Test framework ───────────────────────────────────────────
    local _T=0 _TEST_TOTAL=0 _TEST_PASSED=0 _TEST_FAILED=0 _TEST_FAILED_LIST=""

    next_test() { _T=$((_T + 1)); echo ""; echo "── Test ${_T}: $* ──"; }
    pass_test() { _TEST_TOTAL=$((_TEST_TOTAL + 1)); _TEST_PASSED=$((_TEST_PASSED + 1)); log "PASSED: Test ${_T}: $*"; }
    fail_test() { _TEST_TOTAL=$((_TEST_TOTAL + 1)); _TEST_FAILED=$((_TEST_FAILED + 1)); _TEST_FAILED_LIST="${_TEST_FAILED_LIST}\n  - Test ${_T}: $*"; warn "FAILED: Test ${_T}: $*"; }

    test_group_header() {
        echo ""
        echo "======================================================="
        echo "  $1"
        echo "======================================================="
    }

    # assert_http expected_code description curl_args...
    assert_http() {
        local expected="$1" desc="$2"
        shift 2
        next_test "${desc}"
        local http_code
        http_code=$(curl -sk -o /dev/null -w '%{http_code}' "$@")
        echo "  HTTP ${http_code}"
        if [ "$http_code" = "$expected" ]; then
            pass_test "${desc}"
        else
            fail_test "Expected ${expected}, got HTTP ${http_code}"
        fi
    }

    local response http_code body

    # ── Internal helpers ─────────────────────────────────────────

    # Extract JSON field (jq with grep fallback)
    _jval() {
        local field="$1" data="$2" val
        val=$(echo "$data" | jq -r ".${field} // empty" 2>/dev/null)
        [ -z "$val" ] && val=$(echo "$data" | grep -o "\"${field}\":\"[^\"]*\"" | head -1 | cut -d'"' -f4)
        echo "$val"
    }

    # Upload file + create batch; sets _BATCH_ID on success
    _create_batch() {
        local url="$1" header="$2" model="$3"
        shift 3
        local extra_h=("$@")

        local input_file="/tmp/batch-$$-${RANDOM}.jsonl"
        cat > "${input_file}" <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${model}","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}}
JSONL
        response=$(curl -sk -w "\n%{http_code}" -X POST "${url}/v1/files" \
            -H "${header}" -F "purpose=batch" -F "file=@${input_file}")
        http_code=$(echo "$response" | sed -n '$p')
        body=$(echo "$response" | sed '$d')
        rm -f "${input_file}"

        _BATCH_FILE_ID=""
        if [ "$http_code" = "200" ]; then
            _BATCH_FILE_ID=$(_jval id "$body")
            echo "  File uploaded: ${_BATCH_FILE_ID}"
        else
            return 1
        fi

        local create_args=(-H "${header}" -H 'Content-Type: application/json')
        for h in ${extra_h[@]+"${extra_h[@]}"}; do create_args+=(-H "$h"); done
        response=$(curl -sk -w "\n%{http_code}" -X POST "${url}/v1/batches" \
            "${create_args[@]}" \
            -d "{\"input_file_id\":\"${_BATCH_FILE_ID}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")
        http_code=$(echo "$response" | sed -n '$p')
        body=$(echo "$response" | sed '$d')

        _BATCH_ID=""
        if [ "$http_code" = "200" ]; then
            _BATCH_ID=$(_jval id "$body")
            echo "  Batch created: ${_BATCH_ID}"
        else
            return 1
        fi
    }

    # Poll batch until terminal state; sets _BATCH_STATUS, _BATCH_FAILED
    _poll_batch() {
        local url="$1" batch_id="$2" header="$3"
        _BATCH_STATUS="unknown"
        _BATCH_FAILED=0
        local poll=0
        while [ "$poll" -lt 60 ]; do
            response=$(curl -sk "${url}/v1/batches/${batch_id}" -H "${header}")
            _BATCH_STATUS=$(_jval status "$response")
            _BATCH_FAILED=$(echo "$response" | jq -r '.request_counts.failed // 0' 2>/dev/null)
            [ "$_BATCH_FAILED" = "null" ] || [ -z "$_BATCH_FAILED" ] && \
                _BATCH_FAILED=$(echo "$response" | grep -o '"failed":[0-9]*' | head -1 | cut -d':' -f2)
            echo "  Poll $((poll+1)): status=${_BATCH_STATUS}, failed=${_BATCH_FAILED:-0}"
            case "$_BATCH_STATUS" in completed|failed|expired|cancelled) break ;; esac
            poll=$((poll + 1))
            sleep 5
        done
    }

    # ── 1. LLM Authn ───────────────────────────────────
    test_group_header "LLM Authn"
    assert_http 401 "Unauthenticated inference -> 401" \
        -X POST "${llm_url}" -H "Content-Type: application/json" -d "${inference_payload}"
    assert_http 200 "Authenticated inference -> 200" \
        -X POST "${llm_url}" -H "${authorized_header}" -H "Content-Type: application/json" -d "${inference_payload}"

    # ── 2. LLM Authz ────────────────────────────────────
    test_group_header "LLM Authz"
    assert_http 403 "Unauthorized inference -> 403" \
        -X POST "${llm_url}" -H "${unauthorized_header}" -H "Content-Type: application/json" -d "${inference_payload}"
    assert_http 200 "Authorized inference -> 200" \
        -X POST "${llm_url}" -H "${authorized_header}" -H "Content-Type: application/json" -d "${inference_payload}"

    # ── 3. LLM Token Rate Limit ─────────────────────────────────
    test_group_header "LLM Token Rate Limit"
    next_test "Token rate limiting (inference)"
    echo "  Goal: Send inference requests until 429"
    local trl_success=0 trl_limited=false
    local trl_args=(-H "${authorized_header}" -H "Content-Type: application/json")
    for h in ${extra_headers[@]+"${extra_headers[@]}"}; do trl_args+=(-H "$h"); done
    for i in $(seq 1 100); do
        http_code=$(curl -sk -o /dev/null -w '%{http_code}' \
            "${trl_args[@]}" \
            -d "{\"model\":\"${model_name}\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}],\"max_tokens\":100}" \
            "${llm_url}")
        if [ "$http_code" = "429" ]; then
            echo "  Request $i: 429 Token Rate Limited"
            trl_limited=true
            break
        else
            trl_success=$((trl_success + 1))
            [ "$i" -le 3 ] && echo "  Request $i: $http_code"
        fi
        sleep 0.2
    done
    if [ "$trl_limited" = "true" ]; then
        pass_test "Token rate limiting triggered after $trl_success requests"
    else
        fail_test "Token rate limit not triggered after 100 requests"
    fi
    echo ""
    echo "  Waiting 60s for rate limit counters to reset..."
    sleep 60

    # ── 4. Batch Authn ──────────────────────────────────
    test_group_header "Batch Authn"
    assert_http 401 "Unauthenticated batch request -> 401" "${batch_url}/v1/batches"
    assert_http 200 "Authenticated batch request -> 200" -H "${authorized_header}" "${batch_url}/v1/batches"

    # ── 5. Batch Authz (LLM route enforces) ──────────────
    test_group_header "Batch Authz (LLM route enforces)"
    next_test "Unauthorized user batch -> LLM route rejects with 403"
    echo "  Goal: Batch processor forwards unauthorized credentials to LLM route, gets 403"

    if _create_batch "${batch_url}" "${unauthorized_header}" "${model_name}"; then
        _poll_batch "${batch_url}" "${_BATCH_ID}" "${unauthorized_header}"
        if [ "${_BATCH_FAILED:-0}" -gt 0 ] || [ "$_BATCH_STATUS" = "failed" ]; then
            local output_file_id
            output_file_id=$(curl -sk "${batch_url}/v1/batches/${_BATCH_ID}" \
                -H "${unauthorized_header}" | jq -r '.output_file_id // .error_file_id // empty')
            local output_content=""
            [ -n "$output_file_id" ] && \
                output_content=$(curl -sk "${batch_url}/v1/files/${output_file_id}/content" \
                    -H "${unauthorized_header}")
            if echo "$output_content" | grep -qE '"status_code":403|HTTP 403'; then
                pass_test "LLM route rejected with 403 (status=${_BATCH_STATUS})"
            else
                fail_test "Requests failed but not with 403: ${output_content:0:200}"
            fi
        else
            fail_test "Expected failed requests, got status=${_BATCH_STATUS}, failed=${_BATCH_FAILED:-0}"
        fi
    else
        fail_test "Batch creation failed (HTTP $http_code)"
    fi

    # ── 6. Batch Lifecycle ───────────────────────────────────────
    test_group_header "Batch Lifecycle"
    next_test "Upload file + create batch"
    if _create_batch "${batch_url}" "${authorized_header}" "${model_name}" ${extra_headers[@]+"${extra_headers[@]}"}; then
        pass_test "File uploaded + batch created (batch: ${_BATCH_ID})"

        next_test "Batch completion"
        echo "  Goal: Verify processor forwards credentials through gateway to model"
        _poll_batch "${batch_url}" "${_BATCH_ID}" "${authorized_header}"
        if [ "$_BATCH_STATUS" = "completed" ]; then
            pass_test "Batch completed successfully"
        else
            fail_test "Batch ended with status=${_BATCH_STATUS} (expected completed)"
        fi
    else
        fail_test "Batch creation failed (HTTP $http_code)"
    fi

    # ── 7. Batch Request Rate Limit ──────────────────────────────
    test_group_header "Batch Request Rate Limit"
    next_test "Batch API rate limiting"
    local rl_success=0 rl_limited=0
    for i in $(seq 1 25); do
        http_code=$(curl -sk -o /dev/null -w '%{http_code}' -H "${authorized_header}" "${batch_url}/v1/batches")
        if [ "$http_code" = "429" ]; then
            rl_limited=$((rl_limited + 1))
            echo "  Request $i: 429 Rate Limited"
        else
            rl_success=$((rl_success + 1))
            [ "$i" -le 3 ] && echo "  Request $i: $http_code"
        fi
    done
    echo "  Result: $rl_success passed, $rl_limited rate-limited"
    if [ "$rl_limited" -ge 1 ]; then
        pass_test "Rate limiting is working"
    else
        fail_test "No 429 received after 25 requests"
    fi
    echo ""
    echo "  Waiting 60s for rate limit counters to reset..."
    sleep 60

    # ── Summary ─────────────────────────────────────────────────
    test_group_header "Summary"
    if [ "$_TEST_FAILED" -eq 0 ]; then
        log "All ${_TEST_TOTAL} tests passed!"
    else
        warn "${_TEST_FAILED}/${_TEST_TOTAL} tests failed:"
        echo -e "${_TEST_FAILED_LIST}"
        echo ""
    fi
    echo "  Passed: ${_TEST_PASSED}  Failed: ${_TEST_FAILED}  Total: ${_TEST_TOTAL}"

}
