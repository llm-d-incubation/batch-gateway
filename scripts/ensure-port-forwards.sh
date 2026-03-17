#!/bin/bash
set -euo pipefail

# Source common functions and configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/dev-common.sh"

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
    # Check if kubectl is available
    if ! command -v kubectl &>/dev/null; then
        die "kubectl not found. Cannot set up port forwarding."
    fi

    # Check if required services exist
    if ! kubectl get svc "${HELM_RELEASE}-apiserver" -n "${NAMESPACE}" &>/dev/null; then
        die "API server service not found. Did you run 'make dev-deploy'?"
    fi

    local need_apiserver=false
    local need_processor=false
    local need_jaeger=false

    # Check which port forwards are needed
    if ! check_endpoint "http://localhost:${LOCAL_OBS_PORT}/health" "API server"; then
        need_apiserver=true
    fi

    if ! check_endpoint "http://localhost:${LOCAL_PROCESSOR_PORT}/health" "Processor"; then
        need_processor=true
    fi

    if ! check_endpoint "http://localhost:${JAEGER_PORT}/" "Jaeger"; then
        need_jaeger=true
    fi

    # If everything is already accessible, we're done
    if [ "${need_apiserver}" = false ] && [ "${need_processor}" = false ] && [ "${need_jaeger}" = false ]; then
        log "All port forwards are already active and services are accessible."
        exit 0
    fi

    # Start port forwards as needed
    if [ "${need_apiserver}" = true ]; then
        kill_ports "${LOCAL_PORT}" "${LOCAL_OBS_PORT}"
        start_apiserver_port_forward
    fi

    if [ "${need_processor}" = true ]; then
        kill_ports "${LOCAL_PROCESSOR_PORT}"
        start_processor_port_forward
    fi

    if [ "${need_jaeger}" = true ]; then
        kill_ports "${JAEGER_PORT}"
        start_jaeger_port_forward
    fi

    log "Port forwards are now active."
}

main "$@"
