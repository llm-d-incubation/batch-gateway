# Batch Processor Design

-   **Revision**: 5
-   **Last Updated**: 2026-03-11

-------------------------------------------------------------------

### Overview

#### Background

The batch processor is designed to execute large batch inference jobs (up to 50,000 requests per job, up to 200MB input file) while
maintaining bounded memory usage and predictable scheduling behavior.

The current design focuses on:
-   Preventing memory explosion when processing large input files
-   Enabling model-aware execution ordering
-   Ensuring fairness across models
-   Remaining deployment-agnostic (independent of GPU topology or
    backend layout)
-   Maintaining OpenAI-compatible request/response schema parity

This document describes the ingestion → execution → finalization processing architecture and scheduling model.

-------------------------------------------------------------------

### Design Goals

#### 1. Bounded Memory Usage

-   Maximum 50,000 requests per batch
-   Maximum 200MB input file
-   Memory usage must be bounded regardless of request count
-   Request bodies are never accumulated in memory

-------------------------------------------------------------------

#### 2. Model-Aware Scheduling

-   Requests targeting the same model should be grouped
-   Locality should be preserved where possible
        - Keep same model request execution (MVP)
        - Keep same endpoint group execution (TBD)
        - Keep the same execution context (TBD)
-   Avoid unnecessary churn in backend execution context

Locality is defined as executing similar work back-to-back to reuse warm state (caches, connections, scheduling context).

-------------------------------------------------------------------

#### 3. Fairness Across Models

-   No model should starve other models
-   Hot models must not monopolize execution capacity
-   Scheduling policy must prevent starvation

-------------------------------------------------------------------

#### 4. Clear Module Boundaries

Separate responsibilities into independent modules:

-   Polling
-   Validation
-   Ingestion
-   Plan storage
-   Scheduling
-   Execution
-   Result writing

Each module must be testable independently.

-------------------------------------------------------------------

#### 5. Deployment-Agnostic

-   The processor treats the inference backend as an opaque execution layer and does not assume any specific model-to-device mapping or GPU layout.

-------------------------------------------------------------------

#### 6. OpenAI Schema Parity

-   Request JSON schema parity: required
-   Response JSON schema parity: required
-   Error schema parity: required
-   Allowed API methods must match OpenAI parity
-   Functional parity: not guaranteed

Unsupported features must return OpenAI-compatible error responses.

-------------------------------------------------------------------

**Non-Goals (MVP)**

-   GPU placement or resource allocation strategies
-   Cross-job fairness (time-sliced or budget-based scheduling across multiple jobs) is not provided. Jobs are processed in a run-to-completion manner once assigned to a worker. Model-level fairness is achieved via bounded concurrent dispatch with global and per-model concurrency limits using semaphores.
-   Cost-based (token-length-aware) scheduling - cost(expected total tokens: input + max output tokens, expected execution time, GPU compute usage, memory footprint, average latency history) is not considered.
-   Advanced adaptive scheduling (e.g., latency-aware scheduling, backpressure-based throttling, token-cost-aware scheduling, dynamic budget tuning) is not provided.
-   Assumption (MVP): the priority queue provides exclusive dequeue semantics. Lease/heartbeat-based recovery for worker death is out of scope for MVP.

-------------------------------------------------------------------

#### Startup Recovery

When the processor starts, `recoverStaleJobs` scans the local work directory for job directories left behind by a previous container crash (OOM, panic, node eviction). For each stale directory it finds the corresponding DB record and takes action based on the job's status:

-   `in_progress` (no output on disk): re-enqueue the job so another worker can process it from scratch. If SLO expired, mark as `expired`. If re-enqueue fails, mark as `failed`.
-   `in_progress` (output exists on disk): upload partial results and mark as `failed` (inference cost is significant — preserve completed work rather than retry from scratch).
-   `finalizing`: attempt to complete finalization (upload output). If upload fails, mark as `failed`.
-   `cancelling`: upload partial results and transition to `cancelled`.
-   `validating`: re-enqueue (ingestion had not started). If SLO expired, mark as `expired`.
-   Other terminal states: clean up the directory only.

