#!/bin/bash
set -euo pipefail

# ── Token mode: authentication via K8s token, authorization via SubjectAccessReview ──

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

# ── Token Configuration ───────────────────────────────────────────────────────
TIER_GOLD_NS="${TIER_GOLD_NS:-tier-gold}"
TIER_FREE_NS="${TIER_FREE_NS:-tier-free}"
GOLD_SA="${GOLD_SA:-gold-user}"
FREE_SA="${FREE_SA:-free-user}"

# ── Service Accounts & RBAC ───────────────────────────────────────────────────

create_service_accounts() {
    step "Creating tier namespaces and service accounts..."

    for ns in "${TIER_GOLD_NS}" "${TIER_FREE_NS}"; do
        if ! kubectl get namespace "${ns}" &>/dev/null; then
            kubectl create namespace "${ns}"
            log "Created namespace '${ns}'."
        fi
    done

    # Create service accounts
    kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${GOLD_SA}
  namespace: ${TIER_GOLD_NS}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${FREE_SA}
  namespace: ${TIER_FREE_NS}
EOF

    log "Service accounts created."
    log "  gold: ${TIER_GOLD_NS}/${GOLD_SA} (group: system:serviceaccounts:${TIER_GOLD_NS})"
    log "  free: ${TIER_FREE_NS}/${FREE_SA} (group: system:serviceaccounts:${TIER_FREE_NS})"
}

create_model_rbac() {
    step "Creating per-model RBAC (Role + RoleBinding)..."

    # RBAC uses InferencePool resources for SubjectAccessReview
    # InferencePool names match model names (free-model, gold-model)
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
  name: system:serviceaccounts:${TIER_GOLD_NS}
  apiGroup: rbac.authorization.k8s.io
- kind: Group
  name: system:serviceaccounts:${TIER_FREE_NS}
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
# RoleBinding: grant ${GOLD_MODEL} access to gold tier only
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${GOLD_MODEL}-tier-binding
  namespace: ${LLM_NAMESPACE}
subjects:
- kind: Group
  name: system:serviceaccounts:${TIER_GOLD_NS}
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role
  name: ${GOLD_MODEL}-access
  apiGroup: rbac.authorization.k8s.io
EOF

    log "RBAC created."
    log "  ${FREE_MODEL}: gold + free"
    log "  ${GOLD_MODEL}: gold only"
}

get_sa_token() {
    local ns="$1"
    local sa="$2"
    kubectl create token "${sa}" -n "${ns}" --duration=1h \
        --audience=https://kubernetes.default.svc 2>/dev/null
}

# ── Kuadrant Policies (Token mode) ───────────────────────────────────────────

apply_batch_auth_policy() {
    step "Applying batch-auth AuthPolicy (K8s token authentication)..."

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
    # Assumes SA is in a tier-* namespace (e.g. tier-gold, tier-free).
    # If SA is not in a tier-* namespace, filter returns empty and [0] will error.
    response:
      success:
        filters:
          "identity":
            json:
              properties:
                "tier":
                  expression: |
                    auth.identity.user.groups
                      .filter(g, g.startsWith("system:serviceaccounts:tier-"))
                      .map(g, g.replace("system:serviceaccounts:tier-", ""))
                      [0]
        headers:
          "x-tier":
            plain:
              expression: |
                auth.identity.user.groups
                  .filter(g, g.startsWith("system:serviceaccounts:tier-"))
                  .map(g, g.replace("system:serviceaccounts:tier-", ""))
                  [0]
          "x-username":
            plain:
              selector: auth.identity.user.username
          "x-group":
            plain:
              selector: auth.identity.user.groups.@tostr
EOF

    log "batch-auth applied (K8s token authentication, tier from SA namespace)."
}

apply_llm_auth_policy() {
    step "Applying llm-auth AuthPolicy (K8s token + SubjectAccessReview)..."

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
                      .filter(g, g.startsWith("system:serviceaccounts:tier-"))
                      .map(g, g.replace("system:serviceaccounts:tier-", ""))
                      [0]
        headers:
          "x-tier":
            plain:
              expression: |
                auth.identity.user.groups
                  .filter(g, g.startsWith("system:serviceaccounts:tier-"))
                  .map(g, g.replace("system:serviceaccounts:tier-", ""))
                  [0]
          "x-username":
            plain:
              selector: auth.identity.user.username
          "x-group":
            plain:
              selector: auth.identity.user.groups.@tostr
EOF

    log "llm-auth applied (K8s token + SubjectAccessReview, targets llm-route)."
}

