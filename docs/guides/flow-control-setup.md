# Flow Control Setup for Batch and Online Inference

This guide describes how to configure the Gateway API Inference Extension (GIE) flow control system to efficiently support both online (interactive) and batch inference workloads on shared GPU infrastructure.

## Goal

- Online inference requests get low-latency, high-priority treatment.
- Batch inference requests fill remaining capacity when the backend is underutilized.
- When backend saturation increases due to online traffic, batch request dispatch automatically throttles.
- When backend saturation decreases, batch request dispatch automatically increases.

## How Flow Control Works

GIE's flow control is a sharded queuing and dispatch engine that sits between the gateway and the inference backend. When the `flowControl` feature gate is enabled, all inference requests pass through a three-tier dispatch hierarchy:

1. **Priority Band Selection** -- Requests are assigned to priority bands by numerical level. Higher-priority bands are dispatched first; lower bands are served only when higher bands are empty.
2. **Fairness Policy** -- Within a priority band, a fairness policy selects which flow (logical grouping of requests) to serve next. Options are round-robin (prevents starvation) or global-strict (maximizes throughput).
3. **Ordering Policy** -- Within a selected flow, an ordering policy picks the next request. Options are FCFS, EDF (earliest-deadline-first), or SLO-deadline (orders by `ReceivedTimestamp + x-slo-ttft-ms`).

A **saturation detector** monitors backend load and applies head-of-line blocking when saturation reaches 1.0 -- this is the mechanism that pauses batch dispatch when the backend is busy.

## Recommended Configuration

### Priority Band Design

| Band | Priority | Workload | Fairness | Ordering | Rationale |
|------|----------|----------|----------|----------|-----------|
| Online | 100 | Interactive requests | round-robin | slo-deadline | Low latency, fair across tenants, respects SLO deadlines |
| Batch | 0 | Batch Gateway requests | global-strict | slo-deadline | Maximizes throughput, respects batch job SLO deadlines |

**Why this works:** The priority band hierarchy ensures online requests (priority 100) are always dispatched before batch requests (priority 0). When no online requests are queued, batch requests flow freely. When online traffic arrives, batch dispatch pauses until the online queue drains. The SLO-deadline ordering within the batch band ensures that batch jobs closer to their completion deadline are dispatched first.

### EndpointPickerConfig

```yaml
apiVersion: config.gateway-api-inference-extension.sigs.k8s.io/v1alpha1
kind: EndpointPickerConfig
metadata:
  name: batch-and-online-flow-control
spec:
  featureGates:
    - flowControl

  plugins:
    - name: slo-deadline-ordering
      type: slo-deadline-ordering-policy
    - name: round-robin-fairness
      type: round-robin-fairness-policy
    - name: global-strict-fairness
      type: global-strict-fairness-policy
    - name: utilization-detector
      type: utilization-detector

  flowControl:
    # Global queue capacity across all bands and shards.
    # Size according to expected peak concurrent requests.
    maxBytes: "4Gi"

    # Fallback TTL for requests that don't specify x-slo-ttft-ms.
    # Online requests without SLO headers expire after 30 seconds.
    defaultRequestTTL: 30s

    priorityBands:
      # --- Online band: high priority, fair, SLO-aware ---
      - priority: 100
        maxBytes: "1Gi"
        fairnessPolicyRef: round-robin-fairness
        orderingPolicyRef: slo-deadline-ordering

      # --- Batch band: low priority, throughput-optimized, SLO-aware ---
      - priority: 0
        maxBytes: "3Gi"
        fairnessPolicyRef: global-strict-fairness
        orderingPolicyRef: slo-deadline-ordering

    # Template for any priority values not explicitly listed above.
    defaultPriorityBand:
      maxBytes: "512Mi"
      fairnessPolicyRef: global-strict-fairness
      orderingPolicyRef: slo-deadline-ordering

  saturationDetector:
    # Utilization-based: reads queue depth and KV-cache metrics from
    # vLLM endpoints. Provides proportional backpressure that scales
    # with overload depth. Preferred for mixed workloads because it
    # reacts to actual backend state rather than counting in-flight
    # requests.
    pluginRef: utilization-detector
```

