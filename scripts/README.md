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
