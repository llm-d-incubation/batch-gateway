# Batch Gateway Deployment

This doc demonstrates how to deploy batch-gateway with Istio Gateway API, GAIE (Gateway API Inference Extension) on Kubernetes or OpenShift.

For Kuadrant-based authentication and rate limiting, see [kuadrant-integration.md](kuadrant-integration.md).

## 1. Architecture

![Architecture](diagrams/deployment-arch.png)

### 1.1 Verified Component Versions

| Component | Version | Description |
|-----------|---------|-------------|
| Istio | v1.28.0 | Service mesh with Gateway API and GAIE support |
| Gateway API | v1.1.0 | Kubernetes Gateway API CRDs |
| GAIE | v1.3.1 | Gateway API Inference Extension (InferencePool + EPP) |
| cert-manager | v1.15.3 | TLS certificate management and auto-renewal |
| PostgreSQL | 18.3.0 | Primary database for batch jobs and files |
| Redis | 8.6.1 | Priority queue, events, and status tracking |

### 1.2 Namespace Layout

| Namespace | Purpose |
|-----------|---------|
| `istio-ingress` | Gateway data plane (Envoy proxy), Gateway TLS certificate |
| `istio-system` | Istio control plane (istiod) |
| `cert-manager` | cert-manager operator |
| `batch-api` | batch-gateway (apiserver + processor), Redis, PostgreSQL, batch-route |
| `llm` | Model servers (vLLM), InferencePools, EPP, llm-route |

### 1.3 Data Flow

1. Client sends HTTPS request to Envoy Proxy (e.g. `POST ${GATEWAY_URL}/v1/batches`)
2. Envoy Proxy matches `batch-route`, forwards to batch-gateway apiserver
3. Apiserver stores the batch job in PostgreSQL, enqueues to Redis priority queue
4. Processor dequeues job from Redis, sends inference request to Envoy Proxy (e.g. `${GATEWAY_URL}/llm/free-model/v1/chat/completions`)
5. Envoy Proxy matches `llm-route`, calls EPP via gRPC ext-proc filter → EPP picks the best vLLM pod → vLLM processes inference
6. Processor uploads results to file storage, updates job status in PostgreSQL

### 1.4 TLS Architecture

```
Client    --HTTPS--> Envoy Proxy (TLS terminate) --TLS re-encrypt--> apiserver (HTTPS :8000)
Processor --HTTPS--> Envoy Proxy (TLS terminate) --HTTP--> InferencePool/EPP --HTTP--> vLLM
```