### How Requests Get Assigned to Bands

Flow control assigns requests to priority bands based on the `InferenceObjective` Kubernetes CRD referenced by each request. The request carries the CRD name in the `x-gateway-inference-objective` header, and GIE looks up the corresponding `InferenceObjective` resource to determine the priority band.

**Setup:**

1. Create `InferenceObjective` CRDs for each workload class:

```yaml
apiVersion: inference.gateway.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: online-default
spec:
  priority: 100

---
apiVersion: inference.gateway.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: batch-low-priority
spec:
  priority: 0
```

2. Configure Batch Gateway to reference the batch objective (see [Batch Gateway Configuration](#batch-gateway-configuration) below).

3. Online workloads can reference `online-default` via their own `x-gateway-inference-objective` header, or rely on GIE's default priority (0) if no header is sent.

## Batch Gateway Configuration

### Headers Sent

Batch Gateway sets the following flow-control headers on each inference request (see `executor.go:mergeFlowControlHeaders`):

- **`x-slo-ttft-ms`**: Remaining milliseconds until the batch job's SLO deadline. GIE's `slo-deadline-ordering-policy` reads this header to order batch requests by urgency within the batch priority band.
- **`x-gateway-inference-objective`**: Name of the `InferenceObjective` CRD that determines the priority band. Only sent when `inference_objective` is configured (see below).

### Recommended Processor Settings

The following `batch-processor` configuration settings interact with flow control:

```yaml
# Processor concurrency settings
global_concurrency: 100        # Max in-flight requests across all models
per_model_max_concurrency: 20  # Max in-flight requests per model

# Gateway client settings (per gateway or global)
request_timeout: "5m"          # Generous timeout; flow control may queue requests
max_retries: 3                 # Retry on 429 (rate limit) and 5xx
initial_backoff: "2s"          # Initial retry backoff
max_backoff: "30s"             # Max retry backoff
```

**Key considerations:**

- **`request_timeout`**: With flow control enabled, requests may spend time in the GIE queue before reaching the backend. Set this high enough to accommodate queuing time plus inference time. 5 minutes is a reasonable starting point.
- **`max_retries` and backoff**: When the flow control queue is full, GIE rejects requests with HTTP 429. The batch processor's retry logic (exponential backoff) naturally creates a feedback loop: the processor backs off when the gateway is saturated and resumes when capacity becomes available.
- **`global_concurrency`**: This is the processor's own concurrency limit, independent of flow control. It controls how many requests the processor submits to the gateway concurrently. With flow control handling admission, you can set this relatively high and let the gateway-side queue manage the actual dispatch rate.

### Helm Values

```yaml
processor:
  config:
    globalConcurrency: 100
    perModelMaxConcurrency: 20
    # Name of the InferenceObjective CRD for batch requests.
    # Must match a deployed InferenceObjective resource in the cluster.
    inferenceObjective: "batch-low-priority"
    globalInferenceGateway:
      url: "http://inference-gateway:8000"
      requestTimeout: "5m"
      maxRetries: 3
      initialBackoff: "2s"
      maxBackoff: "30s"
```

## How the System Behaves Under Load

### Scenario 1: Backend Idle (No Online Traffic)

1. Saturation detector reports low utilization.
2. Flow control dispatches from the online band (empty) then the batch band.
3. Batch requests flow at full capacity, limited only by the processor's `global_concurrency`.
4. Batch jobs make maximum progress toward their SLO deadlines.

### Scenario 2: Online Traffic Arrives

1. Online requests enter the priority-100 band.
2. Flow control dispatches online requests first (strict priority).
3. Saturation detector detects increasing backend utilization.
4. When saturation reaches 1.0, head-of-line blocking pauses ALL dispatch (both bands).
5. As online requests complete and saturation drops below 1.0, dispatch resumes.
6. Online band is served first again; batch band gets remaining capacity.

### Scenario 3: Online Traffic Surge (Full Saturation)

1. Backend is fully saturated by online traffic.
2. Flow control's HoL blocking pauses all dispatch.
3. Batch requests accumulate in the priority-0 queue.
4. If batch requests exceed their TTL (derived from SLO deadline), they are evicted from the queue.
5. Batch Gateway receives eviction errors and marks those requests as `batch_expired`.
6. When online traffic subsides, batch requests resume from the queue.

### Scenario 4: Batch Job Near SLO Deadline

1. Batch Gateway sends requests with decreasing `x-slo-ttft-ms` values as the deadline approaches.
2. SLO-deadline ordering policy promotes these requests to the front of the batch queue.
3. These urgent batch requests are dispatched ahead of newer batch requests with more remaining time.
4. If the SLO deadline passes before dispatch, Batch Gateway skips the request locally (executor's `sloCtx.Err()` check) without wasting backend capacity.

## Proposed Batch Gateway Code Changes

### 1. Send InferenceObjective Header (Implemented)

Batch Gateway sends `x-gateway-inference-objective` on inference requests when the `inference_objective` config field is set. GIE uses this header to look up the named `InferenceObjective` CRD and assign the request to the corresponding priority band.

**Where:** `executor.go:mergeFlowControlHeaders` -- sends the header alongside the existing `x-slo-ttft-ms` header.

**Config:** `inference_objective` in the processor config (Helm: `processor.config.inferenceObjective`). Empty by default (header not sent).

### 2. Handle Queue Eviction Responses

**Why:** When flow control evicts a request from the queue (TTL expiry, capacity rejection), GIE returns specific HTTP error codes. The batch processor should recognize these as transient and either re-enqueue the request or mark it as expired, rather than treating them as inference failures.

**Where:** `pkg/clients/http/http_client.go` error categorization -- add handling for flow-control-specific response codes/headers if GIE provides them.

### 3. Adaptive Concurrency (Optional, Future)

**Why:** The processor's `global_concurrency` is currently static. With flow control providing real-time backpressure signals (429 responses), the processor could dynamically adjust its concurrency -- increasing when few 429s are received and decreasing when 429s are frequent.

**Where:** New adaptive concurrency controller wrapping the existing semaphore.

**Impact:** More complex change. Not required for initial setup but would improve efficiency by reducing unnecessary queuing at the GIE layer.

## Monitoring

Key metrics to watch when running batch and online workloads together:

| Metric | Source | What to Watch |
|--------|--------|---------------|
| Saturation level | GIE saturation detector | Should hover below 1.0 during mixed workloads |
| Queue depth per band | GIE flow control | Batch band queue growing = backend saturated by online traffic |
| Request evictions (TTL) | GIE flow control | Batch evictions = SLO deadlines being missed |
| 429 response rate | Batch Gateway metrics | High 429 rate = flow control is throttling batch |
| Batch job completion rate | Batch Gateway metrics | Should meet SLO deadlines under normal load |
| `x-slo-ttft-ms` values | Batch Gateway request headers | Decreasing values indicate jobs approaching deadline |

## Summary

The combination of GIE flow control and Batch Gateway provides automatic, infrastructure-level workload balancing:

- **GIE flow control** handles admission, queuing, and dispatch ordering based on priority and SLO deadlines.
- **Batch Gateway** communicates urgency via `x-slo-ttft-ms` headers and handles backpressure via retries.
- **Priority bands** ensure online traffic always takes precedence.
- **SLO-deadline ordering** ensures the most urgent batch requests are served first within the batch band.
- **Saturation detection** automatically throttles all dispatch when the backend is overloaded.

No complex rate-limiting configuration is needed. The system is self-regulating: batch throughput scales up and down with available backend capacity.
