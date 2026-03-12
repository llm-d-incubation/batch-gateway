#!/bin/bash
set -euo pipefail

# ── User Token mode (OpenShift): authentication via user token, authorization via SubjectAccessReview ──
# Requires OpenShift cluster with htpasswd identity provider

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

# ── User Token Configuration ─────────────────────────────────────────────────
GOLD_USER="${GOLD_USER:-gold-user}"
GOLD_PASS="${GOLD_PASS:-gold-pass}"
FREE_USER="${FREE_USER:-free-user}"
FREE_PASS="${FREE_PASS:-free-pass}"
GOLD_GROUP="${GOLD_GROUP:-tier-gold}"
FREE_GROUP="${FREE_GROUP:-tier-free}"

# ── OpenShift Users & Groups ─────────────────────────────────────────────────

create_users_and_groups() {
    step "Creating OpenShift users and tier groups..."

    # Create htpasswd file
    local htpasswd_file
    htpasswd_file=$(mktemp)
    htpasswd -c -B -b "${htpasswd_file}" "${GOLD_USER}" "${GOLD_PASS}"
    htpasswd -B -b "${htpasswd_file}" "${FREE_USER}" "${FREE_PASS}"

    # Create or update htpasswd secret
    kubectl create secret generic htpass-secret \
        --from-file=htpasswd="${htpasswd_file}" \
        -n openshift-config \
        --dry-run=client -o yaml | kubectl apply -f -
    rm -f "${htpasswd_file}"

    # Ensure htpasswd identity provider is configured
    local has_htpasswd
    has_htpasswd=$(kubectl get oauth cluster -o jsonpath='{.spec.identityProviders[?(@.name=="htpasswd_provider")].name}' 2>/dev/null || true)
    if [ -z "$has_htpasswd" ]; then
        step "Adding htpasswd identity provider to OAuth..."
        kubectl get oauth cluster -o json | python3 -c "
import json, sys
oauth = json.load(sys.stdin)
providers = oauth.get('spec', {}).get('identityProviders', [])
providers.append({
    'name': 'htpasswd_provider',
    'mappingMethod': 'claim',
    'type': 'HTPasswd',
    'htpasswd': {'fileData': {'name': 'htpass-secret'}}
})
oauth.setdefault('spec', {})['identityProviders'] = providers
json.dump(oauth, sys.stdout)
" | kubectl apply -f -
        log "htpasswd identity provider added. Waiting for OAuth server to restart..."
        sleep 30
    else
        log "htpasswd identity provider already configured."
    fi

    # Create OpenShift groups
    for group in "${GOLD_GROUP}" "${FREE_GROUP}"; do
        if ! oc get group "${group}" &>/dev/null; then
            oc adm groups new "${group}"
            log "Created group '${group}'."
        fi
    done

    # Add users to groups
    oc adm groups add-users "${GOLD_GROUP}" "${GOLD_USER}" 2>/dev/null || true
    oc adm groups add-users "${FREE_GROUP}" "${FREE_USER}" 2>/dev/null || true

    log "Users and groups created."
    log "  ${GOLD_USER} (password: ${GOLD_PASS}) -> group: ${GOLD_GROUP}"
    log "  ${FREE_USER} (password: ${FREE_PASS}) -> group: ${FREE_GROUP}"
}

