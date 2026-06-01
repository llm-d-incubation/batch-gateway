# Batch Gateway on Kubernetes

This guide demonstrates how to deploy batch-gateway on vanilla Kubernetes (or OpenShift) using open-source Helm charts. It uses the [llm-d](https://llm-d.ai) stack (Istio + GAIE InferencePool + vLLM) and [Kuadrant](https://kuadrant.io/) for authentication, authorization, and rate limiting.

## 1. Architecture

### 1.1 Namespace Layout

| Namespace | Purpose |
|-----------|---------|
| `istio-system` | Istio control plane (istiod) |
| `istio-ingress` | Gateway data plane (Istio/Envoy proxy) |
| `cert-manager` | cert-manager controller, webhook, cainjector |
| `kuadrant-system` | Kuadrant operator, Authorino, Limitador |
| `batch-api` | batch-gateway (apiserver + processor + gc), Redis, PostgreSQL |
| `llm` | llm-d stack: InferencePool, EPP, vLLM |

### 1.2 Data Flow

![Deployment Architecture](diagrams/deploy-k8s-arch.png)

**Batch inference flow**:
1. Client sends a batch request (e.g. `POST /v1/batches`) to the External Gateway (`istio-gateway`, HTTPS :443) with a Kubernetes token
2. Gateway matches `/v1/batches`, `/v1/files` → **batch-route** (HTTPRoute)
    - **AuthPolicy** on the batch-route performs authentication only (kubernetesTokenReview, no authorization check) — unauthenticated requests are rejected with 401
    - **RateLimitPolicy** on the batch-route enforces per-user request rate limiting (e.g. 20 req/min), keyed by Kubernetes username (user or ServiceAccount) from TokenReview — excess requests are rejected with 429
    - Authenticated request is forwarded to **batch-gateway apiserver**, which stores the batch job
3. **Processor** dequeues the batch job and sends inference requests to the **Internal Gateway** (`batch-internal-gateway`, ClusterIP HTTP :80) with the user's original token (via `passThroughHeaders: [Authorization]`)
4. The Internal Gateway matches `/{ns}/{model}/v1/*` → **batch-llm-route** (HTTPRoute)
    - **AuthPolicy** on the batch-llm-route performs authentication and authorization (SubjectAccessReview — checks if the original user can `get inferencepools/<model-name>`, where `<model-name>` is extracted from the URL path, not the backend InferencePool object name) — if the user lacks permission, the request is rejected with 403
    - No **TokenRateLimitPolicy** on the batch-llm-route — batch inference requests bypass per-user token rate limiting
5. Request is routed to **InferencePool** → **EPP** (endpoint picker) → **vLLM** model server, and the response is returned to the Processor, which adds the response to the batch job's output file
    - The Processor sets the `x-gateway-inference-objective: batch-sheddable` header, which assigns the request to the **batch priority band (priority -1)** in EPP's flow control. This band is **sheddable** — when the backend is saturated, batch requests are rejected immediately instead of queued, and the Processor retries with backoff

**Online inference flow**:
1. Client sends an inference request (e.g. `POST /{ns}/{model}/v1/chat/completions`) to the External Gateway with a Kubernetes token
2. Gateway matches `/{ns}/{model}/v1/*` → **llm-route** (HTTPRoute, manually created with URL rewrite rules)
    - **AuthPolicy** on the llm-route performs authentication and authorization (SubjectAccessReview — same model access check as the batch-llm-route)
    - **TokenRateLimitPolicy** on the llm-route enforces per-user token rate limiting, keyed by Kubernetes username from TokenReview
3. Request is routed to **InferencePool** → **EPP** → **vLLM** model server
    - Interactive requests without an `x-gateway-inference-objective` header default to **priority 0**, which outranks the batch band (priority -1). Requests with `x-gateway-inference-objective: interactive-default` are assigned to **priority 100**

> **Why two gateways?** The Internal Gateway is a ClusterIP-only Envoy proxy that is not accessible from outside the cluster. The batch processor uses it to bypass the TokenRateLimitPolicy applied on the External Gateway's llm-route. This ensures batch jobs are not throttled by per-user token rate limits intended for interactive use. The Internal Gateway's batch-llm-route still enforces AuthPolicy (authentication + model authorization), so unauthorized access is always blocked.

> **Why flow control?** Without flow control, batch and interactive requests compete equally for backend resources. Under heavy batch load, interactive latency spikes because the backend is saturated with batch requests. Flow control solves this with priority-based dispatch: interactive requests (priority 0/100) are always dispatched before batch requests (priority -1). When the backend is saturated, batch requests are shed (rejected immediately) rather than queued, and the batch processor retries with backoff. This ensures interactive traffic always gets low-latency treatment while batch fills remaining capacity. See [Flow Control Setup](flow-control-setup.md) for detailed configuration.

### 1.3 Authentication

Both the LLM route and the batch route use **kubernetesTokenReview** for authentication. Clients must provide a valid Kubernetes token via the `Authorization: Bearer <token>` header. The token must include the audience `https://kubernetes.default.svc`.

```bash
# Create a token for a ServiceAccount
kubectl create token <sa-name> -n <namespace> --audience=https://kubernetes.default.svc --duration=10m
```

HTTPRoute authentication behavior:
- **LLM route**: Requires a valid Kubernetes token — unauthenticated requests are rejected with **401**
- **Batch route**: Requires a valid Kubernetes token — unauthenticated requests are rejected with **401**

### 1.4 Authorization Model

Users need RBAC `get` permission on the `inferencepools` resource whose name matches the **model name in the URL path**. The AuthPolicy extracts the resource name from the URL via `request.path.split("/")[2]`.

> **Important**: The SAR resource name (derived from the URL path segment) is **independent** of the HTTPRoute backend `InferencePool` metadata name. Which `InferencePool` a given path segment routes to is determined by **routing** (the HTTPRoute / route map), not by SAR. SAR controls *who* can access a model endpoint; the HTTPRoute controls *where* that endpoint's traffic is sent. For example, a URL path segment `random` may route to an `InferencePool` named `gaie-llmd` — the RBAC `resourceNames` should use the URL path segment (`random`), not the `InferencePool` object name.

To grant access, create a Role and RoleBinding:

> **Note**: Unlike RHOAI (which checks `llminferenceservices`), the k8s deployment checks `inferencepools` because the llm-route directly references InferencePool backends.

```bash
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: model-access
  namespace: <llm-namespace>
rules:
- apiGroups: ["inference.networking.k8s.io"]
  resources: ["inferencepools"]
  resourceNames: ["<model-name>"]   # must match the model name in the URL path /{namespace}/{model-name}/v1/*
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: model-access-binding
  namespace: <llm-namespace>
subjects:
- kind: ServiceAccount
  name: <sa-name>
  namespace: <llm-namespace>
roleRef:
  kind: Role
  name: model-access
  apiGroup: rbac.authorization.k8s.io
EOF
```

Verify that the user has access:

```bash
kubectl auth can-i get inferencepools/<model-name> -n <llm-namespace> --as=system:serviceaccount:<namespace>:<sa-name>
# Expected output: yes
```

HTTPRoute authorization behavior:
- **llm-route** / **batch-llm-route**: SubjectAccessReview checks if user can `get inferencepools/<model-name>` (extracted from URL path) — unauthorized requests are rejected with **403**
- **batch-route**: No authorization check — authorization is enforced by the **batch-llm-route** (on the Internal Gateway) when the processor forwards inference requests with the user's original token

### 1.5 Security boundary: batch-route vs batch-llm-route vs llm-route

For security and operations readers: **admission on the batch API is not the same as authorization for inference.**

- **batch-route** (External Gateway) proves the caller has a valid Kubernetes token and applies batch-side **RateLimitPolicy**. Invalid or missing credentials are rejected with **401**; excess batch API traffic is rejected with **429**. It does **not** evaluate whether the caller may use a specific model.
- **batch-llm-route** (Internal Gateway) runs **authentication and authorization** (SubjectAccessReview on `inferencepools`) on each inference request the processor sends. This route does **not** have a TokenRateLimitPolicy, so batch inference is not token-rate-limited. A user can create a batch job and still see **per-request failures** (often surfaced as failed lines or job errors) when the batch-llm-route returns **403** — this is **by design**, not a bypass of model access control.
- **llm-route** (External Gateway) runs **authentication, authorization, and token rate limiting** on each online inference request. This is the user-facing inference endpoint.

The `Authorization` header is included in `passThroughHeaders`, so the processor forwards the end user's bearer token on inference calls automatically. Without it, the Internal Gateway cannot attribute inference traffic to the original caller and model-level checks cannot run as intended.

## 2. Prerequisites

- Kubernetes cluster (or OpenShift 4.x)
- CLI tools: `kubectl`, `helm`, `helmfile`, `git`, `curl`, `jq`, `yq`
- Helm plugin: `helm-diff` (`helm plugin install https://github.com/databus23/helm-diff`)

### Environment Variables

Set these once before running any installation or test step. All subsequent code blocks reference these variables.

```bash
# Gateway
export GATEWAY_NAME=istio-gateway
export GATEWAY_NAMESPACE=istio-ingress
export INTERNAL_GW_NAME=batch-internal-gateway

# Namespaces
export LLM_NS=llm
export BATCH_NS=batch-api
export KUADRANT_NS=kuadrant-system

# Component versions
export CERT_MANAGER_VERSION=v1.15.3
export KUADRANT_VERSION=1.3.1
export LLMD_VERSION=v0.7.0
export LLMD_GIT_DIR="/tmp/llm-d-${LLMD_VERSION}"
export GAIE_CHART_VERSION=v1.5.0
export MODELSERVICE_CHART_VERSION=v0.4.12

# llm-d model
export LLMD_RELEASE_POSTFIX=llmd
export LLMD_POOL_NAME=gaie-${LLMD_RELEASE_POSTFIX}
export MODEL_NAME=random

# Flow control
export INTERACTIVE_FLOW_CONTROL_OBJECTIVE=interactive-default
export BATCH_FLOW_CONTROL_OBJECTIVE=batch-sheddable
```

## 3. Installation Steps

### 3.1 Install cert-manager

<details>
<summary>Install cert-manager via Helm</summary>

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
    --namespace cert-manager \
    --create-namespace \
    --version "${CERT_MANAGER_VERSION}" \
    --set crds.enabled=true

# Wait for deployments
kubectl rollout status deploy/cert-manager -n cert-manager --timeout=120s
kubectl rollout status deploy/cert-manager-webhook -n cert-manager --timeout=120s
kubectl rollout status deploy/cert-manager-cainjector -n cert-manager --timeout=120s
```

</details>

<details>
<summary>Create a self-signed ClusterIssuer</summary>

```bash
kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
EOF
```

</details>

### 3.2 Install llm-d Dependencies (CRD + Istio)

Install Gateway API CRDs and Istio from the [llm-d repository](https://github.com/llm-d/llm-d).

<details>
<summary>Clone llm-d and install CRDs + Istio</summary>

```bash
git clone --depth 1 --branch "${LLMD_VERSION}" \
    https://github.com/llm-d/llm-d.git "${LLMD_GIT_DIR}"

# Install Gateway API + GAIE CRDs
bash "${LLMD_GIT_DIR}/guides/prereq/gateway-provider/install-gateway-provider-dependencies.sh"

# Install Istio via helmfile
helmfile apply -f "${LLMD_GIT_DIR}/guides/prereq/gateway-provider/istio.helmfile.yaml"
```

</details>

<details>
<summary>Check Istiod installation</summary>

```
kubectl get all -n istio-system

NAME                          READY   STATUS    RESTARTS   AGE
pod/istiod-66b5776d74-ddprr   1/1     Running   0          10m

NAME             TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)                                 AGE
service/istiod   ClusterIP   172.30.29.80   <none>        15010/TCP,15012/TCP,443/TCP,15014/TCP   10m

NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/istiod   1/1     1            1           10m

NAME                                DESIRED   CURRENT   READY   AGE
replicaset.apps/istiod-66b5776d74   1         1         1       10m

NAME                                         REFERENCE           TARGETS       MINPODS   MAXPODS   REPLICAS   AGE
horizontalpodautoscaler.autoscaling/istiod   Deployment/istiod   cpu: 0%/80%   1         5         1          10m
```
</details>

### 3.3 Install Kuadrant

<details>
<summary>Install Kuadrant operator via Helm</summary>

```bash
helm repo add kuadrant https://kuadrant.io/helm-charts/ --force-update
helm upgrade --install kuadrant-operator kuadrant/kuadrant-operator \
    --version "${KUADRANT_VERSION}" \
    --create-namespace \
    --namespace "${KUADRANT_NS}"

# Wait for operator deployments
kubectl rollout status deploy/authorino-operator -n ${KUADRANT_NS} --timeout=120s
kubectl rollout status deploy/kuadrant-operator-controller-manager -n ${KUADRANT_NS} --timeout=120s
kubectl rollout status deploy/limitador-operator-controller-manager -n ${KUADRANT_NS} --timeout=120s
```

</details>

<details>
<summary>Create Kuadrant CR</summary>

```bash
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: ${KUADRANT_NS}
spec: {}
EOF

