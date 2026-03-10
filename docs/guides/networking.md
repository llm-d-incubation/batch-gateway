# Networking

## 1. API Server

The API Server runs two listeners on separate ports. The API port serves REST endpoints with optional TLS, while the observability port serves health, readiness, and metrics over plain HTTP.

### 1.1 TLS Configuration

TLS is disabled by default. Choose **one** of the following options to enable it.

#### Option A: Pre-existing Kubernetes Secret (`secretName`)

Use this when you already have a `kubernetes.io/tls` Secret in the cluster, for example one managed by HashiCorp Vault, an enterprise PKI, or shared across multiple services.

```yaml
apiserver:
  tls:
    enabled: true
    secretName: "my-existing-tls-secret"
```

The Secret must contain `tls.crt` and `tls.key` entries:

```bash
kubectl create secret tls my-existing-tls-secret \
  --cert=server.crt \
  --key=server.key
```

#### Option B: cert-manager (`certManager`)

Use this for automated certificate issuance and renewal via [cert-manager](https://cert-manager.io/). cert-manager must be installed in the cluster.

```yaml
apiserver:
  tls:
    enabled: true
    certManager:
      enabled: true
      issuerName: "letsencrypt-prod"
      issuerKind: ClusterIssuer        # or Issuer
      dnsNames:
        - "batch-api.example.com"
```

The chart creates a `Certificate` resource; cert-manager handles issuance, storage, and rotation of the TLS Secret automatically.

### 1.2 Endpoints

#### API port (default 8000)

Base URL: `http(s)://<host>:8000` — scheme depends on TLS configuration.

| Endpoint | Description |
|---|---|
| `POST /v1/batches` | Create a batch |
| `GET /v1/batches/{id}` | Get batch status |
| `POST /v1/batches/{id}/cancel` | Cancel a batch |
| `POST /v1/files` | Upload a file |
| `GET /v1/files/{id}` | Get file metadata |
| `GET /v1/files/{id}/content` | Download file content |

#### Observability port (default 8081)

Base URL: `http://<host>:8081` — always plain HTTP.

| Endpoint | Description |
|---|---|
| `GET /health` | Liveness check |
| `GET /ready` | Readiness check |
| `GET /metrics` | Prometheus metrics |

## 2. Processor

The Processor is a background worker and only serves observability endpoints. It does not support TLS.

### 2.1 Endpoints

#### Observability port (default 9090) — always HTTP

| Endpoint | Description |
|---|---|
| `GET /health` | Liveness check |
| `GET /ready` | Readiness check |
| `GET /metrics` | Prometheus metrics |
