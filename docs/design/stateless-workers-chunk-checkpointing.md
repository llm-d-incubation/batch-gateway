# Stateless Workers and Chunk Checkpointing

Status: draft / discussion
Author: Will Eaton
Date: 2026-07-07

## Problem

A single worker owns an entire batch job end to end, dispatching requests from an
in-memory loop with no mid-job checkpointing. The only durable progress is whatever
partial output files survive on that pod's local emptyDir.

Consequences (verified against current code):

- **emptyDir exhaustion evicts the pod and destroys all job state.** Startup recovery
  (`internal/processor/worker/recovery.go`) is workdir-based and explicitly scopes out
  pod-level failures. The orphan reconciler (`internal/gc/reconciler/reconciler.go`)
  triages orphaned `in_progress` jobs to **failed** (not re-enqueued), with partial
  outputs lost, detected up to ~1-2h later (60m default interval).
- A 200 MB job pins one worker for hours; other workers idle.
- Cancellation/expiry drain walks every undispatched entry.

## Target architecture

A batch job becomes **an actor whose state lives in the DB**, driven by task messages
that any worker can process. Workers are stateless message processors. Task types:
`Preprocess`, `ExecuteChunk`, `Finalize`, `Cancel`. Every state transition is a CAS
write on the job record.

**Checkpointing unit: the chunk** — ~500-1000 contiguous plan entries, sized in bytes
to satisfy S3's >=5 MB multipart-part minimum. A chunk task is self-contained:

```
{jobID, model, chunkIndex, inputFileRef, offsetSpan, partNumber, jobEpoch}
```

Executing one is idempotent: ranged-GET the input slice, run the requests, upload
results as part `partNumber` of the job's multipart upload (deterministic — a retry
overwrites its own part), CAS the chunk-done record with its counts. A crash costs
one chunk, not the job.

**Fencing**: each (re)issue of a job's task bumps a `jobEpoch` in the DB; chunk-done
CAS includes the epoch so a worker that stalled through its lease cannot commit stale
results after redelivery.

## Phases

### Phase 0 — primitives already in motion
- Ranged storage reads (`RetrieveRange`, PR #554).
- `custom_id` stored in plan entries: fixes the sequential drain reads and makes plan
  entries self-describing task payloads.

### Phase 1 — externalize all per-job state
- Plans move from emptyDir to object storage (small, written once by preprocessor).
- Output/error files become S3 multipart uploads streamed during execution; MPU upload
  ID stored on the job record.
- After this phase emptyDir holds nothing durable. Sellable purely as crash-safety
  hardening; no queue semantics touched.

### Phase 2 — checkpointing inside the current ownership model
- Executor dispatch loop gets chunk boundaries; after each chunk, upload the part and
  CAS a checkpoint `{chunksDone, counts}`.
- Recovery (same-pod restart or reconciler) changes from "fail the job" to
  "re-enqueue with checkpoint"; executor skips completed chunks on resume.
- Reconciler branch `in_progress -> failed` becomes `in_progress -> re-enqueue`.
- Big observable win: a killed pod costs one chunk per in-flight job.

### Phase 3 — generalize the queue to tasks
- Extend `BatchPriorityQueueClient` from destructive dequeue to claim/lease/ack:
  dequeue moves the item to a claimed set with a deadline, heartbeat extends,
  reconciler requeues expired claims. `InFlightClient` is already this lease table
  in miniature, keyed by job; re-key by task.
- Split job lifecycle into separate task types so preprocess/execute/finalize can run
  on different pods. Tasks scored with the parent job's SLO to preserve priority.

### Phase 4 — chunk-level dispatch (the actor model realized)
- Preprocessor enqueues `ExecuteChunk` tasks; any worker claims chunks from any job.
- Last chunk-done enqueues `Finalize`, which completes the MPU and writes terminal
  status.
- Must move from process-local to shared: per-endpoint concurrency/AIMD state
  (Redis/DB-backed adaptive limiter, or accept per-pod approximation initially) and
  cancellation (chunk tasks check job status at claim; in-flight chunks poll
  periodically — contexts already provide the hook).

### Phase 5 — demolition
- Delete workdir scanning, `recoverStaleJobs`, partial-upload salvage, the drain path
  (undispatched entries are just unclaimed tasks), most of the reconciler triage
  matrix. Recovery collapses to: expired lease -> requeue.

## Cross-cutting decisions

- **Output ordering**: part order = chunk order = input order per model. OpenAI's API
  does not guarantee output order, so this is stricter than required.
- **Count idempotency**: counts commit only via the chunk-done CAS (once per chunk),
  never per-request, so retries cannot double-count.
- **Error file**: second MPU per job. Chunks with zero errors upload nothing; finalize
  uses UploadPartCopy compaction or tolerates sparse part numbers.
- **Small batches**: a job below one chunk degenerates to exactly today's behavior —
  one execute task; the common case pays no coordination overhead.
- **Rollout**: version field on the job record; old-format jobs finish under the old
  path, new jobs use tasks; reconciler understands both during overlap. No migration
  of in-flight state.
- **Acceptance test**: e2e kill test — `kubectl delete pod` mid-execution in Kind,
  assert the batch completes with correct counts and no duplicate output lines.

## Sequencing strategy

Phases 0-2 require no alignment on work-queue semantics; each is a self-justifying
crash-safety fix, and phase 2 alone eliminates the eviction-destroys-everything
failure mode. Phases 3-4 shrink the contested surface from "rearchitect the system"
to "change the dequeue granularity", argued with a working checkpoint mechanism
already in production.