After recovery, stale directories are removed. Resume-from-checkpoint is not supported — recovered jobs are retried from scratch.

The `batch_startup_recovery_total{status,action}` metric tracks recovery outcomes for operational visibility.

--------------------------------------------------------------------
### High-Level Architecture
#### Processor and Worker
> Processor (Manager): Actively monitors the priority queue and tracks the availability of the worker pool.

> Job Dispatching: When a Worker becomes idle, the Processor claims the job and assigns it to that worker.

> Isolation: Each Worker independently handles the entire lifecycle of its assigned job, ensuring failure isolation.

#### 1. Job Lifecycle

```mermaid
flowchart TD
    poll["Polled from priority queue"] --> assign["Assigned to worker"]
    assign --> phase1

    subgraph phase1 ["Ingestion"]
        direction TB
        download["Download input file to local storage"]
        download --> parsePlan["Parse lines: extract model ID, PrefixHash"]
        parsePlan --> sort["Sort entries per model by PrefixHash"]
        sort --> writePlans["Write plan files + metadata"]
    end

    phase1 --> phase2

    subgraph phase2 ["Execution"]
        direction TB
        dispatch["Dispatch requests concurrently per model"]
        dispatch --> readPlan["Read plan entry, read input line"]
        readPlan --> sendInfer["Send to inference client"]
        sendInfer --> writeResult["Write result to output.jsonl or error.jsonl"]
    end

    phase2 --> phase3

    subgraph phase3 ["Finalization"]
        direction TB
        flushFiles["Flush output.jsonl and error.jsonl"]
        flushFiles --> uploadFiles["Upload to shared storage"]
        uploadFiles --> createRecords["Create file records in DB"]
        createRecords --> cleanupLocal["Cleanup local artifacts"]
    end

    phase3 --> completedState["completed"]

    phase1 -->|"cancel or error"| failedNoOutput["failed — no output"]
    phase2 -->|"SLO expired"| expiredState["expired — partial output"]
    phase2 -->|"user cancel"| cancelledState["cancelled — partial output"]
    phase2 -->|"system error"| failedPartial["failed — partial output"]
    phase3 -->|"upload retry exhausted"| failedCounts["failed — counts only"]
```

**Ingestion**

1.  Download the input file from shared storage to local disk (`input.jsonl`)
2.  Parse each line minimally to extract the target model ID and PrefixHash
3.  Accumulate request location (offset, length) and PrefixHash in memory
4.  Sort entries per model by PrefixHash for cache-friendly dispatch ordering
5.  Write sorted plan files (one per model) and a metadata file (model map + total line count)

**Execution**

1.  One goroutine per model dispatches requests concurrently, bounded by global and per-model semaphores
2.  For each plan entry: read the input line at the recorded offset, parse, send to the inference client
3.  Write the response to `output.jsonl` (success) or `error.jsonl` (inference error)
4.  If interrupted (SLO expiry, cancel, system error): drain undispatched entries to `error.jsonl` with the appropriate error code, flush writers, and return partial counts

**Finalization**

1.  Flush buffered `output.jsonl` and `error.jsonl` to disk
2.  Upload non-empty files to shared storage (with exponential backoff retry)
3.  Create file records in DB; set `output_file_id` / `error_file_id` on the job
4.  Transition status to `completed`; cleanup local artifacts
-------------------------------------------------------------------

#### 2. State Transitions

Allowed transitions:
-   `validating` (initial state: set when a batch job is created)
-   `validating → in_progress` (after ingestion succeeds — preProcessJob completes)
-   `in_progress → finalizing` (after all plans drained)
-   `finalizing → completed` (after uploading output and error files)
-   `* → failed` (job is unable to process)
-   `validating → expired` (SLO already exceeded before execution starts — skipped at dequeue)
-   `in_progress → expired` (SLO exceeded during execution — partial results preserved)
-   `in_progress → cancelling` (set by the **API server** when user requests cancellation; the processor does not write this transition)
-   `cancelling → cancelled` (set by the **processor** after handling the cancel event)

