#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Source common functions and configuration
source "${SCRIPT_DIR}/dev-common.sh"

# ── Deployment-Specific Configuration ────────────────────────────────────────
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-batch-gateway-dev}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-mirror.gcr.io}"
IMAGE_TAG="${IMAGE_TAG:-0.0.1}"
SKIP_BUILD="${SKIP_BUILD:-false}"
POSTGRESQL_PASSWORD="${POSTGRESQL_PASSWORD:-postgres}"
INFERENCE_API_KEY="${INFERENCE_API_KEY:-dummy-api-key}"
S3_SECRET_ACCESS_KEY="${S3_SECRET_ACCESS_KEY:-minioadmin}"
FILE_CLIENT_TYPE="${FILE_CLIENT_TYPE:-s3}"
DB_CLIENT_TYPE="${DB_CLIENT_TYPE:-postgresql}"
MINIO_IMAGE="${MINIO_IMAGE:-${IMAGE_REGISTRY}/minio/minio:latest}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-minioadmin}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-minioadmin}"
MINIO_REGION="${MINIO_REGION:-us-east-1}"
VLLM_SIM_MODEL="${VLLM_SIM_MODEL:-sim-model}"
VLLM_SIM_B_MODEL="${VLLM_SIM_B_MODEL:-sim-model-b}"
VLLM_SIM_IMAGE="${VLLM_SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:latest}"
JAEGER_IMAGE="${JAEGER_IMAGE:-${IMAGE_REGISTRY}/jaegertracing/all-in-one:latest}"
PROMETHEUS_IMAGE="${PROMETHEUS_IMAGE:-${IMAGE_REGISTRY}/prom/prometheus:latest}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-${IMAGE_REGISTRY}/grafana/grafana:latest}"
LOG_VERBOSITY="${LOG_VERBOSITY:-5}"
APISERVER_NODE_PORT="${APISERVER_NODE_PORT:-30080}"
APISERVER_OBS_NODE_PORT="${APISERVER_OBS_NODE_PORT:-30081}"
PROCESSOR_NODE_PORT="${PROCESSOR_NODE_PORT:-30090}"
JAEGER_NODE_PORT="${JAEGER_NODE_PORT:-30086}"
PROMETHEUS_NODE_PORT="${PROMETHEUS_NODE_PORT:-30091}"
GRAFANA_NODE_PORT="${GRAFANA_NODE_PORT:-30030}"
MINIO_NODE_PORT="${MINIO_NODE_PORT:-30009}"
APISERVER_IMG="${APISERVER_IMG:-ghcr.io/llm-d-incubation/batch-gateway-apiserver:${IMAGE_TAG}}"
PROCESSOR_IMG="${PROCESSOR_IMG:-ghcr.io/llm-d-incubation/batch-gateway-processor:${IMAGE_TAG}}"
GC_IMG="${GC_IMG:-ghcr.io/llm-d-incubation/batch-gateway-gc:${IMAGE_TAG}}"
# USE_KIND=true  → use kind; create cluster if it doesn't exist (default)
# USE_KIND=false → use existing kubeconfig context (OpenShift / Kubernetes)
USE_KIND="${USE_KIND:-true}"

OS="$(uname -s)"
ARCH="$(uname -m)"

CONTAINER_TOOL=""
KIND_CLUSTER=""

# ── Prerequisites ─────────────────────────────────────────────────────────────

detect_container_tool() {
    if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
        echo "docker"
    elif command -v podman &>/dev/null; then
        echo "podman"
    else
        die "Neither docker (running) nor podman found. Please install one."
    fi
}

check_prerequisites() {
    step "Checking prerequisites..."
    local missing=()
    for cmd in kubectl helm kind make; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        die "Missing required tools: ${missing[*]}. Please install them first."
    fi
    CONTAINER_TOOL="$(detect_container_tool)"
    log "Container tool : ${CONTAINER_TOOL}"
    log "OS / Arch      : ${OS} / ${ARCH}"
}

# ── Cluster ───────────────────────────────────────────────────────────────────

is_openshift() {
    kubectl api-resources 2>/dev/null | grep -q "route.openshift.io"
}

