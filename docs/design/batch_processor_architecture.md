# Batch Processor Design

-   **Revision**: 2
-   **Last Updated**: 2026-03-03

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

This document describes the proposed 3-phase processing architecture and scheduling model.

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
**TODO:**
-   Worker Crash Recovery: If a worker crashes during `in_progress`, the job is retried from scratch upon re-queueing. Partial plan files and input artifacts are treated as temporary and discarded. Resume-from-checkpoint is not supported in MVP.
--------------------------------------------------------------------
### High-Level Architecture
#### Processor and Worker
> Processor (Manager): Actively monitors the priority queue and tracks the availability of the worker pool.

> Job Dispatching: When a Worker becomes idle, the Processor claims the job and assigns it to that worker.

> Isolation: Each Worker independently handles the entire lifecycle of its assigned job, ensuring failure isolation.

#### 1. Job Lifecycle
```
    [ Polled by processor ]
      ↓
    [ Assigned to a worker ]
      ↓
    [ Validated by worker ]
      ↓
    [ Process Phase 1 ] – Ingestion & Plan Building
      ↓
        - Get input file stream
            ↓
        - Read line by line
            ↓
        - Write to local storage (input.jsonl)
            ↓
        - Parse (minimal) to get the requested model of the request and PrefixHash value
            ↓
        - accumulate the request's location, size, and PrefixHash in memory
            ↓
        - Sort accumulated plan entries per model by PrefixHash, write sorted entries to plan files, and create a metadata file for model ID to filename map and total request line information
            ↓
        - Finalize input file
      ↓
    [ Process Phase 2 ] – Scheduling & Execution
      ↓
        - Scheduler dispatches requests subject to concurrency limits. Execution is performed concurrently using bounded goroutines.
            ↓
        - Plan file Read: Get the request line
            ↓
        - Parse the line
            ↓
        - Validate the line
            ↓
        - Send the request using inference client
            ↓
        - Send the received response to the writing channel
      ↓
    [ Finalize ]
      ↓
    Writing channel finalizes the output file
      ↓
    Upload output file
      ↓
    File cleanup (plan files, metadata file, input file)
```
-------------------------------------------------------------------

#### 2. State Transitions

Allowed transitions:
-   `validating` (initial state: set when a batch job is created)
-   `validating → in_progress` (after polling from the queue)
-   `in_progress → finalizing` (after all plans drained)
-   `finalizing → completed` (after uploading output file)
-   `* → failed` (job is unable to process)
-   `* → expired` (SLO exceeded)
-   `in_progress → cancelling` (after user's cancellation signal received)
-   `cancelling → cancelled` (after user-initiated cancellation is completed)

Transient states:
-   cancelling
-   finalizing

Terminal states:
-   completed
-   failed
-   expired
-   cancelled

Transient states such as `cancelling` and `finalizing` indicate that the job has already been claimed and is being handled by an active worker. They are not eligible for reassignment. If encountered in the priority queue, workers skip them; queue clean up is handled by the owning worker or system policy.

Terminal states are removed from the priority queue.

-------------------------------------------------------------------

### Process Flow
#### Phase 1. Ingestion & Plan Building
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
    ├── input.jsonl
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
-   `safe_to_model` is the reverse mapping, used during Phase 2 execution to recover the original model ID.
-   `line_count` is stored since Phase 1 is the first (and only) pass over the entire input file.

For each `input.jsonl` line:
1.  Compute current byte offset in input.jsonl file
2.  Compute request length (including newline)
3.  Parse minimal JSON to extract model
4.  Extract and hash the system prompt content from the request body for grouping requests with identical system prompts in Phase 2. If the system prompt is absent, the hash defaults to 0.
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
    PrefixHash uint32 // 4 bytes: FNV-32a hash of the request's system prompt, used to group requests with identical system prompts in Phase 2
}
```
The request JSON body is NOT stored in the plan.

The plan acts as an index into `input.jsonl`.

------------------------------------------------------------------------

##### Memory Characteristics

-   Plan entry ≈ 16 bytes
-   50,000 requests → ~800KB plan storage
-   Request bodies are never accumulated in memory; only lightweight plan entries (16 bytes each) are held in memory during Phase 1
-   Only in-flight requests reside in memory during Phase 2

Worst-case memory usage (processor-level):
```
Phase 1: O(totalRequestsPerJob × numWorkers) plan entries in memory (≤ 800KB per worker for 50,000 requests)
Phase 2: O(GlobalConcurrency) in-flight requests (shared across all workers)
```

Plan entry overhead is negligible (16 bytes each). Request bodies are never buffered.

------------------------------------------------------------------------

#### Phase 2. Scheduling & Execution
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
- Plan entries are already sorted by `PrefixHash` during Phase 1 finalization.
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
            Ex->>GS: Acquire global slot
            Ex->>MS: Acquire model slot
            Ex->>E: Dispatch request (async)
            activate E
            E->>E: ReadAt(input.jsonl, offset, length)
            E->>B: POST inference request
            B-->>E: Return response
            E->>W: Write result
            E->>MS: Release model slot
            E->>GS: Release global slot
            deactivate E
        end
    and Model B Goroutine
        loop For each plan entry (sorted by PrefixHash)
            Ex->>GS: Acquire global slot
            Ex->>MS: Acquire model slot
            Ex->>E: Dispatch request (async)
            activate E
            E->>B: POST inference request
            B-->>E: Return response
            E->>W: Write result
            E->>MS: Release model slot
            E->>GS: Release global slot
            deactivate E
        end
    end

    Ex->>W: Signal Completion
    W-->>Ex: Finalize & Close File
```

