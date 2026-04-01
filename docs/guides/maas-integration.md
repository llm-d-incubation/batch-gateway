# MaaS Integration

This guide walks through deploying batch-gateway on the [MaaS (Models-as-a-Service)][MaaS] platform with authentication, request rate limiting, and token rate limiting.

[MaaS]: https://github.com/opendatahub-io/models-as-a-service

## Prerequisites

- OpenShift cluster (self-managed)
- [oc]
- [kubectl]
- [Helm]
- [kustomize]
- [jq]
- `htpasswd` (from `httpd-tools` or `apache2-utils`)
- Cluster admin access

[oc]: https://docs.openshift.com/container-platform/latest/cli_reference/openshift_cli/getting-started-cli.html
[kubectl]: https://kubernetes.io/docs/tasks/tools/
[Helm]: https://helm.sh
[kustomize]: https://kustomize.io
[jq]: https://jqlang.github.io/jq/

## Architecture

![MaaS Integration Architecture](diagrams/maas-integration-arch.png)

MaaS provides the gateway, Istio, Kuadrant, cert-manager, and model serving infrastructure. This guide adds batch-gateway on top.

All authentication uses **MaaS API keys** (`sk-oai-*`). Users create API keys via the MaaS API using their OpenShift token, then use API keys for both batch API and model inference requests.

## 1. Install MaaS Platform

Clone the MaaS repo and run the deploy script:

```bash
$ git clone https://github.com/opendatahub-io/models-as-a-service.git /tmp/maas
$ cd /tmp/maas
$ ./scripts/deploy.sh
```

This installs ODH operator, Kuadrant, cert-manager, Istio, MaaS API, MaaS controller, and PostgreSQL.

After installation, patch the maas-api RBAC to include the MaaS controller CRDs (the controller CRDs are installed after the operator creates the maas-api ServiceAccount):

```bash
$ kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: maas-api-extra
rules:
- apiGroups: ["maas.opendatahub.io"]
  resources: ["maassubscriptions", "maasmodelrefs", "maasauthpolicies"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
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
  namespace: opendatahub
roleRef:
  kind: ClusterRole
  name: maas-api-extra
  apiGroup: rbac.authorization.k8s.io
EOF

$ kubectl rollout restart deploy/maas-api -n opendatahub
$ kubectl rollout status deploy/maas-api -n opendatahub --timeout=120s
```

Create a self-signed ClusterIssuer for batch-gateway TLS certificates (MaaS installs the cert-manager operator but does not create a ClusterIssuer):

```bash
$ kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
EOF
```

Verify the gateway is ready:

```bash
$ kubectl get gateway maas-default-gateway -n openshift-ingress
```

## 2. Create Test User

Create the test user and group **before** deploying model policies (MaaSAuthPolicy references the group).

```bash
# Create htpasswd user
$ htpasswd -cbB /tmp/htpasswd testuser testpass
$ oc create secret generic htpass-secret \
    --from-file=htpasswd=/tmp/htpasswd \
    -n openshift-config \
    --dry-run=client -o yaml | oc apply -f -

# Add htpasswd identity provider
$ oc patch oauth cluster --type=merge -p '
spec:
  identityProviders:
  - name: htpasswd
    type: HTPasswd
    htpasswd:
      fileData:
        name: htpass-secret'

# Wait for OAuth restart
$ sleep 30

# Create group and add user
$ oc adm groups new tier-free-users
$ oc adm groups add-users tier-free-users testuser
```

## 3. Deploy a Sample Model

Deploy the simulator-based LLMInferenceService:

```bash
$ kubectl create namespace llm
$ kustomize build /tmp/maas/docs/samples/models/simulator | kubectl apply -f -
```

Wait for the model to be ready:

```bash
$ kubectl rollout status deploy/facebook-opt-125m-simulated-kserve -n llm --timeout=300s
$ oc wait llminferenceservice/facebook-opt-125m-simulated -n llm \
    --for=condition=Ready --timeout=300s
```

## 4. Deploy Batch Gateway

### 4.1 Create Namespace and Databases

```bash
$ kubectl create namespace batch-api

# Install Redis
$ helm repo add bitnami https://charts.bitnami.com/bitnami
$ helm install redis bitnami/redis -n batch-api \
    --set auth.enabled=false \
    --set replica.replicaCount=0 \
    --set master.persistence.enabled=false \
    --wait --timeout 120s

# Install PostgreSQL
$ helm install postgresql bitnami/postgresql -n batch-api \
    --set auth.postgresPassword=postgres \
    --set primary.persistence.enabled=false \
    --wait --timeout 120s
```

### 4.2 Create Secret and PVC