ensure_cluster() {
    if [ "${USE_KIND}" = "true" ]; then
        step "Ensuring kind cluster '${KIND_CLUSTER_NAME}'..."

        if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
            log "Kind cluster '${KIND_CLUSTER_NAME}' already exists. Switching context..."
            kubectl config use-context "kind-${KIND_CLUSTER_NAME}"
        else
            kind create cluster --name "${KIND_CLUSTER_NAME}" --config=- <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: ${APISERVER_NODE_PORT}
    hostPort: ${LOCAL_PORT}
    protocol: TCP
  - containerPort: ${APISERVER_OBS_NODE_PORT}
    hostPort: ${LOCAL_OBS_PORT}
    protocol: TCP
  - containerPort: ${PROCESSOR_NODE_PORT}
    hostPort: ${LOCAL_PROCESSOR_PORT}
    protocol: TCP
  - containerPort: ${JAEGER_NODE_PORT}
    hostPort: ${JAEGER_PORT}
    protocol: TCP
  - containerPort: ${PROMETHEUS_NODE_PORT}
    hostPort: ${PROMETHEUS_PORT}
    protocol: TCP
  - containerPort: ${GRAFANA_NODE_PORT}
    hostPort: ${GRAFANA_PORT}
    protocol: TCP
  - containerPort: ${MINIO_NODE_PORT}
    hostPort: ${MINIO_PORT}
    protocol: TCP
EOF
        fi

        KIND_CLUSTER="${KIND_CLUSTER_NAME}"
        log "Using kind cluster '${KIND_CLUSTER}'."
    else
        step "Checking for an existing Kubernetes / OpenShift cluster..."

        if ! kubectl cluster-info &>/dev/null 2>&1; then
            die "No cluster found. Please log in to a cluster first (or set USE_KIND=true to create a kind cluster)."
        fi

        local ctx
        ctx="$(kubectl config current-context 2>/dev/null || echo "<unknown>")"

        if is_openshift; then
            log "OpenShift cluster detected (context: ${ctx}). Using it."
        else
            log "Kubernetes cluster detected (context: ${ctx}). Using it."
        fi
    fi
}

# ── Exchange (Redis / Valkey) ──────────────────────────────────────────────────

install_exchange() {
    local chart="bitnami/${EXCHANGE_CLIENT_TYPE}"
    step "Installing exchange backend (${chart})..."

    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
    fi
    helm repo update || warn "Some Helm repo updates failed; continuing."

    if helm status "${REDIS_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        local installed_chart
        installed_chart=$(helm get metadata "${REDIS_RELEASE}" -n "${NAMESPACE}" -o json 2>/dev/null | jq -r '.chart')
        if [[ "${installed_chart}" == "${EXCHANGE_CLIENT_TYPE}-"* ]]; then
            log "Exchange backend (${chart}) release '${REDIS_RELEASE}' is already installed. Skipping."
            return
        fi
        warn "Installed exchange chart '${installed_chart}' does not match requested '${EXCHANGE_CLIENT_TYPE}'. Reinstalling..."
        helm uninstall "${REDIS_RELEASE}" -n "${NAMESPACE}" --wait
    fi

    local persistence_key="master.persistence.enabled"
    if [[ "${EXCHANGE_CLIENT_TYPE}" == "valkey" ]]; then
        persistence_key="primary.persistence.enabled"
    fi

    helm install "${REDIS_RELEASE}" "${chart}" \
        --namespace "${NAMESPACE}" \
        --set auth.enabled=false \
        --set replica.replicaCount=0 \
        --set "${persistence_key}"=false \
        --wait --timeout 120s

    log "Exchange backend (${chart}) installed successfully."
}

install_postgresql() {
    step "Installing PostgreSQL..."

    if ! helm repo list 2>/dev/null | grep -q bitnami; then
        helm repo add bitnami https://charts.bitnami.com/bitnami
    fi
    helm repo update || warn "Some Helm repo updates failed; continuing."

    if helm status "${POSTGRESQL_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "PostgreSQL release '${POSTGRESQL_RELEASE}' is already installed. Skipping."
        return
    fi

    helm install "${POSTGRESQL_RELEASE}" bitnami/postgresql \
        --namespace "${NAMESPACE}" \
        --set auth.postgresPassword="${POSTGRESQL_PASSWORD}" \
        --set primary.persistence.enabled=false \
        --wait --timeout 120s

    log "PostgreSQL installed successfully."
}

create_secret() {
    step "Creating secret '${APP_SECRET_NAME}'..."

    local exchange_svc="${REDIS_RELEASE}-master"
    if [[ "${EXCHANGE_CLIENT_TYPE}" == "valkey" ]]; then
        exchange_svc="${REDIS_RELEASE}-valkey-primary"
    fi
    local redis_url="redis://${exchange_svc}.${NAMESPACE}.svc.cluster.local:6379/0"
    local postgresql_url="postgresql://postgres:${POSTGRESQL_PASSWORD}@${POSTGRESQL_RELEASE}.${NAMESPACE}.svc.cluster.local:5432/postgres"

    kubectl create secret generic "${APP_SECRET_NAME}" \
        --namespace "${NAMESPACE}" \
        --from-literal=redis-url="${redis_url}" \
        --from-literal=postgresql-url="${postgresql_url}" \
        --from-literal=inference-api-key="${INFERENCE_API_KEY}" \
        --from-literal=s3-secret-access-key="${S3_SECRET_ACCESS_KEY}" \
        --dry-run=client -o yaml | kubectl apply -f -

    log "Secret '${APP_SECRET_NAME}' applied."
}

