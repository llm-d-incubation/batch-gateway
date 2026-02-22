# Batch Gateway Integration with MaaS Platform

This doc demonstrates how to integrate batch gateway with MaaS.

## 1. Architecture

```
User Request (/v1/batches)
    ↓
MaaS Gateway (openshift-ingress)
    ↓
[AuthPolicy] → Validate token & extract tier from user groups
    ↓
[RateLimitPolicy] → Enforce request limits per tier
    ↓
[HTTPRoute] → Route to batch gateway service
    ↓
Batch Gateway
```

## 2. Setup Instructions

### 2.1 Installation Steps

1. **Install MaaS Platform**

   Follow steps: https://github.com/opendatahub-io/models-as-a-service

   After MaaS installation completes, the MaaS gateway has been created in openshift-ingress:

   ```bash
   oc get gateway maas-default-gateway -n openshift-ingress
   ```

2. **Install Batch Gateway**

   Follow steps: https://github.com/llm-d-incubation/batch-gateway

   Batch gateway:
   - provides [OpenAI compatible batch API](https://developers.openai.com/api/docs/guides/batch)
   - available endpoints: [/v1/files](https://developers.openai.com/api/reference/resources/files) and [/v1/batches](https://developers.openai.com/api/reference/resources/batches)
   - batch gateway **can't** be deployed as `LLMInferenceService` resource using `kserve`

3. **Create HTTP Route**

   Create HTTP route, which refers to MaaS gateway and redirects requests to batch gateway:
   ```yaml
   apiVersion: gateway.networking.k8s.io/v1
   kind: HTTPRoute
   metadata:
     name: batch-gateway-route
     labels:
       app: batch-gateway
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
         name: batch-gateway
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
         name: batch-gateway
         # TODO: change to https and actual port
         port: 80
         weight: 1
       matches:
       - path:
           type: PathPrefix
           value: /v1/batches
   ```

4. **Create Auth Policy**

   With auth policy, the batch gateway will be protected by MaaS authentication and tier-based controls.

   The Authentication Policy will:
   - **Validate user tokens** to ensure only authenticated users can access the batch gateway API
   - **Extract user name and groups** from the validated token
   - **Lookup user tier** by calling the MaaS API with user groups to determine rate limit tier (enterprise, premium, or free)
   - **Enrich original requests** with custom headers (`X-MaaS-Username`, `X-MaaS-Group`) and send to batch gateway

   ```yaml
   apiVersion: kuadrant.io/v1
   kind: AuthPolicy
   metadata:
     name: batch-gateway-auth
   spec:
     targetRef:
       group: gateway.networking.k8s.io
       kind: HTTPRoute
       name: batch-gateway-route

     rules:
       # Authentication configuration
       authentication:
         # Validate OpenShift user identities using Kubernetes TokenReview API
         openshift-identities:
           kubernetesTokenReview:
             # Expected token audiences for validation
             audiences:
             - https://kubernetes.default.svc
             - maas-default-gateway-sa
           credentials: {}

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

           # Add custom headers to backend services
           headers:
             X-MaaS-Username:
               plain:
                 selector: auth.identity.user.username
             X-MaaS-Group:
               plain:
                 selector: auth.identity.user.groups.@tostr
   ```

5. **Create RateLimit Policy**

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
     name: gateway-rate-limits
     namespace: openshift-ingress
   spec:
     targetRef:
       group: gateway.networking.k8s.io
       kind: Gateway
       name: maas-default-gateway
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


## 3. Testing

### 3.1 Setup Test User

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

### 3.2 Get MaaS Gateway Endpoint

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
HOST="https://maas.${CLUSTER_DOMAIN}"
echo "Gateway endpoint: $HOST"
```

### 3.3 Authenticate and Get Token from MaaS

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

### 3.4 Test Batch Gateway Endpoint

Batch request with token should succeed:
```bash
curl -sSk \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "${HOST}/v1/batches"
```

### 3.5 Test Authorization Enforcement

Batch request without token should fail with 401 Unauthorized:
```bash
curl -i -sSk \
  -H "Content-Type: application/json" \
  "${HOST}/v1/batches"
```

### 3.6 Test Request-Count-Based Rate Limiting

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


## 4. References

- [Kuadrant RateLimitPolicy Documentation](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/ratelimitpolicy/)
- [Kuadrant AuthPolicy Documentation](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Gateway API HTTPRoute Specification](https://gateway-api.sigs.k8s.io/api-types/httproute/)
- [MaaS Platform Documentation](https://github.com/opendatahub-io/models-as-a-service)
