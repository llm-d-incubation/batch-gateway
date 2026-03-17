#!/bin/bash
set -euo pipefail

# ── Deploy batch-gateway with Istio + GAIE + end-to-end TLS ──
# Deploys:
#   - Istio (Gateway API + GAIE support)
#   - GAIE InferencePools + EPP
#   - vLLM simulator
#   - Redis + PostgreSQL
#   - batch-gateway (apiserver + processor) via helm chart
#   - End-to-end TLS (cert-manager + DestinationRule)
#
# No Kuadrant, AuthPolicy, or RateLimitPolicy is configured.
# For auth/rate-limiting integration, use deploy-with-kuadrant-*.sh scripts.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/common.sh"

# ── Install ──────────────────────────────────────────────────────────────────

cmd_install() {
    echo ""
    echo "  ╔═══════════════════════════════════════════════════════╗"
    echo "  ║   Istio + GAIE + Batch Gateway Setup                  ║"
    echo "  ╚═══════════════════════════════════════════════════════╝"
    echo ""

    check_prerequisites

    for ns in "${BATCH_NAMESPACE}" "${LLM_NAMESPACE}" "${INGRESS_NAMESPACE}"; do
        if ! kubectl get namespace "${ns}" &>/dev/null; then
            kubectl create namespace "${ns}"
            log "Created namespace '${ns}'."
        fi
    done

    install_crds
    install_cert_manager
    create_selfsigned_issuer
    install_istio

    create_gateway_cr

    deploy_llm
    create_llm_route

    deploy_batch_gateway

    start_gateway_port_forward

    log "Deployment complete! Run '$0 test' to verify."
}

# ── Test ─────────────────────────────────────────────────────────────────────

