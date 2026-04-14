# Orphan batch job detection and reconciliation

## Summary

This work is **not** a replacement for **startup recovery** (`recoverStaleJobs`), which already repairs many failures using **local workdir** state when a processor restarts. The goal here is a separate line of defense: **cluster-level** detection and reconciliation for batch jobs that **fall off the normal track** for **other reasons**. The main picture is **queue–database mismatch**: the job is **no longer in** the Redis priority queue (usually because a worker **dequeued** it) while **PostgreSQL still shows a non-terminal status**, and **startup recovery cannot close the gap** because there is **no surviving processor workdir** (for example pod replaced and ephemeral disk is gone). **Partial failures** or **replica races** can cause the same problem. These are **severe edge cases**, but they **can still happen** in real deployments; without an explicit reconciler, such jobs can remain **non-terminal** indefinitely.

The proposal introduces a precise definition of **orphan batch jobs** in that sense, **together with** supporting terms used in this document (see [Definitions](#definitions), especially [Orphan job (narrow, reconciler scope)](#orphan-job-narrow-reconciler-scope)), **and** a reconciliation design. **Orphan state** in this narrow sense still arises from the **processor + Redis queue + PostgreSQL** interaction—for example a job is dequeued, then the worker or pod is lost before metadata reaches a terminal status, and **no** stale workdir remains for `recoverStaleJobs`.

The API server and garbage collector do **not** create that pattern by themselves; they still matter because a reconciler must **compose safely** with **every processor replica**, **API traffic** (submit, cancel, read paths), and **GC retention** so fixes are not duplicated, raced, or contradicted cluster-wide. When a stale workdir **does** exist, startup recovery remains the **owner**.

## Definitions

### Non-terminal status

A batch job whose PostgreSQL metadata status is not a **terminal** status in the **OpenAI Batch API** sense (for example still `validating`, `in_progress`, `finalizing`, or `cancelling`).

### Actively processing

A processor replica **owns** the job in the runtime sense: the job has been **removed from the Redis priority queue** and a **`runJob` worker goroutine** for that job ID is running (ingestion via `preProcessJob`, then execution, finalization, or cancel handling—see [Batch processor architecture](../design/batch_processor_architecture.md)).

### Diverged job

**PostgreSQL still shows a non-terminal status, but no processor is actively running that job** in the sense of [Actively processing](#actively-processing).

### Stale workdir job (startup-recovery eligible)

A job for which the processor’s local layout still contains a **stale job directory** under `WorkDir` that `recoverStaleJobs` is designed to discover on **processor startup** (same pod / surviving `emptyDir` as today’s architecture). For that job, **startup recovery is the designated owner**: it should re-enqueue, finalize, cancel, fail, or clean up according to existing phase-aware rules.

**Relationship to “diverged”:** After a crash and **before** the next `recoverStaleJobs` run, such a job may look “nothing is processing it” from the outside. It is still **not** an *orphan job* in the **narrow sense** used below, because a recovery path already exists and should run first.

### Orphan job (narrow, reconciler scope)

An **orphan job** is a **diverged job** that is **also** **not** startup-recovery eligible: there is **no** stale processor workdir that `recoverStaleJobs` can use to reconcile the job on restart (for example pod eviction destroyed `emptyDir`, or only a different replica’s disk could be checked and this job’s artifacts are gone). Equivalently:

1. **Redis priority queue:** the job is **not** in the priority queue (in the usual case it was **dequeued** and removed atomically).
2. **PostgreSQL:** metadata is still shows that the job is in **non-terminal** status.
3. **Processor runtime:** no replica is **actively processing** the job ([Actively processing](#actively-processing)—dequeue alone does **not** imply an orphan if a legitimate `runJob` still exists).
4. **Startup recovery:** `recoverStaleJobs` does **not** own the next step—there is **no** applicable **stale workdir** ([Stale workdir job](#stale-workdir-job-startup-recovery-eligible)).

Only when **(1)–(4)** hold is a job a **candidate** for cluster-level reconciliation in the **narrow** sense of this proposal. **Grace periods** and ordering (exclude startup-recovery-eligible work first) still apply elsewhere in this document.

The rest of this document uses **orphan job** in the **narrow** sense unless explicitly stated otherwise.

## Motivation

### Goals

- Detect jobs that are stuck in non-terminal DB states after dequeue or partial progress, including cases where **startup recovery does not apply** (no surviving job directory on disk).
- Reconcile those jobs to a **safe, observable outcome**: re-enqueue for a clean retry where appropriate, or transition to `failed` / `expired` / `cancelled` with clear semantics.
- Provide **metrics and logging** so operators can see detection frequency, actions taken, and remaining edge cases.
- Keep behavior **compatible with existing queue semantics** (`PQDequeue` atomically removes items; implementations must not double-dequeue the same logical work).

### Non-goals

- Replacing **startup recovery** (`recoverStaleJobs`) for the container-restart / same-pod emptyDir case; that remains the fast path when local artifacts exist.
- **Checkpointed resume** of partially executed inference (already out of scope per existing processor design).
- Solving all **Redis–PostgreSQL** consistency issues unrelated to job lifecycle (for example generic distributed transactions).

### Problem statement

The priority queue contract requires atomic removal on dequeue:

```117:125:batch-gateway/internal/database/api/database.go
	// PQDequeue atomically removes and returns the job priority objects at the head of the queue,
	// up to the maximum number of objects specified in maxItems.
	// The function blocks up to the timeout value for a job priority object to be available.
	// If the timeout value is zero or negative, the function returns immediately.
	//
	// Implementations MUST atomically remove dequeued items from the queue. The processor
	// assumes exclusive dequeue semantics: a dequeued job will not be returned by any
	// subsequent PQDequeue call. Non-atomic (peek/lease) implementations will cause the
	// same job to be processed multiple times.
```

If a processor dequeues a job and then the **process or pod is lost** before the job reaches a terminal DB state and before local recovery can run, the job may:

- be absent from Redis, and
- still appear as non-terminal in PostgreSQL,

with **no local directory** to drive `recoverStaleJobs`. Code comments already acknowledge this gap for pod-level failure:

```104:107:batch-gateway/internal/processor/worker/recovery.go
	dbItem, err := p.poller.fetchJobItemByID(ctx, jobID)
	// DB unreachable — can't read status or mark as failed. Leave workdir on disk so the
	// next container restart retries. If the pod is evicted (emptyDir destroyed), this job
	// becomes an orphan that only an external entity can detect.
```

A **cluster-level reconciler** is the natural place to close that gap.

## Proposal

1. **Formalize the orphan definition** (state machine + queue membership + optional timing/heartbeat signals) in project docs and tests.
2. Introduce a **reconciliation component** (exact deployment model TBD in design details) that periodically:
   - lists or queries candidate jobs from PostgreSQL;
   - checks queue membership (and any other signals we adopt);
   - applies **one** reconciliation action per job according to status and safety rules aligned with existing startup recovery behavior.
3. Emit **Prometheus metrics** (and structured logs) for detections and actions; align naming with existing `batch_startup_recovery_*` where concepts overlap.
4. Gate risky behavior behind **configuration** (intervals, age thresholds, dry-run mode if useful) and keep experimental paths off by default if any public behavior changes.

## Design details

### Orphan criteria (draft)

Reconciler actions apply only to jobs that meet the **narrow [Orphan job](#orphan-job-narrow-reconciler-scope)** definition. Candidates that are **[Stale workdir job](#stale-workdir-job-startup-recovery-eligible)** (including the window before the next `recoverStaleJobs` run) must be **excluded** or **deferred** so startup recovery remains the owner.

Precise rules should be agreed in review; a workable starting set for **reconcile-orphans**:

| DB status | Approximate intent | Candidate reconciliation |
|-----------|-------------------|---------------------------|
| `validating` | Not yet executing user workload | Re-enqueue if absent from queue and past grace period; else `expired` if SLO passed |
| `in_progress` | Executing | Re-enqueue only if safe (no partial output persisted / or explicit policy); otherwise mark `failed` with reason |
| `finalizing` | Upload / finalize in progress | Retry finalization if implementation allows; else `failed` |
| `cancelling` | User cancel in flight | Align with startup recovery: complete cancel path or terminal state |

**Anti-double-execution:** Re-enqueue must use existing **idempotent enqueue** semantics (`ZAddNX` in Redis implementation) and/or a **single-writer** reconciliation lease so two reconcilers do not enqueue the same job twice in a racy way. Any new Redis or DB field (for example “reconcile generation” or “last claimed at”) needs a short design note.

**Grace period:** Immediately after dequeue, a short window may exist where DB still shows `validating`/`in_progress` while the processor is starting. Reconciliation must use **minimum age** or **heartbeat** (if added) to avoid fighting normal processing.

### Where the reconciler runs

Options to compare in implementation planning:

- **Dedicated deployment** (small controller / sidecar / job): clear ownership, single leader election optional.
- **Processor leader** or **one replica only**: fewer moving parts; must avoid duplicating work across processor replicas.
- **API server**: couples operational concerns to the request path; generally less attractive unless extremely lightweight and rate-limited.
- **Colocate with batch garbage collector (`batch-gc`)**: fewer moving parts at the cluster level and one more periodic loop in an existing component, but **different problem domain**—today’s GC focuses on **retention / expiry cleanup**, while reconciliation is **queue–metadata consistency**. Combining them **tightens coupling** and can **blur ownership** (operability, on-call, review surface) unless maintainers explicitly agree. Treat as a **valid placement to evaluate**, not a default.

Recommendation in this proposal: prefer a **dedicated reconciler** or **elected single instance** with explicit leader semantics, not N uncoordinated replicas. **Final placement** (including **GC colocation**) is an **ownership and operational** decision for maintainers after review.

### Data sources

- **PostgreSQL**: authoritative job status and metadata.
- **Redis**: queue membership via existing admin/list operations or new indexed queries if needed (today `PQDelete` takes ID+SLO; listing queue members may require new API for efficient reconciliation—call out as open point).

### Observability

- Counters/histograms for: orphans detected, re-enqueued, failed, expired, errors, reconciliation duration.
- Logs: job ID, tenant, previous status, action, reason code.
- **PrometheusRule / Grafana / runbooks:** define and document when the reconciler is implemented; this proposal only assumes **metrics and structured logs** exist so those artifacts can be added later.

### Testing

- Unit tests for state transition tables and race guards.
- Integration tests: simulate dequeue + processor kill / missing workdir + DB non-terminal; assert eventual consistency.
- E2E optional once behavior is stable (follow existing `make test-e2e` patterns).

## Alternatives

1. **Visibility timeout / lease-based dequeue**
   Change queue semantics so dequeue is a **lease** with automatic return on expiry, instead of immediate removal. Fixes orphans at the source but is a **large protocol and implementation change** across API, Redis, and processor assumptions.

2. **Heartbeat column + processor periodic tick**
   Processor updates `last_progress_at` in DB; reconciler only acts when stale. Reduces false positives; requires schema migration and write load tradeoffs.

3. **Manual operator tooling only**
   CLI or runbook to list and fix stuck jobs. Low engineering cost, no automatic safety or SLO for stuck volume.

4. **Rely on GC only**
   Current garbage collection focuses on retention/expiry, not queue–DB divergence; it does not replace this proposal.

## Open questions

- **Deployment:** Should reconciliation live in **`batch-gc`**, a **new binary**, or elsewhere? **Trade-off:** fewer deployments vs. separation of concerns and **clear component ownership**.
- Do we need a **first-class Redis API** to test membership by job ID without scanning the entire sorted set?
- For `in_progress` orphans, is **re-enqueue from scratch** always acceptable to product, or must we **always fail** if any output artifact might exist in object storage?
- Multi-tenant scale: reconciliation **batch size**, **cursor pagination**, and **per-tenant fairness**.
- Interaction with **cancellation** and **pause/resume** if extended in the future.

## Related work

- [PR #135](https://github.com/llm-d-incubation/batch-gateway/pull/135) (draft) proposes **Exchange DB / priority-queue interface changes** for recovery: in-flight state and **heartbeats** around dequeue, **`PQSignalDone`**, and a periodic **`PQReEnqueue`** scanner. That work targets **queue-level** correctness; it was deferred past MVP. **This document does not replace that discussion**—cluster-level reconciliation here addresses **metadata and multi-store** gaps that can remain even with richer queue semantics (for example PostgreSQL vs Redis vs worker truth, or Redis incidents). If #135 lands, **deployment of the scanner**, **Redis data layout** (including alternatives such as an explicit **worker registry** plus per-job heartbeats), and **idempotency** should be **aligned** so two systems do not fight the same jobs.

## References

- Processor startup recovery: `internal/processor/worker/recovery.go`
- Architecture notes: [Batch processor architecture](../design/batch_processor_architecture.md) (Startup Recovery)
- Queue contract: `internal/database/api/database.go` (`BatchPriorityQueueClient`)
