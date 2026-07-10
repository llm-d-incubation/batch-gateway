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

CREATE TABLE IF NOT EXISTS batch_items (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    expiry        BIGINT,
    tags          JSONB,
    spec          JSONB,
    status        JSONB,
    processor_id  TEXT,
    priority      BIGINT
);

CREATE INDEX IF NOT EXISTS idx_batch_items_tenant_id ON batch_items(tenant_id);
CREATE INDEX IF NOT EXISTS idx_batch_items_expiry ON batch_items(expiry) WHERE expiry IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_batch_items_tags ON batch_items USING GIN (tags) WHERE tags IS NOT NULL;

-- Queue index: unclaimed jobs ordered by priority (SLO deadline, earliest first).
CREATE INDEX IF NOT EXISTS idx_batch_items_queue
    ON batch_items (priority ASC)
    WHERE processor_id IS NULL
      AND status IS NOT NULL
      AND status::jsonb->>'status' = 'validating';

-- Processor ownership index: find jobs owned by a specific processor for crash recovery.
CREATE INDEX IF NOT EXISTS idx_batch_items_processor
    ON batch_items (processor_id)
    WHERE processor_id IS NOT NULL;
