# Demo Scripts

One-click deployment scripts for batch-gateway on different platforms. Each script supports `install`, `test`, and `uninstall` commands.

## Safety: shared clusters and `uninstall`

**Default `uninstall` (no env var) is OK on many shared clusters** if you only want to drop this demo’s batch-gateway footprint: it removes Helm releases and CRs in the batch namespace, named routes/policies, the **single** Gateway named `GATEWAY_NAME`, demo RBAC patches, and demo test users (MaaS/OpenShift). It does **not** remove MaaS/ODH, Kuadrant, Istio, cert-manager, operators, or cluster-wide CRDs—so other teams’ platform pieces stay. Still skim the list below before you run it in production.

**Do not use `UNINSTALL_ALL=1` on shared production or multi-team clusters** — that mode tears down operators and platform components others may depend on.

**Full teardown** (throwaway / dedicated demo cluster only) — prefix the command with `UNINSTALL_ALL=1`:

```bash
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-k8s.sh uninstall
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-rhoai.sh uninstall
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-maas.sh uninstall
```

Use that only on **ephemeral or dedicated** demo clusters. See [issue #309](https://github.com/llm-d-incubation/batch-gateway/issues/309) for background.

## 1) Overview

| Script | Cluster | Description |
|--------|---------|-------------|
| `deploy-rhoai.sh` | OpenShift | Deploys batch-gateway on top of RHOAI + RHCL |
| `deploy-k8s.sh` | Kubernetes/OpenShift | Deploys llm-d + Kuadrant + batch-gateway |
| `deploy-maas.sh` | OpenShift | Deploys batch-gateway on top of MaaS |

**Prerequisites**: You must be logged in to the target cluster before running any script. Use `kubectl config current-context` (or `oc whoami` on OpenShift) to verify.

## 2) deploy-rhoai.sh

### Components Installed

| Component | Details |
|-----------|---------|
| cert-manager operator | OLM-managed |
| LeaderWorkerSet operator | OLM-managed |
| OpenShift Gateway | GatewayClass + Gateway (auto-installs Service Mesh) |
| RHCL | Productized Kuadrant (OLM-managed) |
| RHOAI | DSCInitialization + DataScienceCluster |
| Redis | Batch job queue (Bitnami Helm chart) |
| PostgreSQL | Batch metadata store (Bitnami Helm chart) |
| batch-gateway | apiserver + processor (Helm chart) |

### Auth & Rate Limits

| Policy | Target | Limit |
|--------|--------|-------|
| AuthPolicy (kubernetesTokenReview) | batch-route | — |
| TokenRateLimitPolicy | Gateway (inference) | 500 tokens/1min per user |
| RateLimitPolicy | batch-route | 20 req/1min per user |

### Usage

```bash
bash examples/deploy-demo/deploy-rhoai.sh install
bash examples/deploy-demo/deploy-rhoai.sh test
bash examples/deploy-demo/deploy-rhoai.sh uninstall
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-rhoai.sh uninstall   # optional: remove RHOAI operators, Kuadrant, cert-manager, etc.
```

### Install Examples

| Mode | Command |
|------|---------|
| Local chart (default) | `bash examples/deploy-demo/deploy-rhoai.sh install` |
| Specific commit | `BATCH_DEV_VERSION=1f925ff bash examples/deploy-demo/deploy-rhoai.sh install` |
| Released OCI chart | `BATCH_RELEASE_VERSION=v0.1.0 bash examples/deploy-demo/deploy-rhoai.sh install` |
| Overwrite with Midstream images | `BATCH_IMAGE_TAG=v0.1.0 BATCH_APISERVER_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-apiserver BATCH_PROCESSOR_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-processor BATCH_GC_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-gc bash examples/deploy-demo/deploy-rhoai.sh install` |

> `BATCH_RELEASE_VERSION` and `BATCH_DEV_VERSION` cannot be used together. See [Environment Variables](#environment-variables) for all available parameters.


## 3) deploy-k8s.sh

### Components Installed

| Component | Details |
|-----------|---------|
| cert-manager | TLS certificate management |
| Istio | Service mesh + ingress gateway (HTTPS:443) |
| llm-d stack | GAIE InferencePool + vllm-sim (single model, default: random) |
| Kuadrant | Auth + rate limiting (installed via Helm) |
| Redis | Batch job queue (Bitnami Helm chart) |
| PostgreSQL | Batch metadata store (Bitnami Helm chart) |
| MinIO | S3-compatible file storage (when `BATCH_STORAGE_TYPE=s3`) |
| batch-gateway | apiserver + processor (Helm chart) |

### Auth & Rate Limits

| Policy | Target | Limit |
|--------|--------|-------|
| AuthPolicy (kubernetesTokenReview) | llm-route, batch-route | — |
| TokenRateLimitPolicy | Gateway (inference) | 500 tokens/1min per user |
| RateLimitPolicy | batch-route | 20 req/1min per user |

### Usage

```bash
bash examples/deploy-demo/deploy-k8s.sh install
bash examples/deploy-demo/deploy-k8s.sh test
bash examples/deploy-demo/deploy-k8s.sh uninstall
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-k8s.sh uninstall   # optional: remove Kuadrant/Istio/cert-manager too
```

### Install Examples

| Mode | Command |
|------|---------|
| Local chart (default) | `bash examples/deploy-demo/deploy-k8s.sh install` |
| Specific commit | `BATCH_DEV_VERSION=1f925ff bash examples/deploy-demo/deploy-k8s.sh install` |
| Released OCI chart | `BATCH_RELEASE_VERSION=v0.1.0 bash examples/deploy-demo/deploy-k8s.sh install` |
| Overwrite with Midstream images | `BATCH_IMAGE_TAG=v0.1.0 BATCH_APISERVER_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-apiserver BATCH_PROCESSOR_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-processor BATCH_GC_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-gc bash examples/deploy-demo/deploy-k8s.sh install` |

> `BATCH_RELEASE_VERSION` and `BATCH_DEV_VERSION` cannot be used together. See [Environment Variables](#environment-variables) for all available parameters.


## 4) deploy-maas.sh


### Components Installed

| Component | Details |
|-----------|---------|
| MaaS platform | Models-as-a-Service (ODH-based, includes Kuadrant + Istio + cert-manager) |
| Redis | Batch job queue (Bitnami Helm chart) |
| PostgreSQL | Batch metadata store (Bitnami Helm chart) |
| batch-gateway | apiserver + processor (Helm chart) |

### Auth & Rate Limits

| Policy | Target | Limit |
|--------|--------|-------|
| AuthPolicy (MaaS API key) | batch-route | — |
| TokenRateLimitPolicy | model route (via MaaSSubscription) | 500 tokens/1min per user |
| RateLimitPolicy | batch-route | 20 req/1min per user |

### Usage

```bash
bash examples/deploy-demo/deploy-maas.sh install
bash examples/deploy-demo/deploy-maas.sh test
bash examples/deploy-demo/deploy-maas.sh uninstall

# Optional — ephemeral/demo clusters only: MaaS cleanup + cert-manager/LWS (legacy full teardown).
UNINSTALL_ALL=1 bash examples/deploy-demo/deploy-maas.sh uninstall
```

### Install Examples

| Mode | Command |
|------|---------|
| Local chart (default) | `bash examples/deploy-demo/deploy-maas.sh install` |
| Specific commit | `BATCH_DEV_VERSION=1f925ff bash examples/deploy-demo/deploy-maas.sh install` |
| Released OCI chart | `BATCH_RELEASE_VERSION=v0.1.0 bash examples/deploy-demo/deploy-maas.sh install` |
| Overwrite with Midstream images | `BATCH_IMAGE_TAG=v0.1.0 BATCH_APISERVER_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-apiserver BATCH_PROCESSOR_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-processor BATCH_GC_REPO=quay.io/redhat-user-workloads/open-data-hub-tenant/temp-batch-gateway-gc bash examples/deploy-demo/deploy-maas.sh install` |

> `BATCH_RELEASE_VERSION` and `BATCH_DEV_VERSION` cannot be used together. See [Environment Variables](#environment-variables) for all available parameters.

If you change MaaS test user/password env vars and run `install` again on the **same** cluster, delete the OAuth htpasswd secret first so it is recreated: `oc delete secret htpass-secret -n openshift-config`.

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
| `GATEWAY_NAME` | `openshift-ai-inference` / `istio-gateway` / `maas-default-gateway` | per-script | Gateway resource name |
| `GATEWAY_NAMESPACE` | `openshift-ingress` / `istio-ingress` | per-script | Gateway namespace |
| `OPERATOR_TYPE` | `rhoai` | rhoai | `rhoai` or `odh` |
| `RHOAI_VERSION` | `3.4` | rhoai | RHOAI operator version |
| `RHOAI_CHANNEL` | `stable-${RHOAI_VERSION}` | rhoai | RHOAI subscription channel |
| `MODEL_NAME` | `facebook/opt-125m` | rhoai | Model name for simulator |
| `MODEL_REPLICAS` | `2` | rhoai | Number of model replicas |
| `MODEL_URI` | `hf://sshleifer/tiny-gpt2` | rhoai | Model URI for simulator |
| `SIM_IMAGE` | `ghcr.io/llm-d/llm-d-inference-sim:v0.7.1` | rhoai | Simulator image |
| `LLMD_VERSION` | `main` | k8s | llm-d git ref to install |
| `LLMD_RELEASE_POSTFIX` | `llmd` | k8s | Helm release postfix |
| `GATEWAY_LOCAL_PORT` | `8080` | k8s | Port-forward local port |
| `MODEL_NAME` | `random` | k8s | Model name for routing |
| `KUADRANT_VERSION` | `1.3.1` | k8s | Kuadrant Helm chart version |
| `MAAS_REF` | `main` | maas | MaaS git ref |
| `MAAS_MODEL_NAME` | `facebook/opt-125m` | maas | Model name for batch requests |
| `MAAS_TEST_USER` | `testuser` | maas | Test username |
| `MAAS_TEST_PASS` | `testpass` | maas | Test password |
| `MAAS_TEST_GROUP` | `tier-free-users` | maas | Test user group |