kubectl get kuadrant kuadrant -n "${KUADRANT_NS}"

# wait for kuadrant instance is ready
kubectl wait kuadrant/kuadrant --for="condition=Ready=true" \
    -n "${KUADRANT_NS}" --timeout=300s
```

</details>

<details>
<summary>Check Kuadrant Installation</summary>

```
kubectl get all -n "${KUADRANT_NS}"
Warning: apps.openshift.io/v1 DeploymentConfig is deprecated in v4.14+, unavailable in v4.10000+
NAME                                                        READY   STATUS    RESTARTS   AGE
pod/authorino-7758d659c-gnxgz                               1/1     Running   0          12m
pod/authorino-operator-6f859bbd59-7jdjb                     1/1     Running   0          12m
pod/dns-operator-controller-manager-5c659dd95f-msk9k        1/1     Running   0          12m
pod/kuadrant-console-plugin-5d7c7bc6f9-9pg7b                1/1     Running   0          12m
pod/kuadrant-operator-controller-manager-cbd896f76-pdf5c    1/1     Running   0          12m
pod/limitador-limitador-658c8849b8-4c8w7                    1/1     Running   0          12m
pod/limitador-operator-controller-manager-cb6c488bf-94m24   1/1     Running   0          12m

NAME                                                      TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)              AGE
service/authorino-authorino-authorization                 ClusterIP   172.30.229.206   <none>        50051/TCP,5001/TCP   12m
service/authorino-authorino-oidc                          ClusterIP   172.30.51.82     <none>        8083/TCP             12m
service/authorino-controller-metrics                      ClusterIP   172.30.114.175   <none>        8080/TCP             12m
service/authorino-operator-metrics                        ClusterIP   172.30.147.35    <none>        8080/TCP             12m
service/dns-operator-controller-manager-metrics-service   ClusterIP   172.30.215.23    <none>        8080/TCP             12m
service/kuadrant-console-plugin                           ClusterIP   172.30.141.247   <none>        9443/TCP             12m
service/kuadrant-operator-metrics                         ClusterIP   172.30.132.188   <none>        8080/TCP             12m
service/limitador-limitador                               ClusterIP   None             <none>        8080/TCP,8081/TCP    12m
service/limitador-operator-metrics                        ClusterIP   172.30.176.191   <none>        8080/TCP             12m

NAME                                                    READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/authorino                               1/1     1            1           12m
deployment.apps/authorino-operator                      1/1     1            1           12m
deployment.apps/dns-operator-controller-manager         1/1     1            1           12m
deployment.apps/kuadrant-console-plugin                 1/1     1            1           12m
deployment.apps/kuadrant-operator-controller-manager    1/1     1            1           12m
deployment.apps/limitador-limitador                     1/1     1            1           12m
deployment.apps/limitador-operator-controller-manager   1/1     1            1           12m

NAME                                                              DESIRED   CURRENT   READY   AGE
replicaset.apps/authorino-76d7b84c9                               0         0         0       12m
replicaset.apps/authorino-7758d659c                               1         1         1       12m
replicaset.apps/authorino-operator-6f859bbd59                     1         1         1       12m
replicaset.apps/dns-operator-controller-manager-5c659dd95f        1         1         1       12m
replicaset.apps/kuadrant-console-plugin-5d7c7bc6f9                1         1         1       12m
replicaset.apps/kuadrant-operator-controller-manager-cbd896f76    1         1         1       12m
replicaset.apps/limitador-limitador-658c8849b8                    1         1         1       12m
replicaset.apps/limitador-limitador-777cf94b6d                    0         0         0       12m
replicaset.apps/limitador-operator-controller-manager-cb6c488bf   1         1         1       12m
```
</details>

### 3.4 Create Gateway and TLS Certificate

<details>
<summary>Create TLS Certificate for Gateway</summary>

```bash
kubectl create namespace "${GATEWAY_NAMESPACE}" 2>/dev/null || true

kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${GATEWAY_NAME}-tls
  namespace: ${GATEWAY_NAMESPACE}
spec:
  secretName: ${GATEWAY_NAME}-tls
  issuerRef:
    name: selfsigned-issuer
    kind: ClusterIssuer
  dnsNames:
  - "*.${GATEWAY_NAMESPACE}.svc.cluster.local"
  - localhost
EOF

kubectl wait --for=condition=Ready --timeout=60s \
    -n "${GATEWAY_NAMESPACE}" certificate/${GATEWAY_NAME}-tls
```

</details>

<details>
<summary>Create the Istio Gateway (HTTP + HTTPS)</summary>

```bash
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

# wait for gateway instance is ready
kubectl wait --for=condition=Programmed --timeout=300s \
    -n "${GATEWAY_NAMESPACE}" gateway/${GATEWAY_NAME}
```

> **Note**: The Gateway uses a self-signed certificate from cert-manager (not OpenShift router certs). Access via `kubectl port-forward` with `-k` (insecure) flag on curl.

> **Security**: The Gateway uses `allowedRoutes.namespaces.from: Selector` to restrict HTTPRoute attachment. Only namespaces labeled with `llm-d.ai/gateway-route: "true"` can attach HTTPRoutes. This must be applied to the batch and LLM namespaces before creating their HTTPRoutes CRs.

</details>

<details>
<summary>Check envoy proxy installation</summary>

```
kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}"
NAME            CLASS   ADDRESS                                                                  PROGRAMMED   AGE
istio-gateway   istio   a0c489f378d1f492bb6123d83bad0d95-863333050.us-east-2.elb.amazonaws.com   True         13m

kubectl get all -n "${GATEWAY_NAMESPACE}"
Warning: apps.openshift.io/v1 DeploymentConfig is deprecated in v4.14+, unavailable in v4.10000+
NAME                                       READY   STATUS    RESTARTS   AGE
pod/istio-gateway-istio-544bcc95c5-dv6l6   1/1     Running   0          12m

NAME                          TYPE           CLUSTER-IP       EXTERNAL-IP                                                              PORT(S)                                      AGE
service/istio-gateway-istio   LoadBalancer   172.30.218.212   a0c489f378d1f492bb6123d83bad0d95-863333050.us-east-2.elb.amazonaws.com   15021:30205/TCP,80:31849/TCP,443:30658/TCP   12m

NAME                                  READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/istio-gateway-istio   1/1     1            1           12m

NAME                                             DESIRED   CURRENT   READY   AGE
replicaset.apps/istio-gateway-istio-544bcc95c5   1         1         1       12m
```
</details>


### 3.5 Deploy model with llm-d

This doc follows the [simulated-accelerators guide](https://llm-d.ai/docs/guide/Installation/simulated-accelerators). Find more guides at [llm-d guides](https://llm-d.ai/docs/guide/).

```bash
# Create the LLM namespace
kubectl create namespace "${LLM_NS}" 2>/dev/null || true
kubectl label namespace "${LLM_NS}" llm-d.ai/gateway-route=true --overwrite
```

<details>
<summary>Install InferencePool (GAIE EPP) with flow control</summary>

The InferencePool chart deploys the EPP (Endpoint Picker Plugin). The flow control overlay configures priority-based dispatch with two bands: interactive (priority 100) and batch (priority -1, sheddable).

```bash
EPP_HOST="${LLMD_POOL_NAME}-epp.${LLM_NS}.svc.cluster.local"

helm upgrade --install "${LLMD_POOL_NAME}" \
    oci://registry.k8s.io/gateway-api-inference-extension/charts/inferencepool \
    --version "${GAIE_CHART_VERSION}" \
    --namespace "${LLM_NS}" \
    -f examples/deploy-demo/llmd-sim/gaie-sim-values.yaml \
    -f examples/deploy-demo/llmd-sim/overlays/flow-control.yaml \
    --set "provider.istio.destinationRule.host=${EPP_HOST}"
```

> **Flow control overlay**: The `-f overlays/flow-control.yaml` overlay sets `pluginsConfigFile: flow-control-plugins.yaml` and configures the `EndpointPickerConfig` with the `flowControl` feature gate, two priority bands, and a saturation detector. See [Flow Control Setup](flow-control-setup.md) for details on the configuration.

</details>

<details>
<summary>Install ModelService (vllm-sim)</summary>

```bash
helm repo add llm-d-modelservice https://llm-d-incubation.github.io/llm-d-modelservice/ --force-update
helm upgrade --install "ms-${LLMD_RELEASE_POSTFIX}" llm-d-modelservice/llm-d-modelservice \
    --version "${MODELSERVICE_CHART_VERSION}" \
    --namespace "${LLM_NS}" \
    -f examples/deploy-demo/llmd-sim/ms-sim-values.yaml

# Wait for deployments
kubectl rollout status deploy/${LLMD_POOL_NAME}-epp -n ${LLM_NS} --timeout=300s
kubectl rollout status deploy/ms-${LLMD_RELEASE_POSTFIX}-llm-d-modelservice-decode -n ${LLM_NS} --timeout=300s
```

</details>

<details>
<summary>Create InferenceObjective CRDs</summary>

InferenceObjective CRDs assign requests to priority bands based on the `x-gateway-inference-objective` header. Create one for interactive traffic (priority 100) and one for batch traffic (priority -1, sheddable).

```bash
kubectl apply -f - <<EOF
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: ${INTERACTIVE_FLOW_CONTROL_OBJECTIVE}
  namespace: ${LLM_NS}
