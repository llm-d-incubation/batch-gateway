# Scripts

## Development


| Script          | Description                                                                                                                                      |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `dev-deploy.sh` | Local development deployment. Builds images, creates kind cluster, installs Redis/PostgreSQL/Jaeger, deploys batch-gateway with TLS and tracing. |


## Release


| Script                  | Description                                                                                                                                               |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `generate-release.sh`   | Creates a `v*.*.*` tag on GitHub from a given commit SHA via the `gh` CLI, plus a `release-<tag>` branch for final releases (no local git changes). Triggers the Release workflow. See [managing releases](../docs/guides/managing-releases.md). |
| `publish-helm-chart.sh` | Packages the helm chart for a tag and pushes it to `oci://ghcr.io/llm-d/charts` (invoked as `make publish-helm-chart` in release CI).          |