cmd_test() {
    init_test "Istio + GAIE + Batch Gateway"

    local base_url="https://localhost:${LOCAL_PORT}"
    local test_payload='{"model":"'"${FREE_MODEL}"'","messages":[{"role":"user","content":"Hello"}]}'
    local test_total=0
    local test_passed=0
    local test_failed=0
    local failed_tests=""

    pass_test() { test_total=$((test_total + 1)); test_passed=$((test_passed + 1)); log "PASSED: $*"; }
    fail_test() { test_total=$((test_total + 1)); test_failed=$((test_failed + 1)); failed_tests="${failed_tests}\n  - $*"; warn "FAILED: $*"; }

    local response http_code body

    # Helper: pretty-print JSON or print raw
    pretty_print() { echo "$1" | jq . 2>/dev/null || echo "$1"; }

    # ── Automation Test (Go E2E) ──────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Automation Test (Go E2E)"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""

    local obs_port=8081
    if ! curl -sf "http://localhost:${obs_port}/ready" &>/dev/null; then
        step "Starting port-forward for apiserver observability (${obs_port})..."
        kubectl port-forward -n "${BATCH_NAMESPACE}" "svc/${BATCH_INFERENCE_SERVICE}" "${obs_port}:8081" &
        disown
        sleep 3
    fi

    step "Running Go E2E tests..."
    (cd "${REPO_ROOT}/test/e2e" && \
        TEST_APISERVER_URL="https://localhost:${LOCAL_PORT}" \
        TEST_APISERVER_OBS_URL="http://localhost:${obs_port}" \
        TEST_NAMESPACE="${BATCH_NAMESPACE}" \
        TEST_HELM_RELEASE="${HELM_RELEASE}" \
        TEST_MODEL="${FREE_MODEL}" \
        go test -v -count=1 -timeout=300s -run '^TestE2E$/^(Files|Batches)$/^Lifecycle$' . 2>&1)
    local e2e_exit=$?

    if [ "$e2e_exit" -eq 0 ]; then
        log "Go E2E tests passed!"
    else
        warn "Go E2E tests failed (exit code: $e2e_exit)"
    fi

    # ── Manual Test (curl) ────────────────────────────────────────────
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Manual Test (curl)"
    echo "═══════════════════════════════════════════════════════════════"

    # Test 1: LLM direct inference via llm-route
    echo ""
    echo "── Test 1: LLM direct inference via llm-route ──"
    echo "  \$ curl -sk -X POST ${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions"
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "${test_payload}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    pretty_print "$body"
    if [ "$http_code" = "200" ]; then
        pass_test "Test 1: 200 OK (llm-route -> InferencePool -> vLLM)"
    else
        fail_test "Test 1: Expected 200, got $http_code"
    fi

    # Test 1b: Verify EPP
    echo ""
    echo "── Test 1b: Verify EPP (ext-proc) was invoked ──"
    local istio_dir="/tmp/istio-${ISTIO_VERSION#v}"
    "${istio_dir}/bin/istioctl" proxy-config log -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --level ext_proc:debug > /dev/null 2>&1
    sleep 3
    curl -sk -X POST "${base_url}/${LLM_NAMESPACE}/${FREE_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' -d "${test_payload}" > /dev/null 2>&1
    sleep 3
    local envoy_log
    envoy_log=$(kubectl logs -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --since=10s 2>/dev/null)
    "${istio_dir}/bin/istioctl" proxy-config log -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio --level ext_proc:warning > /dev/null 2>&1

    local epp_ok=true
    if echo "$envoy_log" | grep -q "Opening gRPC stream to external processor"; then
        echo "  Envoy -> EPP gRPC stream: opened"
    else
        echo "  Envoy -> EPP gRPC stream: not found"
        epp_ok=false
    fi
    if echo "$envoy_log" | grep -q "Received request headers response"; then
        echo "  EPP response received: yes"
    else
        echo "  EPP response received: not found"
        epp_ok=false
    fi
    local istio_stats
    istio_stats=$(kubectl exec -n "${INGRESS_NAMESPACE}" deploy/${GATEWAY_NAME}-istio -- pilot-agent request GET /stats 2>/dev/null \
        | grep "istio_requests_total.*${FREE_MODEL}.*response_code.200" | head -1)
    if [ -n "$istio_stats" ]; then
        local req_count
        req_count=$(echo "$istio_stats" | grep -o '[0-9]*$')
        echo "  Istio metrics: ${req_count} requests routed via InferencePool"
    else
        echo "  Istio metrics: no InferencePool requests found"
        epp_ok=false
    fi
    if [ "$epp_ok" = true ]; then
        pass_test "Test 1b: EPP (ext-proc) verified working"
    else
        fail_test "Test 1b: EPP verification failed"
    fi

    sleep 1

    # Test 1c: Multi-model routing
    echo ""
    echo "── Test 1c: Multi-model routing (${GOLD_MODEL}) ──"
    local gold_payload='{"model":"'"${GOLD_MODEL}"'","messages":[{"role":"user","content":"Hello gold"}]}'
    response=$(curl -sk -w "\n%{http_code}" \
        -X POST "${base_url}/${LLM_NAMESPACE}/${GOLD_MODEL}/v1/chat/completions" \
        -H 'Content-Type: application/json' -d "${gold_payload}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    local resp_model
    resp_model=$(echo "$body" | jq -r '.model' 2>/dev/null)
    pretty_print "$body"
    if [ "$http_code" = "200" ] && [ "$resp_model" = "${GOLD_MODEL}" ]; then
        pass_test "Test 1c: 200 OK (${GOLD_MODEL} routed correctly)"
    else
        fail_test "Test 1c: Expected 200 with model=${GOLD_MODEL}, got $http_code model=${resp_model}"
    fi

    sleep 1

    # Test 2-6: Batch lifecycle
    echo ""
    echo "==============================================================="
    echo "  Batch Lifecycle: upload -> create -> list -> poll -> download"
    echo "==============================================================="

    echo ""
    echo "── Test 2: Upload batch input file ──"
    local input_file="/tmp/batch-test-input-$$.jsonl"
    cat > "${input_file}" <<JSONL
{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"${FREE_MODEL}","messages":[{"role":"user","content":"Hello"}]}}
{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"${FREE_MODEL}","messages":[{"role":"user","content":"Tell me a joke"}]}}
JSONL
    response=$(curl -sk -w "\n%{http_code}" -X POST "${base_url}/v1/files" -F "purpose=batch" -F "file=@${input_file}")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    rm -f "${input_file}"
    local file_id=""
    pretty_print "$body"
    if [ "$http_code" = "200" ]; then
        file_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
        pass_test "Test 2: File uploaded (id: ${file_id})"
    else
        fail_test "Test 2: File upload failed (HTTP $http_code)"
    fi

    sleep 1

    echo ""
    echo "── Test 3: Create batch job ──"
    local batch_id=""
    if [ -n "$file_id" ]; then
        response=$(curl -sk -w "\n%{http_code}" -X POST "${base_url}/v1/batches" \
            -H 'Content-Type: application/json' \
            -d "{\"input_file_id\":\"${file_id}\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")
        http_code=$(echo "$response" | sed -n '$p')
        body=$(echo "$response" | sed '$d')
        pretty_print "$body"
        if [ "$http_code" = "200" ]; then
            batch_id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
            pass_test "Test 3: Batch created (id: ${batch_id})"
        else
            fail_test "Test 3: Batch creation failed (HTTP $http_code)"
        fi
    else
        fail_test "Test 3: Skipped (no file_id)"
    fi

    sleep 1

    echo ""
    echo "── Test 4: List batches ──"
    response=$(curl -sk -w "\n%{http_code}" -X GET "${base_url}/v1/batches")
    http_code=$(echo "$response" | sed -n '$p')
    body=$(echo "$response" | sed '$d')
    local batch_count
    batch_count=$(echo "$body" | jq '.data | length' 2>/dev/null || echo "?")
    if [ "$http_code" = "200" ]; then
        pass_test "Test 4: 200 OK (${batch_count} batches)"
    else
        fail_test "Test 4: Expected 200, got $http_code"
    fi

    sleep 1

    echo ""
    echo "── Test 5: Poll batch status ──"
    if [ -n "$batch_id" ]; then
        local status="unknown" poll_count=0
        while [ "$poll_count" -lt 60 ]; do
            response=$(curl -sk "${base_url}/v1/batches/${batch_id}")
            status=$(echo "$response" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
            echo "  Poll $((poll_count+1)): status=${status}"
            case "$status" in completed|failed|expired|cancelled) break ;; esac
            poll_count=$((poll_count + 1))
            sleep 5
        done
        pretty_print "$response"
        if [ "$status" = "completed" ]; then
            local completed=$(echo "$response" | grep -o '"completed":[0-9]*' | head -1 | cut -d: -f2)
            pass_test "Test 5: Batch completed (completed=${completed})"
        else
            fail_test "Test 5: Batch ended with status=${status}"
        fi
    else
        fail_test "Test 5: Skipped (no batch_id)"
    fi

    sleep 1

    echo ""
    echo "── Test 6: Download output file ──"
    local output_file_id=""
    [ -n "$batch_id" ] && output_file_id=$(echo "$response" | grep -o '"output_file_id":"[^"]*"' | head -1 | cut -d'"' -f4)
    if [ -n "$output_file_id" ]; then
        local output_content
        output_content=$(curl -sk "${base_url}/v1/files/${output_file_id}/content")
        echo "$output_content" | while IFS= read -r line; do [ -n "$line" ] && pretty_print "$line"; done
        if [ -n "$output_content" ]; then
            pass_test "Test 6: Output file downloaded (id: ${output_file_id})"
        else
            fail_test "Test 6: Output file is empty"
        fi
    else
        fail_test "Test 6: Skipped (no output_file_id)"
    fi

    print_test_summary "$test_total" "$test_passed" "$test_failed" "$failed_tests"
}

