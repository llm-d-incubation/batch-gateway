# Batch Inference Integration with MaaS Platform

This doc demonstrates how to integrate batch inference with MaaS.

## 1. Architecture

```
User Request
    ↓  POST /v1/batches, /v1/files
    ↓
┌─── MaaS Gateway (openshift-ingress) ───────────────────────┐
│                                                             │
│  [AuthPolicy]       → Validate token & extract tier         │
│  [RateLimitPolicy]  → Enforce request limits per tier       │
│  [HTTPRoute]        → Route /v1/batches, /v1/files          │
│                                                             │
└─────────────────────────────────────────────────────────────┘
    ↓
Batch Inference
    ↓  POST /v1/chat/completions
    ↓
Gateway API Inference Extension (LLM-D)
    ↓
vLLM service
```

## 2. Integration Design

### 2.1 Authentication

Authentication is handled by creating a Kuadrant `AuthPolicy` attached to the batch inference `HTTPRoute`. The MaaS gateway validates user tokens before requests reach the batch inference service.

The AuthPolicy:
- **Validates user tokens** via Kubernetes TokenReview API (audiences: `https://kubernetes.default.svc`, `maas-default-gateway-sa`)
- **Looks up user tier** by calling the MaaS API (`/v1/tiers/lookup`) with the user's groups
- **Injects headers** (`X-Maas-Username`, `X-Maas-Group`) into requests forwarded to the batch inference service