spec:
  priority: 100
  poolRef:
    group: inference.networking.k8s.io
    name: ${LLMD_POOL_NAME}
---
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: ${BATCH_FLOW_CONTROL_OBJECTIVE}
  namespace: ${LLM_NS}
spec:
  priority: -1
  poolRef:
    group: inference.networking.k8s.io
    name: ${LLMD_POOL_NAME}
EOF
```

> **Note**: InferenceObjectives are created via `kubectl apply` instead of the InferencePool chart's `inferenceObjectives` values because the chart template (v1.5.0) has a scoping bug that causes `helm install` to fail when `inferenceObjectives` is non-empty.

Verify the objectives:

```bash
kubectl get inferenceobjective -n ${LLM_NS}
```

</details>

<details>
<summary>Check llm-d model installation</summary>

```
kubectl get all -n ${LLM_NS}

NAME                                                      READY   STATUS    RESTARTS   AGE
pod/gaie-llmd-epp-76798cd4dd-sfdck                        1/1     Running   0          14m
pod/ms-llmd-llm-d-modelservice-decode-75cfd56dbb-dv7fm    2/2     Running   0          14m
pod/ms-llmd-llm-d-modelservice-decode-75cfd56dbb-q66n8    2/2     Running   0          14m
pod/ms-llmd-llm-d-modelservice-decode-75cfd56dbb-v5zc9    2/2     Running   0          14m
pod/ms-llmd-llm-d-modelservice-prefill-7d7b78699f-nccpj   1/1     Running   0          14m

NAME                            TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)             AGE
service/gaie-llmd-epp           ClusterIP   172.30.174.214   <none>        9002/TCP,9090/TCP   14m
service/gaie-llmd-ip-d209bc5e   ClusterIP   None             <none>        54321/TCP           14m

NAME                                                 READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/gaie-llmd-epp                        1/1     1            1           14m
deployment.apps/ms-llmd-llm-d-modelservice-decode    3/3     3            3           14m
deployment.apps/ms-llmd-llm-d-modelservice-prefill   1/1     1            1           14m