```bash
# App secret with database connection URLs
$ kubectl create secret generic batch-gateway-secrets -n batch-api \
    --from-literal=redis-url=redis://redis-master.batch-api.svc.cluster.local:6379/0 \
    --from-literal=postgresql-url=postgresql://postgres:postgres@postgresql.batch-api.svc.cluster.local:5432/postgres

# PVC for file storage
$ kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: batch-gateway-files
  namespace: batch-api
spec:
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
EOF
```

### 4.3 Install Batch Gateway via Helm

The processor calls models through the MaaS gateway using HTTPS. `passThroughHeaders={Authorization,X-MaaS-Subscription}` ensures the API key and subscription header are forwarded to the model for authentication and rate limiting.

```bash
$ helm install batch-gateway ./charts/batch-gateway -n batch-api \
    --set "global.secretName=batch-gateway-secrets" \
    --set "global.dbClient.type=postgresql" \
    --set "global.fileClient.type=fs" \
    --set "global.fileClient.fs.basePath=/tmp/batch-gateway" \
    --set "global.fileClient.fs.pvcName=batch-gateway-files" \
    --set 'processor.config.modelGateways.facebook/opt-125m.url=https://maas-default-gateway-istio.openshift-ingress.svc.cluster.local/llm/facebook-opt-125m-simulated' \
    --set 'processor.config.modelGateways.facebook/opt-125m.tlsInsecureSkipVerify=true' \
    --set 'processor.config.modelGateways.facebook/opt-125m.requestTimeout=5m' \
    --set 'processor.config.modelGateways.facebook/opt-125m.maxRetries=3' \
    --set 'processor.config.modelGateways.facebook/opt-125m.initialBackoff=1s' \
    --set 'processor.config.modelGateways.facebook/opt-125m.maxBackoff=60s' \
    --set "apiserver.config.batchAPI.passThroughHeaders={Authorization,X-MaaS-Subscription}" \
    --set "apiserver.tls.enabled=true" \
    --set "apiserver.tls.certManager.enabled=true" \
    --set "apiserver.tls.certManager.issuerName=selfsigned-issuer" \
    --set "apiserver.tls.certManager.issuerKind=ClusterIssuer" \
    --set "apiserver.tls.certManager.dnsNames={batch-gateway-apiserver,batch-gateway-apiserver.batch-api.svc.cluster.local,localhost}" \
    --set "apiserver.podSecurityContext=null" \
    --set "processor.podSecurityContext=null"
```

Verify:

```bash
$ kubectl rollout status deploy/batch-gateway-apiserver -n batch-api
$ kubectl rollout status deploy/batch-gateway-processor -n batch-api
```

### 4.4 Create DestinationRule

The MaaS gateway terminates TLS, then re-encrypts to the apiserver (which also has TLS via cert-manager):

```bash
$ kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: batch-gateway-backend-tls
  namespace: openshift-ingress
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

### 4.5 Create HTTPRoute

Route `/v1/batches` and `/v1/files` through the MaaS gateway to the batch-gateway apiserver:

```bash
$ kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-route
  namespace: batch-api
spec:
  parentRefs:
  - name: maas-default-gateway
    namespace: openshift-ingress
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

## 5. Create Policies

### 5.1 Batch AuthPolicy

The MaaS gateway has a default-deny AuthPolicy. Create a per-route AuthPolicy for the batch-route that validates MaaS API keys via the same HTTP callback that MaaS uses for model routes:

```bash
$ kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-auth
  namespace: batch-api
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-route
  rules:
    metadata:
      apiKeyValidation:
        http:
          url: "https://maas-api.opendatahub.svc.cluster.local:8443/internal/v1/api-keys/validate"
          contentType: application/json
          method: POST
          body:
            expression: '{"key": request.headers.authorization.replace("Bearer ", "")}'
        metrics: false
        priority: 0
    authentication:
      api-keys:
        plain:
          selector: request.headers.authorization
        metrics: false
        priority: 0
    authorization:
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
```

This AuthPolicy validates API keys by calling the MaaS API's `/internal/v1/api-keys/validate` endpoint — the same mechanism MaaS uses for model inference routes. Authorino sends the API key to maas-api, which checks the hash against PostgreSQL and returns `{valid, username, groups, keyId}`.

### 5.2 Batch RateLimitPolicy

Request-count-based rate limiting on the batch-route, keyed per user:

```bash
$ kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: RateLimitPolicy
metadata:
  name: batch-ratelimit
  namespace: batch-api
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
```

### 5.3 MaaS Model Policies

Register the model in MaaS and configure token-based rate limiting on the model inference route. The MaaS controller automatically generates a Kuadrant `AuthPolicy` (API key validation + group membership) and `TokenRateLimitPolicy` from these CRs.

