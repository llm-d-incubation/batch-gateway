# Flow Control Setup for Batch and Interactive Inference

This guide describes how to configure the Gateway API Inference Extension (GIE) flow control system and the Batch Gateway system to efficiently support both interactive (online) and batch (offline) inference workloads on shared infrastructure.

## Goal

- Interactive inference requests get low-latency, high-priority treatment.
- Batch inference requests fill remaining capacity when the backend is underutilized.
- When backend saturation increases due to interactive traffic, batch request dispatch automatically decreases.
- When backend saturation decreases, batch request dispatch automatically increases.

## How Flow Control Works

GIE's flow control is a sharded queuing and dispatch engine that sits between the llm-d Router and the model servers. When the `flowControl` feature gate is enabled, all inference requests pass through a three-tier dispatch hierarchy:

1. **Priority Band Selection** -- Requests are assigned to priority bands by numerical level. Higher-priority bands are dispatched first; lower bands are served only when higher bands are empty.
2. **Fairness Policy** -- Within a priority band, a fairness policy selects which flow (logical grouping of requests) to serve next. Options are `round-robin` (prevents starvation) or `global-strict` (maximizes throughput).
3. **Ordering Policy** -- Within a selected flow, an ordering policy picks the next request. Options are `FCFS`, `EDF` (earliest-deadline-first), or `SLO-deadline` (orders by `ReceivedTimestamp + x-slo-ttft-ms`).

A **saturation detector** monitors backend load and applies head-of-line blocking when saturation reaches 1.0 -- pausing all dispatch until capacity becomes available. Higher-priority bands resume first.