See [Section 3.4 - Create Auth Policy](#34-create-auth-policy) for the full AuthPolicy manifest.

**If token validation fails**, the MaaS gateway rejects the request directly and returns `401 Unauthorized`. The request never reaches the batch inference service.

**If token validation succeeds**, the MaaS gateway forwards the request to the batch inference service with the following enriched headers:

| Header                          | Example Value |
|---------------------------------|---------------|
| `Authorization`                 | `Bearer eyJhbG...` |
| `X-Maas-Username`              | `system:serviceaccount:maas-default-gateway-tier-premium:testuser-45c571a1` |
| `X-Maas-Group`                 | `["system:serviceaccounts","system:serviceaccounts:maas-default-gateway-tier-premium","system:authenticated"]` |
| `X-Request-Id`                 | `a4d20d82-076f-4ed7-afd3-33306e391472` |

### 2.2 Authorization

MaaS has existing authorization mechanisms for model inference, but they cannot be directly reused for batch inference.

#### 2.2.1 How MaaS authorization works today

The `odh-model-controller` generates per-model AuthPolicies with Kubernetes `SubjectAccessReview`. It checks whether the user has `get` and `post` permissions on the `llminferenceservices` resource, with namespace and model name extracted from the request path:

```
Request path: /{namespace}/{model-name}/v1/chat/completions
                    ↓              ↓
          SubjectAccessReview:
            resource: llminferenceservices
            namespace: {namespace}
            name: {model-name}
            verb: get / post
```

The `odh-model-controller` automatically creates RBAC Roles and RoleBindings per tier (e.g., `system:serviceaccounts:maas-default-gateway-tier-premium`) to grant model access based on `LLMInferenceService` tier annotations.

#### 2.2.2 Why this doesn't work for batch inference

1. **Path-based authorization not applicable** — The batch gateway service itself cannot be deployed as an `LLMInferenceService` (see [Section 3.2](#32-install-batch-inference)), so the existing per-model AuthPolicy does not cover batch API routes
2. **URL path format mismatch** — MaaS model routes use `/{namespace}/{model-name}/v1/...`, but batch API paths are `/v1/batches` and `/v1/files`. The AuthPolicy cannot extract namespace or model name from these paths
3. **Model info not in URL** — In a batch request, the target model is specified inside the input file body (per-line in the JSONL), not in the URL path. The gateway-level AuthPolicy has no visibility into the request body or uploaded files

#### 2.2.3 Possible batch inference authorization approach

Since the gateway-level AuthPolicy cannot perform model-level authorization for batch requests (the model is inside the JSONL file body, not in the URL path), the batch gateway processor performs model-level authorization internally by leveraging the existing MaaS RBAC infrastructure.

```
1. User deploys vLLM model as `LLMInferenceService` with tier annotations, `odh-model-controller` automatically creates role and rolebinding

2. User creates batch job via MaaS gateway
   → AuthPolicy authenticates token, injects X-Maas-Username and X-Maas-Group headers
   → Batch apiserver stores username and groups with the batch job record

3. Batch processor picks up the job and parses the JSONL input file
   → For each unique model referenced in the JSONL lines, performs a Kubernetes SubjectAccessReview:

      SubjectAccessReview:
        user: <X-Maas-Username from batch job>
        groups: <X-Maas-Group from batch job>
        resourceAttributes:
          group: serving.kserve.io
          resource: llminferenceservices
          namespace: <model-namespace>
          name: <model-name>
          verb: post

   → If the user's group (e.g., system:serviceaccounts:maas-default-gateway-tier-premium)
     is bound to the model's Role via the RoleBinding, the SAR check passes

4. If any model fails the SAR check:
   → The batch job is marked as failed with an authorization error
   → No inference requests are dispatched for that batch
```

This approach reuses the existing MaaS RBAC infrastructure without requiring any changes to `odh-model-controller` or additional CRDs. The only requirement is that models used in batch inference are deployed as `LLMInferenceService` resources with appropriate tier annotations.

### 2.3 Rate Limiting

MaaS provides a default `TokenRateLimitPolicy` based on token usage, but [batch API](https://developers.openai.com/api/reference/resources/batches) responses don't include token usage (`usage.total_tokens`). Instead, a request-count-based `RateLimitPolicy` is used:

| Tier | Limit | Window |
|------|-------|--------|
| Enterprise | 100 requests | 1 minute |
| Premium | 15 requests | 1 minute |
| Free | 5 requests | 1 minute |

Rate limits are keyed by `auth.identity.userid` and skip `/v1/models` (discovery endpoint).

See [Section 3.5 - Create RateLimit Policy](#35-create-ratelimit-policy) for the full RateLimitPolicy manifest.

### 2.4 Monitoring

The batch processor propagates MaaS headers when dispatching individual inference requests to the downstream inference service (GAIE / LLM-D). This enables per-user metering and chargeback attribution.

```
Batch Inference (processor)
    ↓  POST /v1/chat/completions
    ↓  Headers:
    ↓    Authorization: Bearer <inference-api-key>
    ↓    X-Maas-Username: <original user from batch job>
    ↓    X-Maas-Group: <original groups from batch job>
    ↓    X-Request-Id: <generated per-request>
    ↓
Inference Service (GAIE / LLM-D)
```

The flow:
1. When a user creates a batch job via the MaaS gateway, the AuthPolicy injects `X-Maas-Username` and `X-Maas-Group` headers
2. The batch apiserver extracts these headers and stores the user context with the batch job record
3. When the batch processor dispatches individual inference requests, it reads the stored user context and propagates `X-Maas-Username` and `X-Maas-Group` to the downstream inference service
4. The inference service (GAIE / LLM-D) uses these headers for per-user metering and access control

## 3. Setup Instructions

<a id="31-install-maas-platform"></a>

### 3.1 Install MaaS Platform

Follow steps: https://github.com/opendatahub-io/models-as-a-service

After MaaS installation completes, the MaaS gateway has been created in openshift-ingress:

```bash
oc get gateway maas-default-gateway -n openshift-ingress
```

<a id="32-install-batch-inference"></a>

### 3.2 Install Batch Inference

Follow steps: https://github.com/llm-d-incubation/batch-gateway

Batch Inference:
- provides [OpenAI compatible batch API](https://developers.openai.com/api/docs/guides/batch)
- available endpoints: [/v1/files](https://developers.openai.com/api/reference/resources/files) and [/v1/batches](https://developers.openai.com/api/reference/resources/batches)
- batch inference cannot be deployed as `LLMInferenceService` resource using `kserve`

### 3.3 Create HTTP Route

Create HTTP route, which refers to MaaS gateway and redirects requests to batch inference:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: batch-inference-route
  labels:
    app: batch-inference
spec:
  parentRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: maas-default-gateway
    namespace: openshift-ingress
  rules:
  - backendRefs:
    - group: ""
      kind: Service
      name: batch-inference
      # TODO: change to https and actual port
      port: 80
      weight: 1
    matches:
    - path:
        type: PathPrefix
        value: /v1/files
  - backendRefs:
    - group: ""
      kind: Service
      name: batch-inference
      # TODO: change to https and actual port
      port: 80
      weight: 1
    matches:
    - path:
        type: PathPrefix
        value: /v1/batches
```

<a id="34-create-auth-policy"></a>

### 3.4 Create Auth Policy

With auth policy, the batch inference will be protected by MaaS authentication and tier-based controls.

The Authentication Policy will:
- **Validate user tokens** to ensure only authenticated users can access the batch inference API
- **Extract user name and groups** from the validated token
- **Lookup user tier** by calling the MaaS API with user groups to determine rate limit tier (enterprise, premium, or free)
- **Enrich original requests** with custom headers (`X-Maas-Username`, `X-Maas-Group`) and send to batch inference

```yaml
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-inference-auth
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-inference-route

  rules:
    # Authentication configuration
    authentication:
      kubernetes-user:
        kubernetesTokenReview:
          audiences:
          - https://kubernetes.default.svc
          - maas-default-gateway-sa
        overrides:
          fairness:
            value: "https://kubernetes.default.svc"
          objective:
            expression: "auth.identity.user.username.startsWith('system:serviceaccount:') ? auth.identity.user.username.split(':')[2] : 'authenticated'"

    # Metadata enrichment - lookup user tier from MaaS API
    metadata:
      matchedTier:
        http:
          # Send user's groups to MaaS tier lookup endpoint
          body:
            expression: '{ "groups": auth.identity.user.groups }'
          contentType: application/json
          method: POST
          url: https://maas-api.opendatahub.svc.cluster.local:8443/v1/tiers/lookup

    # Response handling on successful authentication
    response:
      success:
        filters:
          identity:
            json:
              properties:
                # Extract tier from metadata lookup response
                tier:
                  expression: auth.metadata.matchedTier["tier"]
                # Extract username from authenticated identity
                userid:
                  selector: auth.identity.user.username

        headers:
          X-Maas-Username:
            plain:
              selector: auth.identity.user.username
          X-Maas-Group:
            plain:
              selector: auth.identity.user.groups.@tostr
          # Flow control headers for Gateway API Inference Extension
          x-gateway-inference-fairness-id:
            metrics: false
            plain:
              expression: auth.identity.fairness
            priority: 0
          x-gateway-inference-objective:
            metrics: false
            plain:
              expression: auth.identity.objective
            priority: 0
```

<a id="35-create-ratelimit-policy"></a>

### 3.5 Create RateLimit Policy

MaaS provides a default `TokenRateLimitPolicy` based on token usage, but [batch API](https://developers.openai.com/api/reference/resources/batches) responses don't include token usage like:
```json
"usage": {
  "prompt_tokens": 1000,
  "completion_tokens": 1000,
  "total_tokens": 2000
}
```

So we need to create request-count-based `RateLimitPolicy` instead:
- **Enterprise tier**: 100 requests per minute
- **Premium tier**: 15 requests per minute
- **Free tier**: 5 requests per minute

```yaml
apiVersion: kuadrant.io/v1
kind: RateLimitPolicy
metadata:
  name: batch-inference-rate-limits
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-inference-route
  limits:
    enterprise-user-requests:
      counters:
      - expression: auth.identity.userid
      rates:
      - limit: 100
        window: 1m
      when:
      - predicate: auth.identity.tier == "enterprise" && !request.path.endsWith("/v1/models")
    premium-user-requests:
      counters:
      - expression: auth.identity.userid
      rates:
      - limit: 15
        window: 1m
      when:
      - predicate: auth.identity.tier == "premium" && !request.path.endsWith("/v1/models")
    free-user-requests:
      counters:
      - expression: auth.identity.userid
      rates:
      - limit: 5
        window: 1m
      when:
      - predicate: auth.identity.tier == "free" && !request.path.endsWith("/v1/models")
```

## 4. Testing

### 4.1 Setup Test User

The example creates test user `testuser` and adds to group `tier-premium-users`:

```bash
# Create the tier group
oc adm groups new tier-premium-users

# Create htpasswd secret with test user
htpasswd -c -B -b /tmp/htpasswd testuser testpass

# Create or update the htpasswd OAuth identity provider
oc create secret generic htpass-secret \
  --from-file=htpasswd=/tmp/htpasswd \
  -n openshift-config \
  --dry-run=client -o yaml | oc apply -f -

# Configure OAuth to use htpasswd (if not already configured)
# Check current OAuth configuration
oc get oauth cluster -o yaml

# If htpasswd provider is not configured, add it:
cat <<EOF | oc apply -f -
apiVersion: config.openshift.io/v1
kind: OAuth
metadata:
  name: cluster
spec:
  identityProviders:
  - name: htpasswd_provider
    mappingMethod: claim
    type: HTPasswd
    htpasswd:
      fileData:
        name: htpass-secret
EOF

# Add user to the premium tier group
oc adm groups add-users tier-premium-users testuser

# Verify the user and group
oc get users testuser
oc get groups tier-premium-users
```

### 4.2 Get MaaS Gateway Endpoint

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
HOST="https://maas.${CLUSTER_DOMAIN}"
echo "Gateway endpoint: $HOST"
```

### 4.3 Authenticate and Get Token from MaaS

Log in as test user and test the following:

```bash
# Login openshift as test user
oc login <server> -u testuser -p testpass

# Get openshift token for test user
oc_token=$(oc whoami -t)

# Get authenticated token from MaaS
TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${oc_token}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration": "10m"}' \
  "${HOST}/maas-api/v1/tokens")

TOKEN=$(echo $TOKEN_RESPONSE | jq -r .token)
echo "Token: ${TOKEN:0:20}..."
```

### 4.4 Test Batch Inference Endpoint

Batch request with token should succeed:
```bash
curl -sSk \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "${HOST}/v1/batches"
```

### 4.5 Test Authentication Enforcement

Batch request without token should fail with 401 Unauthorized:
```bash
curl -i -sSk \
  -H "Content-Type: application/json" \
  "${HOST}/v1/batches"
```

### 4.6 Verify MaaS Headers Received by Batch Inference

To verify that the MaaS gateway is correctly injecting authentication headers and that the batch inference service receives them, check the batch inference pod logs:

```bash
# Get batch inference pod logs
oc logs -l app=batch-inference -n batch-inference -f
```

When a request is sent through the MaaS gateway, the batch inference service should receive the following MaaS specific headers:

```
  Authorization: Bearer eyJhbG...
  X-Maas-Username: system:serviceaccount:maas-default-gateway-tier-premium:testuser-45c571a1
  X-Maas-Group: ["system:serviceaccounts","system:serviceaccounts:maas-default-gateway-tier-premium","system:authenticated"]
  X-Request-Id: a4d20d82-076f-4ed7-afd3-33306e391472
  X-Forwarded-For: 100.64.0.3
  X-Forwarded-Proto: https
  X-Envoy-External-Address: 100.64.0.3
  X-Envoy-Attempt-Count: 1
```

Key headers to verify:
- `X-Maas-Username` — should contain the authenticated user identity (e.g., `system:serviceaccount:maas-default-gateway-tier-premium:testuser-45c571a1`)
- `X-Maas-Group` — should contain the user's groups as a JSON array
- `Authorization` — should contain a valid MaaS-issued SA token

These are the same headers the batch processor will propagate to the downstream inference service (GAIE / LLM-D) for per-user metering and chargeback attribution. See [Section 2.4 - Monitoring](#24-monitoring) for the propagation flow.

### 4.7 Test Request-Count-Based Rate Limiting

For premium tier users, the rate limit is **15 requests per minute**.

Send requests to trigger rate limiting, should see HTTP 429 (Too Many Requests) after 15 requests:
```bash
for i in {1..20}; do
  http_code=$(curl -sSk -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    "${HOST}/v1/batches")
  echo "Request $i: $http_code"
done
```


## 5. References

### Cluster Resource Inventory

The following table lists all Gateway API, AuthPolicy, RateLimitPolicy, and TokenRateLimitPolicy resources in the cluster, along with their namespace and how they were created.

**Namespace: `openshift-ingress`**

| Kind                   | Name                       | Created By              |
|------------------------|----------------------------|-------------------------|
| Gateway                | `maas-default-gateway`     | MaaS deploy script      |
| Gateway                | `data-science-gateway`     | ODH operator            |
| HTTPRoute              | `oauth-callback-route`     | ODH operator            |
| AuthPolicy             | `gateway-auth-policy`      | ODH operator            |
| TokenRateLimitPolicy   | `gateway-token-rate-limits`| MaaS deploy script      |

**Namespace: `opendatahub`**

| Kind              | Name                       | Created By              |
|-------------------|----------------------------|-------------------------|
| HTTPRoute         | `maas-api-route`           | ODH operator            |
| AuthPolicy        | `maas-api-auth-policy`     | ODH operator            |

**Namespace: `batch-inference`**

| Kind              | Name                       | Created By              |
|-------------------|----------------------------|-------------------------|
| Deployment        | `batch-inference`          | User                    |
| HTTPRoute         | `batch-inference-route`    | User                    |
| AuthPolicy        | `batch-inference-auth`     | User                    |
| RateLimitPolicy   | `batch-inference-rate-limits` | User                |

### Links

- [Kuadrant RateLimitPolicy Documentation](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/ratelimitpolicy/)
- [Kuadrant AuthPolicy Documentation](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Gateway API HTTPRoute Specification](https://gateway-api.sigs.k8s.io/api-types/httproute/)
- [MaaS Platform Documentation](https://github.com/opendatahub-io/models-as-a-service)