Transient states:
-   cancelling
-   finalizing

Terminal states:
-   completed
-   failed
-   expired
-   cancelled

Transient states such as `cancelling` and `finalizing` indicate that the job has already been claimed and is being handled by an active worker. They are not eligible for reassignment. If a worker dequeues a job and finds it in `cancelling` state (e.g. the API server wrote `cancelling` between dequeue and the runnable check), the worker transitions it directly to `cancelled` so it cannot stick indefinitely. Other non-runnable transient states (e.g. `finalizing`) are skipped; queue cleanup is handled by the owning worker or system policy.

Terminal states are removed from the priority queue.

-------------------------------------------------------------------

#### 3. Context Hierarchy

The processor uses a layered context tree to propagate cancellation signals.
The critical invariant is the **fork** at `ctx`: `pollingCtx` and `jobCtx` are siblings, so cancelling the polling loop does not kill in-flight jobs.

```mermaid
graph TD
    ctx(["ctx — SIGTERM / SIGINT"])

    ctx -->|"semaphore guard"| pollingCtx(["pollingCtx"])
    ctx -->|"per job"| jobCtx(["jobCtx ×N"])

    pollingCtx -.-> polling["acquire · dequeue · DB fetch · validate · poll wait"]

    jobCtx --> sloCtx(["sloCtx — SLO deadline"])
    jobCtx -.-> watchCancel["watchCancel goroutine"]

    sloCtx --> abortCtx(["abortCtx — user cancel"])
    abortCtx --> execCtx(["execCtx — first model error"])
```

| Context | Cancelled by | Blast radius |
|---------|-------------|--------------|
| `ctx` | SIGTERM / SIGINT | Everything — polling loop exits, in-flight jobs re-enqueue |
| `pollingCtx` | Semaphore double-release guard (also inherits `ctx` cancellation) | **Polling loop + pre-launch** — acquire, dequeue, DB fetch, validation, and guard re-enqueue all use `pollingCtx`. Stops accepting new jobs; running jobs unaffected. Jobs dequeued but not yet launched are re-enqueued (fallback: marked failed). |
| `jobCtx` | Parent `ctx` cancellation (SIGTERM / SIGINT) | Single job lifecycle (passed to `runJob`). Created only at launch commit, **after** all pre-launch checks pass. **Not** cancelled when only `pollingCtx` is cancelled (e.g. semaphore guard). |
| `sloCtx` | SLO deadline fires | Stops dispatch; in-flight requests finish; undispatched drained as `batch_expired` |
| `abortCtx` | `watchCancel` calls `abortInferFn` | Aborts in-flight HTTP inference requests immediately |
| `execCtx` | First model goroutine error | Stops dispatch in all model goroutines; already-dispatched requests complete |

**Design notes:**
-   The fork is intentional: `pollingCtx` controls the loop, `jobCtx` controls the job. Cancelling `pollingCtx` (e.g. on semaphore double-release) stops new-job intake while in-flight jobs finish normally. By contrast, **SIGTERM / SIGINT cancel `ctx`**, so both polling and in-flight jobs see cancellation and re-enqueue / teardown as designed.
-   `abortCtx` is derived from `sloCtx` so the SLO deadline propagates to inference requests automatically.
-   `execCtx` is derived from `abortCtx` so both user cancel and SLO expiry stop dispatch.
-   The `cancelRequested` flag is **not** used to stop dispatch (context cancellation handles that). It is only consulted in the error-handling path to distinguish the cancellation reason (user cancel vs SLO vs pod shutdown) and to drain undispatched entries with the correct error code.
-   `watchCancel` runs in a separate goroutine and does not update DB status to `cancelling` — the API server already did that before sending the cancel event.
-   Pre-launch operations (DB fetch, conversion, expired/runnable checks) run under `pollingCtx` so they abort promptly when the guard fires. `jobCtx` is created from `jobBaseCtx` only at the moment we commit to launching `runJob`.
-   On semaphore double-release: guard cancels `pollingCtx` → pre-launch aborts or guard re-enqueue fires → `Run` returns → `main.go` sets `ready=false` → K8s removes the pod from service (readiness probe fails). If re-enqueue also fails, the job is marked failed as a terminal fallback. The pod is restarted only if a liveness probe or restart policy triggers it.

