# Batch Processor Design

-   **Revision**: 1
-   **Last Updated**: 2026-02-17

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
-   Memory usage must be bounded and independent of request count
-   Only active requests should reside in memory

Target memory complexity:

```
O(activeModels + concurrency)
```

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
-   Cross-job fairness (time-sliced or budget-based scheduling across multiple jobs) is not provided. Jobs are processed in a run-to-completion manner once assigned to a worker. Model-level fairness is achieved via bounded concurrent dispatch with per-model concurrency limits and round-robin scheduling.
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
        - Write to local storage
            ↓
        - Parse (minimal) to get the requested model of the request
            ↓
        - Write the request's location and size to plan files
            ↓
        - Finalize input file
            ↓
        - Finalize plan files per model, and a metadata file for model ID to filename map, and total request line information
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
    ├── metadata.json
    └── plans/
        ├── <model_id_1>.plan
        ├── <model_id_2>.plan
        └── ...
```
-   `input.jsonl` is append-only. Each line is an inference request in json format.
-   `metadata.json` includes information for file name map, and total request line count.
```
{
    "file_name_map": {
        "model_a": "model_a.plan",
        "model_b": "model_b.plan",
        ...
    },
    "total_request_count": 5000
}
```
-   `file_name_map` is needed as model ids are sanitized for safe file name
-   `total_request_count` is stored since this is the first process we read the whole file.

For each `input.jsonl` line:
1.  Compute current byte offset in input.jsonl file
2.  Compute request length (including newline)
3.  Parse minimal JSON to extract model
4.  Intern modelID for plan file name
5.  Append plan entry to plan file (per model)


-------------------------------------------------------------------

##### Plan File Structure

Each model has its own plan file:

    jobs/<job_id>/plans/<model_id>.plan

Incomplete files are written as:

    <model_id>.plan.tmp

Renamed atomically upon completion.

- Plan entry format:

``` go
// Plan Entry Structure (Binary, 12 bytes)
type PlanEntry struct {
    Offset int64  // 8 bytes: Position in input.jsonl
    Length uint32 // 4 bytes: Length of the JSON line
}
```
The request JSON body is NOT stored in the plan.

The plan acts as an index into `input.jsonl`.

------------------------------------------------------------------------

##### Memory Characteristics

-   Plan entry ≈ 12 bytes
-   50,000 requests → ~600KB plan storage
-   Requests are never accumulated in memory
-   Only in-flight requests reside in memory

Worst-case memory usage:
```
O(activeModels + concurrency)
```

Independent of total request count.

------------------------------------------------------------------------

#### Phase 2. Scheduling & Execution
##### Execution Inputs
-   `input.jsonl`
-   Per-model plan files

Each plan file functions as a per-model execution queue.

-------------------------------------------------------------------

##### Scheduling Policy: Sticky Model + Budget (round-robin) with Bounded Concurrency
**Scheduling semantics (important):**
- Round-robin is used only for **model selection at dispatch time**.
- It is **not** a sequential "one-model-at-a-time" execution mode.
- Actual request execution is always asynchronous and bounded by concurrency controls.
- The scheduler continuously fills available slots up to `GlobalConcurrency`, while respecting `PerModelConcurrency`.
- `model_request_budget` controls fairness/locality trade-off per turn, not overall parallelism.

###### Scheduling and Execution Sequence
``` mermaid
sequenceDiagram
    participant S as Scheduler
    participant P as Plan Reader
    participant E as Executor (Goroutines)
    participant B as Inference Backend
    participant W as Result Writer

    Note over S: round-robin rotation starts

    loop Until All Plans Drained
        S->>S: Select Next Model (Sticky Model)

        loop Within Model Budget & Global/Per-Model Concurrency
            S->>P: Request Plan Entry (Offset/Length)
            P-->>S: Return Entry

            S->>E: Dispatch Request (Async)
            activate E
            E->>E: ReadAt(input.jsonl, offset, length)
            E->>B: Post Inference Request
            B-->>E: Return Response
            E->>W: Send to Writing Channel
            deactivate E

            W->>W: Append to output.jsonl
        end

        Note over S: Switch to Next Model
    end

    S->>W: Signal Completion
    W-->>S: Finalize & Close File
```

###### Algorithm:
1.  Scheduler iterates models in round-robin order.
2.  For each model, it dispatches up to `model_request_budget` requests subject to:
    - `GlobalConcurrency` (max in-flight requests per job)
    - `PerModelConcurrency` (max in-flight requests per model)
3.  When a model is drained, it is removed from rotation.

Goals:
-   Preserve locality
-   Ensure fairness
-   Prevent starvation

If a model has fewer than `model_request_budget` entries remaining, it is drained and removed at once.

-------------------------------------------------------------------

###### Concurrency Budget Terms
**Global Concurrency**: Limits total in-flight inference calls.
**Per Model Concurrency**: Limits concurrent execution per model.
    - Default recommendation: small value (e.g., 1-2)
**Model Request Budget**: Limits number of requests that can be processed in one turn (once this is exceeded, process other models as we don't want to starve models with smaller requests)

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
-   Build per-model plans
-   Create metadata file
-   Append entries
-   Manage open file limits
-   Provide plan readers

#### Scheduler
-   Model selection
-   Budget enforcement
-   Concurrency control

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

- `processor_inflight_requests` (gauge)
  Global number of in-flight inference requests (bounded by `GlobalConcurrency`).
  This is the primary saturation signal for scheduler/executor health and is kept intentionally
  even when per-model metrics are present.

- `model_inflight_requests{model}` (gauge)
  Per-model in-flight request count (bounded by `PerModelConcurrency`).
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
