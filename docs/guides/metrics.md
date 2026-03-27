# Metrics

**Revision:** 1.1
**Last Modified:** 2026-03-26

Metric names below match `internal/*/metrics/metrics.go` (and related packages). Deployments may add a namespace/subsystem prefix when registering; check `/metrics` on the running binary for the exact series name.

## API Server

The API server exposes the following Prometheus metrics (`internal/apiserver/metrics/metrics.go`):

**Request Metrics:**

- `http_requests_total{method,path,status}` (Counter) - Total HTTP requests by method, path, and status code.
- `http_request_duration_seconds{method,path,status}` (Histogram) - HTTP request latency histogram.
- `http_requests_in_flight{method,path}` (Gauge) - Current number of HTTP requests being processed by the api server.

## Processor

The processor exposes the following Prometheus metrics (`internal/processor/metrics/metrics.go`):

**Job-Level Metrics:**

- `jobs_processed_total{result,reason}` (Counter) - Total jobs processed by result and reason.

  **result** values: `success`, `failed`, `skipped`, `re_enqueued`, `expired`.

  **reason** values (see `internal/processor/metrics/metrics.go`): `system_error`, `guard_shutdown`, `db_transient`, `db_inconsistency`, `not_runnable_state`, `expired_dequeue`, `expired_execution`, `none`.

- `job_processing_duration_seconds{tenantID,size_bucket}` (Histogram) - End-to-end job processing duration. `size_bucket` is derived from input line count (`100`, `1000`, `10000`, `30000`, `large`).

- `job_queue_wait_duration_seconds{tenantID}` (Histogram) - Time spent in the priority queue before being picked up.

- `plan_build_duration_seconds{tenantID,size_bucket}` (Histogram) - Duration of ingestion and plan build in seconds.

**Worker Metrics:**

- `total_workers` (Gauge) - Configured worker pool size (`NumWorkers`).

- `active_workers` (Gauge) - Currently active workers.

- `processor_inflight_requests` (Gauge) - Global in-flight inference requests during execution.

- `processor_max_inflight_concurrency` (Gauge) - Configured `GlobalConcurrency` ceiling.

**Model Metrics:**

- `model_inflight_requests{model}` (Gauge) - Per-model in-flight requests.

- `model_request_execution_duration_seconds{model}` (Histogram) - Per-request execution duration by model.

**Error Metrics:**

- `request_errors_by_model_total{model}` (Counter) - Total number of request errors by model.

**Startup Recovery:**

- `batch_startup_recovery_total{status,action}` (Counter) - Jobs recovered during processor startup after a container restart. Non-zero values indicate prior container-level crashes (OOM, panic). Label values are defined in code paths that call `RecordStartupRecovery`.

## Shared (file storage retry client)

Used by components that wrap file storage with retries (`internal/files_store/retryclient/metrics.go`):

- `file_storage_operations_total{operation,component,status}` (Counter) - File storage operations by outcome. `operation` is `store` / `retrieve` / `delete`; `component` is `processor` / `apiserver` / `garbage-collector`; `status` is `success`, `retry`, or `exhausted`.