For full details on GIE flow control, see the [Flow Control Configuration Guide](https://gateway-api-inference-extension.sigs.k8s.io/guides/flow-control/#configuration-guide) and the [EndpointPickerConfig Reference](https://gateway-api-inference-extension.sigs.k8s.io/guides/epp-configuration/config-text/#flow-control-configuration).

## Recommended Flow Control Configuration

### Priority Band Design

|Band|Priority|Workload|Fairness|Ordering|Rationale|
|------|--------|----------------------|-------------|------------|-----------------------------------------------|
|Interactive|100|Interactive requests|round-robin|fcfs|Low latency, fair across tenants|
|Batch|-1|Batch requests|global-strict|slo-deadline|Sheddable; maximizes throughput, dispatches by SLO urgency|

**Why this works:** When the backend is not saturated, both bands dispatch freely. When saturation reaches 1.0 and head-of-line blocking activates, the priority hierarchy ensures interactive requests (priority 100) are dispatched before batch requests (priority -1). Because batch requests have negative priority, they are **sheddable** — GIE rejects them immediately at admission when the system is saturated, instead of queuing them. Batch dispatch resumes as soon as saturation drops. Interactive requests without an `InferenceObjective` header default to priority 0 and are still protected, since they outrank the -1 batch band. If batch requests are queued but saturation persists, they are evicted when their TTL expires.

**Why sheddable (negative priority) for batch:** With non-negative priority, batch requests would queue inside GIE during saturation — consuming queue memory and likely getting evicted by TTL anyway. Shedding rejects them early and lets the batch processor handle backoff, which it's already designed to do. The processor sends a fresh `x-slo-ttft-ms` on each retry, so retried requests are re-prioritized correctly by SLO urgency when they're eventually admitted.

**SLO-deadline ordering for batch:** The batch band uses `slo-deadline-ordering-policy`, which orders requests by their SLO deadline (`ReceivedTimestamp + x-slo-ttft-ms`), ensuring inference requests of jobs closer to their completion deadline are dispatched first. See [SLO Deadline Ordering Policy](https://gateway-api-inference-extension.sigs.k8s.io/guides/epp-configuration/config-text/#slodeadlineorderingpolicy).

**FCFS ordering for interactive:** Interactive requests typically don't carry `x-slo-ttft-ms` headers, so `slo-deadline-ordering-policy` would assign them all a far-future deadline — effectively degrading to FCFS with extra overhead. FCFS also provides predictable arrival-order dispatch. If interactive clients do send SLO headers (e.g., latency-sensitive API tiers), consider switching the interactive band to `slo-deadline` as well.

### EndpointPickerConfig

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
featureGates:
  - "flowControl"

# schedulingProfiles is omitted — GIE auto-populates a default profile.

plugins:
  - type: round-robin-fairness-policy
  - type: global-strict-fairness-policy
  - type: slo-deadline-ordering-policy
  - type: utilization-detector
    parameters:
      # Max pending queue depth per model server before saturation is triggered.
      queueDepthThreshold: 5
      # Max KV-cache utilization (0-1.0) before saturation is triggered.
      kvCacheUtilThreshold: 0.8

flowControl:
  # Global queue capacity across all bands and shards.
  # Size according to expected peak concurrent requests.
  maxBytes: 4294967296  # 4Gi

  # Fallback TTL for requests that don't specify x-slo-ttft-ms.
  # Interactive requests without SLO headers expire after 30 seconds.
  defaultRequestTTL: 30s

  priorityBands:
    # --- Interactive band: high priority, fair ---
    - priority: 100
      maxBytes: 1073741824  # 1Gi
      fairnessPolicyRef: round-robin-fairness-policy
      orderingPolicyRef: fcfs-ordering-policy

    # --- Batch band: sheddable, throughput-optimized ---
    - priority: -1
      maxBytes: 3221225472  # 3Gi
      fairnessPolicyRef: global-strict-fairness-policy
      orderingPolicyRef: slo-deadline-ordering-policy

  # Template for any priority values not explicitly listed above.
  defaultPriorityBand:
    maxBytes: 536870912  # 512Mi
    fairnessPolicyRef: global-strict-fairness-policy
    orderingPolicyRef: fcfs-ordering-policy

saturationDetector:
  pluginRef: utilization-detector
```

### How Requests Get Assigned to Bands

Flow control assigns requests to priority bands based on the `InferenceObjective` Kubernetes CRD referenced by each request. The request carries the CRD name in the `x-gateway-inference-objective` header, and GIE looks up the corresponding `InferenceObjective` resource to determine the priority band.

**Setup:**

1. Create `InferenceObjective` CRDs for each workload class:

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: interactive-default
spec:
  priority: 100
  poolRef:
    group: inference.networking.k8s.io
    name: <your-inference-pool>

---
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  name: batch-sheddable
spec:
  priority: -1
  poolRef:
    group: inference.networking.k8s.io
    name: <your-inference-pool>
```

2. Configure Batch Gateway to reference the batch objective (see [Recommended Batch Gateway Configuration](#recommended-batch-gateway-configuration) below).

3. Interactive workloads can optionally send `x-gateway-inference-objective: interactive-default` to get priority 100. Without this header, requests default to priority 0, which still outranks the batch band (priority -1).

## Recommended Batch Gateway Configuration

### Headers Sent

Batch Gateway sets the following flow-control headers on each inference request:

- **`x-slo-ttft-ms`**: Remaining milliseconds until the batch job's SLO deadline. GIE's `slo-deadline-ordering-policy` reads this header to order batch requests by urgency within the batch priority band.
- **`x-gateway-inference-objective`**: Name of the `InferenceObjective` CRD that determines the priority band. Only sent when `inference_objective` is configured (see below).
- **`x-gateway-inference-fairness-id`**: Tenant identifier for per-tenant fairness within a priority band. Automatically set to the job's tenant ID when it is non-empty. GIE uses this header to group requests into separate flows so that a `round-robin` fairness policy can schedule them fairly.

**Note:** The recommended batch band configuration above uses `global-strict` fairness, which ignores flow boundaries and maximizes throughput. The fairness header only has effect if operators switch the batch band's fairness policy to `round-robin`.

### Recommended Processor Settings

The following `batch-processor` configuration settings interact with flow control:

```yaml
# Name of the InferenceObjective CRD for batch requests.
inference_objective: "batch-sheddable"

# Processor concurrency settings
global_concurrency: 100        # Max in-flight requests across all models
per_model_max_concurrency: 20  # Max in-flight requests per model

# Gateway client settings (per gateway or global)
request_timeout: "5m"          # Generous timeout; flow control may queue or reject requests
max_retries: 3                 # Retry on 429 (rate limit) and 5xx
initial_backoff: "2s"          # Initial retry backoff
max_backoff: "30s"             # Max retry backoff
```

#### Key Considerations

- **`request_timeout`**: With flow control enabled, requests may spend time in the GIE queue before reaching the backend. Set this high enough to accommodate queuing time plus inference time. 5 minutes is a reasonable starting point.
- **`max_retries` and `max_backoff`**: When the system is saturated, GIE sheds batch requests with HTTP 429. The batch processor's retry logic (exponential backoff) naturally creates a feedback loop: the processor backs off when the llm-d Router is saturated and resumes when capacity becomes available.
- **`global_concurrency`**: This is the processor's own concurrency limit, independent of flow control. It controls how many requests the processor submits to the llm-d Router concurrently. With flow control handling admission, you can set this relatively high and let the Router-side queue manage the actual dispatch rate.

#### Helm Values

```yaml
processor:
  config:
    globalConcurrency: 100
    perModelMaxConcurrency: 20
    inferenceObjective: "batch-sheddable"
    globalInferenceGateway:
      url: "http://llm-d-router:8000"
      requestTimeout: "5m"
      maxRetries: 3
      initialBackoff: "2s"
      maxBackoff: "30s"
```

## How the System Behaves Under Load

### Scenario 1: Backend Idle (No Interactive Traffic)

1. Saturation detector reports low utilization.
2. Flow control dispatches from the interactive band (empty) then the batch band.
3. Batch requests flow at full capacity, limited only by the processor's `global_concurrency`.
4. Batch jobs make maximum progress toward their SLO deadlines.

### Scenario 2: Interactive Traffic Arrives

1. Interactive requests enter the priority-100 band.
2. Flow control dispatches interactive requests first (strict priority).
3. Saturation detector detects increasing backend utilization.
4. When saturation reaches 1.0, head-of-line blocking pauses ALL dispatch (both bands).
5. As requests complete and saturation drops below 1.0, dispatch resumes.
6. Interactive band is served first again; batch band gets remaining capacity.

### Scenario 3: Interactive Traffic Surge (Full Saturation)

1. Backend is fully saturated by interactive traffic.
2. Flow control's head-of-line blocking pauses all dispatch.
3. New batch requests are shed at admission (rejected immediately) due to negative priority.
4. Batch Gateway receives rejection errors and retries with exponential backoff.
5. Any batch requests already queued before saturation hit are evicted if their TTL expires.
6. When interactive traffic subsides and saturation drops, batch dispatch resumes.

### Scenario 4: Batch Job Near SLO Deadline

1. Batch Gateway sends requests with decreasing `x-slo-ttft-ms` values as the deadline approaches.
2. SLO-deadline ordering policy promotes these requests to the front of the batch queue.
3. These urgent batch requests are dispatched ahead of newer batch requests with more remaining time.
4. If the SLO deadline passes before dispatch, the processor checks the deadline and skips the request without sending it to the backend.

## Monitoring

Key metrics to watch when running batch and interactive workloads together:

| Metric | Source | What to Watch |
|--------|--------|---------------|
| `inference_extension_flow_control_pool_saturation` | GIE | Should hover below 1.0 during mixed workloads |
| `inference_extension_flow_control_queue_size` | GIE | Batch band queue growing = saturation approaching; at 1.0, new batch requests are shed instead of queued |
| `inference_extension_flow_control_request_queue_duration_seconds` | GIE | High queue time in batch band = sustained saturation |
| Request evictions (TTL) | GIE flow control | Batch evictions = SLO deadlines being missed |
| 429 response rate | Batch Gateway metrics | High 429 rate = flow control is shedding batch |
| Batch job completion rate | Batch Gateway metrics | Should meet SLO deadlines under normal load |

## Summary

The combination of GIE flow control and Batch Gateway provides automatic, infrastructure-level workload balancing:

- **GIE flow control** handles admission, queuing, and dispatch ordering based on priority and SLO deadlines.
- **Batch Gateway** communicates priority via `x-gateway-inference-objective`, urgency via `x-slo-ttft-ms`, and handles backpressure via retries.
- **Priority bands** ensure interactive traffic always takes precedence.
- **SLO-deadline ordering** ensures the most urgent batch requests are served first within the batch band.
- **Saturation detection** automatically throttles all dispatch when the backend is overloaded.

The system is self-regulating: batch throughput scales up and down with available backend capacity.
