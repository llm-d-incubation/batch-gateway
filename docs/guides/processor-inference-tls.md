# Processor TLS for HTTPS inference backends

The batch **processor** opens outbound HTTPS connections to the configured inference gateway (`globalInferenceGateway` or per-model `modelGateways`). This guide shows how to supply trust anchors and optional client certificates when the gateway uses TLS (for example vLLM or an Envoy gateway with TLS enabled).

For the **batch API server** listening with TLS and cert-manager, see the deployment guide ([Kubernetes](deploy-k8s.md)).

## Behavior summary

| Scenario | Helm / config |
|----------|----------------|
| Public CA (e.g. well-known TLS on the internet) | `https://...` URL, `tlsInsecureSkipVerify: false`, leave CA / client cert fields unset — the processor uses the container **system root CAs**. No extra config needed. |
| [Private / corporate CA](#custom-ca-private-ca) | Mount the CA PEM into the processor pod and set `tlsCaCertFile` to the **in-container path**. |
| [Mutual TLS](#mtls-client-certificate) (client presents a cert) | Mount client cert and key PEM files and set `tlsClientCertFile` and `tlsClientKeyFile` (both required together). |
| [cert-manager](https://cert-manager.io/)-managed Secrets | Let cert-manager issue or store PEMs in a standard TLS Secret; mount it like any other Secret and set the same `tls*` paths (see [Certificates from cert-manager](#certificates-from-cert-manager)). |
| [Demos or self-signed only](#relationship-to-tlsinsecureskipverify) (insecure) | `tlsInsecureSkipVerify: true` — **testing only**; do not use in production. |

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

1. Put client cert, key, and (optional) CA PEMs in a Secret — filenames below match typical cert-manager TLS Secrets (`tls.crt`, `tls.key`, `ca.crt`):

```bash
kubectl create secret generic myinference-mtls \
  -n batch-api \
  --from-file=tls.crt=./client.crt \
  --from-file=tls.key=./client.key \
  --from-file=ca.crt=./ca.crt
```

2. Install or upgrade with volume mounts and **both** client fields (and optional CA) on the gateway block. The processor rejects config where only one of `tlsClientCertFile` / `tlsClientKeyFile` is set.

```bash
helm upgrade --install batch-gateway ./charts/batch-gateway \
  --namespace batch-api \
  --set "processor.config.modelGateways.mymodel.url=https://gateway.batch-api.svc.cluster.local:8443" \
  --set "processor.config.modelGateways.mymodel.tlsInsecureSkipVerify=false" \
  --set "processor.config.modelGateways.mymodel.tlsCaCertFile=/etc/inference-mtls/ca.crt" \
  --set "processor.config.modelGateways.mymodel.tlsClientCertFile=/etc/inference-mtls/tls.crt" \
  --set "processor.config.modelGateways.mymodel.tlsClientKeyFile=/etc/inference-mtls/tls.key" \
  --set-json 'processor.volumes=[{"name":"inference-mtls","secret":{"secretName":"myinference-mtls"}}]' \
  --set-json 'processor.volumeMounts=[{"name":"inference-mtls","mountPath":"/etc/inference-mtls","readOnly":true}]'
```

Add the other required `modelGateways` fields (`requestTimeout`, `maxRetries`, `initialBackoff`, `maxBackoff`) via more `--set` lines or a values file — same as [Custom CA](#custom-ca-private-ca). For **`globalInferenceGateway`**, use `processor.config.globalInferenceGateway.*` keys instead of `modelGateways.mymodel.*`.

**Values file (mTLS fragment):**

```yaml
processor:
  volumes:
    - name: inference-mtls
      secret:
        secretName: myinference-mtls
  volumeMounts:
    - name: inference-mtls
      mountPath: /etc/inference-mtls
      readOnly: true
  config:
    modelGateways:
      mymodel:
        url: "https://gateway.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-mtls/ca.crt
        tlsClientCertFile: /etc/inference-mtls/tls.crt
        tlsClientKeyFile: /etc/inference-mtls/tls.key
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
```

### Certificates from cert-manager

This is not a separate TLS method — it is a **variant of [Custom CA](#custom-ca-private-ca) and [mTLS](#mtls-client-certificate)** where cert-manager creates and rotates the Secret instead of you managing PEM files by hand. The processor has no direct cert-manager integration; it only reads PEM files from paths inside the container. Mount the cert-manager-managed Secret with `processor.volumes` / `processor.volumeMounts` and set the same `tls*` fields.

Install the cert-manager controller if you do not already have it (for example the steps in [deploy-k8s.md § 3.1](deploy-k8s.md#31-install-cert-manager)).

**Trust anchor (gateway private CA)**
Use whatever your cluster provides: a Secret that holds the CA bundle PEM cert-manager or your platform keeps up to date, an exported CA from your `Issuer` / `ClusterIssuer`, or a dedicated bundle Secret. Mount it and set `tlsCaCertFile` to the in-container path of the CA PEM.

**Client certificate (mTLS)**
Create a cert-manager `Certificate` that requests credentials your inference gateway accepts (subject, `dnsNames`, `uris`, etc. are **gateway-specific**). The resulting TLS Secret typically contains:

- `tls.crt` — client certificate chain (map to `tlsClientCertFile`)
- `tls.key` — private key (`tlsClientKeyFile`)
- `ca.crt` — optional; issuing CA or bundle for trust (`tlsCaCertFile` when you need a private CA)

Example (placeholders only — adjust `issuerRef`, names, and SANs for your environment):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: processor-inference-client
  namespace: batch-api
spec:
  secretName: processor-inference-client-tls
  duration: 720h
  renewBefore: 168h
  commonName: batch-processor-inference-client
  # dnsNames: [...]
  # uris: [...]
  issuerRef:
    name: my-private-ca-issuer
    kind: ClusterIssuer
    group: cert-manager.io
```

Mount `processor-inference-client-tls` at e.g. `/etc/inference-mtls` and point the gateway block at `tls.crt` / `tls.key` / `ca.crt` (or use `items` to rename keys on disk).

**Helm example** — mount the cert-manager-managed Secret and set TLS paths (same pattern as [mTLS](#mtls-client-certificate), only the Secret name changes):

```bash
helm upgrade --install batch-gateway ./charts/batch-gateway \
  --namespace batch-api \
  --set "processor.config.modelGateways.mymodel.url=https://gateway.batch-api.svc.cluster.local:8443" \
  --set "processor.config.modelGateways.mymodel.tlsInsecureSkipVerify=false" \
  --set "processor.config.modelGateways.mymodel.tlsCaCertFile=/etc/inference-mtls/ca.crt" \
  --set "processor.config.modelGateways.mymodel.tlsClientCertFile=/etc/inference-mtls/tls.crt" \
  --set "processor.config.modelGateways.mymodel.tlsClientKeyFile=/etc/inference-mtls/tls.key" \
  --set-json 'processor.volumes=[{"name":"inference-mtls","secret":{"secretName":"processor-inference-client-tls"}}]' \
  --set-json 'processor.volumeMounts=[{"name":"inference-mtls","mountPath":"/etc/inference-mtls","readOnly":true}]'
```

**Renewal**
TLS material is loaded when the processor builds its HTTP clients (effectively **at process startup**). When cert-manager rotates the Secret in place, **roll or restart processor pods** so new PEMs are read.

### Per-model layout patterns

Use this when you chose **`modelGateways`** and need more than one model entry or different TLS material per backend. Each entry is **independent** — there is no inheritance between models.

Typical patterns:

- **Different CAs or mTLS identities**: separate Secrets, separate volume names, and distinct `mountPath` values (e.g. `/etc/inference-tls/model-a`, `/etc/inference-tls/model-b`), with each entry’s `tlsCaCertFile` / client paths pointing at the right directory.
- **Same CA and client cert for several models, but different gateway URLs**: one mounted directory; repeat the same `tlsCaCertFile` / client paths in each `modelGateways` entry. If URL and TLS are **identical for every model**, use [`globalInferenceGateway`](#routing-globalinferencegateway-vs-modelgateways) instead of duplicating.

Model keys that contain `/` (for example `org/model`) must be **quoted** in YAML.

**Helm (`--set`)** — use two models with **distinct** Secrets and mount paths (TLS-related keys only; add retries/timeouts as needed):

```bash
helm upgrade --install batch-gateway ./charts/batch-gateway \
  --namespace batch-api \
  --set "processor.config.modelGateways.model-a.url=https://gateway-a.batch-api.svc.cluster.local:8443" \
  --set "processor.config.modelGateways.model-a.tlsInsecureSkipVerify=false" \
  --set "processor.config.modelGateways.model-a.tlsCaCertFile=/etc/inference-tls/model-a/ca.crt" \
  --set "processor.config.modelGateways.model-b.url=https://gateway-b.batch-api.svc.cluster.local:8443" \
  --set "processor.config.modelGateways.model-b.tlsInsecureSkipVerify=false" \
  --set "processor.config.modelGateways.model-b.tlsCaCertFile=/etc/inference-tls/model-b/ca.crt" \
  --set-json 'processor.volumes=[{"name":"inference-tls-a","secret":{"secretName":"inference-ca-model-a"}},{"name":"inference-tls-b","secret":{"secretName":"inference-ca-model-b"}}]' \
  --set-json 'processor.volumeMounts=[{"name":"inference-tls-a","mountPath":"/etc/inference-tls/model-a","readOnly":true},{"name":"inference-tls-b","mountPath":"/etc/inference-tls/model-b","readOnly":true}]'
```

Create `inference-ca-model-a` / `inference-ca-model-b` (or your names) in `batch-api` before upgrading. For model names with `/`, prefer a **values file** so you do not fight shell escaping; example:

```yaml
processor:
  volumes:
    - name: inference-tls-a
      secret:
        secretName: inference-ca-model-a
    - name: inference-tls-b
      secret:
        secretName: inference-ca-model-b
  volumeMounts:
    - name: inference-tls-a
      mountPath: /etc/inference-tls/model-a
      readOnly: true
    - name: inference-tls-b
      mountPath: /etc/inference-tls/model-b
      readOnly: true
  config:
    modelGateways:
      model-a:
        url: "https://gateway-a.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-tls/model-a/ca.crt
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
      model-b:
        url: "https://gateway-b.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-tls/model-b/ca.crt
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
      "acme/llama-3":                                  # shares CA volume with model-a
        url: "https://gateway-c.batch-api.svc.cluster.local:8443"
        tlsInsecureSkipVerify: false
        tlsCaCertFile: /etc/inference-tls/model-a/ca.crt
        requestTimeout: "5m"
        maxRetries: 3
        initialBackoff: "1s"
        maxBackoff: "60s"
```

## Relationship to `tlsInsecureSkipVerify`

Demos in [deploy-k8s.md](deploy-k8s.md) often set `tlsInsecureSkipVerify=true` for in-cluster gateways with self-signed certificates. For production, prefer mounting the real CA (or using a public URL with system roots) and keep `tlsInsecureSkipVerify: false`.

## Further reading

- Processor gateway configuration and client pooling: [batch_processor_architecture.md](../design/batch_processor_architecture.md#gateway-routing)
- Helm processor deployment (volumes / volumeMounts): [`processor-deployment.yaml`](../../charts/batch-gateway/templates/processor-deployment.yaml)
- cert-manager concepts and `Certificate` API: [cert-manager documentation](https://cert-manager.io/docs/)

## Future improvement

A future chart enhancement could add convenience values (for example referencing a Secret name per gateway) so users do not hand-wire `processor.volumes` / `processor.volumeMounts` and paths. That is optional and can be tracked separately from this guide.