###### Algorithm:
1.  Executor launches one goroutine per model.
2.  Each goroutine iterates its plan entries in order (already sorted by `PrefixHash` during Phase 1 finalization) and dispatches requests concurrently, subject to:
    - `GlobalConcurrency` (global semaphore: max in-flight requests across all workers. this is to protect system resource from too many goroutines being opened at once)
    - `PerModelMaxConcurrency` (per-model semaphore: max in-flight requests per model. this is to protect downstream from too many requests being dumped at once)
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

A potential improvement is to sort by the system prompt string lexicographically instead of by hash. Lexicographic sorting naturally places prompts with a shared prefix adjacent to each other, which would improve KV cache hit rates for prompts that are similar but not identical. This avoids tokenization dependency (which would couple the batch gateway to model-specific tokenizers) while still capturing partial prefix overlap at the string level. The main trade-off is memory: it requires holding system prompt strings in memory during Phase 1 rather than just a 4-byte hash. This is tracked as a post-MVP enhancement.

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
-   Call inference backend
-   Return result

#### ResultWriter

-   Write success/failure
-   Update metrics
-   Finalize job

-------------------------------------------------------------------
### Failure Handling

-   SLO expiration → expired
-   Worker crash during plan build → incomplete `.tmp` files discarded
-   Atomic rename ensures plan integrity
-   Inference failure handled per request
-   Job-level failure only on systemic error

-------------------------------------------------------------------
### Observability
#### Metrics (Already Implemented)
**Job-Level Metrics**

- `jobs_processed_total{result,reason}` (counter)
  Tracks total number of jobs processed with result classification:
  - `success`
  - `failed`
  - `skipped`
  - `re_enqueued`

  Reasons include:
  - `system_error`
  - `db_transient`
  - `db_inconsistency`
  - `not_runnable_state`
  - `expired`
  - `none`

- `job_processing_duration_seconds{tenantID,size_bucket}` (histogram)
  Measures total job processing duration (end-to-end, including ingestion and execution).

- `job_queue_wait_duration{tenantID}` (histogram)
  Measures how long a job waited in the priority queue before being picked up.

**Worker Utilization Metrics**

- `total_workers` (gauge)
  Total configured worker count (static; set during initialization).

- `active_workers` (gauge)
  Current number of workers actively processing jobs.

**Error Metrics**

- `job_errors_by_model_total{model}` (counter)
  Counts job processing errors grouped by model.


#### Metrics (Planned for MVP: please share your opinion on this)

The following metrics improve visibility into scheduling behavior, concurrency control, and phase-level performance.

- `processor_max_inflight_concurrency` (gauge)
  Configured `GlobalConcurrency` value. Used with `processor_inflight_requests` to compute utilization.

- `processor_inflight_requests` (gauge)
  Global number of in-flight inference requests across all workers (bounded by `GlobalConcurrency`).
  This is the primary saturation signal for scheduler/executor health and is kept intentionally
  even when per-model metrics are present.

- `model_inflight_requests{model}` (gauge)
  Per-model in-flight request count (bounded by `PerModelMaxConcurrency`).
  This is a decomposition metric for in-flight requests status which enables control over request per model


- `plan_build_duration_seconds{tenantID,size_bucket}` (histogram)
  Measures Phase 1 ingestion and plan build duration.

- `model_request_execution_duration_seconds{model}` (histogram)
  Measures per-request execution duration during Phase 2.

#### Metrics (Deferred / Not in MVP)

- `model_queue_depth{model}`
  Per-model remaining plan entries.
  Not included in MVP due to additional state tracking complexity.
  May be added later if required for throughput tuning or debugging.