> **Note**: MaaSAuthPolicy and MaaSSubscription must be in the `models-as-a-service` namespace (the namespace the MaaS controller watches).

```bash
$ kubectl create namespace models-as-a-service

# Register model in MaaS
$ kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: facebook-opt-125m-simulated
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: facebook-opt-125m-simulated
EOF

# Grant access to user group
$ kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: batch-model-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: facebook-opt-125m-simulated
      namespace: llm
  subjects:
    groups:
      - name: tier-free-users
EOF

# Token rate limit (500 tokens/min)
$ kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: batch-test-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: tier-free-users
  modelRefs:
    - name: facebook-opt-125m-simulated
      namespace: llm
      tokenRateLimits:
        - limit: 500
          window: 1m
EOF
```

Wait for the MaaS controller to reconcile:

```bash
# MaaSModelRef should be Ready
$ kubectl get maasmodelref -n llm
NAME                          PHASE   AGE
facebook-opt-125m-simulated   Ready   30s

# AuthPolicies should be Enforced
$ kubectl get authpolicy -A
NAMESPACE   NAME                                    ENFORCED
batch-api   batch-auth                              True
llm         maas-auth-facebook-opt-125m-simulated   True

# TokenRateLimitPolicy should be auto-generated
$ kubectl get tokenratelimitpolicy -n llm
NAME                                    AGE
maas-trlp-facebook-opt-125m-simulated   30s
```

## 6. Testing

### 6.1 Get Gateway Endpoint and API Key

```bash
$ HOST="https://maas.$(kubectl get ingresses.config.openshift.io cluster \
    -o jsonpath='{.spec.domain}')"

# Login as test user in a temporary kubeconfig (preserves admin session)
$ TEMP_KUBECONFIG=$(mktemp)
$ KUBECONFIG="$TEMP_KUBECONFIG" oc login "$(oc whoami --show-server)" \
    -u testuser -p testpass --insecure-skip-tls-verify
$ USER_TOKEN=$(KUBECONFIG="$TEMP_KUBECONFIG" oc whoami -t)
$ rm -f "$TEMP_KUBECONFIG"

# Create MaaS API key (used for all subsequent requests)
$ API_KEY=$(curl -sSk \
    -H "Authorization: Bearer ${USER_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST -d '{"name":"batch-test","expiration":"1h"}' \
    "${HOST}/maas-api/v1/api-keys" | jq -r .key)

$ echo "API key: ${API_KEY:0:20}..."
```

### 6.2 Test Batch API Authentication

```bash
# Without API key → 401
$ curl -sSk -o /dev/null -w "%{http_code}\n" "${HOST}/v1/batches"
401

# With API key → 200
$ curl -sSk -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer ${API_KEY}" "${HOST}/v1/batches"
200
```

### 6.3 Test Batch API Request Rate Limiting

Send 25 requests to trigger the 20 req/min limit:

```bash
$ for i in {1..25}; do
    curl -sSk -o /dev/null -w "Request $i: %{http_code}\n" \
      -H "Authorization: Bearer ${API_KEY}" "${HOST}/v1/batches"
    sleep 0.1
  done
```

Expected: first ~20 return `200`, then `429`.

### 6.4 Test Model Token Rate Limiting

Send inference requests to exhaust the 500 token budget:

```bash
# Wait for rate limit window to reset
$ sleep 60

$ for i in {1..15}; do
    curl -sSk -o /dev/null -w "Request $i: %{http_code}\n" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "X-MaaS-Subscription: batch-test-subscription" \
      -H "Content-Type: application/json" \
      -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":100}' \
      "${HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions"
    sleep 0.2
  done
```

Expected: a few `200`s followed by `429`s.

### 6.5 Test Batch E2E Lifecycle

Wait for rate limit windows to reset, then test the full batch lifecycle:

```bash
$ sleep 60

# Upload input file
$ cat > /tmp/batch-input.jsonl <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}}
JSONL

$ FILE_ID=$(curl -sSk -X POST "${HOST}/v1/files" \
    -H "Authorization: Bearer ${API_KEY}" \
    -F "purpose=batch" -F "file=@/tmp/batch-input.jsonl" | jq -r .id)

# Create batch
$ BATCH_ID=$(curl -sSk -X POST "${HOST}/v1/batches" \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "X-MaaS-Subscription: batch-test-subscription" \
    -H "Content-Type: application/json" \
    -d "{\"input_file_id\":\"${FILE_ID}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}" \
    | jq -r .id)

# Poll until completed
$ while true; do
    STATUS=$(curl -sSk "${HOST}/v1/batches/${BATCH_ID}" \
      -H "Authorization: Bearer ${API_KEY}" | jq -r .status)
    echo "Status: ${STATUS}"
    [[ "$STATUS" =~ ^(completed|failed|expired|cancelled)$ ]] && break
    sleep 5
  done
```