# ── Uninstall ────────────────────────────────────────────────────────────────

cleanup_auth_resources() { true; }

# ── Usage ────────────────────────────────────────────────────────────────────

usage() {
    echo "Usage: $0 {install|test|uninstall|help}"
    echo ""
    echo "Commands:"
    echo "  install    Deploy Istio, GAIE, cert-manager, databases, and batch-gateway with end-to-end TLS"
    echo "  test       Run automated (Go E2E) and manual (curl) tests"
    echo "  uninstall  Remove all deployed components"
    echo "  help       Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  HELM_RELEASE          Helm release name (default: batch-gateway)"
    echo "  DEV_VERSION           Image tag (default: latest)"
    echo "  DB_TYPE               Database type: postgresql or redis (default: postgresql)"
    echo "  STORAGE_TYPE          File storage: fs or s3 (default: fs)"
    echo "  LOCAL_PORT            Local port for Gateway port-forward (default: 8080)"
    echo ""
    echo "Examples:"
    echo "  $0 install                          # Default: fs storage, postgresql, TLS"
    echo "  STORAGE_TYPE=s3 $0 install          # S3 (MinIO) storage"
    exit "${1:-0}"
}

# ── Main ─────────────────────────────────────────────────────────────────────

if [ $# -eq 0 ]; then usage 0; fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    test)      shift; cmd_test "$@" ;;
    uninstall) shift; cmd_uninstall "$@" ;;
    help|-h|--help) usage 0 ;;
    *) echo "Error: Unknown command '$1'"; echo ""; usage 1 ;;
esac
