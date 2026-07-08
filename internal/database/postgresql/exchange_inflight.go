/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file implements the InFlightClient interface on PostgresExchangeClient.
// In-flight entries record which processor owns a dequeued job and when it last
// reported liveness. Storage is the batch_inflight table (job_id PK).

package postgresql

import (
	"context"
	"fmt"
	"time"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

// InFlightSet records or refreshes the in-flight entry for a job. The upsert
// keeps a single row per job_id, updating the owning processor and heartbeat.
func (c *PostgresExchangeClient) InFlightSet(ctx context.Context, jobID, processorID string) error {
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}
	if processorID == "" {
		return fmt.Errorf("processorID is empty")
	}

	lastSeen := time.Now().Unix()

	const sql = `INSERT INTO batch_inflight (job_id, processor_id, last_seen)
VALUES ($1, $2, $3)
ON CONFLICT (job_id) DO UPDATE SET processor_id = EXCLUDED.processor_id, last_seen = EXCLUDED.last_seen`

	if _, err := c.pool.Exec(ctx, sql, jobID, processorID, lastSeen); err != nil {
		return fmt.Errorf("InFlightSet: %w", err)
	}

	return nil
}

// InFlightDelete removes the in-flight entry for a job.
func (c *PostgresExchangeClient) InFlightDelete(ctx context.Context, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}

	if _, err := c.pool.Exec(ctx, "DELETE FROM batch_inflight WHERE job_id = $1", jobID); err != nil {
		return fmt.Errorf("InFlightDelete: %w", err)
	}

	return nil
}

// InFlightGetAll returns all in-flight entries keyed by job ID. An empty (non-nil)
// map is returned when no entries exist, matching the redis implementation.
func (c *PostgresExchangeClient) InFlightGetAll(ctx context.Context) (map[string]*db_api.InFlightEntry, error) {
	rows, err := c.pool.Query(ctx, "SELECT job_id, processor_id, last_seen FROM batch_inflight")
	if err != nil {
		return nil, fmt.Errorf("InFlightGetAll: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*db_api.InFlightEntry)
	for rows.Next() {
		var (
			jobID       string
			processorID string
			lastSeen    int64
		)
		if err := rows.Scan(&jobID, &processorID, &lastSeen); err != nil {
			return nil, fmt.Errorf("InFlightGetAll: scan: %w", err)
		}
		result[jobID] = &db_api.InFlightEntry{
			ProcessorID: processorID,
			LastSeen:    lastSeen,
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("InFlightGetAll: %w", err)
	}

	return result, nil
}