The batch completing proves that `passThroughHeaders` works: the processor forwarded the API key through the MaaS gateway to the model, passing the gateway's AuthPolicy on the model route.

## 7. How passThroughHeaders Works

```
1. User sends batch request with API key and subscription
   POST /v1/batches
   Authorization: Bearer sk-oai-xxx
   X-MaaS-Subscription: my-subscription

2. MaaS gateway validates API key (AuthPolicy on batch-route)
   → Authorino calls /internal/v1/api-keys/validate
   → Forwards request to batch-gateway-apiserver

3. Apiserver stores Authorization and X-MaaS-Subscription headers with the batch job

4. Processor dequeues the job, reads the stored headers
   → Sends inference request to model through MaaS gateway:
   POST /llm/<model>/v1/chat/completions
   Authorization: Bearer sk-oai-xxx
   X-MaaS-Subscription: my-subscription

5. MaaS gateway validates the same API key again (AuthPolicy on model route)
   → Authorino calls /internal/v1/api-keys/validate
   → Checks group membership via MaaSAuthPolicy
   → Applies token rate limit from the specified MaaSSubscription
   → Forwards to LLMInferenceService
```

The processor authenticates to the model using the **original user's API key**, not a service account. By also forwarding `X-MaaS-Subscription`, users with multiple subscriptions can specify which subscription's token budget to use. If omitted, MaaS auto-selects the subscription (only works when the user has exactly one).

## 8. Cleanup

```bash
# Batch gateway
$ kubectl delete ratelimitpolicy batch-ratelimit -n batch-api
$ kubectl delete authpolicy batch-auth -n batch-api
$ kubectl delete httproute batch-route -n batch-api
$ kubectl delete destinationrule batch-gateway-backend-tls -n openshift-ingress
$ helm uninstall batch-gateway -n batch-api
$ helm uninstall redis -n batch-api
$ helm uninstall postgresql -n batch-api
$ kubectl delete pvc batch-gateway-files -n batch-api
$ kubectl delete namespace batch-api

# MaaS model policies
$ kubectl delete maassubscription batch-test-subscription -n models-as-a-service
$ kubectl delete maasauthpolicy batch-model-access -n models-as-a-service
$ kubectl delete maasmodelref facebook-opt-125m-simulated -n llm
$ kubectl delete namespace llm

# RBAC patch and ClusterIssuer
$ kubectl delete clusterrolebinding maas-api-extra
$ kubectl delete clusterrole maas-api-extra
$ kubectl delete clusterissuer selfsigned-issuer

# Test user
$ oc delete group tier-free-users
$ oc delete user testuser
$ oc delete identity htpasswd:testuser

# MaaS platform (uses MaaS cleanup script)
$ /tmp/maas/.github/hack/cleanup-odh.sh --include-crds
```

## 9. Cluster Resource Inventory

**Namespace: `openshift-ingress`**

| Kind | Name | Created By |
|------|------|------------|
| Gateway | `maas-default-gateway` | MaaS deploy script |
| AuthPolicy | `gateway-auth-policy` | ODH operator |
| DestinationRule | `batch-gateway-backend-tls` | User |

**Namespace: `batch-api`**

| Kind | Name | Created By |
|------|------|------------|
| Deployment | `batch-gateway-apiserver` | Helm chart |
| Deployment | `batch-gateway-processor` | Helm chart |
| HTTPRoute | `batch-route` | User |
| AuthPolicy | `batch-auth` | User |
| RateLimitPolicy | `batch-ratelimit` | User |

**Namespace: `llm`**

| Kind | Name | Created By |
|------|------|------------|
| LLMInferenceService | `facebook-opt-125m-simulated` | User |
| MaaSModelRef | `facebook-opt-125m-simulated` | User |
| AuthPolicy | `maas-auth-facebook-opt-125m-simulated` | MaaS controller (auto) |
| TokenRateLimitPolicy | `maas-trlp-facebook-opt-125m-simulated` | MaaS controller (auto) |

**Namespace: `models-as-a-service`**

| Kind | Name | Created By |
|------|------|------------|
| MaaSAuthPolicy | `batch-model-access` | User |
| MaaSSubscription | `batch-test-subscription` | User |

## 10. References

- [MaaS Platform](https://github.com/opendatahub-io/models-as-a-service)
- [Kuadrant AuthPolicy](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Kuadrant RateLimitPolicy](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/ratelimitpolicy/)
- [Gateway API HTTPRoute](https://gateway-api.sigs.k8s.io/api-types/httproute/)
- [Batch API spec (OpenAI compatible)](https://developers.openai.com/api/reference/resources/batches)
