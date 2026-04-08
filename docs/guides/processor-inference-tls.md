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

## Custom CA (private CA)

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

Adjust other required processor settings (database, file storage, `modelGateways` keys for your real model names, etc.) — the snippet above only shows TLS-related fragments.

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

## mTLS (client certificate)

Use a Secret (or Secrets) that hold the client certificate and private key PEM files, mount them, and set both client fields:

```yaml
processor:
  volumes:
    - name: inference-mtls
      secret:
        secretName: myinference-client
  volumeMounts:
    - name: inference-mtls
      mountPath: /etc/inference-mtls
      readOnly: true
  config:
    modelGateways:
      mymodel:
        url: "https://gateway.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-mtls/ca.crt        # optional but typical for private CA
        tlsClientCertFile: /etc/inference-mtls/tls.crt  # or client.pem per your Secret keys
        tlsClientKeyFile: /etc/inference-mtls/tls.key
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
```

The processor validates at startup that `tls_client_cert_file` and `tls_client_key_file` are both set or both omitted.

## `globalInferenceGateway`

When **all** models share one gateway URL and the same TLS material, set TLS on `processor.config.globalInferenceGateway` instead of per-model entries. The same fields apply: `tlsInsecureSkipVerify`, `tlsCaCertFile`, `tlsClientCertFile`, `tlsClientKeyFile`. Mount volumes once; point paths at the mounted files.

> **Note:** If `globalInferenceGateway` is set, `modelGateways` is ignored for routing. Use per-model mode when models need different URLs or TLS settings.

## Per-model TLS (different gateways or different certs)

Each `modelGateways` entry is **independent** — there is no inheritance between models. Typical patterns:

- **Different CAs or mTLS identities**: separate Secrets, separate volume names, and distinct `mountPath` values (e.g. `/etc/inference-tls/model-a`, `/etc/inference-tls/model-b`), with each entry’s `tlsCaCertFile` / client paths pointing at the right directory.
- **Same CA, same client cert for all backends**: one mounted directory; duplicate the same `tlsCaCertFile` / client paths in each entry that uses that gateway.

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