-------------------------------------------------------------------

### Process Flow
#### Ingestion
##### Objectives
-   Download input file
-   Process line-by-line
-   Avoid loading request bodies into memory
-   Build per-model execution plans stored on disk
-   Create metadata file for input file (model-filename map, total request count)

------------------------------------------------------------------------

##### Input Handling

Directory layout:
```
jobs/
└── <job_id>/
    ├── input.jsonl        # downloaded from shared storage; read-only during execution
    ├── output.jsonl       # written during execution; contains successful responses
    ├── error.jsonl        # written during execution; contains failed responses
    ├── model_map.json
    └── plans/
        ├── <safe_model_id_1>.plan
        ├── <safe_model_id_2>.plan
        └── ...
```
-   `input.jsonl` is append-only. Each line is an inference request in json format.
-   `model_map.json` provides bidirectional mapping between original model IDs and sanitized (safe) file names, plus the total request line count.
```json
{
    "model_to_safe": {
        "org/model-A:1": "org_model_A_1",
        "model-B": "model_B"
    },
    "safe_to_model": {
        "org_model_A_1": "org/model-A:1",
        "model_B": "model-B"
    },
    "line_count": 5000
}
```
-   `model_to_safe` maps original model IDs to sanitized file names used for plan files.
-   `safe_to_model` is the reverse mapping, used during execution to recover the original model ID.
-   `line_count` is stored since ingestion is the first (and only) pass over the entire input file.

For each `input.jsonl` line:
1.  Compute current byte offset in input.jsonl file
2.  Compute request length (including newline)
3.  Parse minimal JSON to extract model
4.  Extract and hash the system prompt content from the request body for grouping requests with identical system prompts during execution. If the system prompt is absent, the hash defaults to 0.
5.  Intern modelID for plan file name
6.  Accumulate plan entry (offset, length, prefix hash) in memory per model


-------------------------------------------------------------------

##### Plan File Structure

Each model has its own plan file:

    jobs/<job_id>/plans/<model_id>.plan

Incomplete files are written as:

    <model_id>.plan.tmp

Renamed atomically upon completion.

- Plan entry format:

``` go
// Plan Entry Structure (Binary, 16 bytes)
type PlanEntry struct {
    Offset     int64  // 8 bytes: Position in input.jsonl
    Length     uint32 // 4 bytes: Length of the JSON line
    PrefixHash uint32 // 4 bytes: FNV-32a hash of the request's system prompt, used to group requests with identical system prompts during execution
}
```
The request JSON body is NOT stored in the plan.

The plan acts as an index into `input.jsonl`.

------------------------------------------------------------------------

##### Memory Characteristics

-   Plan entry ≈ 16 bytes
-   50,000 requests → ~800KB plan storage
-   Request bodies are never accumulated in memory; only lightweight plan entries (16 bytes each) are held in memory during ingestion
-   Only in-flight requests reside in memory during execution

Worst-case memory usage (processor-level):
```
Ingestion: O(totalRequestsPerJob × numWorkers) plan entries in memory (≤ 800KB per worker for 50,000 requests)
Execution: O(GlobalConcurrency) in-flight requests (shared across all workers)
```

Plan entry overhead is negligible (16 bytes each). Request bodies are never buffered.

------------------------------------------------------------------------

#### Execution
##### Execution Inputs
-   `input.jsonl`
-   Per-model plan files

Each plan file functions as a per-model execution queue.

-------------------------------------------------------------------

##### Scheduling Policy: Per-Model Goroutines with Semaphore-Based Concurrency Control

**Scheduling semantics:**
- Each model runs in its own goroutine, independently dispatching requests from its plan file.
- (Updated) There is no central round-robin scheduler or model selection loop.
- Concurrency is controlled by two levels of semaphores:
  - **Global semaphore** (`GlobalConcurrency`): limits total in-flight requests across all workers in a processor.
  - **Per-model semaphore** (`PerModelMaxConcurrency`): limits concurrent requests per individual model.
