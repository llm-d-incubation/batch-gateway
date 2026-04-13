# Orphan batch job detection and reconciliation

## Summary

This work is **not** a replacement for **startup recovery** (`recoverStaleJobs`), which already repairs many failures using **local workdir** state when a processor restarts. The goal here is a separate line of defense: **cluster-level** detection and reconciliation for batch jobs that **fall off the normal track** for **other reasons**—cases where startup recovery never gets a chance to run, or queue and DB drift apart in ways the local path cannot see. These situations are **severe edge cases** (pod loss with destroyed ephemeral disk, partial failures across Redis and PostgreSQL, races between replicas, and similar “should be rare” paths), but they **can still happen** in real deployments; without an explicit reconciler, such jobs can remain **non-terminal** indefinitely.

The proposal introduces a precise definition of **orphan batch jobs** in that sense (see [Definitions](#definitions), especially [Orphan job (narrow, reconciler scope)](#orphan-job-narrow-reconciler-scope)) and a reconciliation design for them. **Orphan state** in this narrow sense still arises from the **processor + Redis queue + PostgreSQL** interaction—for example a job is dequeued, then the worker or pod is lost before metadata reaches a terminal status, and **no** stale workdir remains for `recoverStaleJobs`. The API server and garbage collector do **not** create that pattern by themselves; they still matter because a reconciler must **compose safely** with **every processor replica**, **API traffic** (submit, cancel, read paths), and **GC retention** so fixes are not duplicated, raced, or contradicted cluster-wide. When a stale workdir **does** exist, startup recovery remains the **owner**.

## Definitions

### Non-terminal status

A batch job whose PostgreSQL metadata status is not a terminal OpenAI batch status (for example still `validating`, `in_progress`, `finalizing`, or `cancelling` — exact set follows product contracts).

### Actively processing

A processor replica **owns** the job in the runtime sense: the job has been **removed from the Redis priority queue** and a **`runJob` worker goroutine** for that job ID is running (ingestion via `preProcessJob`, then execution, finalization, or cancel handling—see processor architecture).

**`validating` in DB is ambiguous** in this product model: the **API server creates** a batch job and **enqueues** it to Redis with status already **`validating`** (OpenAI-compatible lifecycle: a new job is `validating` until it moves to `in_progress` after ingestion). So “in the queue” and “`validating` in PostgreSQL” are **normal** together—they do **not** by themselves indicate a stuck or orphaned job.

That same status covers two runtime situations until **`in_progress`**: (1) **waiting in the queue**—still enqueued, not yet dequeued—and (2) a **short window after dequeue** where ingestion runs but the status has not yet transitioned to `in_progress`. Only (1) is “idle wait”; (2) **is** actively processing even though the status string is still `validating`.

For orphan detection, **“not actively processing”** therefore requires more than `status == validating`: for example **still present in the priority queue** (queue wait) or **no live worker goroutine** after dequeue (candidate orphan, subject to grace periods and stale-workdir rules elsewhere in this document).

### Diverged job (umbrella, optional term)

Any job that is **non-terminal** in DB while **no processor is actively processing** it. This includes:

- jobs **not** currently in the Redis priority queue (typical after atomic dequeue), and
- jobs still in the queue but stuck for other reasons (if any appear in operations; call out separately if discovered).

This umbrella is useful for metrics or runbooks (“something is wrong with lifecycle”) but is **not** sufficient to choose a recovery action by itself.

### Stale workdir job (startup-recovery eligible)

A job for which the processor’s local layout still contains a **stale job directory** under `WorkDir` that `recoverStaleJobs` is designed to discover on **processor startup** (same pod / surviving `emptyDir` as today’s architecture). For that job, **startup recovery is the designated owner**: it should re-enqueue, finalize, cancel, fail, or clean up according to existing phase-aware rules.

**Relationship to “diverged”:** After a crash and **before** the next `recoverStaleJobs` run, such a job may look “nothing is processing it” from the outside. It is still **not** an *orphan job* in the **narrow sense** used below, because a recovery path already exists and should run first.

### Orphan job (narrow, reconciler scope)

An **orphan job** is a **diverged job** that is **also** **not** startup-recovery eligible: there is **no** stale processor workdir that `recoverStaleJobs` can use to reconcile the job on restart (for example pod eviction destroyed `emptyDir`, or only a different replica’s disk could be checked and this job’s artifacts are gone). Equivalently:

- DB: **non-terminal**
- Redis PQ: **not** enqueued (or not findable as queued — exact membership check is implementation detail)
- Runtime: **no** processor **actively processing** the job
- Local recovery: **no** applicable stale job directory for `recoverStaleJobs`

**Answer to “are recoverStaleJobs-pending jobs orphans?”**

- Under the **broad / observational** view (“diverged”: non-terminal and nobody running it), a stale workdir job **can match** between crash and the next recovery scan — but **ownership** belongs to `recoverStaleJobs`.
- Under the **narrow definition** above (what this proposal’s reconciler should target), **no**: if the job **will be** (or **should be**) handled by `recoverStaleJobs` because a stale workdir exists, it is a **stale workdir job**, **not** an orphan job.

Implementations should use **ordering and exclusion**: e.g. only treat as reconcile-orphan after a **grace period** and after confirming **absence** of a recoverable workdir signal, so the cluster reconciler does not fight startup recovery.

---

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

Recommendation in this proposal: prefer a **dedicated reconciler** or **elected single instance** with explicit leader semantics, not N uncoordinated replicas.

### Data sources

- **PostgreSQL**: authoritative job status and metadata.
- **Redis**: queue membership via existing admin/list operations or new indexed queries if needed (today `PQDelete` takes ID+SLO; listing queue members may require new API for efficient reconciliation—call out as open point).

### Observability

- Counters/histograms for: orphans detected, re-enqueued, failed, expired, errors, reconciliation duration.
- Logs: job ID, tenant, previous status, action, reason code.

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

- Do we need a **first-class Redis API** to test membership by job ID without scanning the entire sorted set?
- For `in_progress` orphans, is **re-enqueue from scratch** always acceptable to product, or must we **always fail** if any output artifact might exist in object storage?
- Multi-tenant scale: reconciliation **batch size**, **cursor pagination**, and **per-tenant fairness**.
- Interaction with **cancellation** and **pause/resume** if extended in the future.

## References

- Processor startup recovery: `internal/processor/worker/recovery.go`
- Architecture notes: `docs/design/batch_processor_architecture.md` (Startup Recovery)
- Queue contract: `internal/database/api/database.go` (`BatchPriorityQueueClient`)