cleanup_auth_resources() {
    for ns in "${TIER_GOLD_NS}" "${TIER_FREE_NS}"; do
        if [ "${ns}" != "default" ]; then
            force_delete_namespace "${ns}"
        fi
    done
    timeout_delete 15s role "${FREE_MODEL}-access" "${GOLD_MODEL}-access" -n "${LLM_NAMESPACE}" || true
    timeout_delete 15s rolebinding "${FREE_MODEL}-tier-binding" "${GOLD_MODEL}-tier-binding" -n "${LLM_NAMESPACE}" || true
}

# ── Install ───────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   Kuadrant + GAIE Setup (Token mode)                  ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

    install_with_kuadrant
    create_service_accounts
    create_model_rbac
    apply_batch_auth_policy
    apply_llm_auth_policy
    wait_for_auth_policies
    start_gateway_port_forward

    log "Deployment complete! Run '$0 test' to verify."
}

# ── Test ──────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "Token"

    step "Generating SA tokens..."
    local GOLD_TOKEN FREE_TOKEN
    GOLD_TOKEN=$(get_sa_token "${TIER_GOLD_NS}" "${GOLD_SA}") \
        || die "Failed to create token for ${TIER_GOLD_NS}/${GOLD_SA}. Run '$0 install' first."
    FREE_TOKEN=$(get_sa_token "${TIER_FREE_NS}" "${FREE_SA}") \
        || die "Failed to create token for ${TIER_FREE_NS}/${FREE_SA}. Run '$0 install' first."
    log "Tokens generated (gold: ${GOLD_TOKEN:0:20}..., free: ${FREE_TOKEN:0:20}...)."

    local base_url="https://localhost:${LOCAL_PORT}"
    local free_payload='{"model":"'"${FREE_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local gold_payload='{"model":"'"${GOLD_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local test_payload="${free_payload}"  # default payload uses free-model
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
    echo "── Test 3: LLM Authentication - With valid token ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify authenticated request succeeds (expect 200)"
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

    # Test 4: Free tier accessing ${GOLD_MODEL} -> 403 (SubjectAccessReview denies)
    echo ""
    echo "── Test 4: LLM Authorization - Free tier accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: free | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify SubjectAccessReview denies free tier access to ${GOLD_MODEL} (expect 403)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${FREE_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "403" ]; then
        pass_test "Test 4: 403 Forbidden (SubjectAccessReview denied free tier)"
    else
        fail_test "Test 4: Expected 403, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 5: Gold tier accessing ${GOLD_MODEL} -> authorized
    echo ""
    echo "── Test 5: LLM Authorization - Gold tier accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify SubjectAccessReview allows gold tier access to ${GOLD_MODEL} (expect not 403)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 5: 200 OK (gold tier authorized for ${GOLD_MODEL} via RBAC)"
    else
        fail_test "Test 5: Expected 200, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 6: Free tier token rate limiting
    echo ""
    echo "── Test 6: LLM Rate Limiting - Free tier token-based ──"
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
    log "LLM free tier: $llm_success successful, $llm_limited rate-limited"
    if [ "$llm_limited" -ge 3 ]; then
        pass_test "Test 6: Token rate limiting is working"
    else
        fail_test "Test 6: No rate limiting triggered"
    fi

    sleep 1

    # Test 7: Gold tier after free exhausted -> 200
    echo ""
    echo "── Test 7: LLM Rate Limiting - Gold tier after free tier exhausted ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify gold tier is unaffected by free tier token rate limit (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: Bearer ${GOLD_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 7: 200 OK (gold tier unaffected by free tier limit)"
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
    echo "── Test 10: Batch Authentication - With valid token ──"
    echo "  Route: batch-route | Tier: gold | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify authenticated request succeeds (expect 200)"
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

    # Test 11: Free tier request rate limiting (GET)
    echo ""
    echo "── Test 11: Batch Rate Limiting - Free tier request-based ──"
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
    log "Free tier: $free_success passed, $free_limited rate-limited"
    if [ "$free_limited" -ge 3 ]; then
        pass_test "Test 11: Request rate limiting is working"
    else
        fail_test "Test 11: Free tier should have been rate-limited"
    fi

    sleep 1

    # Test 12: Gold tier after free rate-limited -> 200
    echo ""
    echo "── Test 12: Batch Rate Limiting - Gold tier after free tier rate-limited ──"
    echo "  Route: batch-route | Tier: gold | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify gold tier is unaffected by free tier request rate limit (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: Bearer ${GOLD_TOKEN}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 12: 200 OK (gold request rate limit unaffected by free tier)"
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
    echo "  Goal: Upload JSONL file, create batch job with gold tier auth"
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