- Fairness across models is achieved naturally through goroutine scheduling and the global semaphore: no single model can monopolize all slots because each model is independently bounded by `PerModelMaxConcurrency`.

**Downstream-aware ordering:**
- Plan entries are already sorted by `PrefixHash` during ingestion.
- This groups requests with the same system prompt together, enabling the downstream inference gateway to maximize KV prefix cache hits by routing similar requests to the same backend pod.

###### Scheduling and Execution Sequence
``` mermaid
sequenceDiagram
    participant Ex as Executor
    participant GS as GlobalSemaphore
    participant MS as ModelSemaphore
    participant E as Goroutine
    participant B as Inference Backend
    participant W as Result Writer

    Note over Ex: Launch one goroutine per model

    par Model A Goroutine
        loop For each plan entry (sorted by PrefixHash)
            Ex->>MS: Acquire model slot
            Ex->>GS: Acquire global slot
            Ex->>E: Dispatch request (async)
            activate E
            E->>E: ReadAt(input.jsonl, offset, length)
            E->>B: POST inference request
            B-->>E: Return response
            E->>W: Write result
            E->>GS: Release global slot
            E->>MS: Release model slot
            deactivate E
        end
    and Model B Goroutine
        loop For each plan entry (sorted by PrefixHash)
            Ex->>MS: Acquire model slot
            Ex->>GS: Acquire global slot
            Ex->>E: Dispatch request (async)
            activate E
            E->>B: POST inference request
            B-->>E: Return response
            E->>W: Write result
            E->>GS: Release global slot
            E->>MS: Release model slot
            deactivate E
        end
    end

    Ex->>W: Signal Completion
    W-->>Ex: Finalize & Close File
```

###### Algorithm:
1.  Executor launches one goroutine per model.
2.  Each goroutine iterates its plan entries in order (already sorted by `PrefixHash` during ingestion) and dispatches requests concurrently, subject to:
    - `PerModelMaxConcurrency` (per-model semaphore, acquired **first**: max in-flight requests per model. Protects downstream from too many requests being dumped at once)
    - `GlobalConcurrency` (global semaphore, acquired **second**: max in-flight requests across all workers. Protects system resources from unbounded goroutine growth)
    - **Acquisition order: per-model before global.** This prevents starving other models — if the goroutine blocks waiting for a global slot, only a per-model slot is held, not a global one that other models could use. Release order is LIFO (global first, then per-model).
3.  When a model's plan is fully drained, its goroutine exits.
4.  The executor waits for all model goroutines to complete before finalizing.

Goals:
-   Maximize downstream prefix cache efficiency via sorted dispatch order
-   Ensure fairness across models via bounded per-model concurrency
-   Prevent system overload via global concurrency cap

**Limitation — PrefixHash grouping (exact match only):**

The current implementation hashes the full system prompt text using FNV-32a and groups requests with identical hashes. This means only requests with **exactly the same** system prompt are grouped together — requests with **similar but not identical** prompts are not considered neighbors.

This is a known limitation. Downstream inference engines (e.g., vLLM) perform prefix matching at the token level, where even partially overlapping prompts can benefit from KV cache reuse. A more advanced approach (e.g., greedy string prefix matching or token-aware grouping) could capture these "similar prefix" cases, but adds complexity (tokenization dependency, O(n²) comparisons, etc.) that is out of scope for the MVP.

For the MVP, exact-match grouping via hashing provides a simple, O(n log n) solution that captures the most common case (many requests sharing the same system prompt verbatim) without introducing additional dependencies.

FNV-32a hash collisions (32-bit space, ~4 billion values) are theoretically possible but do not affect correctness — they only reduce grouping optimality. In practice, the number of distinct system prompts per batch is small relative to the hash space, making collisions negligible.

**Future work — similar-prefix grouping:**

