-- Copyright 2026 The llm-d Authors
-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at

-- http://www.apache.org/licenses/LICENSE-2.0

-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.

-- ---- Priority queue (BatchPriorityQueueClient) ----
-- slo_score = BatchJobPriority.SLO.UnixMicro() (int64). ORDER BY slo_score ASC == Redis Min.
-- PK is job_id (enqueue is idempotent per job via ON CONFLICT DO NOTHING, matching ZAddNX).
-- expires_at = unix seconds; NULL means no TTL (safety-net GC, per-row equivalent of Redis
-- whole-key Expire).
CREATE TABLE IF NOT EXISTS batch_queue (
    job_id     TEXT   PRIMARY KEY,
    slo_score  BIGINT NOT NULL,
    data       BYTEA,
    expires_at BIGINT
);
CREATE INDEX IF NOT EXISTS idx_batch_queue_slo ON batch_queue (slo_score, job_id);
CREATE INDEX IF NOT EXISTS idx_batch_queue_expires_at ON batch_queue (expires_at) WHERE expires_at IS NOT NULL;

-- ---- In-flight tracking (InFlightClient) ----
-- last_seen = unix seconds (time.Now().Unix()), refreshed on every heartbeat.
CREATE TABLE IF NOT EXISTS batch_inflight (
    job_id       TEXT   PRIMARY KEY,
    processor_id TEXT   NOT NULL,
    last_seen    BIGINT NOT NULL
);

-- ---- Volatile status (BatchStatusClient) ----
-- UNLOGGED: skips WAL for the one high-frequency write path. Data is volatile/TTL'd by the
-- interface contract, so loss on crash is acceptable. expires_at = unix seconds.
CREATE UNLOGGED TABLE IF NOT EXISTS batch_status (
    job_id     TEXT   PRIMARY KEY,
    data       BYTEA  NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_batch_status_expires_at ON batch_status (expires_at);

-- ---- Durable-until-consumed events (BatchEventChannelClient) ----
-- id BIGSERIAL gives FIFO ordering. event_type = int(BatchEventType). expires_at = unix seconds.
-- Rows are the source of truth (durable, late-attach-safe); NOTIFY is only a latency hint.
CREATE TABLE IF NOT EXISTS batch_events (
    id         BIGSERIAL PRIMARY KEY,
    job_id     TEXT      NOT NULL,
    event_type INTEGER   NOT NULL,
    expires_at BIGINT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_batch_events_job_id ON batch_events (job_id, id);
CREATE INDEX IF NOT EXISTS idx_batch_events_expires_at ON batch_events (expires_at);