create_model_rbac() {
    step "Creating per-model RBAC (Role + RoleBinding)..."

    # RBAC uses InferencePool resources for SubjectAccessReview
    # InferencePool names match model names (free-model, gold-model)
    # RoleBinding subjects use OpenShift group names (tier-gold, tier-free)
    kubectl apply -f - <<EOF
# Role: allows GET inferencepools/free-model (both tiers)
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${FREE_MODEL}-access
  namespace: ${LLM_NAMESPACE}
rules:
- apiGroups: ["inference.networking.k8s.io"]
  resources: ["inferencepools"]
  resourceNames: ["${FREE_MODEL}"]
  verbs: ["get"]
---
# RoleBinding: grant free-model access to both tiers
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${FREE_MODEL}-tier-binding
  namespace: ${LLM_NAMESPACE}
subjects:
- kind: Group
  name: ${GOLD_GROUP}
  apiGroup: rbac.authorization.k8s.io
- kind: Group
  name: ${FREE_GROUP}
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role
  name: ${FREE_MODEL}-access
  apiGroup: rbac.authorization.k8s.io
---
# Role: allows GET inferencepools/gold-model (gold tier only)
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${GOLD_MODEL}-access
  namespace: ${LLM_NAMESPACE}
rules:
- apiGroups: ["inference.networking.k8s.io"]
  resources: ["inferencepools"]
  resourceNames: ["${GOLD_MODEL}"]
  verbs: ["get"]
---
# RoleBinding: grant gold-model access to gold tier only
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${GOLD_MODEL}-tier-binding
  namespace: ${LLM_NAMESPACE}
subjects:
- kind: Group
  name: ${GOLD_GROUP}
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role
  name: ${GOLD_MODEL}-access
  apiGroup: rbac.authorization.k8s.io
EOF

    log "RBAC created."
    log "  ${FREE_MODEL}: ${GOLD_GROUP} + ${FREE_GROUP}"
    log "  ${GOLD_MODEL}: ${GOLD_GROUP} only"
}

get_user_token() {
    local user="$1"
    local pass="$2"
    # Save current context
    local prev_context
    prev_context=$(kubectl config current-context 2>/dev/null)
    # Login as user and get token
    oc login -u "${user}" -p "${pass}" --insecure-skip-tls-verify=true >/dev/null 2>&1 || return 1
    local token
    token=$(oc whoami -t 2>/dev/null) || return 1
    # Switch back to previous context
    kubectl config use-context "${prev_context}" >/dev/null 2>&1 || true
    echo "${token}"
}

# ── Kuadrant Policies (User Token mode) ──────────────────────────────────────

apply_batch_auth_policy() {
    step "Applying batch-auth AuthPolicy (User token authentication)..."

    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: batch-auth
  namespace: ${BATCH_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: batch-route
  rules:
    authentication:
      "k8s-token":
        kubernetesTokenReview:
          audiences:
          - https://kubernetes.default.svc
    # Assumes user is in a tier-* OpenShift group (e.g. tier-gold, tier-free).
    # If user is not in a tier-* group, filter returns empty and [0] will error.
    response:
      success:
        filters:
          "identity":
            json:
              properties:
                "tier":
                  expression: |
                    auth.identity.user.groups
                      .filter(g, g.startsWith("tier-"))
                      .map(g, g.replace("tier-", ""))
                      [0]
        headers:
          "x-tier":
            plain:
              expression: |
                auth.identity.user.groups
                  .filter(g, g.startsWith("tier-"))
                  .map(g, g.replace("tier-", ""))
                  [0]
          "x-username":
            plain:
              selector: auth.identity.user.username
          "x-group":
            plain:
              selector: auth.identity.user.groups.@tostr
EOF

    log "batch-auth applied (User token authentication, tier from OpenShift group)."
}

apply_llm_auth_policy() {
    step "Applying llm-auth AuthPolicy (User token + SubjectAccessReview)..."

    kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: llm-auth
  namespace: ${LLM_NAMESPACE}
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  rules:
    authentication:
      "k8s-token":
        kubernetesTokenReview:
          audiences:
          - https://kubernetes.default.svc
    authorization:
      "model-access":
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
    response:
      success:
        filters:
          "identity":
            json:
              properties:
                "tier":
                  expression: |
                    auth.identity.user.groups
                      .filter(g, g.startsWith("tier-"))
                      .map(g, g.replace("tier-", ""))
                      [0]
        headers:
          "x-tier":
            plain:
              expression: |
                auth.identity.user.groups
                  .filter(g, g.startsWith("tier-"))
                  .map(g, g.replace("tier-", ""))
                  [0]
          "x-username":
            plain:
              selector: auth.identity.user.username
          "x-group":
            plain:
              selector: auth.identity.user.groups.@tostr
EOF

    log "llm-auth applied (User token + SubjectAccessReview, targets llm-route)."
}

cleanup_auth_resources() {
    # Delete OpenShift groups
    for group in "${GOLD_GROUP}" "${FREE_GROUP}"; do
        oc delete group "${group}" 2>/dev/null || true
    done
    # Delete users
    for user in "${GOLD_USER}" "${FREE_USER}"; do
        oc delete user "${user}" 2>/dev/null || true
        oc delete identity "htpasswd_provider:${user}" 2>/dev/null || true
    done
    timeout_delete 15s role "${FREE_MODEL}-access" "${GOLD_MODEL}-access" -n "${LLM_NAMESPACE}" || true
    timeout_delete 15s rolebinding "${FREE_MODEL}-tier-binding" "${GOLD_MODEL}-tier-binding" -n "${LLM_NAMESPACE}" || true
}

# ── Install ───────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   Kuadrant + GAIE Setup (User Token mode)            ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

    # Check OpenShift and python3
    if ! command -v oc &>/dev/null; then
        die "This script requires OpenShift (oc command). Use deploy-with-kuadrant-satoken.sh for standard K8s."
    fi
    if ! command -v python3 &>/dev/null; then
        die "This script requires python3 for JSON manipulation."
    fi

    install_with_kuadrant
    create_users_and_groups
    create_model_rbac
    apply_batch_auth_policy
    apply_llm_auth_policy
    wait_for_auth_policies
    start_gateway_port_forward

    log "Deployment complete! Run '$0 test' to verify."
}