NAME                                                            DESIRED   CURRENT   READY   AGE
replicaset.apps/gaie-llmd-epp-76798cd4dd                        1         1         1       14m
replicaset.apps/ms-llmd-llm-d-modelservice-decode-75cfd56dbb    3         3         3       14m
replicaset.apps/ms-llmd-llm-d-modelservice-prefill-7d7b78699f   1         1         1       14m
```
</details>


### 3.6 Configure HTTPRoute and Policies for LLM model

<details>
<summary>Create HTTPRoute for LLM inference</summary>

The llm-route is manually created with URL rewrite rules that map `/{namespace}/{model}/v1/*` to the InferencePool backend.

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: ${LLM_NS}
spec:
  parentRefs:
  - name: ${GATEWAY_NAME}
    namespace: ${GATEWAY_NAMESPACE}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}/v1/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}/v1/chat/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/chat/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
EOF
```

> **Note**: Unlike RHOAI where `LLMInferenceService` auto-generates the HTTPRoute, the k8s deployment requires a manually created llm-route with explicit URL rewrite rules.

</details>

<details>
<summary>Create AuthPolicy for LLM route (authentication + authorization)</summary>

```bash
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: llm-route-auth
  namespace: ${LLM_NS}
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
```

> **Note**: The authorization uses `inferencepools` (not `llminferenceservices` as in RHOAI). The `request.path.split("/")[2]` extracts the **model name** from the URL path `/{namespace}/{model}/...` for the SAR check. This is the user-facing model name, not the backend `InferencePool` object name — the HTTPRoute determines which `InferencePool` actually receives traffic for each path prefix (see [Section 1.4](#14-authorization-model)).

</details>

<details>
<summary>Create TokenRateLimitPolicy for LLM route</summary>

```bash
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: inference-token-limit
  namespace: ${LLM_NS}
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

kubectl wait tokenratelimitpolicy/inference-token-limit \
    --for="condition=Enforced=true" \
    -n ${LLM_NS} --timeout=120s
```

> **Note**: The TokenRateLimitPolicy targets the HTTPRoute (not the Gateway), because the llm-route is manually created with a stable name.

</details>

### 3.7 Install Batch Gateway

The batch processor routes inference requests through a separate, ClusterIP-only Internal Gateway to bypass the TokenRateLimitPolicy on the External Gateway while still enforcing model-level authorization (AuthPolicy).

<details>
<summary>Create Internal Gateway (ClusterIP)</summary>

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${INTERNAL_GW_NAME}
  namespace: ${GATEWAY_NAMESPACE}
  annotations:
    networking.istio.io/service-type: ClusterIP
spec:
  gatewayClassName: istio
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            llm-d.ai/gateway-route: "true"
EOF

kubectl wait --for=condition=Programmed --timeout=300s \
    -n "${GATEWAY_NAMESPACE}" gateway/${INTERNAL_GW_NAME}
```

> **Note**: The `networking.istio.io/service-type: ClusterIP` annotation ensures the Internal Gateway's Service is ClusterIP-only (no LoadBalancer, no external IP). This prevents direct external access — all external traffic must go through the External Gateway.

</details>

<details>
<summary>Create batch-llm-route (HTTPRoute on Internal Gateway)</summary>

The batch-llm-route is attached to the Internal Gateway and has the same URL rewrite rules as the llm-route, but targets the Internal Gateway instead of the External Gateway.

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-llm-route
  namespace: ${LLM_NS}
spec:
  parentRefs:
  - name: ${INTERNAL_GW_NAME}
    namespace: ${GATEWAY_NAMESPACE}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}/v1/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}/v1/chat/completions
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /v1/chat/completions
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
  - matches:
    - path:
        type: PathPrefix
        value: /${LLM_NS}/${MODEL_NAME}
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /
    backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: ${LLMD_POOL_NAME}
EOF
```

</details>

<details>
<summary>Create AuthPolicy for batch-llm-route (authentication + authorization)</summary>

```bash
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-llm-route-auth
  namespace: ${LLM_NS}
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
```

> **Note**: The batch-llm-route has AuthPolicy (authentication + authorization) but **no TokenRateLimitPolicy**. This is intentional — batch inference requests should not be throttled by per-user token rate limits. The External Gateway's llm-route handles token rate limiting for online (interactive) requests only.

</details>

Deploy batch-gateway with the model gateway URL pointing to the Internal Gateway:

<details>
<summary>Create namespace and install dependencies</summary>

```bash
kubectl create namespace "${BATCH_NS}" 2>/dev/null || true
kubectl label namespace "${BATCH_NS}" llm-d.ai/gateway-route=true --overwrite

# Install Redis (or Valkey — see alternative below)
helm upgrade --install redis oci://registry-1.docker.io/bitnamicharts/redis \
    --namespace ${BATCH_NS} --create-namespace \
    --set architecture=standalone \
    --set auth.enabled=false
kubectl rollout status statefulset/redis-master -n ${BATCH_NS} --timeout=120s

# Alternative: Install Valkey (wire-protocol compatible with Redis)
# helm upgrade --install redis oci://registry-1.docker.io/bitnamicharts/valkey \
#     --namespace ${BATCH_NS} --create-namespace \
#     --set architecture=standalone \
#     --set auth.enabled=false
# kubectl rollout status statefulset/redis-valkey-primary -n ${BATCH_NS} --timeout=120s
# Note: when using Valkey, update the redis-url secret below to use:
#   redis://redis-valkey-primary.${BATCH_NS}.svc.cluster.local:6379/0

# Install PostgreSQL
helm upgrade --install postgresql oci://registry-1.docker.io/bitnamicharts/postgresql \
    --namespace ${BATCH_NS} --create-namespace \
    --set auth.postgresPassword=<your-postgres-password> \
    --set auth.database=batch
kubectl rollout status statefulset/postgresql -n ${BATCH_NS} --timeout=120s

# Install MinIO (S3-compatible object storage for batch files)
MINIO_USER=<your-minio-user>
MINIO_PASSWORD=<your-minio-password>
MINIO_BUCKET=batch-gateway

kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: ${BATCH_NS}
  labels:
    app: minio
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
      - name: minio
        image: quay.io/minio/minio:RELEASE.2024-12-18T13-15-44Z
        args: ["server", "/data", "--console-address", ":9001"]
        env:
        - name: MINIO_ROOT_USER
          value: "${MINIO_USER}"
        - name: MINIO_ROOT_PASSWORD
          value: "${MINIO_PASSWORD}"
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
  name: minio
  namespace: ${BATCH_NS}
  labels:
    app: minio
spec:
  selector:
    app: minio
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
  type: ClusterIP
EOF

until kubectl get deployment minio -n ${BATCH_NS} &>/dev/null; do sleep 5; done
kubectl rollout status deployment/minio -n ${BATCH_NS} --timeout=180s

# Create application secret
kubectl create secret generic batch-gateway-secrets \
    --namespace ${BATCH_NS} \
    --from-literal=redis-url="redis://redis-master.${BATCH_NS}.svc.cluster.local:6379/0" \
    --from-literal=postgresql-url="postgresql://postgres:<your-postgres-password>@postgresql.${BATCH_NS}.svc.cluster.local:5432/batch?sslmode=disable" \
    --from-literal=s3-secret-access-key="${MINIO_PASSWORD}" \
    --dry-run=client -o yaml | kubectl apply -f -
```

> **Note**: Redis auth and PostgreSQL persistence are disabled for demo purposes. For production, enable Redis authentication and configure persistent storage.

</details>

<details>
<summary>Install batch-gateway</summary>

```bash
IMAGE_TAG=v0.1.0
APISERVER_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-apiserver
PROCESSOR_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-processor
GC_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-gc
```

```bash
# Discover the Internal Gateway's Service (ClusterIP, HTTP :80)
INTERNAL_GW_SVC=$(kubectl get svc -n ${GATEWAY_NAMESPACE} \
    -l "gateway.networking.k8s.io/gateway-name=${INTERNAL_GW_NAME}" \
    -o jsonpath='{.items[0].metadata.name}')

# Model gateway URL: route through the Internal Gateway (which has AuthPolicy but no TokenRateLimitPolicy)
MODEL_GW_URL="http://${INTERNAL_GW_SVC}.${GATEWAY_NAMESPACE}.svc.cluster.local/${LLM_NS}/${MODEL_NAME}"

helm upgrade --install batch-gateway ./charts/batch-gateway \
    --namespace ${BATCH_NS} \
    --set "apiserver.image.repository=${APISERVER_REPO}" \
    --set "apiserver.image.tag=${IMAGE_TAG}" \
    --set "processor.image.repository=${PROCESSOR_REPO}" \
    --set "processor.image.tag=${IMAGE_TAG}" \
    --set "gc.image.repository=${GC_REPO}" \
    --set "gc.image.tag=${IMAGE_TAG}" \
    --set "global.secretName=batch-gateway-secrets" \
    --set "global.dbClient.type=postgresql" \
    --set "global.fileClient.type=s3" \
    --set "global.fileClient.s3.endpoint=http://minio.${BATCH_NS}.svc.cluster.local:9000" \
    --set "global.fileClient.s3.region=us-east-1" \
    --set "global.fileClient.s3.accessKeyId=${MINIO_USER}" \
    --set "global.fileClient.s3.prefix=${MINIO_BUCKET}" \
    --set "global.fileClient.s3.usePathStyle=true" \
    --set "global.fileClient.s3.autoCreateBucket=true" \
    --set "processor.config.modelGateways.${MODEL_NAME}.url=${MODEL_GW_URL}" \
    --set "processor.config.modelGateways.${MODEL_NAME}.requestTimeout=5m" \
    --set "processor.config.modelGateways.${MODEL_NAME}.maxRetries=3" \
    --set "processor.config.modelGateways.${MODEL_NAME}.initialBackoff=1s" \
    --set "processor.config.modelGateways.${MODEL_NAME}.maxBackoff=60s" \
    --set "processor.config.modelGateways.${MODEL_NAME}.inferenceObjective=${BATCH_FLOW_CONTROL_OBJECTIVE}" \
    --set "apiserver.config.batchAPI.passThroughHeaders={Authorization}" \
    --set apiserver.tls.enabled=true \
    --set apiserver.tls.certManager.enabled=true \
    --set apiserver.tls.certManager.issuerName=selfsigned-issuer \
    --set apiserver.tls.certManager.issuerKind=ClusterIssuer \
    --set "apiserver.tls.certManager.dnsNames={batch-gateway-apiserver,batch-gateway-apiserver.${BATCH_NS}.svc.cluster.local,localhost}"
```

> - **`modelGateways.<model>.inferenceObjective`**: Set to the `InferenceObjective` CRD name for batch traffic (`batch-sheddable`). The processor sends this as the `x-gateway-inference-objective` header on every inference request, which EPP's flow control uses to assign the request to the batch priority band (priority -1). Without this, batch requests default to priority 0 and compete equally with interactive traffic.
> - **`modelGateways.<model>.url`**: The processor uses this URL to send inference requests. It points to the **Internal Gateway's** model endpoint (via in-cluster Service DNS), not the External Gateway or the model server directly. The Internal Gateway enforces AuthPolicy (model access check) but bypasses TokenRateLimitPolicy, so batch inference is not token-rate-limited.
> - **`passThroughHeaders`**: Set to `[Authorization]` so the processor forwards the end user's bearer token on inference calls. Without this, the Internal Gateway cannot attribute inference traffic to the original caller and model-level authorization checks will fail.
> - **No `tlsInsecureSkipVerify`**: The Internal Gateway uses plain HTTP (ClusterIP :80), so TLS verification is not needed for the processor → model gateway connection.
> - **`apiserver.tls.certManager.*`**: Enables TLS for the batch API server using cert-manager. The `dnsNames` should include the Service name and FQDN so the External Gateway can verify the backend certificate when re-encrypting traffic (see DestinationRule in 3.8).
> - **File storage**: This example uses S3-compatible storage (MinIO). To use a PVC instead, replace the `s3` options with:
>   ```
>   --set "global.fileClient.type=fs"
>   --set "global.fileClient.fs.basePath=/tmp/batch-gateway"
>   --set "global.fileClient.fs.pvcName=batch-gateway-files"
>   ```
>   and create a PVC with `ReadWriteMany` access mode. Note that `ReadWriteMany` requires a storage class that supports RWX (e.g. NFS, CephFS, EFS). Block storage (e.g. gp2, gp3) does not support RWX.

</details>

### 3.8 Configure HTTPRoute and Policies for Batch Gateway

<details>
<summary>Create HTTPRoute and DestinationRule for Batch API Server</summary>

```bash
# Batch HTTPRoute
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-route
  namespace: ${BATCH_NS}
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
    - name: batch-gateway-apiserver
      port: 8000
EOF

# DestinationRule for TLS re-encrypt between Gateway and batch apiserver
kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: batch-gateway-backend-tls
  namespace: ${GATEWAY_NAMESPACE}
spec:
  host: batch-gateway-apiserver.${BATCH_NS}.svc.cluster.local
  trafficPolicy:
    portLevelSettings:
    - port:
        number: 8000
      tls:
        mode: SIMPLE
        insecureSkipVerify: true
EOF
```

</details>

<details>
<summary>Create AuthPolicy for Batch API Server</summary>

```bash
# Batch AuthPolicy (authentication only, no authorization)
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-route-auth
  namespace: ${BATCH_NS}
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
```

</details>

<details>
<summary>Create RateLimitPolicy for Batch API Server</summary>

```bash
# Batch RateLimitPolicy (20 requests/min per user)
kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: RateLimitPolicy
metadata:
  name: batch-ratelimit
  namespace: ${BATCH_NS}
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
```

</details>

## 4. Test

### 4.1 Setup Test Accounts

```bash
# Get Gateway address
GW_ADDR=$(kubectl get gateway ${GATEWAY_NAME} -n ${GATEWAY_NAMESPACE} \
    -o jsonpath='{.status.addresses[0].value}')
export GW_URL="https://${GW_ADDR}"
```

```bash
# Create authorized SA with RBAC to access the InferencePool
kubectl create serviceaccount test-authorized-sa -n ${LLM_NS} 2>/dev/null || true
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: model-access
  namespace: ${LLM_NS}
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
  namespace: ${LLM_NS}
subjects:
- kind: ServiceAccount
  name: test-authorized-sa
  namespace: ${LLM_NS}
roleRef:
  kind: Role
  name: model-access
  apiGroup: rbac.authorization.k8s.io
EOF

AUTH_TOKEN=$(kubectl create token test-authorized-sa -n ${LLM_NS} \
    --audience=https://kubernetes.default.svc --duration=10m)

# Create unauthorized SA (no RBAC)
kubectl create serviceaccount test-unauthorized-sa -n ${LLM_NS} 2>/dev/null || true
UNAUTH_TOKEN=$(kubectl create token test-unauthorized-sa -n ${LLM_NS} \
    --audience=https://kubernetes.default.svc --duration=10m)
```

### 4.2 LLM Authentication

```bash
# Unauthenticated -> 401
curl -sk -o /dev/null -w "%{http_code}" \
    ${GW_URL}/${LLM_NS}/${MODEL_NAME}/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"'${MODEL_NAME}'","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'

# Authenticated -> 200
curl -sk -o /dev/null -w "%{http_code}" \
    ${GW_URL}/${LLM_NS}/${MODEL_NAME}/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -d '{"model":"'${MODEL_NAME}'","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'
```

### 4.3 LLM Authorization

```bash
# Unauthorized SA -> 403
curl -sk -o /dev/null -w "%{http_code}" \
    ${GW_URL}/${LLM_NS}/${MODEL_NAME}/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${UNAUTH_TOKEN}" \
    -d '{"model":"'${MODEL_NAME}'","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'

# Authorized SA -> 200
curl -sk -o /dev/null -w "%{http_code}" \
    ${GW_URL}/${LLM_NS}/${MODEL_NAME}/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -d '{"model":"'${MODEL_NAME}'","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'
```

### 4.4 LLM Token Rate Limit

```bash
# Send requests until 429 (token rate limit)
for i in $(seq 1 100); do
    http_code=$(curl -sk -o /dev/null -w '%{http_code}' \
        ${GW_URL}/${LLM_NS}/${MODEL_NAME}/v1/chat/completions \
        -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${AUTH_TOKEN}" \
        -d '{"model":"'${MODEL_NAME}'","messages":[{"role":"user","content":"Hello"}],"max_tokens":100}')
    if [ "$http_code" = "429" ]; then
        echo "Request $i: 429 Token Rate Limited"
        break
    fi
done
# Wait 60s for rate limit counters to reset
sleep 60
```

### 4.5 Batch Authentication

```bash
# Unauthenticated -> 401
curl -sk -o /dev/null -w "%{http_code}" ${GW_URL}/v1/batches

# Authenticated -> 200
curl -sk -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${AUTH_TOKEN}" ${GW_URL}/v1/batches
```

### 4.6 Batch Authorization (batch-llm-route enforcement)

```bash
# Unauthorized user creates a batch — batch is accepted (batch route has no authz),
# but the processor forwards requests to the batch-llm-route (via Internal Gateway)
# with the unauthorized token, and the batch-llm-route's AuthPolicy rejects with 403.

# Create input file
cat > /tmp/batch-input.jsonl <<EOF
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${MODEL_NAME}","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}}
EOF

FILE_ID=$(curl -sk ${GW_URL}/v1/files \
    -H "Authorization: Bearer ${UNAUTH_TOKEN}" \
    -F purpose=batch \
    -F "file=@/tmp/batch-input.jsonl" \
    | jq -r '.id')

BATCH_ID=$(curl -sk ${GW_URL}/v1/batches \
    -H "Authorization: Bearer ${UNAUTH_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"input_file_id":"'${FILE_ID}'","endpoint":"/v1/chat/completions","completion_window":"24h"}' \
    | jq -r '.id')

# Wait for processing, then check status — expect failed requests with 403
sleep 30
curl -sk ${GW_URL}/v1/batches/${BATCH_ID} \
    -H "Authorization: Bearer ${UNAUTH_TOKEN}" | jq '{status, request_counts}'
```

### 4.7 Batch Lifecycle

```bash
# Upload input file (reuse /tmp/batch-input.jsonl from 4.6, or create it)
FILE_ID=$(curl -sk ${GW_URL}/v1/files \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -F purpose=batch \
    -F "file=@/tmp/batch-input.jsonl" \
    | jq -r '.id')

# Create batch
BATCH_ID=$(curl -sk ${GW_URL}/v1/batches \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"input_file_id":"'${FILE_ID}'","endpoint":"/v1/chat/completions","completion_window":"24h"}' \
    | jq -r '.id')

# Wait for processing, then check status
sleep 30
curl -sk ${GW_URL}/v1/batches/${BATCH_ID} \
    -H "Authorization: Bearer ${AUTH_TOKEN}" | jq '.status'

# Download results (after status is "completed")
OUTPUT_FILE_ID=$(curl -sk ${GW_URL}/v1/batches/${BATCH_ID} \
    -H "Authorization: Bearer ${AUTH_TOKEN}" | jq -r '.output_file_id')

curl -sk ${GW_URL}/v1/files/${OUTPUT_FILE_ID}/content \
    -H "Authorization: Bearer ${AUTH_TOKEN}"
```

### 4.8 Batch Request Rate Limit

```bash
# Send 25 rapid requests — expect 429 after 20 (rate limit: 20 req/min)
for i in $(seq 1 25); do
    http_code=$(curl -sk -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${AUTH_TOKEN}" ${GW_URL}/v1/batches)
    echo "Request $i: $http_code"
done
```
