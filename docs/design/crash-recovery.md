# Crash Recovery Design

-   **Revision**: 1
-   **Last Updated**: 2026-03-10
-   **Status**: Proposed

---

## Overview

This document describes the design for detecting and recovering batch jobs that were being processed when a processor pod crashes.

When a processor pod crashes (OOM kill, node eviction, hardware failure), any jobs that were dequeued and in progress become orphaned: they are no longer in the priority queue, and no processor is working on them. These jobs remain in a non-terminal status (`validating`, `in_progress`, `finalizing`, `cancelling`) indefinitely, visible to users as stuck jobs.

---

## Recovery Strategy

Recovery behavior depends on the job's status at the time of crash, and on whether local artifacts survived (container restart within the same pod vs. pod-level failure).

### `emptyDir` Lifetime

The processor's `workdir` is an `emptyDir` volume. Its lifetime depends on the failure mode:

- **Pod-level failure** (node eviction, node crash, pod deletion): `emptyDir` is destroyed along with the pod. All local artifacts are lost. The only option is restart from scratch.
- **Container-level failure** (OOM kill, process panic): Kubernetes restarts the container within the same pod. `emptyDir` survives and local artifacts remain on disk. Phase-aware recovery is possible.

### Required Changes

Recovery requires four additions to the system:

