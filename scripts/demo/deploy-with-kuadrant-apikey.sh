#!/bin/bash
set -euo pipefail

# ── API Key mode: authentication via API Key secrets, authorization via OPA ──

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

# ── API Key Configuration ─────────────────────────────────────────────────────
# WARNING: Default keys are for demo only. For production, override via env vars:
#   API_KEY_GOLD="$(openssl rand -hex 32)" API_KEY_FREE="$(openssl rand -hex 32)" ./deploy-with-kuadrant-apikey.sh install
API_KEY_GOLD="${API_KEY_GOLD:-gold-key-12345}"
API_KEY_FREE="${API_KEY_FREE:-free-key-67890}"

# ── API Key Secrets ───────────────────────────────────────────────────────────

create_api_key_secrets() {
    step "Creating tier-based API key secrets in ${KUADRANT_NAMESPACE}..."

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: api-key-gold
  namespace: ${KUADRANT_NAMESPACE}
  labels:
    app: batch-api
    authorino.kuadrant.io/managed-by: authorino
  annotations:
    username: "gold-user"
    group: "gold"
    tier: "gold"
    models: "${FREE_MODEL},${GOLD_MODEL}"
    secret.kuadrant.io/user-id: "gold-user"
    kuadrant.io/groups: "gold"
stringData:
  api_key: ${API_KEY_GOLD}
---
apiVersion: v1
kind: Secret
metadata:
  name: api-key-free
  namespace: ${KUADRANT_NAMESPACE}
  labels:
    app: batch-api
    authorino.kuadrant.io/managed-by: authorino
  annotations:
    username: "free-user"
    group: "free"
    tier: "free"
    models: "${FREE_MODEL}"
    secret.kuadrant.io/user-id: "free-user"
    kuadrant.io/groups: "free"
stringData:
  api_key: ${API_KEY_FREE}
EOF

    log "API key secrets created."
    log "  gold (${API_KEY_GOLD}): tier=gold, models=${FREE_MODEL},${GOLD_MODEL}"
    log "  free (${API_KEY_FREE}): tier=free, models=${FREE_MODEL}"
}

# ── Kuadrant Policies (API Key mode) ─────────────────────────────────────────

apply_batch_auth_policy() {
    step "Applying batch-auth AuthPolicy (API Key authentication)..."

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
      "api-key-authn":
        apiKey:
          allNamespaces: true
          selector:
            matchLabels:
              app: batch-api
        credentials:
          authorizationHeader:
            prefix: APIKEY
    response:
      success:
        filters:
          "identity":
            json:
              properties:
                "tier":
                  selector: auth.identity.metadata.annotations.tier
        headers:
          "x-tier":
            plain:
              selector: auth.identity.metadata.annotations.tier
          "x-username":
            plain:
              selector: auth.identity.metadata.annotations.username
          "x-group":
            plain:
              selector: auth.identity.metadata.annotations.group
EOF

    log "batch-auth applied (API Key authentication, targets batch-route)."
}

apply_llm_auth_policy() {
    step "Applying llm-auth AuthPolicy (API Key + OPA model authorization)..."

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
      "api-key-authn":
        apiKey:
          allNamespaces: true
          selector:
            matchLabels:
              app: batch-api
        credentials:
          authorizationHeader:
            prefix: APIKEY
    authorization:
      "model-access":
        opa:
          rego: |
            path_parts := split(input.context.request.http.path, "/")
            requested_model := path_parts[2]
            allowed_csv := object.get(input.auth.identity.metadata.annotations, "models", "")
            allowed_models := split(allowed_csv, ",")
            allow { allowed_models[_] == requested_model }
          allValues: true
    response:
      success:
        filters:
          "identity":
            json:
              properties:
                "tier":
                  selector: auth.identity.metadata.annotations.tier
        headers:
          "x-tier":
            plain:
              selector: auth.identity.metadata.annotations.tier
          "x-username":
            plain:
              selector: auth.identity.metadata.annotations.username
          "x-group":
            plain:
              selector: auth.identity.metadata.annotations.group
EOF

    log "llm-auth applied (API Key + OPA model authorization, targets llm-route)."
}

cleanup_auth_resources() {
    timeout_delete 15s secret api-key-gold api-key-free -n "${KUADRANT_NAMESPACE}" \
        || warn "No API key secrets to delete"
}

# ── Install ───────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   Kuadrant + GAIE Setup (API Key mode)                ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

    install_with_kuadrant
    create_api_key_secrets
    apply_batch_auth_policy
    apply_llm_auth_policy
    wait_for_auth_policies
    start_gateway_port_forward

    log "Deployment complete! Run '$0 test' to verify."
}

