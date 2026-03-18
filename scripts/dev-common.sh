#!/bin/bash
# Common functions and configuration for dev scripts
# Source this file from other dev-*.sh scripts

# ── Colors ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# ── Logging Functions ─────────────────────────────────────────────────────────
log()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
step() { echo -e "${BLUE}[STEP]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── Common Configuration (override via env vars) ──────────────────────────────
NAMESPACE="${NAMESPACE:-default}"
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"

# Port configuration
LOCAL_PORT="${LOCAL_PORT:-8000}"
LOCAL_OBS_PORT="${LOCAL_OBS_PORT:-8081}"
LOCAL_PROCESSOR_PORT="${LOCAL_PROCESSOR_PORT:-9090}"
JAEGER_PORT="${JAEGER_PORT:-16686}"

# Service names
REDIS_RELEASE="${REDIS_RELEASE:-redis}"
POSTGRESQL_RELEASE="${POSTGRESQL_RELEASE:-postgresql}"
JAEGER_NAME="${JAEGER_NAME:-jaeger}"
PROMETHEUS_NAME="${PROMETHEUS_NAME:-prometheus}"
PROMETHEUS_PORT="${PROMETHEUS_PORT:-9091}"
VLLM_SIM_NAME="${VLLM_SIM_NAME:-vllm-sim}"
VLLM_SIM_B_NAME="${VLLM_SIM_B_NAME:-vllm-sim-b}"

# Secret and PVC names
TLS_SECRET_NAME="${TLS_SECRET_NAME:-${HELM_RELEASE}-tls}"
APP_SECRET_NAME="${APP_SECRET_NAME:-${HELM_RELEASE}-secrets}"
FILES_PVC_NAME="${FILES_PVC_NAME:-${HELM_RELEASE}-files}"

# ── Common Functions ──────────────────────────────────────────────────────────

kill_port_forwards() {
    local ports=(8000 8081 9090 9091 16686)
    for port in "${ports[@]}"; do
        local pids=$(lsof -ti tcp:${port} 2>/dev/null || true)
        if [ -n "${pids}" ]; then
            log "Killing processes on port ${port}: ${pids}"
            kill ${pids} 2>/dev/null || true
        fi
    done
}

kill_ports() {
    # Kill specific ports passed as arguments
    local ports=("$@")
    for port in "${ports[@]}"; do
        local pids=$(lsof -ti tcp:${port} 2>/dev/null || true)
        if [ -n "${pids}" ]; then
            log "Killing stale port-forward on port ${port} (PIDs: ${pids})"
            kill ${pids} 2>/dev/null || true
            sleep 0.5
        fi
    done
}

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

start_apiserver_port_forward() {
    local svc="svc/${HELM_RELEASE}-apiserver"
    local max_retries=3
    local health_check_attempts=30

    kill_ports "${LOCAL_PORT}" "${LOCAL_OBS_PORT}"

    for retry in $(seq 1 ${max_retries}); do
        step "Starting port-forward: ${svc} ${LOCAL_PORT}:8000 ${LOCAL_OBS_PORT}:8081 -n ${NAMESPACE} (attempt ${retry}/${max_retries})..."
        kubectl port-forward "${svc}" "${LOCAL_PORT}:8000" "${LOCAL_OBS_PORT}:8081" -n "${NAMESPACE}" &>/dev/null &
        local pf_pid=$!
        disown "${pf_pid}"
        log "Port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"

        log "Waiting for http://localhost:${LOCAL_OBS_PORT}/health ..."

        local success=false
        for i in $(seq 1 ${health_check_attempts}); do
            if curl -sf "http://localhost:${LOCAL_OBS_PORT}/health" >/dev/null 2>&1; then
                log "API server is ready at https://localhost:${LOCAL_PORT}"
                success=true
                break
            fi
            sleep 1
        done

        if [ "${success}" = true ]; then
            return 0
        fi

        # Health check failed - kill the port-forward and retry
        warn "Port-forward health check failed on attempt ${retry}/${max_retries}"
        kill "${pf_pid}" 2>/dev/null || true
        kill_ports "${LOCAL_PORT}" "${LOCAL_OBS_PORT}"

        if [ ${retry} -lt ${max_retries} ]; then
            log "Retrying port-forward in 2 seconds..."
            sleep 2
        fi
    done

    die "Timed out waiting for API server to become ready after ${max_retries} attempts"
}

start_processor_port_forward() {
    local deploy="deployment/${HELM_RELEASE}-processor"
    local max_retries=3
    local health_check_attempts=30

    kill_ports "${LOCAL_PROCESSOR_PORT}"

    for retry in $(seq 1 ${max_retries}); do
        step "Starting port-forward: ${deploy} ${LOCAL_PROCESSOR_PORT}:9090 -n ${NAMESPACE} (attempt ${retry}/${max_retries})..."
        kubectl port-forward "${deploy}" "${LOCAL_PROCESSOR_PORT}:9090" -n "${NAMESPACE}" &>/dev/null &
        local pf_pid=$!
        disown "${pf_pid}"
        log "Processor port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"

        log "Waiting for http://localhost:${LOCAL_PROCESSOR_PORT}/health ..."

        local success=false
        for i in $(seq 1 ${health_check_attempts}); do
            if curl -sf "http://localhost:${LOCAL_PROCESSOR_PORT}/health" >/dev/null 2>&1; then
                log "Processor is ready at http://localhost:${LOCAL_PROCESSOR_PORT}"
                success=true
                break
            fi
            sleep 1
        done

        if [ "${success}" = true ]; then
            return 0
        fi

        # Health check failed - kill the port-forward and retry
        warn "Port-forward health check failed on attempt ${retry}/${max_retries}"
        kill "${pf_pid}" 2>/dev/null || true
        kill_ports "${LOCAL_PROCESSOR_PORT}"

        if [ ${retry} -lt ${max_retries} ]; then
            log "Retrying port-forward in 2 seconds..."
            sleep 2
        fi
    done

    die "Timed out waiting for Processor to become ready after ${max_retries} attempts"
}

start_jaeger_port_forward() {
    local svc="svc/${JAEGER_NAME}"

    kill_ports "${JAEGER_PORT}"

    step "Starting port-forward: ${svc} ${JAEGER_PORT}:16686 -n ${NAMESPACE}..."
    kubectl port-forward "${svc}" "${JAEGER_PORT}:16686" -n "${NAMESPACE}" &>/dev/null &
    local pf_pid=$!
    disown "${pf_pid}"

    log "Jaeger port-forward PID: ${pf_pid}  (stop with: kill ${pf_pid})"
    log "Jaeger UI available at http://localhost:${JAEGER_PORT}"
}

start_prometheus_port_forward() {
    local svc="svc/${PROMETHEUS_NAME}"

    log "Starting port-forward: ${svc} ${PROMETHEUS_PORT}:9090 -n ${NAMESPACE}..."
    kubectl port-forward "${svc}" "${PROMETHEUS_PORT}:9090" -n "${NAMESPACE}" &>/dev/null &
    local pf_pid=$!
    disown "${pf_pid}"
    log "Prometheus port-forward PID: ${pf_pid}"
    log "Prometheus UI available at http://localhost:${PROMETHEUS_PORT}"
}