A potential improvement is to sort by the system prompt string lexicographically instead of by hash. Lexicographic sorting naturally places prompts with a shared prefix adjacent to each other, which would improve KV cache hit rates for prompts that are similar but not identical. This avoids tokenization dependency (which would couple the batch gateway to model-specific tokenizers) while still capturing partial prefix overlap at the string level. The main trade-off is memory: it requires holding system prompt strings in memory during ingestion rather than just a 4-byte hash. This is tracked as a post-MVP enhancement.

-------------------------------------------------------------------

###### Concurrency Budget Terms
**Global Concurrency** (`GlobalConcurrency`): Limits total in-flight inference calls across all workers in a processor. Protects system resources (goroutines, sockets, memory) from unbounded growth as models and jobs scale.
    - Default: 100
**Per-Model Concurrency** (`PerModelMaxConcurrency`): Limits concurrent execution per model. Protects downstream inference gateway from being overwhelmed by a single model's requests.
    - Default: 10

-------------------------------------------------------------------

###### Input File Access
Concurrent access to `input.jsonl` must be safe.

Approaches:
-   Use `ReadAt` using offset
-   Per-worker file descriptors
-   Synchronization around file reads

-------------------------------------------------------------------
### Module Boundaries

#### Poller
-   Dequeue jobs from priority queue
-   Delete jobs from queue if not runnable
-   Fetches job Database item

#### Validator
-   Validate job state (if runnable, expired)
-   Check SLO

#### Planner
-   Download input
-   Build per-model plans (accumulate entries in memory)
-   Sort plan entries by PrefixHash for downstream optimization
-   Write sorted plan files to disk
-   Create metadata file
-   Provide plan readers

#### Scheduler
-   Per-model goroutine lifecycle management
-   Global and per-model concurrency control via semaphores

#### Executor
-   Read request via offset/length
-   Resolve per-model inference client via `GatewayResolver`
-   Call inference backend
-   Return result

###### Gateway Routing
The processor supports two mutually exclusive gateway modes. Exactly one must be configured; `Validate()` enforces this at startup.

**Global mode** routes all inference requests to a single endpoint, regardless of model name. Use this for MaaS platforms, multi-model gateways, or LoRA-only deployments where all adapters share a single vLLM instance:

```yaml
global_inference_gateway:
  url: "http://inference-gateway:8000"
  request_timeout: "5m"
  max_retries: 3
  initial_backoff: "1s"
  max_backoff: "60s"
```

**Per-model mode** maps specific model names to individual gateway endpoints. Only models listed here are routed; requests for unlisted models receive a request-level `model_not_found` error:

```yaml
model_gateways:
  "llama-3":
    url: "http://gateway-a:8000"
    request_timeout: "5m"
    max_retries: 3
    initial_backoff: "1s"
    max_backoff: "60s"
  "mistral":
    url: "http://gateway-b:8000"
    api_key_name: "gateway-b-api-key"
    request_timeout: "2m"
    max_retries: 1
    initial_backoff: "1s"
    max_backoff: "30s"
```

Each per-model entry must be fully specified — there is no inheritance between entries. The optional `api_key_name` identifies a key within the mounted app secret (`/etc/.secrets/`), and `api_key_file` reads from an arbitrary path. TLS fields (`tls_insecure_skip_verify`, `tls_ca_cert_file`, `tls_client_cert_file`, `tls_client_key_file`) are also optional.

`GatewayResolver` (in `pkg/clients/inference/inference_client_resolver.go`) manages the client pool. `NewGlobalResolver` creates a single client for all models; `NewPerModelResolver` creates per-model clients, sharing instances when settings are identical to reuse connection pools.

#### ResultWriter

-   Write successful responses to `output.jsonl`
-   Write failed responses to `error.jsonl`
-   Update metrics
-   Finalize job (upload non-empty files, set `output_file_id` / `error_file_id` on job record)

-------------------------------------------------------------------
### Failure Handling

#### SLO Expiration

The `completion_window` field on a batch job defines the deadline by which the job must complete. This is stored as `expires_at` (Unix timestamp) and used as the SLO enforcement boundary.

**Behavior follows the OpenAI Batch API spec:**

