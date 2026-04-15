# Orphan batch job detection and reconciliation

## Summary

This work is **not** a replacement for **startup recovery**, which already repairs many failures using **local workdir** state when a processor restarts. The goal here is a separate line of defense: **cluster-level** detection and reconciliation for batch jobs that **fall off the normal track**. The core case is **queue–database mismatch**: a worker dequeued a job, then the pod was replaced and ephemeral disk destroyed, so the job is no longer in Redis while PostgreSQL still shows a non-terminal status—and startup recovery has nothing to work with. These are **edge cases**, but without an explicit reconciler such jobs can remain **non-terminal** indefinitely.

The proposal introduces a precise definition of **orphan batch jobs** in that sense, together with supporting terms used in this document (see [Definitions](#definitions), especially [Orphan job (narrow, reconciler scope)](#orphan-job-narrow-reconciler-scope)), and a reconciliation design. **Orphan state** in this narrow sense arises from two categories of failure: (1) the worker or pod is **lost** after dequeue before metadata reaches a terminal status, and no stale workdir remains for startup recovery; (2) the worker is **still running** but abandoned the job after a best-effort failure (e.g. re-enqueue and terminal DB write both failed), logged the error, and moved on — the pod is healthy but no component is aware the job is stuck.

This proposal is intentionally smaller than earlier recovery designs (see [PR #118](https://github.com/llm-d-incubation/batch-gateway/pull/118)). It removes processor registries, version-based fencing, and per-processor Redis sets in favor of prerequisite hardening, per-job in-flight tracking, and a DB-driven reconciler that only closes the remaining gap.

The hot-path cost is minimal: the normal job processing flow gains only an `HSET` after dequeue and an `HDEL` on completion (both Redis O(1)). All heavier logic — DB-driven scan, ZSET membership checks, K8s API queries — runs in a separate background reconciler process and does not affect job processing latency.

## Definitions

### Non-terminal status

A batch job whose PostgreSQL metadata status is not a **terminal** status in the **OpenAI Batch API** sense (for example still `validating`, `in_progress`, `finalizing`, or `cancelling`).

### Actively processing

A processor replica **owns** the job in the runtime sense: the job has been **removed from the Redis priority queue** and a **worker goroutine** for that job ID is running.

### Diverged job

**PostgreSQL still shows a non-terminal status, but no processor is actively running that job** in the sense of [Actively processing](#actively-processing).

### Best-effort abandonment

A job worker that fails to either re-enqueue the job or transition it to a terminal DB state will **log the error and move on**. When DB transition attempt fails, the worker releases the token and moves on. This is the correct trade-off: retrying indefinitely against an unavailable dependency would hold the worker token, block other jobs, and risk stalling the entire processor. The trade-off is that the abandoned job remains non-terminal in PostgreSQL with no component aware of it — which is exactly the gap this proposal's reconciler closes.

Examples of paths where this occurs today (all in the polling loop or spawned worker):

- (polling loop) Dequeue succeeds → DB fetch fails → re-enqueue fails → `continue`
- (polling loop) Dequeue succeeds → DB status update to `expired` or `cancelled` fails → `continue`
- (job worker) terminal DB write fails → `return`
- (job worker) panic recovery → terminal DB write fails → `return`

In each case the pod stays healthy and processes other jobs normally. The abandoned job becomes an orphan that only the reconciler can detect.

### Stale workdir job (startup-recovery eligible)

A job for which the processor's local layout still contains a **stale job directory** under `WorkDir` that startup recovery is designed to discover on **processor startup** (same pod / surviving `emptyDir` as today's architecture). For that job, **startup recovery is the designated owner**: it should re-enqueue, finalize, cancel, fail, or clean up according to existing phase-aware rules.

**Relationship to "diverged job":** After a crash and **before** the container restart, such a job may look orphan from the outside. It is still **not** an *orphan job* in the **narrow sense** used below, because a recovery path already exists and should run first.

### Orphan job (narrow, reconciler scope)

An **orphan job** is a **diverged job** that is **also** **not** startup-recovery eligible: there is **no** stale processor workdir that startup recovery can use to reconcile the job on restart (for example pod eviction destroyed `emptyDir`, or only a different replica's disk could be checked and this job's artifacts are gone).

A job is decided as an orphan when 4 conditions below all meet:
1. **PostgreSQL:** metadata shows the job in a **non-terminal** status.
2. **Redis priority queue:** the job is **not** in the priority queue (in the usual case it was **dequeued** and removed atomically).
3. **Processor runtime:** no replica is **actively processing** the job ([Actively processing](#actively-processing)—dequeue alone does **not** imply an orphan if a legitimate job worker still exists).
4. **Startup recovery:** startup recovery does **not** own the next step—there is **no** applicable **stale workdir** ([Stale workdir job](#stale-workdir-job-startup-recovery-eligible)).

Only when **(1)–(4)** hold is a job a **candidate** for cluster-level reconciliation in the **narrow** sense of this proposal.

The rest of this document uses **orphan job** in the **narrow** sense unless explicitly stated otherwise.

## Motivation

### Goals

- Detect jobs that are stuck in non-terminal DB states with no active processor and no surviving workdir for startup recovery.
- Reconcile those jobs to a **safe, observable outcome**: re-enqueue for a clean retry where appropriate, or transition to `failed` / `expired` / `cancelled` with clear semantics.
- Provide **metrics and logging** so operators can see detection frequency, actions taken, and remaining edge cases.
- Keep behavior **compatible with existing queue semantics** (`PQDequeue` atomically removes items; implementations must not double-dequeue the same logical work).

### Non-goals

- Replacing **startup recovery** for the container-restart / same-pod emptyDir case; that remains the fast path when local artifacts exist.
- **Checkpointed resume** of partially executed inference (already out of scope per existing processor design).
- **Scan throughput tuning**: pagination, per-cycle scan caps, Redis/Kubernetes API fanout limits, and bulk recovery rate limiting are implementation concerns. This proposal defines correctness and reconciliation behavior, not final operational tuning parameters.
- **Full Redis flush or restart**: all queue entries and `in_flight` data are lost while PostgreSQL retains non-terminal job records. The reconciler's DB-driven scan would detect these jobs and re-enqueue them, and actively running jobs would self-heal via heartbeat refresh on the next tick. However, this proposal does not specifically optimize for this scenario (e.g. rate-limiting bulk re-enqueue after a full Redis recovery).

### Problem statement

The priority queue contract requires atomic removal on dequeue:

From `internal/database/api/database.go`:

```go
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

with **no local directory** to drive startup recovery. Code comments already acknowledge this gap for pod-level failure:

From `internal/processor/worker/recovery.go`:

```go
dbItem, err := p.poller.fetchJobItemByID(ctx, jobID)
// DB unreachable — can't read status or mark as failed. Leave workdir on disk so the
// next container restart retries. If the pod is evicted (emptyDir destroyed), this job
// becomes an orphan that only an external entity can detect.
```

A **cluster-level reconciler** is the natural place to close that gap.

## Design Details

### Prior work: PR #135 — interface sketch only

[PR #135](https://github.com/llm-d-incubation/batch-gateway/pull/135) proposed three new methods on `BatchPriorityQueueClient`: `PQDequeue` extended to create a "heartbeat mechanism" for the dequeued job, `PQSignalDone` to close it on clean completion, and `PQReEnqueue` to periodically scan for orphaned jobs and re-enqueue them. PR #135 left the heartbeat implementation unspecified and did not address the interaction with startup recovery.

This proposal does not adopt `PQReEnqueue` as a queue interface method. `PQReEnqueue` scans for stale heartbeat entries in Redis, but that approach cannot find orphans that have no `in_flight` data at all—for example a job whose DB cleanup failed after a failed enqueue, leaving the job in PostgreSQL but never in the queue. Instead, this proposal uses a **DB-driven scan**: the reconciler queries PostgreSQL for all non-terminal jobs and checks Redis state per job. Because PostgreSQL is the authoritative record, the scan catches every orphan regardless of whether Redis state exists.

PR #135 also did not include a method for checking whether a specific job ID is present in the queue. This proposal requires a new **`PQContains(ctx, jobID)`** method on `BatchPriorityQueueClient` (Redis: `ZSCORE`, O(log N)) so the reconciler can distinguish jobs still waiting in the queue from jobs that have been dequeued and lost. See [Layer 2](#layer-2-detect) for details.

**The one case the reconciler must not interfere with.** The reconciler should act on any stale in-flight record—unless the owning **container** is actively restarting within the same pod. In that case, `emptyDir` survives and startup recovery will run on container startup to handle the job; the reconciler acting at the same time would race against it. All other cases require the reconciler to step in: pod replaced (new pod, `emptyDir` destroyed, startup recovery has nothing to work with), or container Running but the job worker abandoned the job after a best-effort failure. The container is healthy, startup recovery will not run, and no component is aware the job is stuck.

The challenge is therefore limited to one question: **is the owning pod currently restarting?** Answering it requires persistent ownership data—the pod name and a timestamp—that survives processor death and remains queryable by the reconciler.

### This proposal: timestamp hash for persistent ownership and liveness

This proposal implements the heartbeat mechanism as a **Redis hash** that stores both the owning pod and the last heartbeat timestamp:

```
Redis hash key: "in_flight"
  field: <job_id>
  value: {"pod": "<pod-name>", "ts": <unix_timestamp>}
```

The entry is written at dequeue and deleted only on `PQSignalDone` or when the reconciler acts on a confirmed orphan.

Liveness is determined by timestamp staleness (`now - ts > threshold`). The reconciler always has access to the pod name, enabling a per-job Kubernetes API check at clear time that distinguishes all three failure modes:

- Pod not found or failed → pod was replaced, `emptyDir` destroyed → safe to act.
- Pod exists, container has **not** recently restarted + stale heartbeat → the job worker abandoned the job after a best-effort failure; the container is healthy but no component is aware the job is stuck; startup recovery will not run without a container restart → safe to act.
- Pod exists, container **recently restarted** → `emptyDir` survives; startup recovery has not yet run or is still executing → skip this job.

Because the pod name persists in the hash even after the heartbeat goes stale, the reconciler can query the K8s API and check the container's restart status to distinguish an abandoned job from a container that is about to run startup recovery.

## Proposal

Work proceeds in a **prerequisite hardening step** followed by **four layers** (Layer 1 provides the liveness signal that Layers 2–4 consume):

0. **Harden existing error paths** — full design: [Prerequisite](#prerequisite-harden-existing-error-paths).
1. **Track in-flight jobs** — full design: [Layer 1](#layer-1-track-in-flight-jobs).
2. **Detect** candidate orphan jobs — full design: [Layer 2](#layer-2-detect).
3. **Triage** each candidate to determine the correct action — full design: [Layer 3](#layer-3-triage).
4. **Clear** orphans by applying the chosen action safely — full design: [Layer 4](#layer-4-clear).

### Prerequisite: harden existing error paths

The following paths currently create orphan jobs:

- (polling loop) DB fetch fails → re-enqueue fails → `continue`.
- (polling loop) Expired status DB write fails → `continue`.
- (polling loop) Cancelling→cancelled DB write fails → `continue`.
- (job worker) terminal DB write fails → `return`.
- (job worker) panic recovery → terminal DB write fails → `return`.

For the first path, re-enqueue is already attempted but can also fail. For the remaining paths, re-enqueueing would be incorrect — expired jobs would expire again, cancelled jobs would re-cancel, completed/failed jobs would be reprocessed. There is nothing more the worker can do at this point beyond retrying the failed operation itself.

The only actionable improvement is to wrap these DB writes and re-enqueue calls with the existing `retry.Do` exponential backoff (currently only used for file storage uploads). This narrows the window for transient-failure orphans but does not eliminate it — if retries are exhausted, the job remains an orphan that the reconciler (Layers 2–4) must handle.

Best-effort abandonment is a **deliberate design choice**, not a bug: retrying indefinitely against an unavailable dependency would hold the worker token, block other jobs, and risk stalling the entire processor. Prevention therefore has a hard ceiling — past that ceiling, only a cluster-level reconciler can close the gap.

### Layer 1: Track in-flight jobs

**Problem today.** `PQDequeue` **atomically removes** the job from the Redis priority queue. If the processor **dies after dequeue** and **no workdir** survives for startup recovery, the job is **nowhere in Redis** while PostgreSQL can stay **non-terminal**—the classic orphan pattern.

**Objective.** Keep jobs **observable as in-flight in Redis** until the worker commits a terminal outcome in PostgreSQL (or explicitly returns work to the queue). This gives Layers 2–4 the liveness signal they need to distinguish active jobs from orphans.

**Design: timestamp hash — unified liveness and ownership signal.**

A **single Redis hash** tracks every in-flight job. The reconciler reads the `ts` field to determine liveness: a **fresh timestamp** (within the staleness threshold) means the job is actively being processed — whether normally or by startup recovery — and should not be touched.

**Redis schema.**

```
Redis hash key: "in_flight"
  field: <job_id>
  value: {"pod": "<pod-name>", "ts": <unix_timestamp>}
```

In Kubernetes, `os.Hostname()` returns the pod name by default. The `in-flight` key implementation can call it directly — no additional configuration or parameter passing needed.

The entry has **no TTL**. It persists until explicitly removed by `PQSignalDone` (clean completion) or by the reconciler (orphan clearance). This ensures the `pod` field survives processor restarts and remains available for K8s API lookups.

**Staleness.** The reconciler determines liveness by comparing `now - ts` against a configurable staleness threshold. For example, if `heartbeat_interval = 10s` and `missed_beats_threshold = 3`, a job whose `ts` is older than 30s is considered stale. Both values must be configurable.

**Protocol.**

1. **`PQDequeue`** — after removing the job from the ZSET, immediately writes `HSET in_flight <job_id> {"pod":"<self>","ts":<now>}`. The two Redis operations are **not** a single transaction (noted as an open point in PR #135); a crash in that window is a small residual gap that Layers 2–4 cover.
2. **job worker heartbeat** — a per-job background goroutine refreshes the `ts` field (`HSET in_flight <job_id> ...`) on a `heartbeat_interval` ticker, independent of processing stages. This is necessary because batch jobs can run for extended periods within a single stage (e.g. thousands of inference requests during execution). Refreshing only at stage boundaries would cause the entry to appear stale while the job is still actively processing. The goroutine is stopped via a `done` channel closed by `defer` when `runJob` returns — no additional context needed. The `pod` field is left unchanged. When the worker abandons a job after a best-effort failure (re-enqueue failed, terminal DB write failed), `runJob` returns, the deferred `close(done)` stops the heartbeat goroutine, but the `in_flight` entry is **not** removed — it simply stops being refreshed. The stale entry is the signal for the reconciler to detect and act on the orphan.
3. **startup recovery** — before acting on a stale workdir job, checks `HGET in_flight <job_id>` first. If a **fresh** entry exists, another processor is already actively running this job (for example, it dequeued the reconciler's re-enqueue and has since advanced the DB status); skip the recovery action and clean up the stale workdir only. If absent or stale, writes `HSET in_flight <job_id> {"pod":"<self>","ts":<now>}` and proceeds with phase-aware recovery. This guard is necessary because startup recovery reads the DB status snapshot once and acts on it; without the check, it can race with an active processor even when the reconciler re-enqueued the job as `validating`.
4. **`PQSignalDone`** — called on clean completion (terminal DB write succeeded); removes the entry immediately with `HDEL in_flight <job_id>`.
5. **reconciler scanner** — a **single leader** process (see [Open questions](#open-questions)) periodically runs a **DB-driven scan**: query PostgreSQL for non-terminal jobs, then for each job check the `in_flight` entry (`HGET in_flight <job_id>`) and ZSET membership. Jobs that pass the orphan candidate criteria (see [Layer 2](#layer-2-detect)) are re-enqueued by calling the existing **`PQEnqueue`**, which already uses `ZAddNX` and is idempotent—if the job is somehow back in the queue already, the call is a no-op.

**Suggested sequencing.**
1. Add `in_flight:<job_id>` write to `PQDequeue` and `PQSignalDone` cleanup.
2. Add heartbeat refresh to the job worker; add `in_flight` registration to startup recovery.
3. Implement reconciler scanner. Team discussion is needed on where it should live. [Open questions](#open-questions)

**Non-goals for Layer 1.** Replace startup recovery; change OpenAI-visible semantics; checkpointed resume of partial inference.

### Layer 2: Detect

Once Layer 1 is in place, detection evaluates four conditions per job, matching the [Orphan job](#orphan-job-narrow-reconciler-scope) definition:

1. **PostgreSQL non-terminal status** — DB-driven scan finds jobs that have not reached a terminal state.
2. **Job not in Redis priority queue (ZSET)** — excludes jobs that are simply queued and waiting to be picked up (they have no `in_flight` entry yet because Layer 1 writes it only after `PQDequeue`).
3. **`in_flight` entry absent OR stale** — `HGET in_flight <job_id>` returns nothing, or the stored `ts` satisfies `now - ts > heartbeat_interval × missed_beats_threshold`. This excludes jobs actively being processed by a worker.
4. **Not owned by startup recovery** — partially covered by condition 3 (once startup recovery starts, it writes `HSET in_flight` and keeps `ts` fresh). However, between container restart and startup recovery actually starting, the `in_flight` entry is still stale from the previous run. This gap is closed by the **K8s API check in [Layer 4](#layer-4-clear)**: if the container recently restarted, the reconciler skips the job and waits for startup recovery to act.

The ZSET membership check requires a new `PQContains(ctx, jobID)` method on `BatchPriorityQueueClient` (Redis implementation: `ZSCORE batch:queue <jobID>`, O(log N)). This method does not exist today and must be added as part of the Layer 1 or Layer 2 implementation work.

### Layer 3: Triage

At this layer, no local workdir or output file data is recoverable: workdir artifacts are absent by definition (any job with a surviving workdir is handled by startup recovery), and output file presence in external storage cannot be used as a signal because the file's storage key includes a randomly generated `fileID` that is only persisted to the batch job record when `UpdateCompletedStatus` succeeds—if the processor dies before that point, no queryable back-reference from the job to any uploaded file exists.

The triage rule is therefore:

- **`cancelling`** → transition to `cancelled`. A cancel was already requested; there is no point re-processing.
- **`expires_at` is set and has passed** → transition to `expired`. Re-enqueuing an expired job would waste processor capacity.
- **All other non-terminal statuses** (`validating`, `in_progress`, `finalizing`) → **re-enqueue as `validating`**. No recoverable data exists regardless of which phase the job was in; the reconciler has no local artifacts, so a clean retry from `validating` is the only meaningful action.

### Layer 4: Clear

Apply the chosen action **idempotently** and **safely** with respect to other components (API, GC, multiple processor replicas).

**Startup recovery non-interference at clear time.** Between the detection scan and the clear step, a container may have restarted and startup recovery may have updated the `in_flight` entry. The reconciler must execute the following sequence immediately before applying any action:

1. `HGET in_flight <job_id>` — read the current entry.
2. If the entry is now **fresh** (`now - ts ≤ threshold`), **skip this job**; the goroutine is alive.
3. If the entry is stale or absent, extract `pod` from the entry (if present) and query the Kubernetes API (`GET /api/v1/namespaces/{ns}/pods/{pod}`):
   - Pod **not found (404)** or `Failed` → pod was replaced; `emptyDir` destroyed; safe to proceed.
   - Pod exists — check the **container status** for the processor container. If the container has recently restarted (`lastState.terminated.finishedAt` within startup recovery window), startup recovery may still be executing; **skip this job**. If startup recovery succeeds, the job is resolved and will not appear in the next scan. If startup recovery fails or does not cover this job, the job remains an orphan candidate and is picked up in the **next reconciler cycle** (by then the restart window has passed, so the reconciler proceeds). Use the container's `restartCount` and `lastState` rather than pod `startTime`, because container restarts do not change pod `startTime`.
   - Pod exists, container has **not** recently restarted → the container is healthy but the job worker abandoned the job; startup recovery will not run without a container restart; safe to proceed.
   - **No pod name in entry** (entry was absent) → no ownership data; safe to proceed.
4. **Remove the `in_flight` entry first** with `HDEL in_flight <job_id>`, **then** apply the triage action. This order prevents a race: if re-enqueue happens before HDEL, another processor can dequeue the job and write a fresh `in_flight` entry, which the reconciler's HDEL would then incorrectly delete. By deleting first, the new processor's HSET is never clobbered. When the action is re-enqueue, **reset the DB status to `validating`** before calling `PQEnqueue`. Because the reconciler has no local artifacts, there is no partial output to preserve; the only safe action is a clean retry from `validating`. This also ensures safety with startup recovery: if the container later restarts and finds a stale workdir for this job, startup recovery reads the DB status, sees `validating`, and takes the `recoverValidating` path—which only calls `enqueueOne` (no-op via `ZAddNX`) and cleans up the workdir, without mutating DB status.

The startup recovery window (how long startup recovery can take to complete after container restart) must be configurable.

## Open questions

The following decisions are deferred for team discussion before implementation begins.

**① Where does the reconciler/scanner live?**

The Layer 1 reconciler scanner and the cluster-level reconciler (Layers 2–4) require a single-leader process. Three options:

- **Co-located in `batch-gc`**: already a periodic single-pod scanner; no new component needed. However, `batch-gc`'s responsibility is cleanup of expired data; adding orphan recovery changes its role from "garbage collector" to something broader, which may be a naming and ownership concern.
- **Co-located in the processor with Redis lease election**: keeps reconciliation close to the job execution path; requires leader election logic in the processor.
- **Dedicated component** (e.g., `batch-reconciler`): cleanest separation of concerns; independent configuration and scaling; increases the number of components to operate.

**② Kubernetes API dependency for startup recovery non-interference**

This proposal's Layer 4 clear step queries the Kubernetes API to determine whether the owning pod is still running. This makes startup recovery non-interference a per-job hard guarantee, at the cost of a new dependency: `k8s.io/client-go` and a `pods/get` RBAC rule for the reconciler's ServiceAccount (a Role and RoleBinding must be added to the Helm chart).

Two options:

- **K8s API check (recommended)**: query pod/container status to distinguish container restart from job abandonment. Requires `k8s.io/client-go` dependency and a `pods/get`-only RBAC rule (Role + RoleBinding in the Helm chart). No write permissions needed.
- **Skip-and-wait fallback**: if the `in_flight` entry is stale, do not act immediately — wait one more reconciler cycle. If it is still stale on the next cycle, proceed. This avoids the `client-go` dependency but is probabilistic: if startup recovery takes longer than one cycle, the reconciler may interfere.

## Alternatives considered

### Redis keyspace notifications for instant orphan detection

Redis keyspace notifications (`__keyevent@0__:expired`) can fire the moment a TTL key expires, enabling immediate re-enqueue without a periodic scan. Rejected for multiple reasons:

- **At-most-once delivery**: Redis pub/sub is fire-and-forget. If no subscriber is listening when the event fires (e.g. reconciler restarting), the event is lost and the orphan is never recovered.
- **Redis restart loses all events**: if Redis itself restarts, pending TTL expirations and notifications are gone.
- **Event alone is insufficient**: on receiving the expiry event, the reconciler still needs to query PostgreSQL to confirm the job is genuinely orphaned (not gracefully completed just before expiry). The DB round-trip is unavoidable regardless.

The DB-driven periodic scan (see [Layer 2](#layer-2-detect)) is simpler, reliable, and self-healing—it catches any orphan eventually regardless of event delivery.

## Related work

[PR #135](https://github.com/llm-d-incubation/batch-gateway/pull/135) proposed extending the `BatchPriorityQueueClient` interface with three operations:

- **`PQDequeue`** extended to mark the dequeued item as in-processing and create a heartbeat mechanism for it.
- **`PQSignalDone`**: the caller must call this when processing finishes to close the heartbeat and remove the in-flight entry.
- **`PQReEnqueue`**: scans items in processing, identifies orphans by stale heartbeat, and re-enqueues them into the priority queue with a flag indicating recovery is needed. Meant to be called periodically by a single scanner process. **Not adopted by this proposal** — a Redis-only scan cannot find orphans with no `in_flight` entry (e.g. entry lost to Redis restart, or never written due to crash between ZSET removal and HSET). The DB-driven scan in Layer 2 is strictly more complete. Re-enqueueing is done via the existing `PQEnqueue` (`ZAddNX`, idempotent).

PR #135 explicitly noted that marking in-processing after dequeue may not be implementable as a single Redis transaction, leaving a small crash window. This proposal adopts PR #135's `PQDequeue` extension and `PQSignalDone` as the **chosen approach for Layer 1**, fills in the unspecified heartbeat implementation as a no-TTL timestamp hash (see [Design Details](#design-details)), and adds the startup recovery integration requirement that was not covered in the original PR.

Layers 2–4 are not limited to covering that small crash window. They are needed for cases that Layer 1 cannot address: jobs that became orphans before Layer 1 was deployed, jobs whose `in_flight` entries were lost due to a Redis flush or restart, and any path where a non-terminal job ends up with no active processor and no heartbeat.
