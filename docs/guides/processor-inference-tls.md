# Processor TLS for HTTPS inference backends

The batch **processor** opens outbound HTTPS connections to the configured inference gateway (`globalInferenceGateway` or per-model `modelGateways`). This guide shows how to supply trust anchors and optional client certificates when the gateway uses TLS (for example vLLM or an Envoy gateway with TLS enabled).

For the **batch API server** listening with TLS and cert-manager, see the deployment guides ([Kubernetes](deploy-k8s.md), [RHOAI](deploy-rhoai.md), [MaaS](deploy-maas.md)).

## Behavior summary

| Scenario | Helm / config |
|----------|----------------|
| Public CA (e.g. well-known TLS on the internet) | `https://...` URL, `tlsInsecureSkipVerify: false`, leave CA / client cert fields unset — the processor uses the container **system root CAs**. |
| Private / corporate CA | Mount the CA PEM into the processor pod and set `tlsCaCertFile` to the **in-container path**. |
| Mutual TLS (client presents a cert) | Mount client cert and key PEM files and set `tlsClientCertFile` and `tlsClientKeyFile` (both required together). |
| Demos or self-signed only (insecure) | `tlsInsecureSkipVerify: true` — **testing only**; do not use in production. |

Rendered application config uses snake_case (`tls_ca_cert_file`, …). Helm values use camelCase (`tlsCaCertFile`, …), as in [`values.yaml`](../../charts/batch-gateway/values.yaml).

## Mounting certificate files

Certificate paths in config must exist **inside the processor container**. The Helm chart exposes:

- `processor.volumes` — extra pod volumes (e.g. `secret`, `projected`).
- `processor.volumeMounts` — mounts for the processor container (e.g. `mountPath: /etc/inference-tls`).

The processor image uses a **read-only root filesystem**; additional mounts are the supported way to bring PEM files into the pod.

## Scenario guides

