# Postgres-Only Mode (Optional Redis)

Status: draft / discussion
Author: Will Eaton
Date: 2026-07-07

## Problem

The persistent DB layer is already pluggable (`db_client.type: redis | postgresql`),
but every deployment still requires Redis for the **exchange client**: a single Redis
client wired unconditionally in `internal/util/clientset/clientset.go` (see the TODO
there referencing PR #102) that implements four interfaces:

1. **`BatchPriorityQueueClient`** — SLO-scored sorted set; blocking, atomic,
   *destructive* dequeue (the interface contract requires exclusive dequeue).
2. **`InFlightClient`** — job → `{processorID, lastSeen}` heartbeat map.
3. **`BatchStatusClient`** — TTL'd volatile store for progress counts; documented as
   lightweight, frequent, non-persistent.
4. **Event channels** (`EC*`) — per-job Redis *list* consumed via `BLMPop`
   (`redis_event.go`). Events are durable-until-consumed and a consumer attaching
   late still receives them. Used to deliver cancellation to the running worker.

Goal: implement Postgres backends for these four interfaces so a deployment can run
with Postgres alone. Redis remains fully supported; this is additive.

## Postgres designs per interface

Driver: the codebase already uses `pgx/v5` with a pool, which has first-class
`LISTEN/NOTIFY`. No new driver needed.

### Priority queue

Canonical `SKIP LOCKED` pattern, contract-compatible in one statement:

```sql
DELETE FROM batch_queue WHERE job_id IN (
  SELECT job_id FROM batch_queue
  ORDER BY slo_score LIMIT $1 FOR UPDATE SKIP LOCKED
) RETURNING job_id, slo_score;
```

Blocking dequeue = `LISTEN` on a wake channel (`NOTIFY` fired by `PQEnqueue`) plus a
coarse poll fallback (~1s) for dropped notifications. `PQGetIDs` is a plain SELECT.

### InFlight

Three-column table (`job_id PK, processor_id, last_seen`); `InFlightSet` is an
upsert, `InFlightGetAll` a select. Trivial.

### Status (volatile progress counts)

**`UNLOGGED` table** with `expires_at`, filtered on read, swept opportunistically
(the gc binary is a natural home — Postgres has no native TTL). UNLOGGED skips WAL,
which matters because this is the one high-frequency write path; losing it on a
Postgres crash is acceptable by the interface's own definition (volatile, TTL'd).

During implementation, measure the actual write rate of `UpdateProgressCounts`; if
it is effectively per-request, add ~500ms coalescing in `StatusUpdater` (a win for
the Redis backend too).

### Events

Events table (`id bigserial, job_id, type`) plus `NOTIFY` on insert. The consumer
holds **one shared LISTEN connection** dispatching to per-job Go channels — not one
connection per job (`ECConsumerGetChannel` is called per running job, up to
NumWorkers concurrent) — and consumes rows via
`DELETE ... RETURNING ... ORDER BY id FOR UPDATE SKIP LOCKED`.

This reproduces the Redis list's durable-until-consumed, late-attach-safe semantics.
Bare `LISTEN/NOTIFY` alone would not (it is at-most-once, ephemeral); the table is
the source of truth and NOTIFY is only a latency optimization over polling.

## Wiring, config, deployment

- New `exchange_client.type: redis | postgresql` (default `redis`; zero behavior
  change for existing deployments). Resolves the clientset TODO.
- When `postgresql`, share the existing pgx pool from `db_client` rather than
  opening a second one.
- Config validation: `postgresql` exchange requires `postgresql` db_client.
- Helm: values toggle; drop the Redis secret requirement when both are postgres; a
  postgres-only values profile for the Kind e2e path.
- Schema: `exchange_schema.sql` alongside the existing `batch_schema.sql` /
  `file_schema.sql`.

## Async inference caveat (verified 2026-07-07)

**llm-d-async is Redis-only; there is no Postgres backend** — checked in the
vendored `producer@v0.7.2` (latest tag; sole implementation is
`redis_sortedset_producer.go`), upstream `main` (no postgres/sql files anywhere in
the tree), and the repo's PR history (nothing in flight). The resolver in
`pkg/clients/inference/async_inference_client_resolver.go` constructs
`producer.NewRedisSortedSetProducer` directly, and `clientset.go` falls back to
borrowing the exchange client's Redis URL when async mode doesn't set its own.

Therefore: **postgres-only mode v1 = sync dispatch only.** Enforce at startup —
`asyncInference` configured together with `exchange_client.type: postgresql` is a
config error with a clear message, not a silent degradation.

Mitigations if async-on-postgres ever matters:

- The upstream `Producer` interface is three methods (`SubmitRequest`, `GetResult`,
  `Close`); a `SKIP LOCKED`-based Postgres producer would be a modest contribution.
- The consumer side (the async dispatcher deployment) needs the matching
  implementation, so this is an llm-d-async project decision, not something
  batch-gateway can do unilaterally.

## Build vs. adopt

Considered off-the-shelf: **River** (well-maintained pgx-native Go job queue),
pgmq. River is good, but its job-kind/worker abstraction doesn't map onto these
four small interfaces, the SLO-score ordering contract, or the destructive-dequeue
semantics the processor assumes; we'd be adapting around it rather than using it.
Recommendation: in-house `SKIP LOCKED` implementation — roughly the size of the
existing `postgresql/batch_db.go` and consistent with the codebase's
backend-per-interface pattern.

## Trade-offs

- Dequeue wake latency: comparable to Redis with NOTIFY; the poll fallback only
  matters if a notification is dropped.
- Throughput is a non-issue: everything here is job-granularity (tens/sec at most)
  except progress counts, handled by UNLOGGED + coalescing.
- Table churn (queue, events) is small and autovacuum-friendly at these volumes.

## Effort and sequencing

Four implementation files + schema in `internal/database/postgresql/`,
clientset/config/Helm wiring, tests mirroring the existing postgres test harness,
plus a postgres-only e2e profile. **~1-2 weeks.**

Strategic note: the Postgres queue is the natural place to later add claim/lease
semantics (`SKIP LOCKED` claims are already lease-shaped) — exactly phase 3 of
[stateless-workers-chunk-checkpointing](stateless-workers-chunk-checkpointing.md).
Build it contract-compatible now, but keep the schema amenable to a
`claimed_by / lease_expires_at` column pair later.