# ── Test ──────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "User Token"

    step "Getting user tokens..."
    local GOLD_TOKEN FREE_TOKEN

    GOLD_TOKEN=$(get_user_token "${GOLD_USER}" "${GOLD_PASS}") \
        || die "Failed to get token for ${GOLD_USER}. Run '$0 install' first."
    FREE_TOKEN=$(get_user_token "${FREE_USER}" "${FREE_PASS}") \
        || die "Failed to get token for ${FREE_USER}. Run '$0 install' first."

    if [ -z "${GOLD_TOKEN}" ] || [ -z "${FREE_TOKEN}" ]; then
        die "Failed to get user tokens. Users may not be ready yet (OAuth restart takes ~60s)."
    fi
    log "Tokens obtained (gold: ${GOLD_TOKEN:0:20}..., free: ${FREE_TOKEN:0:20}...)."

    local base_url="https://localhost:${LOCAL_PORT}"
    local free_payload='{"model":"'"${FREE_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local gold_payload='{"model":"'"${GOLD_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local test_payload="${free_payload}"
    local test_total=0
    local test_passed=0
    local test_failed=0
    local failed_tests=""

    pass_test() { test_total=$((test_total + 1)); test_passed=$((test_passed + 1)); log "PASSED: $*"; }
    fail_test() { test_total=$((test_total + 1)); test_failed=$((test_failed + 1)); failed_tests="${failed_tests}\n  - $*"; warn "FAILED: $*"; }

    local response http_code body

    # ── LLM Route Tests ───────────────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  LLM Route Tests (llm-route)"
    echo "  - Authentication (TokenReview), Authorization (SubjectAccessReview), Rate Limiting (token-based)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 1: No token -> 401
    echo ""
    echo "── Test 1: LLM Authentication - No token ──"
    echo "  Route: llm-route | Tier: none | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify auth rejects unauthenticated requests (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 1: 401 Unauthorized"
    else
        fail_test "Test 1: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 2: Invalid token -> 401
    echo ""
    echo "── Test 2: LLM Authentication - Invalid token ──"
    echo "  Route: llm-route | Tier: none | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify invalid token is rejected (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer invalid-token-99999" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 2: 401 Unauthorized (invalid token rejected)"
    else
        fail_test "Test 2: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 3: Valid token -> 200
    echo ""
    echo "── Test 3: LLM Authentication - With valid user token ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify authenticated user request succeeds (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 3: 200 OK"
        echo "  Response: $body"
    else
        fail_test "Test 3: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 4: Free user accessing gold-model -> 403
    echo ""
    echo "── Test 4: LLM Authorization - Free user accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: free | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify SubjectAccessReview denies free user access to ${GOLD_MODEL} (expect 403)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${FREE_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "403" ]; then
        pass_test "Test 4: 403 Forbidden (free user denied access to ${GOLD_MODEL})"
    else
        fail_test "Test 4: Expected 403, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 5: Gold user accessing gold-model -> 200
    echo ""
    echo "── Test 5: LLM Authorization - Gold user accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify SubjectAccessReview allows gold user access to ${GOLD_MODEL} (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 5: 200 OK (gold user authorized for ${GOLD_MODEL})"
        echo "  Response: $body"
    else
        fail_test "Test 5: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 6: Free user token rate limiting
    echo ""
    echo "── Test 6: LLM Rate Limiting - Free user token-based ──"
    echo "  Route: llm-route | Tier: free | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify token rate limit triggers 429 after budget exhausted (150 tokens/min)"
    local llm_success=0
    local llm_limited=0

    for i in $(seq 1 10); do
        http_code=$(curl -sk -o /dev/null -w "%{http_code}" \
            -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
            -H "Authorization: Bearer ${FREE_TOKEN}" \
            -H 'Content-Type: application/json' \
            -d "${test_payload}")
        if [ "$http_code" = "200" ]; then
            llm_success=$((llm_success + 1))
            echo "  Request $i: 200 OK"
        elif [ "$http_code" = "429" ]; then
            llm_limited=$((llm_limited + 1))
            echo "  Request $i: 429 Rate Limited"
        else
            echo "  Request $i: $http_code (unexpected)"
        fi
        sleep 0.2
    done

    echo ""
    log "LLM free user: $llm_success successful, $llm_limited rate-limited"
    if [ "$llm_limited" -ge 3 ]; then
        pass_test "Test 6: Token rate limiting is working"
    else
        fail_test "Test 6: No rate limiting triggered"
    fi

    sleep 1

    # Test 7: Gold user after free exhausted -> 200
    echo ""
    echo "── Test 7: LLM Rate Limiting - Gold user after free user exhausted ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify gold user is unaffected by free user token rate limit (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 7: 200 OK (gold user unaffected by free user limit)"
        echo "  Response: $body"
    else
        fail_test "Test 7: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    # ── Batch Route Tests ─────────────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Batch Route Tests (batch-route)"
    echo "  - Authentication (TokenReview), Rate Limiting (request-based)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 8: No token -> 401
    echo ""
    echo "── Test 8: Batch Authentication - No token ──"
    echo "  Route: batch-route | Tier: none | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify auth rejects unauthenticated requests (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 8: 401 Unauthorized"
    else
        fail_test "Test 8: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 9: Invalid token -> 401
    echo ""
    echo "── Test 9: Batch Authentication - Invalid token ──"
    echo "  Route: batch-route | Tier: none | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify invalid token is rejected (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: Bearer invalid-token-99999")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 9: 401 Unauthorized (invalid token rejected)"
    else
        fail_test "Test 9: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 10: Valid token -> 200
    echo ""
    echo "── Test 10: Batch Authentication - With valid user token ──"
    echo "  Route: batch-route | Tier: gold | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify authenticated user request succeeds (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: Bearer ${GOLD_TOKEN}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 10: 200 OK"
    else
        fail_test "Test 10: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 11: Free user request rate limiting (GET)
    echo ""
    echo "── Test 11: Batch Rate Limiting - Free user request-based ──"
    echo "  Route: batch-route | Tier: free | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify request rate limit triggers 429 after 5 req/10s"
    local free_success=0
    local free_limited=0

    for i in $(seq 1 8); do
        http_code=$(curl -sk -o /dev/null -w "%{http_code}" \
            -X GET "${base_url}/v1/batches" \
            -H "Authorization: Bearer ${FREE_TOKEN}")
        if [ "$http_code" = "429" ]; then
            free_limited=$((free_limited + 1))
            echo "  Request $i: 429 Rate Limited"
        else
            free_success=$((free_success + 1))
            echo "  Request $i: $http_code"
        fi
        sleep 0.1
    done

    echo ""
    log "Free user: $free_success passed, $free_limited rate-limited"
    if [ "$free_limited" -ge 3 ]; then
        pass_test "Test 11: Request rate limiting is working"
    else
        fail_test "Test 11: Free user should have been rate-limited"
    fi

    sleep 1

    # Test 12: Gold user after free rate-limited -> 200
    echo ""
    echo "── Test 12: Batch Rate Limiting - Gold user after free user rate-limited ──"
    echo "  Route: batch-route | Tier: gold | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify gold user is unaffected by free user request rate limit (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: Bearer ${GOLD_TOKEN}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 12: 200 OK (gold user unaffected by free user rate limit)"
    else
        fail_test "Test 12: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    # ── E2E Batch Lifecycle Tests ─────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  E2E Batch Lifecycle Tests"
    echo "  - Flow: upload file -> create batch -> processor -> llm-route -> vllm-sim"
    echo "  - Proves: passThroughHeaders forwards Authorization to llm-route"
    echo "═══════════════════════════════════════════════════════════════"

    sleep 1

    # Test 13: Upload file + create batch
    echo ""
    echo "── Test 13: E2E - Upload file and create batch ──"
    echo "  Goal: Upload JSONL file, create batch job with gold user auth"
    local input_file="/tmp/batch-test-input-$$.jsonl"
    cat > "${input_file}" <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${FREE_MODEL}","messages":[{"role":"user","content":"Hello from kuadrant"}]}}
