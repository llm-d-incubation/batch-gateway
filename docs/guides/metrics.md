# Metrics

**Revision:** 1.0
**Last Modified:** 2026-03-10

## API Server

The API server exposes the following Prometheus metrics:

**Request Metrics:**

- `http_requests_total{method,path,status}` (Counter) - Total HTTP requests by method, path, and status code.
- `http_request_duration_seconds{method,path,status}` (Histogram) - HTTP request latency histogram.
- `http_requests_in_flight{method,path}` (Gauge) - Current number of HTTP requests being processed by the api server.

## Processor

The processor exposes the following Prometheus metrics:

**Job-Level Metrics:**

- `jobs_processed_total{result,reason}` (Counter) - Total jobs processed by result.
- `job_processing_duration_seconds{tenantID,size_bucket}` (Histogram) - Job processing duration histogram.
- `job_queue_wait_duration{tenantID}` (Histogram) - Time jobs spend in queue.
- `plan_build_duration_seconds{tenantID,size_bucket}` (Histogram) - Duration of ingestion and plan build in seconds

**Worker Metrics:**

- `total_workers` (Gauge) - Configured worker pool size.
- `active_workers` (Gauge) - Currently active workers.
- `processor_inflight_requests` (Gauge) - Global in-flight request count.
- `processor_max_inflight_concurrency` (Gauge) - Configured maximum number of concurrent in-flight inference requests.

**Model Metrics:**

- `model_inflight_requests{model}` (Gauge) - Per-model in-flight requests.
- `model_request_execution_duration_seconds{model}` (Histogram) - Per-request execution phase duration in seconds by model.

**Error Metrics:**

- `request_errors_by_model_total{model}` (Counter) - Total number of request errors by model