- **Client → Envoy Proxy**: TLS terminated at Envoy Proxy (cert-manager self-signed certificate)
- **Envoy Proxy → apiserver**: TLS re-encrypted via Istio DestinationRule (`tls.mode: SIMPLE`)
- **Processor → Envoy Proxy**: HTTPS with `tlsInsecureSkipVerify: true` (self-signed cert)
- **Envoy Proxy → vLLM**: HTTP via InferencePool/EPP (model servers don't require TLS)
- **cert-manager** automatically renews certificates before expiry (default: 30 days before 90-day expiry)

## 2. Install

### 2.1 Install CRDs

Install the Gateway API and GAIE (Inference Extension) Custom Resource Definitions. These define the `Gateway`, `HTTPRoute`, and `InferencePool` resources used throughout this guide.

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.1.0/standard-install.yaml
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.3.1/manifests.yaml
```

### 2.2 Install cert-manager

Install cert-manager for automatic TLS certificate provisioning and renewal. Used by both the Gateway and the batch-gateway apiserver.

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm install cert-manager jetstack/cert-manager \
    --namespace cert-manager \
    --create-namespace \
    --version v1.15.3 \
    --set crds.enabled=true
```

Create Self-Signed ClusterIssuer for demo/development environments.

For production, replace with a real CA issuer.

```bash
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
EOF
```

### 2.3 Install Istio

Install Istio as the Gateway API implementation. Istio provisions Envoy Proxy pods that handle all inbound traffic, TLS termination, and routing to backend services. The `ENABLE_GATEWAY_API_INFERENCE_EXTENSION` flag enables GAIE InferencePool support.

```bash
# For Kubernetes
istioctl install -y \
    --set components.ingressGateways[0].enabled=false \
    --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true \
    --set values.pilot.autoscaleEnabled=false

# For OpenShift
istioctl install -y \
    --set components.ingressGateways[0].enabled=false \
    --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true \
    --set values.pilot.autoscaleEnabled=false \
    --set values.global.platform=openshift
```

Verify Istio is running:

```bash
# Check istiod deployment
kubectl get deployment istiod -n istio-system

# Check istiod pod is ready
kubectl get pods -n istio-system
```

### 2.4 Create Gateway Instance
Create Gateway TLS Certificate
```bash
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: istio-gateway-tls
  namespace: istio-ingress
spec:
  secretName: istio-gateway-tls
  issuerRef:
    name: selfsigned-issuer
    kind: ClusterIssuer
  dnsNames:
  - "*.istio-ingress.svc.cluster.local"
  - localhost
EOF
```

Create Gateway CR

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: istio-gateway
  namespace: istio-ingress
spec:
  gatewayClassName: istio
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
    tls:
      mode: Terminate
      certificateRefs:
      - name: istio-gateway-tls
    allowedRoutes:
      namespaces:
        from: All
EOF
```

After applying, Istio provisions an Envoy Proxy deployment (`istio-gateway-istio`). Wait for the Gateway to become `Programmed`:

```bash
# Wait for Gateway to be programmed
kubectl wait --for=condition=Programmed --timeout=300s -n istio-ingress gateway/istio-gateway

# Verify Gateway status
kubectl get gateway istio-gateway -n istio-ingress

# Verify Envoy Proxy deployment and pod are running
kubectl get deployment istio-gateway-istio -n istio-ingress
kubectl get pods -n istio-ingress
```

### 2.5 Install Model Servers (vLLM)

Deploy one vLLM simulator instance per model:

```bash
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-free-model
  namespace: llm
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vllm-free-model
  template:
    metadata:
      labels:
        app: vllm-free-model
    spec:
      containers:
      - name: vllm
        image: ghcr.io/llm-d/llm-d-inference-sim:latest
        args: ["--model", "free-model", "--port", "8000"]
        ports:
        - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-free-model
  namespace: llm
spec:
  selector:
    app: vllm-free-model
  ports:
  - name: http
    port: 8000
    targetPort: 8000
EOF
```

Verify model servers are running:

```bash
kubectl get deployment -n llm
kubectl get pods -n llm
```

### 2.6 Install InferencePools and EPP

Deploy one InferencePool per model via the GAIE Helm chart. Each InferencePool includes an EPP (Endpoint Picker) that intelligently routes inference requests to the best vLLM pod based on load balancing.

```bash
GAIE_VERSION=v1.3.1
curl -sL "https://github.com/kubernetes-sigs/gateway-api-inference-extension/archive/refs/tags/${GAIE_VERSION}.tar.gz" \
    | tar -xz -C /tmp
GAIE_CHART="/tmp/gateway-api-inference-extension-${GAIE_VERSION#v}/config/charts/inferencepool"

helm install free-model \
    --namespace llm \
    --dependency-update \
    --set inferencePool.modelServers.matchLabels.app=vllm-free-model \
    --set inferencePool.modelServerType=vllm \
    --set provider.name=istio \
    --set experimentalHttpRoute.enabled=false \
    "${GAIE_CHART}"
```

Verify InferencePools and EPP pods are running:

```bash
# Check InferencePool resources
kubectl get inferencepool -n llm

# Check EPP deployments and pods
kubectl get deployment -n llm
kubectl get pods -n llm
```

### 2.7 Create LLM HTTPRoute

Create an HTTPRoute to route LLM inference requests from the Envoy Proxy to InferencePools. URL rewrite strips the `/{ns}/{model}` prefix before forwarding to vLLM.

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: llm
spec:
  parentRefs:
  - name: istio-gateway
    namespace: istio-ingress
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /llm/free-model/v1/chat/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/chat/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: free-model
EOF
```

### 2.8 Install Redis and PostgreSQL

Install the database backends for batch-gateway. PostgreSQL is the primary database for batch jobs and files. Redis is used for the priority queue, events, and status tracking.

```bash
helm repo add bitnami https://charts.bitnami.com/bitnami
helm repo update

# Redis (for priority queue, events, status)
helm install redis bitnami/redis \
    --namespace batch-api \
    --set auth.enabled=false \
    --set replica.replicaCount=0 \
    --set master.persistence.enabled=false \
    --wait --timeout 120s

# PostgreSQL (primary database)
helm install postgresql bitnami/postgresql \
    --namespace batch-api \
    --set auth.postgresPassword=postgres \
    --set primary.persistence.enabled=false \
    --wait --timeout 120s
```

Verify Redis and PostgreSQL are running:

```bash
kubectl get statefulset -n batch-api
kubectl get pods -n batch-api
```

### 2.9 Create Application Secret

Create a Kubernetes Secret containing database connection URLs. This secret is mounted into the apiserver and processor pods at `/etc/.secrets/`.

```bash
kubectl create secret generic batch-gateway-secrets \
    --namespace batch-api \
    --from-literal=redis-url="redis://redis-master.batch-api.svc.cluster.local:6379/0" \
    --from-literal=postgresql-url="postgresql://postgres:postgres@postgresql.batch-api.svc.cluster.local:5432/postgres"
```

### 2.10 Install Batch Gateway

Deploy batch-gateway via Helm chart with TLS enabled on the apiserver:

```bash
helm install batch-gateway ./charts/batch-gateway \
    --namespace batch-api \
    --set global.secretName=batch-gateway-secrets \
    --set global.databaseType=postgresql \
    --set global.fileClient.type=fs \
    --set global.fileClient.fs.pvcName=batch-gateway-files \
    --set processor.config.modelGateways.default.url=http://vllm-free-model.llm.svc.cluster.local:8000 \
    --set apiserver.tls.enabled=true \
    --set apiserver.tls.certManager.enabled=true \
    --set apiserver.tls.certManager.issuerName=selfsigned-issuer \
    --set apiserver.tls.certManager.issuerKind=ClusterIssuer \
    --set "apiserver.tls.certManager.dnsNames={batch-gateway-apiserver,batch-gateway-apiserver.batch-api.svc.cluster.local,localhost}"
```

> **OpenShift**: Add `--set apiserver.podSecurityContext=null --set processor.podSecurityContext=null` to let OpenShift SCC assign UIDs.

#### Upgrading: removing model gateways

`modelGateways` is a map type. Helm's 3-way strategic merge **merges** maps rather than replacing them. If you previously added a per-model gateway via `--set` and later remove it, the old entry persists in the release values.

To cleanly remove a model gateway entry, use `--reset-values` so Helm discards all previous values and applies only the ones you provide:

```bash
helm upgrade batch-gateway ./charts/batch-gateway --reset-values \
    --set processor.config.modelGateways.default.url=http://vllm:8000 \
    ...
```

Verify batch-gateway is running:

```bash
kubectl get deployment -n batch-api -l app.kubernetes.io/instance=batch-gateway
kubectl get pods -n batch-api -l app.kubernetes.io/instance=batch-gateway
```

### 2.11 Create Backend TLS DestinationRule

This tells the Envoy Proxy to use TLS when connecting to the apiserver backend (TLS re-encrypt):

```bash
kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: batch-gateway-backend-tls
  namespace: istio-ingress
spec:
  host: batch-gateway-apiserver.batch-api.svc.cluster.local
  trafficPolicy:
    portLevelSettings:
    - port:
        number: 8000
      tls:
        mode: SIMPLE
        insecureSkipVerify: true
EOF
```

### 2.12 Create Batch HTTPRoute

Create an HTTPRoute to route batch API requests (`/v1/batches`, `/v1/files`) from the Envoy Proxy to the batch-gateway apiserver.

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-route
  namespace: batch-api
spec:
  parentRefs:
  - name: istio-gateway
    namespace: istio-ingress
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /v1/batches
    - path:
        type: PathPrefix
        value: /v1/files
    backendRefs:
    - name: batch-gateway-apiserver
      port: 8000
EOF
```

## 3. Test

Get the Gateway URL:

```bash
# Option 1: Port-forward (for local testing)
kubectl port-forward -n istio-ingress svc/istio-gateway-istio 8080:443 &
export GATEWAY_URL=https://localhost:8080

# Option 2: Use the external address (if LoadBalancer is available)
export GATEWAY_URL=https://$(kubectl get gateway istio-gateway -n istio-ingress -o jsonpath='{.status.addresses[0].value}')
```

### 3.1 LLM Direct Inference

```bash
curl -sk -X POST ${GATEWAY_URL}/llm/free-model/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"free-model","messages":[{"role":"user","content":"Hello"}]}'
```

### 3.2 Batch Lifecycle

```bash
# 1. Upload input file
cat > batch-input.jsonl <<EOF
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"free-model","messages":[{"role":"user","content":"Hello"}]}}
{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"free-model","messages":[{"role":"user","content":"Tell me a joke"}]}}
EOF

