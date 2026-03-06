# E2E Tests

End-to-end tests for the batch-gateway. They run against a live deployment and cover the `/v1/files` and `/v1/batches` REST APIs.

## Prerequisites

- `kubectl`, `helm`, `kind`, Docker or Podman
- Go 1.25+

## 1. Deploy the server

```bash
make dev-deploy
```

This script:
1. Creates a kind cluster if none is reachable (`KIND_CLUSTER_NAME`)
2. Builds and loads the apiserver and processor container images
3. Installs Redis via Helm
4. Installs PostgreSQL via Helm
5. Deploys a vLLM simulator as the inference backend
6. Deploys batch-gateway via Helm
7. Starts a `kubectl port-forward` in the background at `http://localhost:8000`

**Environment variables**

| Variable              | Default                                    | Description                                        |
|-----------------------|--------------------------------------------|--------------------------------------------------- |
| `KIND_CLUSTER_NAME`   | `batch-gateway-dev`                        | Kind cluster name (created if needed)              |
| `HELM_RELEASE`        | `batch-gateway`                            | Helm release name                                  |
| `NAMESPACE`           | `default`                                  | Kubernetes namespace                               |
| `DEV_VERSION`         | `0.0.1`                                    | Image tag to build and deploy                      |
| `LOCAL_PORT`          | `8000`                                     | Local port for the port-forward                    |
| `LOG_VERBOSITY`       | `4`                                        | klog verbosity for apiserver and processor         |
| `POSTGRESQL_RELEASE`  | `postgresql`                               | Helm release name for PostgreSQL                   |
| `POSTGRESQL_PASSWORD` | `postgres`                                 | PostgreSQL admin password                          |
| `INFERENCE_API_KEY`   | `dummy-api-key`                            | API key written to the app secret                  |
| `S3_SECRET_ACCESS_KEY`| `dummy-s3-secret-access-key`               | S3 secret access key written to the app secret     |
| `APP_SECRET_NAME`     | `<HELM_RELEASE>-secrets`                   | Name of the Kubernetes secret created by the script|
| `FILES_PVC_NAME`      | `<HELM_RELEASE>-files`                     | Name of the PVC created for file storage           |
| `VLLM_SIM_NAME`       | `vllm-sim`                                 | Name of the vLLM simulator deployment              |
| `VLLM_SIM_MODEL`      | `sim-model`                                | Model name served by the simulator                 |
| `VLLM_SIM_IMAGE`      | `ghcr.io/llm-d/llm-d-inference-sim:latest` | vLLM simulator image                               |

Example with overrides:

```bash
NAMESPACE=dev LOCAL_PORT=9000 LOG_VERBOSITY=5 make dev-deploy
```

## 2. Run the tests

```bash
make test-e2e
```

**Environment variables**

| Variable         | Default                  | Description                              |
|------------------|--------------------------|------------------------------------------|
| `TEST_BASE_URL`  | `http://localhost:8000`  | Base URL of the running API server       |
| `TEST_TENANT_ID` | `default`                | Tenant ID sent in the `X-MaaS-Username` header |

Example with overrides:

```bash
TEST_BASE_URL=http://localhost:9000 TEST_TENANT_ID=my-tenant make test-e2e
```

## 3. Cleanup

```bash
helm uninstall batch-gateway -n default
helm uninstall redis -n default
helm uninstall postgresql -n default
kubectl delete deployment,svc vllm-sim -n default
kubectl delete secret batch-gateway-secrets -n default
kubectl delete pvc batch-gateway-files -n default

# If using a kind cluster:
kind delete cluster --name batch-gateway-dev
```