> *"Batches that do not complete in time eventually move to an `expired` state; unfinished requests within that batch are cancelled, and any responses to completed requests are made available via the batch's output file."*
> — [OpenAI Batch API: Batch expiration](https://platform.openai.com/docs/guides/batch#batch-expiration)

Concretely:

-   Requests that completed before expiration → written to `output.jsonl`, preserved in output file
-   Requests that did not execute before expiration → written to `error.jsonl` with error code `batch_expired`:
    ```jsonl
    {"id": "batch_req_...", "custom_id": "...", "response": null, "error": {"code": "batch_expired", "message": "This request could not be executed before the completion window expired."}}
    ```
-   Job status → `expired` with `expired_at` timestamp set

**Implementation:**

SLO is enforced via `context.WithDeadline(ctx, slo)` on the job execution context. When the deadline fires:
1.  New request dispatch stops — semaphore acquisition fails on the expired context, breaking the dispatch loop
2.  In-flight inference requests that are mid-flight have their context cancelled; they are written to the error file with whatever error the inference client returns (not `batch_expired`)
3.  Requests that were never dispatched (pending in the plan but not yet started) are written to the error file as `batch_expired`
4.  Requests that already completed successfully remain in the output file

The job then transitions directly `in_progress → expired` (no `finalizing` transient state).

**Why not configurable?**

An earlier design considered making expiration behavior configurable (continue vs. stop on SLO breach). This was dropped in favor of strict OpenAI spec alignment: the `expired` state and its semantics are well-defined in the OpenAI API, and diverging from them would break client expectations. Partial results are always preserved.

#### Partial Output Preservation

This is an extension of the OpenAI Batch API — OpenAI discards results when a batch is cancelled or fails. We preserve them because completed inference results are expensive to produce and should not be thrown away.

For all terminal states where work was interrupted (expired, cancelled, failed), any completed inference results are preserved in the output file, and unexecuted requests are recorded in the error file with the appropriate error code (`batch_expired`, `batch_cancelled`, `batch_failed`). `batch_expired` is defined by the OpenAI spec; `batch_cancelled` and `batch_failed` are our extensions.

**Path-by-path behavior:**

-   **Expired (SLO deadline)**: Undispatched requests are drained as `batch_expired`, partial output is uploaded, status transitions to `expired`. (OpenAI spec behavior.)
-   **Cancelled (user-initiated)**: In-flight requests complete, undispatched requests are drained as `batch_cancelled`, partial output is uploaded, status transitions to `cancelled`.
-   **Failed (execution system error)**: Undispatched requests are drained as `batch_failed`, partial output is uploaded, status transitions to `failed`.
-   **Failed (ingestion)**: No output files exist — nothing to preserve. Status transitions to `failed` without file IDs.
-   **Failed (finalization — upload retry exhausted)**: Upload retries (exponential backoff, `MaxRetries + 1` attempts) are already exhausted inside `finalizeJob`. Re-attempting upload would fail for the same reason. Only `requestCounts` are recorded in the `failed` status; no file IDs.
-   **Graceful shutdown (pod termination)**: Job is re-enqueued for another worker to process from scratch. No partial upload — preserving the chance for a complete result.

Partial upload is best-effort: upload failures are logged but do not block the terminal status transition.

---

-   Worker crash during plan build → incomplete `.tmp` files discarded
-   Atomic rename ensures plan integrity
-   Inference failure handled per request
-   Job-level failure only on systemic error

-------------------------------------------------------------------
### Observability

#### Tracing (OpenTelemetry)

The root `"process-batch"` span covers the full job lifecycle (ingestion → execution → finalization). Additional child/linked spans provide finer-grained visibility:

| Span | Parent | Description |
|------|--------|-------------|
| `process-batch` | apiserver trace (linked via propagated trace context) | Root span for the entire job lifecycle |
| `storage.Store` | `process-batch` (during ingestion/finalization) | File upload to shared storage (S3/filesystem) |
| `storage.Retrieve` | `process-batch` (during ingestion) | File download from shared storage |
| `storage.Delete` | `process-batch` (during cleanup) | File deletion from shared storage |
| `re-enqueue` | Detached (linked to `process-batch`) | Best-effort re-enqueue after failure; uses `DetachedContext` so it survives parent cancellation |

The `process-batch` span records the following attributes:

| Attribute | Type | Set at | Description |
|---|---|---|---|
| `batch.id` | string | span start | Batch ID |
| `tenant.id` | string | span start | Tenant ID |
| `file.id` | string | span start | Input file ID |
| `batch.output_file.id` | string | Any terminal state with partial output | Output file ID; set on completed, expired, cancelled (execution), and failed (execution) |
| `batch.error_file.id` | string | Any terminal state with partial output | Error file ID; set on completed, expired, cancelled (execution), and failed (execution) |
| `batch.request.total` | int64 | Any terminal state after execution | Total request count |
| `batch.request.completed` | int64 | Any terminal state after execution | Successfully completed request count |
| `batch.request.failed` | int64 | Any terminal state after execution | Failed request count |

Errors at each stage are recorded via `span.RecordError()` and `span.SetStatus(codes.Error, ...)`.

Tracing is disabled when `OTEL_EXPORTER_OTLP_ENDPOINT` is not set (no-op provider).

-------------------------------------------------------------------

#### Metrics

**Job-Level Metrics**

- `jobs_processed_total{result,reason}` (counter)
  Tracks total number of jobs processed with result classification:
  - `success`
  - `failed`
  - `skipped`
  - `re_enqueued`
  - `expired`

  Reasons include:
  - `system_error`
  - `guard_shutdown` — semaphore double-release guard triggered graceful shutdown; job re-enqueued
  - `db_transient`
  - `db_inconsistency`
  - `not_runnable_state`
  - `expired_dequeue` — SLO already exceeded before execution started
  - `expired_execution` — SLO deadline fired during execution; partial results preserved
  - `none` — no additional reason beyond the result (e.g. success, cancelled)

- `job_processing_duration_seconds{size_bucket}` (histogram)
  Measures total job processing duration (end-to-end, including ingestion and execution).

- `job_queue_wait_duration_seconds` (histogram)
  Measures how long a job waited in the priority queue before being picked up.

**Worker Utilization Metrics**

- `total_workers` (gauge)
  Total configured worker count (static; set during initialization).

- `active_workers` (gauge)
  Current number of workers actively processing jobs.

- `processor_inflight_requests` (gauge)
  Global number of in-flight inference requests across all workers (bounded by `GlobalConcurrency`).
  Primary saturation signal for scheduler/executor health.

- `processor_max_inflight_concurrency` (gauge)
  Configured `GlobalConcurrency` value. Used with `processor_inflight_requests` to compute utilization.

- `model_inflight_requests{model}` (gauge)
  Per-model in-flight request count (bounded by `PerModelMaxConcurrency`).

**Duration Metrics**

- `plan_build_duration_seconds{size_bucket}` (histogram)
  Measures ingestion and plan build duration.

- `model_request_execution_duration_seconds{model}` (histogram)
  Measures per-request execution duration.

**Error and Retry Metrics**

- `request_errors_by_model_total{model}` (counter)
  Counts inference request errors grouped by model.

- `file_storage_operations_total{operation,component,status}` (counter)
  Counts file storage operations by outcome. `operation` is `store`/`retrieve`/`delete`,
  `component` is `processor`/`apiserver`/`garbage-collector`, and `status` is
  `success` (operation completed), `retry` (retry attempt), or `exhausted`
  (all retries failed). Emitted by the `retryclient` decorator.

**Startup Recovery Metrics**

- `batch_startup_recovery_total{status,action}` (counter)
  Counts jobs recovered during processor startup after a container restart.
  - `status`: the recovered job status at the time recovery ran. Common values include `in_progress`, `finalizing`, `cancelling`, `validating`, and `unknown` when the DB lookup failed or no DB record existed.
  - `action`: the recovery action taken. Values include `re_enqueued`, `failed`, `finalized`, `cancelled`, `expired`, `cleaned_up`, and `error`.
  Non-zero values indicate container-level crashes (OOM, panic) or stale on-disk artifacts from a prior processor instance.