curl -sk -X POST ${GATEWAY_URL}/v1/files \
    -F purpose=batch \
    -F file=@batch-input.jsonl

# 2. Create batch job (replace FILE_ID with the id from step 1)
curl -sk -X POST ${GATEWAY_URL}/v1/batches \
    -H 'Content-Type: application/json' \
    -d '{"input_file_id":"FILE_ID","endpoint":"/v1/chat/completions","completion_window":"24h"}'

# 3. Poll batch status (replace BATCH_ID with the id from step 2)
curl -sk ${GATEWAY_URL}/v1/batches/BATCH_ID

# 4. Download output file (replace OUTPUT_FILE_ID from the completed batch response)
curl -sk ${GATEWAY_URL}/v1/files/OUTPUT_FILE_ID/content
```

## 5. References

- [Batch Gateway Repository](../../README.md)
- [Kuadrant Integration Guide](kuadrant-integration.md) — Authentication, authorization, and rate limiting
- [Gateway API Documentation](https://gateway-api.sigs.k8s.io/)
- [Gateway API Inference Extension (GAIE)](https://github.com/kubernetes-sigs/gateway-api-inference-extension)
- [Istio Documentation](https://istio.io/latest/docs/)
- [cert-manager Documentation](https://cert-manager.io/docs/)
- [Istio DestinationRule TLS](https://istio.io/latest/docs/reference/config/networking/destination-rule/#ClientTLSSettings)