Step-by-step TLS wiring for common cases. See [Behavior summary](#behavior-summary) for a quick comparison.

Think in two layers: **where** config lives (`globalInferenceGateway` vs `modelGateways`), then **what** TLS you need (custom CA, optional mTLS client cert). The sections below follow that order.

### Routing: `globalInferenceGateway` vs `modelGateways`

These two are **mutually exclusive** — if both are set, processor config validation fails.

| Use | When |
|-----|------|
| **`processor.config.globalInferenceGateway`** | Every model uses the **same** gateway URL and the **same** TLS settings (same CA / mTLS material). Mount volumes once; set `tlsCaCertFile`, `tlsClientCertFile`, etc. on that single block. |
| **`processor.config.modelGateways`** | Models need **different** gateway URLs and/or **different** TLS material. Each model key has a full gateway config; there is **no inheritance** between entries (see [Per-model layout patterns](#per-model-layout-patterns)). |

TLS field names are the same in either mode: `tlsInsecureSkipVerify`, `tlsCaCertFile`, `tlsClientCertFile`, `tlsClientKeyFile`.

### Custom CA (private CA)

1. Create a Secret in the processor namespace with your CA bundle (PEM). Example (names are placeholders — use your namespace and Secret name, and the same name in `secretName` below):

```bash
kubectl create secret generic myinference-ca \
  -n batch-api \
  --from-file=ca.crt=./ca.crt
```

2. Install or upgrade the release with a volume, a mount, `https` URL, and `tlsCaCertFile`:

```bash
helm upgrade --install batch-gateway ./charts/batch-gateway \
  --namespace batch-api \
  --set "processor.config.modelGateways.mymodel.url=https://gateway.batch-api.svc.cluster.local:8443" \
  --set "processor.config.modelGateways.mymodel.tlsInsecureSkipVerify=false" \
  --set "processor.config.modelGateways.mymodel.tlsCaCertFile=/etc/inference-tls/ca.crt" \
  --set-json 'processor.volumes=[{"name":"inference-tls","secret":{"secretName":"myinference-ca"}}]' \
  --set-json 'processor.volumeMounts=[{"name":"inference-tls","mountPath":"/etc/inference-tls","readOnly":true}]'
```

Adjust other required processor settings (database, file storage, `modelGateways` keys for your real model names, etc.) — the snippet above only shows TLS-related fragments. If you use **`globalInferenceGateway`** instead, put the same `url`, `tlsInsecureSkipVerify`, `tlsCaCertFile`, and retry fields on that single object (see [Routing](#routing-globalinferencegateway-vs-modelgateways)).

**Values file (often clearer than long `--set` lines):**

```yaml
processor:
  volumes:
    - name: inference-tls
      secret:
        secretName: myinference-ca
  volumeMounts:
    - name: inference-tls
      mountPath: /etc/inference-tls
      readOnly: true
  config:
    modelGateways:
      mymodel:
        url: "https://gateway.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-tls/ca.crt
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
```

If the Secret uses different filenames, either rename keys when creating the Secret (`--from-file=ca.crt=./your-ca.pem`) or use `secret.items` under the volume to map keys to paths.

### mTLS (client certificate)

Add a **client certificate and private key** so the processor presents an identity to the gateway. This stacks on top of normal TLS trust: you still need either system CAs, `tlsCaCertFile` (typical for a private CA), or `tlsInsecureSkipVerify` (non-production only).

1. Put cert and key PEMs in a Secret, mount them (same `processor.volumes` / `volumeMounts` pattern as [Custom CA](#custom-ca-private-ca)).
2. Set **both** `tlsClientCertFile` and `tlsClientKeyFile` on the gateway block you use (`modelGateways.<model>` or `globalInferenceGateway`). The processor rejects config where only one of the two is set.

Example — only the TLS-related keys (reuse your existing `url`, `requestTimeout`, retries, and volume mounts):

```yaml
# Under modelGateways.mymodel OR under globalInferenceGateway:
tlsInsecureSkipVerify: false
tlsCaCertFile: /etc/inference-mtls/ca.crt   # optional; typical with a private CA
tlsClientCertFile: /etc/inference-mtls/tls.crt  # Secret key names may differ
tlsClientKeyFile: /etc/inference-mtls/tls.key
```

### Per-model layout patterns

Use this when you chose **`modelGateways`** and need more than one model entry or different TLS material per backend. Each entry is **independent** — there is no inheritance between models.

Typical patterns:

- **Different CAs or mTLS identities**: separate Secrets, separate volume names, and distinct `mountPath` values (e.g. `/etc/inference-tls/model-a`, `/etc/inference-tls/model-b`), with each entry’s `tlsCaCertFile` / client paths pointing at the right directory.
- **Same CA and client cert for several models, but different gateway URLs**: one mounted directory; repeat the same `tlsCaCertFile` / client paths in each `modelGateways` entry. If URL and TLS are **identical for every model**, use [`globalInferenceGateway`](#routing-globalinferencegateway-vs-modelgateways) instead of duplicating.

Model keys that contain `/` (for example `org/model`) must be **quoted** in YAML:

```yaml
processor:
  config:
    modelGateways:
      "acme/llama-3":
        url: "https://..."
        tlsCaCertFile: /etc/inference-tls/ca.crt
        # ... other required fields ...
```

## Relationship to `tlsInsecureSkipVerify`

Demos in [deploy-k8s.md](deploy-k8s.md) often set `tlsInsecureSkipVerify=true` for in-cluster gateways with self-signed certificates. For production, prefer mounting the real CA (or using a public URL with system roots) and keep `tlsInsecureSkipVerify: false`.

## Further reading

- Processor gateway configuration and client pooling: [batch_processor_architecture.md](../design/batch_processor_architecture.md#gateway-routing)
- Helm processor deployment (volumes / volumeMounts): [`processor-deployment.yaml`](../../charts/batch-gateway/templates/processor-deployment.yaml)

## Future improvement

A future chart enhancement could add convenience values (for example referencing a Secret name per gateway) so users do not hand-wire `processor.volumes` / `processor.volumeMounts` and paths. That is optional and can be tracked separately from this guide.
