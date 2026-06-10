# Orphan Job Recovery Design

- **Revision**: 1
- **Last Updated**: 2026-05-15
- **Issue**: [#434](https://github.com/llm-d/llm-d-batch-gateway/issues/434)

---

## Problem

When a batch-processor pod crashes (OOM kill, node eviction, pod deletion), jobs that were dequeued from the Redis priority queue can become **orphaned** — stuck in a non-terminal DB status with no processor working on them.

The existing startup recovery (`recovery.go`) only handles **container restarts within the same pod** (it scans the local `emptyDir` workdir). If the pod is **replaced** (e.g., node eviction destroys `emptyDir`), those jobs are lost forever.

### Orphan scenarios

1. **Pod replacement**: Processor dequeues a job, then the pod is replaced. Workdir is gone; startup recovery on new pods can't find it.
2. **Silent abandonment**: A worker fails to re-enqueue or transition a job to a terminal state (e.g., DB unreachable during error handling), logs the error and moves on. The pod is healthy, so startup recovery won't run.

### The orphan window

After `PQDequeue` atomically removes a job from Redis, if the pod crashes before the worker creates a workdir and transitions the status:
- **Queue**: Job removed (won't be re-dequeued)
- **Workdir**: Doesn't exist yet (no startup recovery can find it)
- **DB**: Stuck with non-terminal status forever

---

## Design Overview

Three mechanisms work together to detect orphans, recover them, and guarantee data consistency:

1. **In-flight tracker with heartbeat** — processor records job ownership in Redis, refreshes every 5 minutes
2. **Orphan reconciler** (PR 2) — batch-gc periodically cross-references DB, queue, and in-flight hash to find and recover orphans
3. **Compare-and-swap (CAS) on DB status transitions** — prevents a reconciled job from being overwritten by a stale processor

### Data flow

```
Normal path:
  PQDequeue → InFlightSet(jobID, podID) → process (heartbeat every 5m)
            → CAS terminal write → InFlightDelete(jobID)

Crash recovery:
  PQDequeue → InFlightSet(jobID, podID) → heartbeat → [POD CRASHES]
                                                          ↓
  [Reconciler cycle, ~60 min later]
    Query DB: non-terminal jobs
    Query queue: pending job IDs  →  jobID not in queue
    Query in-flight hash          →  heartbeat stale (>60 min old)
    Triage: re-enqueue or CAS to terminal
    InFlightDelete(jobID)         →  cleanup

False positive recovery (processor still alive but reconciler acted):
  Processor heartbeat reads DB status → sees unexpected status → cancels job context
  OR: Processor tries CAS terminal write → ErrConflict → processor aborts
```

---

## Mechanism 1: In-Flight Tracker with Heartbeat

### Redis data structure

**Key**: `llmd_batch:inflight` (single Redis hash)
**Fields**: job IDs
**Values**: JSON `{"pid":"<hostname>","ts":<unix_seconds>}`

Why a hash: `HSCAN` lets the reconciler iterate all in-flight jobs without knowing IDs upfront. Single key, no key-space pollution, `HSET`/`HDEL` are O(1).

### Interface

File: `internal/database/api/database.go`

```go
type InFlightEntry struct {
    ProcessorID string `json:"pid"`
    LastSeen    int64  `json:"ts"`
}

type InFlightClient interface {
    store.BatchClientAdmin
    InFlightSet(ctx context.Context, jobID, processorID string) error
    InFlightDelete(ctx context.Context, jobID string) error
    InFlightGetAll(ctx context.Context) (map[string]*InFlightEntry, error)
}
```

Separate from `BatchPriorityQueueClient` so the reconciler gets `InFlightGetAll` without queue mutation methods.

### Redis implementation

File: `internal/database/redis/redis_inflight.go`

- `InFlightSet`: `HSET llmd_batch:inflight <jobID> <json>` — O(1). Serves as both initial registration and heartbeat refresh.
- `InFlightDelete`: `HDEL llmd_batch:inflight <jobID>` — O(1).
- `InFlightGetAll`: `HSCAN llmd_batch:inflight 0 * 100` — paginated iteration.

### Processor instrumentation

**Registration** — after dequeue (`worker.go`):
Call `InFlightSet(jobID, processorID)` immediately after `PQDequeue` succeeds. Non-fatal on error.

> **Why not atomic with PQDequeue**: `PQDequeue` uses `BZMPOP` (blocking pop), which Redis forbids inside Lua scripts. The gap between the two calls is microseconds, while the reconciler runs every 60 minutes. Even if the reconciler scans in between, it would see a `validating` job and re-enqueue it — CAS prevents data corruption when a second processor picks up the duplicate.

**Heartbeat** — in `runJob()` (`job_runner.go`):
A goroutine with a 5-minute ticker calls `InFlightSet` to refresh the timestamp. The goroutine stops when the job context is cancelled.

**Cleanup** — deferred in `runJob()`:
`InFlightDelete` as the first defer (LIFO = executes last, after terminal transitions and panic recovery).

**Re-enqueue paths** (`worker.go`):
All pre-launch paths that re-enqueue a job (DB fetch failure, malformed job, expired, cancelling, etc.) also call `InFlightDelete`.

### Processor identity

`os.Hostname()` = Kubernetes pod name. Already used in `cmd/batch-processor/main.go` for logging.

---

## Mechanism 2: Orphan Reconciler

> Implemented in PR 2.

Runs in **batch-gc** as a periodic scan. Default interval: 60 minutes (also used as the staleness threshold — ≥ 12× the heartbeat interval).

### Algorithm

1. Fetch non-terminal jobs from DB (paginated).
2. Fetch queue membership via `PQGetIDs()` → `map[string]bool`.
3. Fetch in-flight entries via `InFlightGetAll()`.
4. For each non-terminal job:
   - In queue → skip (waiting to be picked up).
   - In-flight and not stale → skip (actively being processed).
   - Otherwise → orphan (triage).
5. Triage:
   - `cancelling` → CAS to `cancelled`.
   - SLO expired → CAS to `expired`.
   - `validating` → re-enqueue.
   - `in_progress` / `finalizing` → CAS to `failed` (local files are lost).
6. Cleanup stale in-flight entries.
7. Cleanup terminal in-flight entries (processor crashed between CAS write and `InFlightDelete`).

### PQGetIDs

New method on `BatchPriorityQueueClient`. Implemented as a Lua script that iterates the sorted set server-side via `ZSCAN`, extracts IDs with `cjson.decode`, returns the ID list. Go side converts to `map[string]bool`.

---

## Mechanism 3: Compare-and-Swap (CAS) on DB Status Transitions

### Problem

Without CAS, the reconciler and processor can race: the processor overwrites the reconciler's `failed` with `completed`, leading to status flicker or orphaned file records.

### Design

`DBUpdate` accepts an optional `expectedStatus []byte`. If non-nil, the update only proceeds if the current status matches; otherwise returns `ErrConflict`.

- **PostgreSQL**: `UPDATE ... SET status = $new WHERE id = $id AND status = $expected` — 0 rows affected = conflict.
- **Redis**: Lua script checks status field before writing. Single atomic call.

Callers that don't need CAS pass `nil` (e.g., apiserver cancel handler).

### Conflict handling

Both processor and reconciler use CAS, so regardless of which writes first, the other detects the conflict and yields.

---

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| False positive (reconciler recovers a job still being processed) | Heartbeat makes this unlikely (requires ~60 min of sustained Redis unreachability). CAS prevents data corruption. |
| InFlightSet fails (Redis down at dequeue time) | Non-fatal. Reconciler detects orphan via DB + queue cross-reference. |
| CAS conflict during normal operation | Only if reconciler and processor race. Processor sees `ErrConflict`, logs, aborts — no data loss. |
| Large DB scan | Paginated (existing GC pattern). Active batch count bounded by throughput. |
| Race: reconciler vs. startup recovery | Startup recovery runs before polling. `PQEnqueue` uses `ZADD NX` (idempotent). CAS prevents conflicting writes. |
| In-flight hash unbounded | Entries ~100 bytes. Reconciler clears stale entries. |

---

## Implementation Plan

### PR 1: In-flight tracking + CAS + processor instrumentation

Adds tracking infrastructure and processor instrumentation. Writes breadcrumbs but nothing reads them yet — zero behavioral risk.

### PR 2: Orphan reconciler + GC wiring

Builds reconciler logic and wires it into batch-gc. Activates orphan detection and recovery.

### Follow-up: Hardening and observability

- Prometheus metrics for reconciler
- Reconciler dry-run mode validation
