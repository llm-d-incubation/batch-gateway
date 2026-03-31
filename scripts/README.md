# Scripts

## Development


| Script          | Description                                                                                                                                      |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `dev-deploy.sh` | Local development deployment. Builds images, creates kind cluster, installs Redis/PostgreSQL/Jaeger, deploys batch-gateway with TLS and tracing. |


## Release


| Script                  | Description                                                                                                                                               |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `generate-release.sh`   | Creates and pushes a `v*.*.*` tag from `main`, which triggers the GitHub release workflows. See [managing releases](../docs/guides/managing-releases.md). |
| `publish-helm-chart.sh` | Packages the helm chart for a tag and pushes it to `oci://ghcr.io/llm-d-incubation/charts` (invoked as `make publish-helm-chart` in release CI).          |


## Demo (`demo/`)

Demo scripts for deploying batch-gateway on Kubernetes/OpenShift with Istio, GAIE, and optional Kuadrant/MaaS integration.


| Script                                   | Description                                                                                                                                                                                                      |
| ---------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `demo/common.sh`                         | Shared functions used by all demo scripts (Istio, GAIE, vLLM, Gateway, helpers).                                                                                                                                 |
| `demo/deploy.sh`                         | Deploy batch-gateway with Istio + GAIE + end-to-end TLS (cert-manager). No auth or rate limiting. See [deployment guide](../docs/guides/deployment.md).                                                          |
| `demo/deploy-with-kuadrant-apikey.sh`    | Deploy with Kuadrant using API Key authentication + OPA authorization + rate limiting. See [kuadrant integration guide](../docs/guides/kuadrant-integration.md).                                                 |
| `demo/deploy-with-kuadrant-satoken.sh`   | Deploy with Kuadrant using ServiceAccount Token authentication + SubjectAccessReview authorization. Works on any Kubernetes cluster.                                                                             |
| `demo/deploy-with-kuadrant-usertoken.sh` | Deploy with Kuadrant using OpenShift User Token authentication + SubjectAccessReview authorization. Requires OpenShift.                                                                                          |
| `demo/deploy-with-maas.sh`               | Deploy on MaaS (Models-as-a-Service) platform with API key authentication, request rate limiting, and token rate limiting. Requires OpenShift. See [MaaS integration guide](../docs/guides/maas-integration.md). |


### Usage

Each demo script supports `install`, `test`, `uninstall`, and `help` commands:

```bash
# Deploy without auth (Istio + GAIE + batch-gateway)
scripts/demo/deploy.sh install
scripts/demo/deploy.sh test
scripts/demo/deploy.sh uninstall

# Deploy with Kuadrant API Key auth
scripts/demo/deploy-with-kuadrant-apikey.sh install
scripts/demo/deploy-with-kuadrant-apikey.sh test
scripts/demo/deploy-with-kuadrant-apikey.sh uninstall

# Deploy on MaaS platform (OpenShift only)
scripts/demo/deploy-with-maas.sh install
scripts/demo/deploy-with-maas.sh test
scripts/demo/deploy-with-maas.sh uninstall

# See all options
scripts/demo/deploy.sh help
```
