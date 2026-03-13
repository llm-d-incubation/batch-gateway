#!/bin/bash
set -euo pipefail

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── Configuration ─────────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-default}"
LOCAL_PORT="${LOCAL_PORT:-8000}"
LOCAL_OBS_PORT="${LOCAL_OBS_PORT:-8081}"
LOCAL_PROCESSOR_PORT="${LOCAL_PROCESSOR_PORT:-9090}"
JAEGER_PORT="${JAEGER_PORT:-16686}"
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
JAEGER_NAME="${JAEGER_NAME:-jaeger}"

# ── Functions ─────────────────────────────────────────────────────────────────

check_endpoint() {
    local url="$1"
    local name="$2"

    if curl -sf "${url}" >/dev/null 2>&1; then
        log "${name} is accessible at ${url}"
        return 0
    else
        return 1
    fi
}

kill_stale_port_forwards() {
    local ports=("$@")
    for port in "${ports[@]}"; do
        local pids=$(lsof -ti tcp:${port} 2>/dev/null || true)
        if [ -n "${pids}" ]; then
            log "Killing stale port-forward on port ${port} (PIDs: ${pids})"
            kill ${pids} 2>/dev/null || true
            sleep 1
        fi
    done
}

start_apiserver_port_forward() {
    local svc="svc/${HELM_RELEASE}-apiserver"

    log "Starting port-forward: ${svc} ${LOCAL_PORT}:8000 ${LOCAL_OBS_PORT}:8081 -n ${NAMESPACE}..."
    kubectl port-forward "${svc}" "${LOCAL_PORT}:8000" "${LOCAL_OBS_PORT}:8081" -n "${NAMESPACE}" &>/dev/null &
    local pf_pid=$!
    disown "${pf_pid}"
    log "Port-forward PID: ${pf_pid}"

    # Wait for it to be ready
    for i in {1..30}; do
        if curl -sf "http://localhost:${LOCAL_OBS_PORT}/health" >/dev/null 2>&1; then
            log "API server is ready at https://localhost:${LOCAL_PORT}"
            return 0
        fi
        sleep 1
    done

    warn "API server health check timed out, but port-forward is running"
}

start_processor_port_forward() {
    local deploy="deployment/${HELM_RELEASE}-processor"

    log "Starting port-forward: ${deploy} ${LOCAL_PROCESSOR_PORT}:9090 -n ${NAMESPACE}..."
    kubectl port-forward "${deploy}" "${LOCAL_PROCESSOR_PORT}:9090" -n "${NAMESPACE}" &>/dev/null &
    local pf_pid=$!
    disown "${pf_pid}"
    log "Processor port-forward PID: ${pf_pid}"

    # Wait for it to be ready
    for i in {1..30}; do
        if curl -sf "http://localhost:${LOCAL_PROCESSOR_PORT}/health" >/dev/null 2>&1; then
            log "Processor is ready at http://localhost:${LOCAL_PROCESSOR_PORT}"
            return 0
        fi
        sleep 1
    done

    warn "Processor health check timed out, but port-forward is running"
}

start_jaeger_port_forward() {
    local svc="svc/${JAEGER_NAME}"

    log "Starting port-forward: ${svc} ${JAEGER_PORT}:16686 -n ${NAMESPACE}..."
    kubectl port-forward "${svc}" "${JAEGER_PORT}:16686" -n "${NAMESPACE}" &>/dev/null &
    local pf_pid=$!
    disown "${pf_pid}"
    log "Jaeger port-forward PID: ${pf_pid}"
}

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
        kill_stale_port_forwards "${LOCAL_PORT}" "${LOCAL_OBS_PORT}"
        start_apiserver_port_forward
    fi

    if [ "${need_processor}" = true ]; then
        kill_stale_port_forwards "${LOCAL_PROCESSOR_PORT}"
        start_processor_port_forward
    fi

    if [ "${need_jaeger}" = true ]; then
        kill_stale_port_forwards "${JAEGER_PORT}"
        start_jaeger_port_forward
    fi

    log "Port forwards are now active."
}

main "$@"