create_tls_secret() {
    step "Creating self-signed TLS certificate for apiserver..."

    local tmp_dir
    tmp_dir="$(mktemp -d)"
    trap "rm -rf ${tmp_dir}" RETURN

    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "${tmp_dir}/tls.key" \
        -out "${tmp_dir}/tls.crt" \
        -days 365 \
        -subj "/CN=batch-gateway-apiserver" \
        -addext "subjectAltName=DNS:${HELM_RELEASE}-apiserver,DNS:${HELM_RELEASE}-apiserver.${NAMESPACE}.svc.cluster.local,DNS:localhost,IP:127.0.0.1" \
        2>/dev/null

    kubectl create secret tls "${TLS_SECRET_NAME}" \
        --namespace "${NAMESPACE}" \
        --cert="${tmp_dir}/tls.crt" \
        --key="${tmp_dir}/tls.key" \
        --dry-run=client -o yaml | kubectl apply -f -

    log "TLS secret '${TLS_SECRET_NAME}' applied."
}

# ── Images ────────────────────────────────────────────────────────────────────

get_target_arch() {
    case "${ARCH}" in
        arm64|aarch64) echo "arm64" ;;
        x86_64|amd64)  echo "amd64" ;;
        *)
            warn "Unknown arch '${ARCH}'; defaulting to amd64."
            echo "amd64"
            ;;
    esac
}

build_images() {
    local target_arch
    target_arch="$(get_target_arch)"

    step "Building container images (TARGETARCH=${target_arch}, version=${IMAGE_TAG})..."
    cd "${REPO_ROOT}"
    CONTAINER_TOOL="${CONTAINER_TOOL}" TARGETARCH="${target_arch}" IMAGE_TAG="${IMAGE_TAG}" make image-build
}

pull_images() {
    step "Pulling images from GHCR..."
    "${CONTAINER_TOOL}" pull "${APISERVER_IMG}"
    "${CONTAINER_TOOL}" pull "${PROCESSOR_IMG}"
    "${CONTAINER_TOOL}" pull "${GC_IMG}"
    log "Images pulled successfully."
}

load_images() {
    local target_arch
    target_arch="$(get_target_arch)"

    if [ "${USE_KIND}" = true ]; then
        step "Loading images into kind cluster '${KIND_CLUSTER}'..."

        if [ "${CONTAINER_TOOL}" = "docker" ]; then
            kind load docker-image "${APISERVER_IMG}" --name "${KIND_CLUSTER}"
            kind load docker-image "${PROCESSOR_IMG}" --name "${KIND_CLUSTER}"
            kind load docker-image "${GC_IMG}" --name "${KIND_CLUSTER}"
        else
            podman save "${APISERVER_IMG}" | kind load image-archive /dev/stdin --name "${KIND_CLUSTER}"
            podman save "${PROCESSOR_IMG}" | kind load image-archive /dev/stdin --name "${KIND_CLUSTER}"
            podman save "${GC_IMG}" | kind load image-archive /dev/stdin --name "${KIND_CLUSTER}"
        fi
        log "Images loaded into kind."
    else
        warn "Not a kind cluster — skipping image load."
        warn "Ensure '${APISERVER_IMG}', '${PROCESSOR_IMG}', and '${GC_IMG}' are accessible from the cluster."
    fi
}

# ── File Storage PVC ──────────────────────────────────────────────────────────

create_pvc() {
    step "Ensuring PVC '${FILES_PVC_NAME}' for file storage..."

    if kubectl get pvc "${FILES_PVC_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "PVC '${FILES_PVC_NAME}' already exists. Skipping."
        return
    fi

    # kind's local-path-provisioner only supports ReadWriteOnce.
    # Real clusters with shared storage (NFS, EFS, CephFS, etc.) use ReadWriteMany
    # so that apiserver and processor pods can be scheduled on different nodes.
    local access_mode
    if [ "${USE_KIND}" = true ]; then
        access_mode="ReadWriteOnce"
    else
        access_mode="ReadWriteMany"
    fi
    log "PVC access mode: ${access_mode}"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${FILES_PVC_NAME}
  namespace: ${NAMESPACE}
spec:
  accessModes:
    - ${access_mode}
  resources:
    requests:
      storage: 1Gi
EOF
    log "PVC '${FILES_PVC_NAME}' created."
}

# ── MinIO (S3-compatible object storage) ─────────────────────────────────────