# ── Test ──────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "API Key"

    API_KEY_GOLD=$(kubectl get secret api-key-gold -n "${KUADRANT_NAMESPACE}" -o jsonpath='{.data.api_key}' 2>/dev/null | base64 -d) \
        || die "api-key-gold secret not found. Run '$0 install' first."
    API_KEY_FREE=$(kubectl get secret api-key-free -n "${KUADRANT_NAMESPACE}" -o jsonpath='{.data.api_key}' 2>/dev/null | base64 -d) \
        || die "api-key-free secret not found. Run '$0 install' first."
    log "API keys read from secrets (gold: ${API_KEY_GOLD}, free: ${API_KEY_FREE})."

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
    echo "  - Authentication, Authorization (OPA model access), Rate Limiting (token-based)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 1: No API key -> 401
    echo ""
    echo "── Test 1: LLM Authentication - No API key ──"
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

    # Test 2: Invalid API key -> 401
    echo ""
    echo "── Test 2: LLM Authentication - Invalid API key ──"
    echo "  Route: llm-route | Tier: none | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify invalid API key is rejected (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: APIKEY invalid-key-99999" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 2: 401 Unauthorized (invalid key rejected)"
    else
        fail_test "Test 2: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 3: Valid API key -> 200
    echo ""
    echo "── Test 3: LLM Authentication - With API key ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    echo "  Goal: Verify authenticated request succeeds (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H "Authorization: APIKEY ${API_KEY_GOLD}" \
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

    # Test 4: Free tier accessing ${GOLD_MODEL} -> 403
    echo ""
    echo "── Test 4: LLM Authorization - Free tier accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: free | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify free tier is denied access to ${GOLD_MODEL} (expect 403)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: APIKEY ${API_KEY_FREE}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "403" ]; then
        pass_test "Test 4: 403 Forbidden (free tier denied access to ${GOLD_MODEL})"
    else
        fail_test "Test 4: Expected 403, got $http_code"
        echo "  Response: $body"
    fi

    sleep 1

    # Test 5: Gold tier accessing ${GOLD_MODEL} -> authorized
    echo ""
    echo "── Test 5: LLM Authorization - Gold tier accessing ${GOLD_MODEL} ──"
    echo "  Route: llm-route | Tier: gold | Method: POST | Path: /${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions"
    echo "  Goal: Verify gold tier can access ${GOLD_MODEL} (in allowed models, expect not 403)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H "Authorization: APIKEY ${API_KEY_GOLD}" \
        -H 'Content-Type: application/json' \
        -d "${gold_payload}")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$ d')
    if [ "$http_code" = "200" ]; then
        pass_test "Test 5: 200 OK (gold tier authorized for ${GOLD_MODEL})"
        echo "  Response: $body"
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
            -H "Authorization: APIKEY ${API_KEY_FREE}" \
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
        -H "Authorization: APIKEY ${API_KEY_GOLD}" \
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
    echo "  - Authentication, Rate Limiting (request-based)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 8: No API key -> 401
    echo ""
    echo "── Test 8: Batch Authentication - No API key ──"
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

    # Test 9: Invalid API key -> 401
    echo ""
    echo "── Test 9: Batch Authentication - Invalid API key ──"
    echo "  Route: batch-route | Tier: none | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify invalid API key is rejected (expect 401)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: APIKEY invalid-key-99999")
    http_code=$(echo "$response" | tail -n1)
    if [ "$http_code" = "401" ]; then
        pass_test "Test 9: 401 Unauthorized (invalid key rejected)"
    else
        fail_test "Test 9: Expected 401, got $http_code"
    fi

    sleep 1

    # Test 10: Valid API key -> 200
    echo ""
    echo "── Test 10: Batch Authentication - With API key ──"
    echo "  Route: batch-route | Tier: gold | Method: GET | Path: /v1/batches"
    echo "  Goal: Verify authenticated request succeeds (expect 200)"
    response=$(curl -sk -w "\n%{http_code}" \
        -X GET "${base_url}/v1/batches" \
        -H "Authorization: APIKEY ${API_KEY_GOLD}")
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
            -H "Authorization: APIKEY ${API_KEY_FREE}")
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
        -H "Authorization: APIKEY ${API_KEY_GOLD}")
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
        -H "Authorization: APIKEY ${API_KEY_GOLD}" \
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
            -H "Authorization: APIKEY ${API_KEY_GOLD}" \
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
                -H "Authorization: APIKEY ${API_KEY_GOLD}")
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
