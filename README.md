# Batch Gateway

[![Go Version](https://img.shields.io/badge/Go-1.25-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

## Overview

Batch Gateway is a high-performance system for processing large-scale batch inference jobs in Kubernetes environments. It provides an OpenAI-compatible API for submitting, tracking, and managing batch inference jobs.

The system is designed to facilitate efficient processing of batch workloads in combination with interactive workloads. It minimizes interference with interactive workloads while satisfying batch jobs' service level objectives (SLOs).

### Use Cases

- Inferencing large datasets
- Generating embeddings for large corpora
- Model evaluations and testing
- Offline analysis and batch processing
- Cost-optimized inference using differential billing for batch vs. interactive workloads

## Key Features

### Batch Processing

- **OpenAI API Compatibility**: Full schema parity with OpenAI's `/v1/batches` and `/v1/files` endpoints
- **Large-Scale Processing**: Support for up to 50,000 requests per job
- **Progress Tracking**: Real-time job status progress updates
- **Job Management and Control**: Enables to manage and control batch jobs, before, during, and after their processing
- **Model-Aware Scheduling**: Groups and orders requests by model and system prompt for optimal downstream utilization
- **Intelligent inference dispatching**: Monitors downstream metrics to determine the flow volume of batch inference requests

### System Design

- **Deployment Flexibility**: Separate API server, batch processor, and request dispatcher components for independent scaling
- **Pluggable Storage Backends**: Supports pluggable storage backends for files, metadata, and queues
- **Fault Tolerance**: Automatic recovery from batch processor crashes

### Operations

- **Kubernetes Native**: Helm charts with OpenShift compatibility
- **Observability**: Prometheus metrics and Open Telemetry integration
- **Health Checks**: Liveness and readiness probes for the system components
- **Security**: TLS support, non-root execution, capability dropping, read-only filesystem

## Architecture

### High-Level System Design

![Architecture Diagram](docs/design/diagrams/arch_1.png)

#### Components

1. **API Server** ([`batch-gateway-apiserver`](cmd/apiserver))
   - Handles REST API requests for batch job submission, status queries, and file management
   - Manages job metadata in PostgreSQL and job queue in Redis
   - Exposes OpenAI-compatible `/v1/batches` and `/v1/files` endpoints
   - Provides authentication, validation, and rate limiting

2. **Batch Processor** ([`batch-gateway-processor`](cmd/batch-processor))
   - Polls priority queue for pending batch jobs
   - Executes two-phase processing pipeline:
     - **Phase 1 (Ingestion)**: Streams input files, builds per-model execution plans sorted by prefix hash
     - **Phase 2 (Execution)**: Dispatches requests using per-model goroutines with semaphore-based concurrency control
   - Writes results to output files and updates job status
   - Supports configurable worker pools for parallel job processing

3. **Data Layer**
   - **PostgreSQL**: Job and file metadata storage
   - **Redis**: Priority queue for job scheduling and event channels for job control
   - **Object Storage**: File storage (S3-compatible or local filesystem)

#### Processing Flow

```text
User → API Server → PostgreSQL (metadata) + Redis (queue) + S3 (input file)
                         ↓
                   Priority Queue
                         ↓
                  Batch Processor (polls jobs)
                         ↓
              Phase 1: Ingestion & Plan Building
                  - Stream input file
                  - Parse model IDs and system prompts
                  - Build per-model plans (sorted by prefix hash)
                  - Write plans to local disk
                         ↓
              Phase 2: Scheduling & Execution
                  - Launch per-model goroutines
                  - Acquire global & per-model semaphores
                  - Read requests from plan files
                  - Send to inference gateway
                  - Write results to output file
                         ↓
                   Upload Results to S3
                         ↓
                  Update Job Status in PostgreSQL
```

### Scheduler Architecture

The processor uses a **per-model goroutine architecture** with two-level semaphore-based concurrency control:

![Dispatcher Architecture](docs/design/diagrams/dispatch_1.png)

- **Global Semaphore** (`GlobalConcurrency`): Limits total in-flight requests across all workers (default: 100)
- **Per-Model Semaphore** (`PerModelMaxConcurrency`): Limits concurrent requests per model (default: 10)
- **Prefix Hash Ordering**: Requests are sorted by system prompt hash to maximize downstream KV cache hits

### Design Documents

For detailed architecture information, see:

- [Batch Inference Architecture](docs/design/batch_inference_architecture.md) - Overall system design and requirements
- [Batch Processor Architecture](docs/design/batch_processor_architecture.md) - Detailed processor design and scheduling
- [Batch Dispatcher](docs/design/batch-dispatcher.md) - Flow control and dispatch budget mechanism
- [Resource Lifecycle](docs/design/resource-lifecycle.md) - Job and file state management

## Repository Structure

```text
batch-gateway/
├── cmd/                          # Application entry points
│   ├── apiserver/                # API server binary
│   └── batch-processor/          # Batch processor binary
├── internal/                     # Private application code
│   ├── apiserver/                # API server implementation
│   │   ├── handlers/             # HTTP request handlers
│   │   ├── middleware/           # HTTP middleware
│   │   └── server/               # Server initialization
│   ├── processor/                # Batch processor implementation
│   │   ├── worker/               # Worker pool and job execution
│   │   ├── planner/              # Phase 1: Plan building
│   │   ├── executor/             # Phase 2: Request execution
│   │   └── metrics/              # Prometheus metrics
│   ├── database/                 # Database clients (PostgreSQL)
│   ├── files_store/              # File storage clients (S3, FS)
│   ├── inference/                # Inference gateway HTTP client
│   ├── shared/                   # Shared types and utilities
│   └── util/                     # Common utilities (logging, TLS, etc.)
├── charts/                       # Helm charts
│   └── batch-gateway/            # Kubernetes deployment manifests
├── docs/                         # Documentation
│   ├── design/                   # Architecture and design documents
│   └── guides/                   # Developer and user guides
├── test/                         # Test suites
│   ├── e2e/                      # End-to-end tests
│   └── integration/              # Integration tests
├── docker/                       # Dockerfiles
│   ├── Dockerfile.apiserver      # API server container image
│   └── Dockerfile.processor      # Processor container image
├── scripts/                      # Development and deployment scripts
├── Makefile                      # Build and development targets
└── go.mod                        # Go module dependencies
```

### Key Directories

- **`cmd/`**: Contains `main.go` entry points for both the API server and processor binaries
- **`internal/`**: All private application code, organized by component (apiserver, processor, storage clients)
- **`charts/`**: Helm chart for deploying both components to Kubernetes
- **`docs/design/`**: Detailed architecture documents with diagrams explaining the batch processing system
- **`test/`**: Integration and E2E test suites for validating the full system

## Getting Started

### Prerequisites

- Go 1.25 or later
- PostgreSQL 12+ (for metadata storage)
- Redis 6+ (for job queue)
- S3-compatible object storage or local filesystem
- Docker or Podman (for containerized deployment)
- Kubernetes 1.19+ and Helm 3.0+ (for Kubernetes deployment)

### Local Development

#### 1. Build Binaries

```bash
# Build both API server and processor
make build

# Or build individually
make build-apiserver
make build-processor
```

#### 2. Run Tests

```bash
# Run all unit tests
make test

# Run with coverage
make test-coverage

# Run integration tests
make test-integration

# Run E2E tests (requires running server)
make test-e2e
```

#### 3. Run Locally

Configure the components via YAML configuration files (see `cmd/apiserver/config.yaml` and `cmd/batch-processor/config.yaml` for examples).

```bash
# Run API server
make run-apiserver

# Run processor (in another terminal)
make run-processor

# Or with verbose logging
make run-apiserver-dev
make run-processor-dev
```

### Kubernetes Deployment

#### Quick Start with Kind

Deploy to a local Kind cluster for development:

```bash
# Creates cluster, builds images, and deploys with Helm
make dev-deploy
```

For detailed instructions, see [Development Guide](docs/guides/DEVELOPMENT.md).

#### Production Deployment

```bash
# Install API server only (default)
helm install batch-gateway ./charts/batch-gateway

# Install with processor enabled
helm install batch-gateway ./charts/batch-gateway \
  --set processor.enabled=true \
  --set processor.replicaCount=3
```

See [Helm Chart README](charts/batch-gateway/README.md) for full configuration options.

### Docker Images

```bash
# Build both images
make image-build

# Or build individually
make image-build-apiserver
make image-build-processor
```

Images are published to:

- `ghcr.io/llm-d-incubation/batch-gateway-apiserver`
- `ghcr.io/llm-d-incubation/batch-gateway-processor`

## API Usage

### Submit a Batch Job

```bash
# 1. Upload input file
curl -X POST http://localhost:8000/v1/files \
  -H "Content-Type: multipart/form-data" \
  -F "file=@batch_requests.jsonl" \
  -F "purpose=batch"

# Response: {"id": "file-abc123", ...}

# 2. Create batch job
curl -X POST http://localhost:8000/v1/batches \
  -H "Content-Type: application/json" \
  -d '{
    "input_file_id": "file-abc123",
    "endpoint": "/v1/chat/completions",
    "completion_window": "24h"
  }'

# Response: {"id": "batch-xyz789", "status": "validating", ...}
```

### Check Job Status

```bash
curl http://localhost:8000/v1/batches/batch-xyz789

# Response includes status: validating, in_progress, finalizing, completed, failed, expired, cancelled
```

### Retrieve Results

```bash
# Get output file ID from batch status
curl http://localhost:8000/v1/batches/batch-xyz789 | jq -r '.output_file_id'

# Download results
curl http://localhost:8000/v1/files/file-output123/content > results.jsonl
```

For complete API documentation, see the [OpenAI Batch API reference](https://platform.openai.com/docs/guides/batch).

## Configuration

### API Server Configuration

Key configuration options (environment variables or config file):

- `DATABASE_URL`: PostgreSQL connection string
- `REDIS_URL`: Redis connection string
- `FILE_STORAGE_TYPE`: `filesystem` or `s3`
- `S3_BUCKET`: S3 bucket name (if using S3)
- `PORT`: HTTP server port (default: 8000)
- `TLS_CERT_FILE`, `TLS_KEY_FILE`: TLS certificate paths

### Processor Configuration

Key configuration options:

- `NUM_WORKERS`: Worker pool size (default: 3)
- `POLL_INTERVAL`: Job queue polling interval (default: 5s)
- `GLOBAL_CONCURRENCY`: Max concurrent requests across all workers (default: 100)
- `PER_MODEL_MAX_CONCURRENCY`: Max concurrent requests per model (default: 10)
- `INFERENCE_GATEWAY_URL`: Inference gateway endpoint
- `SHUTDOWN_TIMEOUT`: Graceful shutdown timeout (default: 30s)

See configuration examples in `cmd/*/config.yaml`.

## Monitoring

### Metrics

The processor exposes Prometheus metrics on port 9090:

**Job-Level Metrics:**

- `jobs_processed_total{result,reason}` - Total jobs processed by result
- `job_processing_duration_seconds{tenantID,size_bucket}` - Job processing duration histogram
- `job_queue_wait_duration{tenantID}` - Time jobs spend in queue

**Worker Metrics:**

- `total_workers` - Configured worker pool size
- `active_workers` - Currently active workers
- `processor_inflight_requests` - Global in-flight request count
- `model_inflight_requests{model}` - Per-model in-flight requests

**Error Metrics:**

- `job_errors_by_model_total{model}` - Errors grouped by model

### Health Checks

**API Server:**

- Health: `GET /health` (port 8000)
- Readiness: `GET /readyz` (port 8000)

**Processor:**

- Health: `GET /health` (port 9090)
- Readiness: `GET /ready` (port 9090)

## Development

### Code Quality

```bash
# Format code
make fmt

# Run linter
make lint

# Run static analysis
make vet

# Run all checks
make ci
```

### Install Development Tools

```bash
make install-tools
```

This installs:

- `golangci-lint` - Linting and static analysis

### Project Structure Conventions

- Use `internal/` for all private code (not intended for external import)
- Place shared types in `internal/shared/`
- Keep component-specific code in dedicated subdirectories (`internal/apiserver/`, `internal/processor/`)
- Write unit tests alongside implementation files (`*_test.go`)
- Place integration tests in `test/integration/`
- Place E2E tests in `test/e2e/`

## Contributing

Contributions are welcome! Please ensure:

1. All tests pass: `make test-all`
2. Code is formatted: `make fmt`
3. Linter passes: `make lint`
4. New features include tests and documentation
5. Commits follow conventional commit format

## Security

This project follows security best practices:

- Non-root container execution (UID 65532)
- Read-only root filesystem
- All Linux capabilities dropped
- No privilege escalation
- Seccomp profile enabled
- TLS support for all network communication
- OpenShift SCC compatibility

To report security vulnerabilities, please contact the maintainers privately.

## License

Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

```text
http://www.apache.org/licenses/LICENSE-2.0
```

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

## Related Projects

- [llm-d-inference-scheduler](https://github.com/llm-d-incubation/llm-d-inference-scheduler) - Inference request scheduler
- [gateway-api-inference-extension](https://gateway-api-inference-extension.sigs.k8s.io/) - Kubernetes Gateway API extensions for inference workloads

## Support

For help and support:

- Open an issue on GitHub
- Review the [design documentation](docs/design/)
- Check the [development guide](docs/guides/DEVELOPMENT.md)
