# Demo Scripts

One-click deployment scripts for batch-gateway on different platforms. Each script supports `install`, `test`, and `uninstall` commands.

## 1) Overview

| Script | Cluster | Description |
|--------|---------|-------------|
| `deploy-k8s.sh` | Kubernetes/OpenShift | Deploys llm-d + Kuadrant + batch-gateway |
| `deploy-rhoai.sh` | OpenShift | Deploys batch-gateway on top of RHOAI + RHCL |
| `deploy-maas.sh` | OpenShift | Deploys batch-gateway on top of MaaS |

**Prerequisites**: You must be logged in to the target cluster before running any script. Use `kubectl config current-context` (or `oc whoami` on OpenShift) to verify.

## 2) deploy-k8s.sh

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
```


## 3) deploy-rhoai.sh

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
```


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
```

If you change MaaS test user/password env vars and run `install` again on the **same** cluster, delete the OAuth htpasswd secret first so it is recreated: `oc delete secret htpass-secret -n openshift-config`.

## Installation Modes

**Local chart (default):**
```bash
bash examples/deploy-demo/deploy-rhoai.sh install
```

**Install from a specific commit (chart + image):**
```bash
BATCH_DEV_VERSION=1f925ff \
  bash examples/deploy-demo/deploy-rhoai.sh install
```

**Install from released OCI Helm chart:**
```bash
BATCH_RELEASE_VERSION=v1.0.0 \
  bash examples/deploy-demo/deploy-rhoai.sh install
```

> `BATCH_RELEASE_VERSION` and `BATCH_DEV_VERSION` cannot be used together.

## Environment Variables

| Variable | Default | Scope | Description |
|----------|---------|-------|-------------|
| `BATCH_HELM_RELEASE` | `batch-gateway` | all | Helm release name |
| `BATCH_RELEASE_VERSION` | — | all | Install from released OCI chart (e.g. `v1.0.0`). Cannot be used with `BATCH_DEV_VERSION` |
| `BATCH_DEV_VERSION` | `local` | all | Image tag / commit SHA. `local` uses local chart + `latest` image. Cannot be used with `BATCH_RELEASE_VERSION` |
| `BATCH_DB_TYPE` | `postgresql` | all | Database backend: `postgresql` or `redis` |
| `BATCH_STORAGE_TYPE` | `s3` | all | File storage: `fs` or `s3` |
| `BATCH_NAMESPACE` | `batch-api` | all | Namespace for batch-gateway |
| `LLM_NAMESPACE` | `llm` | all | Namespace for model serving |
| `LLMD_VERSION` | `main` | k8s | llm-d git ref to install |
| `LLMD_RELEASE_POSTFIX` | `llmd` | k8s | Helm release postfix |
| `GATEWAY_LOCAL_PORT` | `8080` | k8s | Port-forward local port |
| `MODEL_NAME` | `random` | k8s | Model name for routing |
| `OPERATOR_TYPE` | `rhoai` | rhoai | `rhoai` or `odh` |
| `MODEL_NAME` | `facebook/opt-125m` | rhoai | Model name for simulator |
| `MODEL_REPLICAS` | `2` | rhoai | Number of model replicas |
| `SIM_IMAGE` | `ghcr.io/llm-d/llm-d-inference-sim:v0.7.1` | rhoai | Simulator image |
| `MAAS_REF` | `main` | maas | MaaS git ref |
| `MAAS_TEST_USER` | `testuser` | maas | Test username |
| `MAAS_TEST_PASS` | `testpass` | maas | Test password |
| `MAAS_TEST_GROUP` | `tier-free-users` | maas | Test user group |