install_minio() {
    step "Installing MinIO '${MINIO_NAME}'..."

    if kubectl get deployment "${MINIO_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "MinIO '${MINIO_NAME}' already exists. Skipping."
        return
    fi

    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${MINIO_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${MINIO_NAME}
  template:
    metadata:
      labels:
        app: ${MINIO_NAME}
    spec:
      containers:
      - name: minio
        image: ${MINIO_IMAGE}
        args: ["server", "/data", "--console-address", ":9001"]
        env:
        - name: MINIO_ROOT_USER
          value: "${MINIO_ACCESS_KEY}"
        - name: MINIO_ROOT_PASSWORD
          value: "${MINIO_SECRET_KEY}"
        ports:
        - containerPort: 9000
          name: api
        - containerPort: 9001
          name: console
        readinessProbe:
          httpGet:
            path: /minio/health/ready
            port: 9000
          initialDelaySeconds: 5
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: ${MINIO_NAME}
  namespace: ${NAMESPACE}
spec:
  selector:
    app: ${MINIO_NAME}
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
EOF

    log "Waiting for MinIO to be ready..."
    kubectl rollout status deployment "${MINIO_NAME}" -n "${NAMESPACE}" --timeout=120s

    log "MinIO installed."
}

# ── Jaeger (OpenTelemetry collector & trace UI) ──────────────────────────────

install_jaeger() {
    step "Installing Jaeger all-in-one '${JAEGER_NAME}'..."

    if kubectl get deployment "${JAEGER_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Jaeger '${JAEGER_NAME}' already exists. Skipping."
        return
    fi

    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${JAEGER_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${JAEGER_NAME}
  template:
    metadata:
      labels:
        app: ${JAEGER_NAME}
    spec:
      containers:
      - name: jaeger
        image: ${JAEGER_IMAGE}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 4317
          name: otlp-grpc
          protocol: TCP
        - containerPort: 16686
          name: query-http
          protocol: TCP
        - containerPort: 16685
          name: query-grpc
          protocol: TCP
        resources:
          requests:
            cpu: 10m
            memory: 64Mi
---
apiVersion: v1
kind: Service
metadata:
  name: ${JAEGER_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${JAEGER_NAME}
spec:
  selector:
    app: ${JAEGER_NAME}
  ports:
  - name: otlp-grpc
    protocol: TCP
    port: 4317
    targetPort: 4317
  - name: query-http
    protocol: TCP
    port: 16686
    targetPort: 16686
    nodePort: ${JAEGER_NODE_PORT}
  - name: query-grpc
    protocol: TCP
    port: 16685
    targetPort: 16685
  type: NodePort
EOF

    wait_for_deployment "${JAEGER_NAME}" "${NAMESPACE}" 120s
    log "Jaeger installed. OTLP gRPC: ${JAEGER_NAME}:4317, UI: ${JAEGER_NAME}:16686"
}

# ── Prometheus ────────────────────────────────────────────────────────────────

install_prometheus() {
    step "Installing Prometheus '${PROMETHEUS_NAME}'..."

    if kubectl get deployment "${PROMETHEUS_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        log "Prometheus '${PROMETHEUS_NAME}' already exists. Skipping."
        return
    fi

    local apiserver_svc="${HELM_RELEASE}-apiserver.${NAMESPACE}.svc.cluster.local"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${PROMETHEUS_NAME}
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${PROMETHEUS_NAME}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${PROMETHEUS_NAME}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${PROMETHEUS_NAME}
subjects:
- kind: ServiceAccount
  name: ${PROMETHEUS_NAME}
  namespace: ${NAMESPACE}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${PROMETHEUS_NAME}-config
  namespace: ${NAMESPACE}
data:
  prometheus.yml: |
    global:
      scrape_interval: 15s
      evaluation_interval: 15s
    scrape_configs:
    - job_name: 'batch-gateway-apiserver'
      metrics_path: /metrics
      static_configs:
      - targets: ['${apiserver_svc}:8081']
        labels:
          component: apiserver
    - job_name: 'batch-gateway-processor'
      metrics_path: /metrics
      kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: ['${NAMESPACE}']
      relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        regex: processor
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_instance]
        regex: ${HELM_RELEASE}
        action: keep
      - source_labels: [__meta_kubernetes_pod_ip]
        target_label: __address__
        replacement: \$1:9090
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${PROMETHEUS_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${PROMETHEUS_NAME}
  template:
    metadata:
      labels:
        app: ${PROMETHEUS_NAME}
    spec:
      serviceAccountName: ${PROMETHEUS_NAME}
      containers:
      - name: prometheus
        image: ${PROMETHEUS_IMAGE}
        imagePullPolicy: IfNotPresent
        args:
        - --config.file=/etc/prometheus/prometheus.yml
        - --storage.tsdb.retention.time=1d
        - --web.enable-lifecycle
        ports:
        - containerPort: 9090
          name: http
          protocol: TCP
        volumeMounts:
        - name: config
          mountPath: /etc/prometheus
        resources:
          requests:
            cpu: 10m
            memory: 128Mi
      volumes:
      - name: config
        configMap:
          name: ${PROMETHEUS_NAME}-config
---
apiVersion: v1
kind: Service
metadata:
  name: ${PROMETHEUS_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${PROMETHEUS_NAME}
spec:
  selector:
    app: ${PROMETHEUS_NAME}
  ports:
  - name: http
    protocol: TCP
    port: 9090
    targetPort: 9090
  type: ClusterIP
EOF

    wait_for_deployment "${PROMETHEUS_NAME}" "${NAMESPACE}" 120s
    log "Prometheus installed. UI: ${PROMETHEUS_NAME}:9090"
}

# ── Grafana ───────────────────────────────────────────────────────────────────

install_grafana() {
    step "Installing Grafana '${GRAFANA_NAME}'..."

    local grafana_exists=false
    if kubectl get deployment "${GRAFANA_NAME}" -n "${NAMESPACE}" &>/dev/null; then
        grafana_exists=true
    fi

    # Always apply ConfigMaps so dashboard/datasource changes are picked up on re-deploy.
    kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${GRAFANA_NAME}-provisioning-datasources
  namespace: ${NAMESPACE}
data:
  datasources.yaml: |
    apiVersion: 1
    datasources:
    - name: Prometheus
      type: prometheus
      access: proxy
      url: http://${PROMETHEUS_NAME}.${NAMESPACE}.svc.cluster.local:9090
      isDefault: true
      editable: false
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${GRAFANA_NAME}-provisioning-dashboards
  namespace: ${NAMESPACE}
data:
  dashboards.yaml: |
    apiVersion: 1
    providers:
    - name: batch-gateway
      type: file
      options:
        path: /var/lib/grafana/dashboards
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${GRAFANA_NAME}-dashboards
  namespace: ${NAMESPACE}
data:
$(cd "${REPO_ROOT}" && for f in charts/batch-gateway/dashboards/*.json; do
  name="$(basename "$f")"
  echo "  ${name}: |"
  sed 's/^/    /' "$f"
done)
EOF

    if [ "${grafana_exists}" = true ]; then
        # Restart Grafana to pick up updated ConfigMaps
        kubectl rollout restart deployment "${GRAFANA_NAME}" -n "${NAMESPACE}"
        log "Grafana ConfigMaps updated and pod restarted."
    else
        kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${GRAFANA_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${GRAFANA_NAME}
  template:
    metadata:
      labels:
        app: ${GRAFANA_NAME}
    spec:
      containers:
      - name: grafana
        image: ${GRAFANA_IMAGE}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 3000
          name: http
          protocol: TCP
        env:
        - name: GF_AUTH_ANONYMOUS_ENABLED
          value: "true"
        - name: GF_AUTH_ANONYMOUS_ORG_ROLE
          value: "Admin"
        volumeMounts:
        - name: datasources
          mountPath: /etc/grafana/provisioning/datasources
        - name: dashboard-providers
          mountPath: /etc/grafana/provisioning/dashboards
        - name: dashboards
          mountPath: /var/lib/grafana/dashboards
        resources:
          requests:
            cpu: 10m
            memory: 128Mi
      volumes:
      - name: datasources
        configMap:
          name: ${GRAFANA_NAME}-provisioning-datasources
      - name: dashboard-providers
        configMap:
          name: ${GRAFANA_NAME}-provisioning-dashboards
      - name: dashboards
        configMap:
          name: ${GRAFANA_NAME}-dashboards
---
apiVersion: v1
kind: Service
metadata:
  name: ${GRAFANA_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${GRAFANA_NAME}
spec:
  selector:
    app: ${GRAFANA_NAME}
  ports:
  - name: http
    protocol: TCP
    port: 3000
    targetPort: 3000
  type: ClusterIP
EOF

        wait_for_deployment "${GRAFANA_NAME}" "${NAMESPACE}" 120s
        log "Grafana installed. UI: ${GRAFANA_NAME}:3000 (anonymous admin access enabled)"
    fi
}

# ── vLLM Simulator ────────────────────────────────────────────────────────────

install_vllm_sim() {
    local sim_name="$1"
    local sim_model="$2"
    local time_to_first_token="$3"
    local inter_token_latency="$4"

    step "Installing vLLM simulator '${sim_name}' (model: ${sim_model})..."

    if kubectl get deployment "${sim_name}" -n "${NAMESPACE}" &>/dev/null; then
        log "vLLM simulator '${sim_name}' already exists. Skipping."
        return
    fi

    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${sim_name}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${sim_name}
  template:
    metadata:
      labels:
        app: ${sim_name}
    spec:
      containers:
      - name: vllm-sim
        image: ${VLLM_SIM_IMAGE}
        imagePullPolicy: IfNotPresent
        args:
        - --model
        - ${sim_model}
        - --port
        - "8000"
        - --time-to-first-token=${time_to_first_token}
        - --inter-token-latency=${inter_token_latency}
        - --v=5
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
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
  name: ${sim_name}
  namespace: ${NAMESPACE}
  labels:
    app: ${sim_name}
spec:
  selector:
    app: ${sim_name}
  ports:
  - name: http
    protocol: TCP
    port: 8000
    targetPort: 8000
  type: ClusterIP
EOF

    wait_for_deployment "${sim_name}" "${NAMESPACE}" 120s
    log "vLLM simulator installed. Service: ${sim_name}:8000"
}

# ── Batch Gateway ─────────────────────────────────────────────────────────────

install_batch_gateway() {
    step "Installing batch-gateway via Helm (version=${IMAGE_TAG})..."
    cd "${REPO_ROOT}"

    local vllm_sim_url="http://${VLLM_SIM_NAME}.${NAMESPACE}.svc.cluster.local:8000"
    local vllm_sim_b_url="http://${VLLM_SIM_B_NAME}.${NAMESPACE}.svc.cluster.local:8000"

    local helm_args=(
        --set apiserver.image.pullPolicy=IfNotPresent
        --set "apiserver.image.tag=${IMAGE_TAG}"
        --set processor.image.pullPolicy=IfNotPresent
        --set "processor.image.tag=${IMAGE_TAG}"
        --set "global.fileClient.type=${FILE_CLIENT_TYPE}"
        --set "global.secretName=${APP_SECRET_NAME}"
        --set "processor.config.modelGateways.${VLLM_SIM_MODEL}.url=${vllm_sim_url}"
        --set "processor.config.modelGateways.${VLLM_SIM_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${VLLM_SIM_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${VLLM_SIM_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${VLLM_SIM_MODEL}.maxBackoff=60s"
        --set "processor.config.modelGateways.${VLLM_SIM_B_MODEL}.url=${vllm_sim_b_url}"
        --set "processor.config.modelGateways.${VLLM_SIM_B_MODEL}.requestTimeout=5m"
        --set "processor.config.modelGateways.${VLLM_SIM_B_MODEL}.maxRetries=3"
        --set "processor.config.modelGateways.${VLLM_SIM_B_MODEL}.initialBackoff=1s"
        --set "processor.config.modelGateways.${VLLM_SIM_B_MODEL}.maxBackoff=60s"
        --set "processor.logging.verbosity=${LOG_VERBOSITY}"
        --set "apiserver.logging.verbosity=${LOG_VERBOSITY}"
        --set "apiserver.config.batchAPI.passThroughHeaders={X-E2E-Pass-Through-1,X-E2E-Pass-Through-2}"
        --set "apiserver.tls.enabled=true"
        --set "apiserver.tls.secretName=${TLS_SECRET_NAME}"
        --set "global.otel.endpoint=http://${JAEGER_NAME}.${NAMESPACE}.svc.cluster.local:4317"
        --set "global.otel.insecure=true"
        --set "global.otel.redisTracing=true"
        --set "global.otel.postgresqlTracing=true"
        --set "global.dbClient.type=${DB_CLIENT_TYPE}"
        --set "apiserver.config.enablePprof=true"
        --set "processor.config.enablePprof=true"
        --set "processor.resources.requests.memory=256Mi"
        --set "gc.enabled=true"
        --set "gc.image.pullPolicy=IfNotPresent"
        --set "gc.image.tag=${IMAGE_TAG}"
        --set "gc.config.interval=5s"
        --namespace "${NAMESPACE}"
    )

    # Add file client specific helm args
    if [ "${FILE_CLIENT_TYPE}" = "s3" ]; then
        local minio_endpoint="http://${MINIO_NAME}.${NAMESPACE}.svc.cluster.local:9000"
        helm_args+=(
            --set "global.fileClient.s3.region=${MINIO_REGION}"
            --set "global.fileClient.s3.endpoint=${minio_endpoint}"
            --set "global.fileClient.s3.accessKeyId=${MINIO_ACCESS_KEY}"
            --set "global.fileClient.s3.usePathStyle=true"
            --set "global.fileClient.s3.autoCreateBucket=true"
        )
    else
        helm_args+=(
            --set "global.fileClient.fs.pvcName=${FILES_PVC_NAME}"
        )
    fi

    if helm status "${HELM_RELEASE}" -n "${NAMESPACE}" &>/dev/null; then
        log "Release '${HELM_RELEASE}' already exists. Upgrading..."
        helm upgrade "${HELM_RELEASE}" ./charts/batch-gateway "${helm_args[@]}"
        # Force pod restart so the newly-loaded container images are picked up.
        # helm upgrade alone won't recreate pods when only the image contents
        # changed but the tag (e.g. 0.0.1) stayed the same.
        kubectl rollout restart deployment \
            -l "app.kubernetes.io/instance=${HELM_RELEASE}" \
            -n "${NAMESPACE}"
        # rollout status blocks until new ReplicaSet pods are Ready.
        # wait_for_deployment (condition=Available) is insufficient here because
        # the old ReplicaSet satisfies Available immediately after restart.
        wait_for_rollout "${HELM_RELEASE}-apiserver" "${NAMESPACE}" 120s
        wait_for_rollout "${HELM_RELEASE}-processor" "${NAMESPACE}" 120s
        wait_for_rollout "${HELM_RELEASE}-gc" "${NAMESPACE}" 120s
    else
        helm install "${HELM_RELEASE}" ./charts/batch-gateway "${helm_args[@]}"
        wait_for_deployment "${HELM_RELEASE}-apiserver" "${NAMESPACE}" 120s
        wait_for_deployment "${HELM_RELEASE}-processor" "${NAMESPACE}" 120s
        wait_for_deployment "${HELM_RELEASE}-gc" "${NAMESPACE}" 120s
    fi

    log "batch-gateway installed."
}

# ── Verify ────────────────────────────────────────────────────────────────────

verify_deployment() {
    step "Verifying deployment..."
    kubectl get pods -l "app.kubernetes.io/instance=${HELM_RELEASE}" -n "${NAMESPACE}"
    kubectl get svc  -l "app.kubernetes.io/instance=${HELM_RELEASE}" -n "${NAMESPACE}"
}

# wait_for_deployment <name> <namespace> <timeout>
# Suitable for initial install where no old ReplicaSet exists.
wait_for_deployment() {
    local name="$1"
    local ns="$2"
    local timeout="${3:-120s}"

    step "Waiting for deployment '${name}' to be ready..."
    if ! kubectl wait deployment/"${name}" \
        -n "${ns}" --for=condition=Available --timeout="${timeout}"; then
        die "Deployment '${name}' did not become ready within ${timeout}"
    fi
    log "Deployment '${name}' is ready."
}

# wait_for_rollout <name> <namespace> <timeout>
# Blocks until the latest rollout (new ReplicaSet) is fully complete.
# Use after rollout restart; condition=Available can pass prematurely
# when the old ReplicaSet still satisfies the Available condition.
wait_for_rollout() {
    local name="$1"
    local ns="$2"
    local timeout="${3:-120s}"

    step "Waiting for rollout of '${name}' to complete..."
    if ! kubectl rollout status deployment/"${name}" \
        -n "${ns}" --timeout="${timeout}"; then
        die "Rollout of '${name}' did not complete within ${timeout}"
    fi
    log "Rollout of '${name}' complete."
}

# wait_for_http_ready polls the apiserver health endpoint via localhost
# to confirm end-to-end connectivity (NodePort -> pod) is working.
wait_for_http_ready() {
    log "Waiting for http://localhost:${LOCAL_OBS_PORT}/health ..."

    for i in $(seq 1 30); do
        if curl -sf "http://localhost:${LOCAL_OBS_PORT}/health" >/dev/null 2>&1; then
            log "API server is ready at https://localhost:${LOCAL_PORT}"
            return 0
        fi
        sleep 1
    done

    die "Timed out waiting for API server to become ready"
}

create_nodeport_services() {
    step "Creating NodePort services for local access..."

    kubectl apply -n "${NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${HELM_RELEASE}-apiserver-nodeport
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: batch-gateway-apiserver
    app.kubernetes.io/instance: ${HELM_RELEASE}
    app.kubernetes.io/component: apiserver
  ports:
  - name: https
    protocol: TCP
    port: 8000
    targetPort: http
    nodePort: ${APISERVER_NODE_PORT}
  - name: observability
    protocol: TCP
    port: 8081
    targetPort: observability
    nodePort: ${APISERVER_OBS_NODE_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: ${HELM_RELEASE}-processor-nodeport
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: batch-gateway-processor
    app.kubernetes.io/instance: ${HELM_RELEASE}
    app.kubernetes.io/component: processor
  ports:
  - name: metrics
    protocol: TCP
    port: 9090
    targetPort: metrics
    nodePort: ${PROCESSOR_NODE_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: ${PROMETHEUS_NAME}-nodeport
spec:
  type: NodePort
  selector:
    app: ${PROMETHEUS_NAME}
  ports:
  - name: http
    protocol: TCP
    port: 9090
    targetPort: 9090
    nodePort: ${PROMETHEUS_NODE_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: ${GRAFANA_NAME}-nodeport
spec:
  type: NodePort
  selector:
    app: ${GRAFANA_NAME}
  ports:
  - name: http
    protocol: TCP
    port: 3000
    targetPort: 3000
    nodePort: ${GRAFANA_NODE_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: ${MINIO_NAME}-nodeport
spec:
  type: NodePort
  selector:
    app: ${MINIO_NAME}
  ports:
  - name: api
    protocol: TCP
    port: 9000
    targetPort: 9000
    nodePort: ${MINIO_NODE_PORT}
EOF

    log "NodePort services created."
    wait_for_http_ready
}

print_usage() {
    local base="https://localhost:${LOCAL_PORT}"
    local curl_flags="-k"  # skip TLS verification for self-signed cert

    echo ""
    echo "  ╔══════════════════════════════════════════════════════════════╗"
    echo "  ║                        Next Steps                            ║"
    echo "  ╚══════════════════════════════════════════════════════════════╝"
    echo ""
    echo "  1. Run E2E tests:"
    echo ""
    echo "       make test-e2e"
    echo ""
    echo "  2. Upload a batch input file (JSONL):"
    echo ""
    echo "       curl ${curl_flags} -s -X POST ${base}/v1/files \\"
    echo "         -F 'file=@/path/to/requests.jsonl' \\"
    echo "         -F 'purpose=batch'"
    echo ""
    echo "     Each line in the JSONL file should follow this format:"
    echo '       {"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","messages":[{"role":"user","content":"Hello"}]}}'
    echo ""
    echo "     Available models in dev environment:"
    echo "       - sim-model   (vLLM simulator at ${VLLM_SIM_NAME})"
    echo "       - sim-model-b (vLLM simulator at ${VLLM_SIM_B_NAME})"
    echo ""
    echo "  3. Create a batch (replace FILE_ID with the id from step 2):"
    echo ""
    echo "       curl ${curl_flags} -s -X POST ${base}/v1/batches \\"
    echo "         -H 'Content-Type: application/json' \\"
    echo "         -d '{\"input_file_id\":\"FILE_ID\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}'"
    echo ""
    echo "  4. Profiling (pprof):"
    echo ""
    echo "     API Server (port ${LOCAL_OBS_PORT}):"
    echo "       go tool pprof http://localhost:${LOCAL_OBS_PORT}/debug/pprof/profile?seconds=30  # CPU"
    echo "       go tool pprof http://localhost:${LOCAL_OBS_PORT}/debug/pprof/heap               # Heap"
    echo "       go tool pprof http://localhost:${LOCAL_OBS_PORT}/debug/pprof/allocs             # Allocs"
    echo "       go tool pprof http://localhost:${LOCAL_OBS_PORT}/debug/pprof/goroutine          # Goroutine"
    echo ""
    echo "     Processor (port ${LOCAL_PROCESSOR_PORT}):"
    echo "       go tool pprof http://localhost:${LOCAL_PROCESSOR_PORT}/debug/pprof/profile?seconds=30  # CPU"
    echo "       go tool pprof http://localhost:${LOCAL_PROCESSOR_PORT}/debug/pprof/heap               # Heap"
    echo "       go tool pprof http://localhost:${LOCAL_PROCESSOR_PORT}/debug/pprof/allocs             # Allocs"
    echo "       go tool pprof http://localhost:${LOCAL_PROCESSOR_PORT}/debug/pprof/goroutine          # Goroutine"
    echo ""
    echo "  5. Prometheus (metrics):"
    echo ""
    echo "       http://localhost:${PROMETHEUS_PORT}"
    echo ""
    echo "  6. Grafana (dashboards):"
    echo ""
    echo "       http://localhost:${GRAFANA_PORT}"
    echo "       Anonymous admin access enabled — no login required."
    echo ""
    echo "  7. Jaeger UI (trace visualization):"
    echo ""
    echo "       http://localhost:${JAEGER_PORT}"
    echo ""
    echo "     Select service 'batch-gateway' to view traces."
    echo ""
    echo "  8. Cleanup:"
    echo ""
    if [ "${USE_KIND}" = true ]; then
    echo "       make dev-rm-cluster"
    else
    echo "       helm uninstall ${HELM_RELEASE} -n ${NAMESPACE}"
    echo "       helm uninstall ${REDIS_RELEASE} -n ${NAMESPACE}"
    echo "       helm uninstall ${POSTGRESQL_RELEASE} -n ${NAMESPACE}"
    echo "       kubectl delete deployment,svc ${JAEGER_NAME} -n ${NAMESPACE}"
    echo "       kubectl delete deployment,svc,configmap,sa ${PROMETHEUS_NAME} ${PROMETHEUS_NAME}-config -n ${NAMESPACE}"
    echo "       kubectl delete clusterrole,clusterrolebinding ${PROMETHEUS_NAME}"
    echo "       kubectl delete deployment,svc ${VLLM_SIM_NAME} -n ${NAMESPACE}"
    echo "       kubectl delete deployment,svc ${VLLM_SIM_B_NAME} -n ${NAMESPACE}"
    echo "       kubectl delete secret ${APP_SECRET_NAME} ${TLS_SECRET_NAME} -n ${NAMESPACE}"
    echo "       kubectl delete pvc ${FILES_PVC_NAME} -n ${NAMESPACE}"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║   Batch Gateway Deployment Script    ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""

    check_prerequisites
    if [ "${SKIP_BUILD}" = "true" ]; then
        pull_images
    else
        build_images
    fi
    ensure_cluster
    install_exchange
    install_postgresql
    create_secret
    create_tls_secret
    # MinIO is always installed so that S3 integration tests can run
    # against the dev cluster regardless of the batch file client type.
    install_minio
    if [ "${FILE_CLIENT_TYPE}" != "s3" ]; then
        create_pvc
    fi
    load_images
    install_jaeger
    install_prometheus
    install_grafana
    install_vllm_sim "${VLLM_SIM_NAME}" "${VLLM_SIM_MODEL}" "50ms" "100ms"
    install_vllm_sim "${VLLM_SIM_B_NAME}" "${VLLM_SIM_B_MODEL}" "200ms" "500ms"
    install_batch_gateway
    verify_deployment
    if [ "${USE_KIND}" = true ]; then
        create_nodeport_services
    fi
    print_usage

    log "Deployment complete!"
}

main "$@"