JSONL
    response=$(curl -sk -w "\n%{http_code}" -X POST "${base_url}/v1/files" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -F "purpose=batch" -F "file=@${input_file}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    rm -f "${input_file}"
    local file_id=""
    local batch_id=""
    if [ "$http_code" = "200" ]; then
        file_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
        echo "  File uploaded: ${file_id}"
    else
        fail_test "Test 13: File upload failed (HTTP $http_code)"
        echo "  Response: $body"
    fi

    if [ -n "$file_id" ]; then
        response=$(curl -sk -w "\n%{http_code}" -X POST "${base_url}/v1/batches" \
            -H "Authorization: Bearer ${GOLD_TOKEN}" \
            -H 'Content-Type: application/json' \
            -d "{\"input_file_id\":\"${file_id}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")
        http_code=$(echo "$response" | tail -n1)
        body=$(echo "$response" | sed '$ d')
        if [ "$http_code" = "200" ]; then
            batch_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
            pass_test "Test 13: File uploaded + batch created (batch: ${batch_id})"
        else
            fail_test "Test 13: Batch creation failed (HTTP $http_code)"
            echo "  Response: $body"
        fi
    fi

    # Test 14: Poll batch to completion (proves passThroughHeaders works)
    echo ""
    echo "── Test 14: E2E - Batch completion (passThroughHeaders verification) ──"
    echo "  Goal: Verify batch completes (processor forwards auth header to llm-route)"
    if [ -n "$batch_id" ]; then
        local status="unknown" poll_count=0
        while [ "$poll_count" -lt 60 ]; do
            response=$(curl -sk "${base_url}/v1/batches/${batch_id}" \
                -H "Authorization: Bearer ${GOLD_TOKEN}")
            status=$(echo "$response" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
            echo "  Poll $((poll_count+1)): status=${status}"
            case "$status" in completed|failed|expired|cancelled) break ;; esac
            poll_count=$((poll_count + 1))
            sleep 5
        done
        if [ "$status" = "completed" ]; then
            pass_test "Test 14: Batch completed (passThroughHeaders working)"
        else
            fail_test "Test 14: Batch ended with status=${status} (expected completed)"
        fi
    else
        fail_test "Test 14: Skipped (no batch_id from Test 13)"
    fi

    print_test_summary "$test_total" "$test_passed" "$test_failed" "$failed_tests"
}

# ── Main ──────────────────────────────────────────────────────────────────────

if [ $# -eq 0 ]; then
    usage 0
fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    test)      shift; cmd_test "$@" ;;
    uninstall) shift; cmd_uninstall "$@" ;;
    help|-h|--help) usage 0 ;;
    *) echo "Error: Unknown command '$1'"; echo ""; usage 1 ;;
esac