1. **Version-based fencing (optimistic locking).** A `version` field is added to each job record in the DB. All status updates use conditional writes (`UPDATE ... WHERE id=? AND version=?`). When a crashed job is recovered, the version is incremented. This prevents a stale processor (slow, not dead) from overwriting updates made by the new owner. See [Fencing: Optimistic Locking](#fencing-optimistic-locking) for details.

2. **In-process job tracking in Redis.** Each processor maintains a Redis set of job IDs it is currently working on. When a job is dequeued, its ID is added (`SADD "batch:processor-X:jobs" "job-id"`); when it completes (or is released), it is removed (`SREM`).

3. **Startup recovery in the processor.** A recovery step runs at processor startup, before the polling loop begins. It scans the local `workdir` for leftover job artifacts from a previous execution. If any are found, the processor performs phase-aware recovery (see below) — uploading completed output if possible, or re-enqueuing the job for reprocessing — then cleans up stale files before proceeding with normal operation.

4. **Orphan detection in batch-gc (→ batch-reconciler).** The batch-gc process already performs periodic DB scans for maintenance. It is extended to detect orphaned jobs: it queries the DB for all non-terminal jobs, then cross-references them against the in-process job sets in Redis. Any non-terminal job that no alive processor claims is considered orphaned. The reconciler recovers these by resetting them to `validating` and re-enqueuing them for reprocessing. Since the process now handles both garbage collection and crash recovery, it is renamed from `batch-gc` to `batch-reconciler` to reflect its broader scope.

---

## Fencing: Optimistic Locking

When crash recovery re-enqueues a job, the original processor may still be alive (slow, not dead). This creates a risk of two processors working on the same job simultaneously.

### Solution: Version-Based Conditional Updates

A `version` field on each job acts as a fencing token:

1. Worker records the job's `version` when it starts processing.
2. All status updates use: `UPDATE ... SET status=? WHERE id=? AND version=?` — the version value in the row stays unchanged during normal processing.
3. If `affected_rows == 0`, the version changed (another actor took over) → worker stops immediately.
4. Only the scanner (or startup recovery) increments `version` when re-enqueuing a crashed job. This is the sole mechanism that invalidates a stale processor's updates.

---

## Crashed Job Detection

### Mechanism: Redis Processor Registry + Health Check

This design uses a processor-level registry in Redis combined with active health checking to determine whether a processor is alive.

### How It Works

1. **Processor registration**: When a processor pod starts, it registers itself in a Redis set (e.g., `SADD "batch:processors" "processor-A:9090"`). The processor ID is derived from the pod hostname (`os.Hostname()`) and the listening port.
2. **Graceful deregistration**: On graceful shutdown, the processor removes itself (`SREM "batch:processors" "processor-A:9090"`) and deletes its job set (`DEL "batch:processor-A:jobs"`).
3. **Crash leaves stale entries**: If the processor crashes (OOM, node eviction), its registry entry and job set remain in Redis.
4. **In-process job tracking via Redis**: When a processor dequeues a job, it adds the job ID (`SADD "batch:processor-A:jobs" "job-1"`). When a job completes or is released, it removes it (`SREM`). The reconciler reads these sets to determine which jobs are actively being processed.
5. **Scanner (in batch-reconciler)**: Periodically scans for crashed jobs by health-checking registered processors and cross-referencing their Redis job sets with non-terminal jobs in the DB.

### Scanner Location

The crash scanner runs inside the `batch-reconciler` process (formerly `batch-gc`, PR #28), which is already designed as a separate single-replica Deployment for periodic DB maintenance tasks.

**Rationale:**

- Reuses existing infrastructure (Deployment, config, DB clients, graceful shutdown).
- Single replica avoids duplicate scanning across multiple processor pods.
- GC and crash scanner are both periodic DB maintenance tasks — natural fit.
- Fencing (optimistic locking) makes duplicate scanner execution safe regardless, but single-instance is cleaner.

The batch-reconciler process needs additional dependencies:
- A queue client (`PQEnqueue`, `PQExists`) for re-enqueuing and checking orphaned jobs
- A Redis client for reading the processor registry and job sets
- An HTTP client for health-checking processor endpoints

```
batch-reconciler process:
  go gcLoop(interval: 30m)          ← existing: delete expired jobs/files (by DB expiry)
  go crashScanLoop(interval: 1m)    ← new: detect and re-enqueue orphaned jobs
```

The two loops operate on different concerns and do not conflict: the crash scanner skips expired jobs (leaving them for `gcLoop` to delete), and `gcLoop` only deletes records whose `expiry` has passed regardless of status.

### Scanner Flow

```
1. SMEMBERS "batch:processors"
   → ["A:9090", "B:9090", "C:9090", "D:9090"]

2. Health-check each registered processor (GET /health)
   → A: no response (confirmed dead — suspected last scan, still failing)
   → B: 200 OK
   → C: 200 OK
   → D: no response (newly suspected — first failure, skip this scan)

3. Read in-process jobs from Redis sets for all processors:
   Confirmed dead: SMEMBERS "batch:processor-A:jobs" → [job-1, job-2]  (orphaned)
   Alive:          SMEMBERS "batch:processor-B:jobs" → [job-3, job-4]  (active)
   Alive:          SMEMBERS "batch:processor-C:jobs" → [job-5]         (active)
   Suspected:      SMEMBERS "batch:processor-D:jobs" → [job-7]         (skip this scan)

4. Confirmed dead processors' jobs are orphaned: [job-1, job-2]
   Suspected processors' jobs are skipped: [job-7] (wait for next scan to confirm)

5. DB scan: SELECT id FROM jobs WHERE status IN non-terminal statuses
   → [job-1, job-2, job-3, job-4, job-5, job-6, job-7]

6. Jobs not in any processor's set (alive, dead, or suspected): {job-6}
   → Check priority queue: is job-6 still in the queue?
     - Yes: not yet dequeued, waiting to be processed → skip
     - No: dequeued but lost (crashed between dequeue and SADD) → orphaned

7. Recover orphaned jobs:
   - `cancelling` → conditional update to `cancelled` (no re-enqueue)
   - All other statuses → conditional update to `validating` + re-enqueue

8. Cleanup dead processor entries:
   DEL "batch:processor-A:jobs"    ← set
   SREM "batch:processors" "A:9090"
```

### Scanner Logic (Pseudocode)

```
// persistent state across scans:
suspected_dead = set()    // processors that failed health check on previous scan

every scan_interval:
  registered = SMEMBERS "batch:processors"

  alive_processors    = []
  confirmed_dead      = []
  newly_suspected     = []
  for each processor in registered:
    if health_check(processor, retries=3):
      alive_processors.append(processor)
      suspected_dead.remove(processor)    // recovered, clear suspicion
    else if processor in suspected_dead:
      confirmed_dead.append(processor)    // failed twice → truly dead
      suspected_dead.remove(processor)
    else:
      newly_suspected.append(processor)   // first failure → suspect only
      suspected_dead.add(processor)

  // collect in-process jobs from Redis sets
  alive_jobs = set()
  for each processor in alive_processors:
    jobs = SMEMBERS "batch:processor-{id}:jobs"
    alive_jobs.add_all(jobs)

  suspected_jobs = set()
  for each processor in newly_suspected:
    jobs = SMEMBERS "batch:processor-{id}:jobs"
    suspected_jobs.add_all(jobs)

  orphaned_from_dead = set()
  for each processor in confirmed_dead:
    jobs = SMEMBERS "batch:processor-{id}:jobs"
    orphaned_from_dead.add_all(jobs)

  // find additional orphans: non-terminal, non-expired jobs not tracked by any processor set
  non_terminal_jobs = SELECT id, status, version, expiry FROM jobs
    WHERE status IN ('validating', 'in_progress', 'finalizing', 'cancelling')

  for each job in non_terminal_jobs:
    if job.expiry < now():
      continue                    // expired — leave for GC to delete
    if job.id in alive_jobs:
      continue                    // actively being processed
    if job.id in suspected_jobs:
      continue                    // processor suspected dead, wait for next scan to confirm
    if job.id in orphaned_from_dead:
      // known orphan from confirmed dead processor — recover below
    else if PQExists(job.id):
      continue                    // still in queue, waiting to be dequeued
    else:                        // not in any set AND not in queue → lost between dequeue and SADD

    // recovery depends on status:
    if job.status == 'cancelling':
      conditional update: status → 'cancelled' (WHERE version = job.version)
      // version mismatch → processor restarted and already handling it (rare, safety net)
    else:
      conditional update: status → 'validating', version+1 (WHERE version = job.version)
      if update succeeded:
        re-enqueue via PQEnqueue
      // version mismatch → processor restarted and already handling it (rare, safety net)
    record log + metric

  // cleanup confirmed dead processor entries only
  for each processor in confirmed_dead:
    DEL "batch:processor-{id}:jobs"    ← set
    SREM "batch:processors" processor
    record log + metric
```

### Dequeue-to-SADD Race

A job that was just dequeued may not yet appear in the processor's Redis set (race between BZMPOP and SADD). This window is extremely small (sub-millisecond, within the same goroutine). If the reconciler happens to scan during this window, it would see the job as orphaned and re-enqueue it. This is harmless: the original processor's next conditional status update would fail due to version mismatch (fencing), and the job would be processed correctly via the queue. No grace period is needed.

### Health Check Reliability

To avoid false positives from transient failures or container restarts:

- The scanner retries health checks (e.g., 3 attempts with short backoff) within a single scan.
- If all retries fail, the processor is marked as **suspected dead** — but its entries are NOT cleaned up yet.
- On the **next scan cycle**, if the processor still fails health checks, it is confirmed dead. Only then does the scanner recover its jobs and clean up its Redis entries.

This two-phase detection prevents premature cleanup of a processor that is merely restarting. A container restart typically completes in seconds, well within the scan interval (1 min). Without this, a race is possible: the reconciler deletes a processor's registry entry after the processor has already re-registered on restart, causing the processor to silently disappear from the registry.

### Known Limitations

**Stuck goroutine** (processor alive, single worker hung): The processor passes health checks, so the reconciler considers its jobs active. Detection relies on SLO expiry as a backstop.

---

## Recovery Details

### Phase-Aware Recovery

Phase-aware recovery applies only to **container-level restarts** where `emptyDir` survives and local artifacts remain on disk.

For all statuses, the processor **immediately adds discovered jobs to its Redis set** (`SADD "batch:processor-X:jobs" "job-id"`) before taking any recovery action. This prevents the reconciler from treating them as orphaned while recovery is in progress.

| Status at crash | Local artifacts | Recovery action |
|---|---|---|
| `validating` | Partial input, partial plan files | **Restart from scratch.** No inference has been executed (status transitions to `in_progress` only after Phase 1 completes and before Phase 2 begins). Re-processing cost is negligible. Processor resets to `validating` and re-enqueues on startup. |
| `in_progress` | Complete input + plan, partial output/error files | **Restart from scratch.** Resuming would require matching `custom_id` in output/error files against the plan to determine which requests were completed — high implementation complexity for marginal gain in typical workloads. Processor resets to `validating` and re-enqueues on startup. Deferred; revisit if operational data shows significant inference duplication cost. |
| `finalizing` | Complete input + plan + **complete output/error files** | **Resume: upload only.** Phase 2 completed successfully, so output and error files are intact. Processor self-recovers on startup (see below). |
| `cancelling` | Varies | **Transition to `cancelled` directly.** Processor updates status to `cancelled` on startup, since re-enqueuing a `cancelling` job would be skipped by workers (`IsJobRunnable` check). If partial output/error files exist, upload them before transitioning (same as partial output upload for cancelled jobs). |

When local artifacts are not available (pod-level failure), the processor cannot self-recover. The reconciler handles these: `cancelling` jobs are transitioned to `cancelled`; all other statuses are reset to `validating` and re-enqueued from scratch.

### Recovery Actors

| Actor | Trigger | Responsibility |
|---|---|---|
| **Processor (startup)** | Container restart (emptyDir survives) | `finalizing` → upload output → `completed` |
| **Processor (startup)** | Container restart (emptyDir survives) | `cancelling` → upload partial output if present → `cancelled` |
| **Processor (startup)** | Container restart (emptyDir survives) | `validating`, `in_progress` → reset to `validating` + re-enqueue |
| **Reconciler** | Periodic scan (any failure mode) | Non-terminal orphaned jobs except `cancelling` → reset to `validating` + re-enqueue |
| **Reconciler** | Periodic scan (any failure mode) | `cancelling` → transition directly to `cancelled` |

### Processor Startup Recovery

On container restart within the same pod, `emptyDir` retains artifacts from the previous execution. Before entering the polling loop, the processor scans for leftover job directories and performs phase-aware recovery for each (see table above).

The workdir directory structure is `{workdir}/{tenantHash}/jobs/{jobID}` — the job ID can be extracted from the directory name. The tenant ID is not directly available (the folder uses a SHA256 hash), but is retrieved from the DB query by job ID.

```
Recovery sequence (runs at startup, before polling loop):

1. Scan workdir for leftover job directories, extract job IDs from directory names
2. For each discovered job ID:
   a. Add job to Redis set (SADD "batch:processor-X:jobs" "job-id")
   b. Query DB for current status, version, and tenant ID
   c. If terminal: clean up files, SREM from set, skip
   d. Recovery action per status (see Phase-Aware Recovery table):
      - `finalizing`: upload output → conditional update to `completed`
        (WHERE version = current_version). If update fails (version mismatch),
        reconciler handled it concurrently → clean up files. (rare, safety net)
      - `cancelling`: conditional update to `cancelled` (WHERE version = current_version).
        Upload partial output if present. If update fails → reconciler already
        transitioned to `cancelled` (reconciler never re-enqueues `cancelling` jobs,
        see scanner logic) → clean up files. (rare, safety net)
      - `validating`, `in_progress`: conditional update to `validating`, version+1
        (WHERE version = current_version) + PQEnqueue.
        If update fails → reconciler already re-enqueued → clean up files. (rare, safety net)
3. Register in Redis (SADD "batch:processors")   ← idempotent; re-registers if reconciler
                                                    cleaned up the entry during restart window
4. Remove jobs transitioned to terminal in step 2d from Redis set (SREM)
5. Clean up all remaining stale job directories
6. Enter polling loop
```

Step 2a before step 3 ensures that discovered jobs are in the Redis set before the processor becomes visible to the reconciler. This prevents the reconciler from considering these jobs as orphaned during recovery.

No separate "claim" step is needed. The conditional updates on the actual status transitions (step 2d) provide fencing naturally: if the reconciler acts concurrently, one of the two conditional updates will fail due to version mismatch, and the loser backs off.

### Rejected Alternatives

- **Shared PVC for workdir**: Requires ReadWriteMany access mode, introduces security concerns (multiple pods accessing same filesystem), performance implications.
- **Direct write to shared storage**: Every inference result written directly to S3/PVC instead of local buffer. Negates the cost advantage of batch processing over interactive inference. Significant performance degradation from per-request remote I/O.
- **Phase 2 resume via output diffing**: Count valid lines in the partial output/error files, match `custom_id` to plan entries, skip completed requests. Technically possible but high complexity (corrupt last line handling, buffered writer data loss, plan-to-output reconciliation). Deferred unless operational data justifies the investment.

---

## Interface Changes

### DB Interface Changes

Current `DBUpdate` performs unconditional overwrites. Changes required:

1. Add field to `BatchItem`: `Version`
2. Add conditional update capability to `DBUpdate` (or introduce a new method): return whether update was applied
3. Implement in all backends:
   - PostgreSQL: `UPDATE ... WHERE id = ? AND version = ?`
   - Redis: Lua script for atomic check-and-set
   - Mock: in-memory version check

### Queue Interface Changes

`BatchPriorityQueueClient` currently has `PQEnqueue`, `PQDequeue`, and `PQDelete`. The crash scanner needs to check whether a job is still in the queue. Add:

- `PQExists(ctx, jobID) (bool, error)` — checks if a job ID is present in the queue. Implemented via `ZSCORE` (O(1)) in Redis.

### Processor Registry Interface (new)

No existing interface covers processor-level registry or per-processor job tracking. Define a new `BatchProcessorRegistryClient` interface:

- `RegisterProcessor(ctx, processorID) error` — add processor to registry (`SADD "batch:processors"`)
- `UnregisterProcessor(ctx, processorID) error` — remove processor from registry (`SREM "batch:processors"`)
- `ListProcessors(ctx) ([]string, error)` — list all registered processors (`SMEMBERS "batch:processors"`)
- `AddJob(ctx, processorID, jobID) error` — add job to processor's set (`SADD "batch:processor-{id}:jobs"`)
- `RemoveJob(ctx, processorID, jobID) error` — remove job from set (`SREM`)
- `ListJobs(ctx, processorID) ([]string, error)` — list all jobs for a processor (`SMEMBERS`)
- `DeleteJobSet(ctx, processorID) error` — delete entire set (`DEL "batch:processor-{id}:jobs"`)

Redis-only; no PostgreSQL/Mock implementation needed (processor registry is ephemeral by design).

---

## Observability

### Processor Startup Recovery Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `batch_startup_recovery_total` | Counter | `status`, `result` | Jobs discovered during startup recovery. `status`: job status at discovery (`validating`, `in_progress`, `finalizing`, `cancelling`). `result`: outcome (`recovered`, `claim_failed`, `cleanup`). |

### Reconciler Scan Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `batch_orphaned_jobs_detected_total` | Counter | `status` | Orphaned jobs detected per scan, by status at detection. |
| `batch_orphaned_jobs_recovered_total` | Counter | `action` | Orphaned jobs recovered, by action taken (`re_enqueued`, `cancelled`). |
| `batch_dead_processors_cleaned_total` | Counter | | Dead processor entries removed from Redis. |
| `batch_reconciler_scan_duration_seconds` | Histogram | | Time taken per reconciler crash scan cycle. |

These metrics enable data-driven decisions on whether to invest in checkpointing: if `batch_orphaned_jobs_detected_total{status="in_progress"}` is consistently high and the affected jobs are large, the inference duplication cost may justify the investment.

---

## Implementation Plan

> This section is included while the design is under review to help readers understand the scope. It will be removed once the design is finalized.

### PR Dependency Graph

```
PR 1: DB + queue interface changes (version, conditional update, PQExists)
  ├→ PR 2: Fencing (optimistic locking) in processor StatusUpdater
  └→ PR 4: Crashed job scanner in batch-reconciler (also depends on PR #28 and PR 3)
PR 3: Processor registry + in-process job tracking (Redis)
  └→ PR 4
```

PR 1 is a prerequisite for PR 2 and PR 4. PR 3 is a prerequisite for PR 4.

### PR 1: DB + Queue Interface Changes

Add `version` field, conditional update support across all database backends, and `PQExists` to the queue interface.

**Files affected:**
- `internal/database/api/base_item.go` — new `Version` field
- `internal/database/api/database.go` — conditional update interface + `PQExists`
- `internal/database/postgresql/`, `internal/database/redis/` — conditional update (both `DBClient` backends must be updated)
- `internal/database/redis/` — `PQExists` via `ZSCORE`
- `internal/database/mock/` — in-memory implementation for tests

### PR 2: Fencing in Processor

Use version-conditioned status updates so that a stale worker stops processing when its version is outdated.

**Files affected:**
- `internal/processor/worker/status_updater.go` — version-conditioned updates
- `internal/processor/worker/worker.go` — record version on dequeue, abort on version mismatch

### PR 3: Processor Registry + In-Process Job Tracking (Redis)

Define `BatchProcessorRegistryClient` interface and Redis implementation. Register/deregister processor on startup/shutdown. Track in-process jobs via Redis set. Includes startup recovery logic for container restarts.

**Files affected:**
- `internal/database/api/database.go` — new `BatchProcessorRegistryClient` interface
- `internal/database/redis/` — Redis implementation of the interface
- `cmd/batch-processor/main.go` — registry calls on start/shutdown
- `internal/processor/worker/worker.go` — job tracking on dequeue/completion, startup recovery logic
- `internal/processor/config/config.go` — processor ID config

### PR 4: Crashed Job Scanner

Add crash scanner loop to batch-reconciler. Health-check registered processors, read Redis job sets, cross-reference with DB, recover orphaned jobs. Includes renaming `batch-gc` → `batch-reconciler`.

**Depends on:** PR #28 (batch-gc baseline), PR 1 (version field for fencing), PR 3 (registry + job tracking)

**Files affected:**
- `internal/gc/` — new scanner module
- `cmd/batch-reconciler/main.go` — Redis client, HTTP client, queue client initialization (renamed from `cmd/batch-gc/`)
- `internal/gc/config/config.go` — scanner config (interval, health check retries)
- `charts/batch-gateway/` — scanner config in gc values
