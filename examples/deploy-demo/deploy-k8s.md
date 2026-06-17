# Demo Scripts

One-click deployment script for batch-gateway on Kubernetes. Supports `install`, `test`, and `uninstall` commands.

## Overview

**Prerequisites**:

- **Tools**: `kubectl`, `helm`, `helmfile`, `git`, `curl`, `jq`, `yq`, and the [`helm-diff`](https://github.com/databus23/helm-diff) plugin.
- **Cluster access**: You must be logged in to the target cluster. Use `kubectl config current-context` (or `oc whoami` on OpenShift) to verify.

## Usage

### install

```bash
bash examples/deploy-demo/deploy-k8s.sh install
```

#### Components Installed

| Component | Details |
|-----------|---------|
| cert-manager | TLS certificate management |
| Istio | Service mesh + ingress gateway (HTTPS:443) |
| llm-d stack | GAIE InferencePool + vllm-sim (single model, default: random) |
| Kuadrant | Auth + rate limiting (installed via Helm) |
| Redis | Exchange backend (Bitnami Helm chart, configurable via `BATCH_EXCHANGE_CLIENT_TYPE`) |
| PostgreSQL | Batch metadata store (Bitnami Helm chart) |
| MinIO | S3-compatible file storage (when `BATCH_STORAGE_TYPE=s3`) |
| Internal Gateway | ClusterIP gateway for batch processor → LLM inference (bypasses rate limits, preserves AuthPolicy) |
| InferenceObjective | GIE flow control CRDs — priority-based dispatch (interactive=100, batch=-1). Enabled by default (`ENABLE_FLOW_CONTROL=true`) |
| batch-gateway | apiserver + processor + gc (Helm chart) |

#### Routing & Policies

**External Gateway** (`istio-gateway`, HTTPS:443):

| HTTPRoute | Backend | Auth | Rate Limit |
|-----------|---------|------|------------|
| `llm-route` | InferencePool (direct inference) | kubernetesTokenReview + SubjectAccessReview (model-level authz) | 500 tokens/1min per user on `/v1/chat/completions` only (TokenRateLimitPolicy) |
| `batch-route` | batch-gateway apiserver | kubernetesTokenReview only (no authz) | 20 req/1min per user (RateLimitPolicy) |

**Internal Gateway** (`batch-internal-gateway`, ClusterIP, HTTP:80):

| HTTPRoute | Backend | Auth | Rate Limit |
|-----------|---------|------|------------|
| `batch-llm-route` | InferencePool (batch processor access) | kubernetesTokenReview + SubjectAccessReview (model-level authz) | — (none, by design) |

Batch-route has no authorization — model-level authz is enforced downstream when the batch processor forwards requests through the Internal Gateway's `batch-llm-route`.

#### Install Examples

| Mode | Command |
|------|---------|
| Local chart (default) | `bash examples/deploy-demo/deploy-k8s.sh install` |
| Specific commit | `BATCH_DEV_VERSION=1f925ff bash examples/deploy-demo/deploy-k8s.sh install` |
| Released OCI chart | `BATCH_RELEASE_VERSION=v0.1.0 bash examples/deploy-demo/deploy-k8s.sh install` |

> `BATCH_RELEASE_VERSION` and `BATCH_DEV_VERSION` cannot be used together. See [Environment Variables](#environment-variables) for common parameters.

### test

```bash
bash examples/deploy-demo/deploy-k8s.sh test
```

Creates temporary ServiceAccounts (authorized + unauthorized) with short-lived tokens and runs the following test groups:

| # | Test Group | What it verifies |
|---|------------|------------------|
| 1 | LLM Authn | Unauthenticated → 401, authenticated → 200 |
| 2 | LLM Authz | Unauthorized → 403, authorized → 200 |
| 3 | LLM Token Rate Limit | Repeated requests → 429 |
| 4 | Batch Authn | Unauthenticated → 401, authenticated → 200 |
| 5 | Batch Authz | Unauthorized user's batch → requests rejected with 403 by Internal Gateway |
| 6 | Batch Lifecycle | File upload → batch create → poll → completed → download output |
| 7 | Batch Request Rate Limit | Rapid requests → 429 |
| 8 | Flow Control (if enabled) | EPP metrics: interactive requests at priority 0, batch at priority -1, pool saturation metric present |

Internal Gateway isolation is also verified before tests run (service type is ClusterIP, no Route/Ingress exposes it).

### uninstall

```bash
bash examples/deploy-demo/deploy-k8s.sh uninstall
```

Default `uninstall` removes the batch-gateway footprint and associated gateway/policy resources:

- Helm releases and CRs in `BATCH_NAMESPACE` (including all HTTPRoutes)
- Both Gateways: `GATEWAY_NAME` and `BATCH_INTERNAL_GATEWAY_NAME`
- DestinationRule `${BATCH_HELM_RELEASE}-backend-tls`
- Internal Gateway resources (`batch-llm-route`, `batch-llm-route-auth`) in `LLM_NAMESPACE`
- Kuadrant policies (`llm-route-auth`, `batch-route-auth`, `inference-token-limit`, `batch-ratelimit`)
- Demo RBAC (test ServiceAccounts, Role, RoleBinding) in `LLM_NAMESPACE`
- `BATCH_NAMESPACE` itself

It does **not** remove Kuadrant, Istio, cert-manager, operators, or cluster-wide CRDs—so other teams' platform pieces stay.

> **Note**: The default uninstall deletes the Gateway named `GATEWAY_NAME` (default: `istio-gateway`). If this Gateway is shared with other teams, override `GATEWAY_NAME` or remove the Gateway deletion from the script before running.

**Do not use `UNINSTALL_ALL=1` on shared production or multi-team clusters** — that mode tears down operators and platform components others may depend on.

**Full teardown** (throwaway / dedicated demo cluster only) — prefix the command with `UNINSTALL_ALL=1`:

```bash
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-k8s.sh uninstall
```

Use that only on **ephemeral or dedicated** demo clusters. See [issue #309](https://github.com/llm-d/llm-d-batch-gateway/issues/309) for background.

## Environment Variables

| Variable | Default | Scope | Description |
|----------|---------|-------|-------------|
| `BATCH_HELM_RELEASE` | `batch-gateway` | all | Helm release name |
| `BATCH_RELEASE_VERSION` | — | all | Install from released OCI chart (e.g. `v1.0.0`). Cannot be used with `BATCH_DEV_VERSION` |
| `BATCH_DEV_VERSION` | `local` | all | Image tag / commit SHA. `local` uses local chart + `latest` image. Cannot be used with `BATCH_RELEASE_VERSION` |
| `BATCH_IMAGE_TAG` | — | all | Override image tag for all components. Takes precedence over `BATCH_RELEASE_VERSION` / `BATCH_DEV_VERSION` derived tags |
| `BATCH_APISERVER_REPO` | — | all | Override apiserver image repository |
| `BATCH_PROCESSOR_REPO` | — | all | Override processor image repository |
| `BATCH_GC_REPO` | — | all | Override gc image repository |
| `BATCH_DB_TYPE` | `postgresql` | all | Database backend: `postgresql` or `redis` |
| `BATCH_STORAGE_TYPE` | `s3` | all | File storage: `fs` or `s3` |
| `DEMO_TLS_INSECURE_SKIP_VERIFY` | `1` | all | Disables TLS certificate verification for processor → model gateway and Istio Gateway → batch apiserver (**demo/lab only**, [CWE-295](https://cwe.mitre.org/data/definitions/295.html)). Default `1` since demo scripts use self-signed certs. Set to `0` if you have trusted CA certs. |
| `BATCH_NAMESPACE` | `batch-api` | all | Namespace for batch-gateway |
| `LLM_NAMESPACE` | `llm` | all | Namespace for model serving |
| `GATEWAY_NAME` | `istio-gateway` | k8s | Gateway resource name |
| `GATEWAY_NAMESPACE` | `istio-ingress` | k8s | Gateway namespace |
| `LLMD_VERSION` | `v0.7.0` | k8s | llm-d git ref to install |
| `LLMD_RELEASE_POSTFIX` | `llmd` | k8s | Helm release postfix |
| `GATEWAY_LOCAL_PORT` | `8080` | k8s | Port-forward local port |
| `MODEL_NAME` | `random` | k8s | Model name for routing |
| `KUADRANT_VERSION` | `1.3.1` | k8s | Kuadrant Helm chart version |
| `GATEWAY_CLASS_NAME` | `istio` | k8s | GatewayClass name |
| `CERT_MANAGER_VERSION` | `v1.15.3` | k8s | cert-manager Helm chart version |
| `GAIE_CHART_VERSION` | `v1.5.0` | k8s | InferencePool (GAIE) Helm chart version |
| `MODELSERVICE_CHART_VERSION` | `v0.4.12` | k8s | ModelService (vllm-sim) Helm chart version |
| `ENABLE_FLOW_CONTROL` | `true` | k8s | Enable GIE priority-based flow control |
| `BATCH_FLOW_CONTROL_OBJECTIVE` | `batch-sheddable` | k8s | InferenceObjective name for batch requests (priority -1) |
| `BATCH_EXCHANGE_CLIENT_TYPE` | `redis` | all | Exchange backend type (`redis` or `valkey`) |
| `BATCH_INTERNAL_GATEWAY_NAME` | `batch-internal-gateway` | k8s | Internal Gateway resource name |
| `BATCH_INTERNAL_GATEWAY_NAMESPACE` | `${GATEWAY_NAMESPACE}` | k8s | Internal Gateway namespace |
| `GW_REQUEST_TIMEOUT` | `5m` | all | Model gateway HTTP request timeout |
| `GW_MAX_RETRIES` | `3` | all | Model gateway max retries |
| `GW_INITIAL_BACKOFF` | `1s` | all | Model gateway initial retry backoff |
| `GW_MAX_BACKOFF` | `60s` | all | Model gateway max retry backoff |
