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

// This file implements the BatchStatusClient interface for PostgresExchangeClient,
// backed by the UNLOGGED batch_status table. Status data is volatile and TTL'd, so
// every read filters out expired rows inline (DB-side NOW() avoids app/db clock skew).

package postgresql

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

const (
	// sqlStatusSet upserts the status payload, refreshing data and expiry on conflict.
	sqlStatusSet = `INSERT INTO batch_status (job_id, data, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (job_id) DO UPDATE SET data = EXCLUDED.data, expires_at = EXCLUDED.expires_at`

	// sqlStatusGet reads the payload for a job, filtering out expired rows so a stale
	// entry reads as a miss (matches the redis TTL-expiry contract).
	sqlStatusGet = `SELECT data FROM batch_status WHERE job_id = $1 AND expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT`

	sqlStatusDelete = `DELETE FROM batch_status WHERE job_id = $1`

	// sqlStatusSweep removes rows whose TTL has elapsed. Reads never see these (they
	// filter expiry inline); this is a safety-net reclaim called by the gc binary.
	sqlStatusSweep = `DELETE FROM batch_status WHERE expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT`
)

// StatusSet stores or updates the status payload for a job. A non-positive TTL falls
// back to statusTTLDefaultSec to match the redis default.
func (c *PostgresExchangeClient) StatusSet(ctx context.Context, ID string, TTL int, data []byte) (err error) {
	if len(ID) == 0 {
		return fmt.Errorf("StatusSet: ID is empty")
	}
	if len(data) == 0 {
		return fmt.Errorf("StatusSet: data is empty for ID %s", ID)
	}

	if TTL <= 0 {
		TTL = statusTTLDefaultSec
	}
	expiresAt := time.Now().Unix() + int64(TTL)

	if _, err = c.pool.Exec(ctx, sqlStatusSet, ID, data, expiresAt); err != nil {
		return fmt.Errorf("StatusSet: %w", err)
	}

	c.logger.V(logging.INFO).Info("StatusSet: succeeded", "id", ID)
	return nil
}

// StatusGet returns the status payload for a job, or (nil, nil) when the job has no
// entry or its entry has expired.
func (c *PostgresExchangeClient) StatusGet(ctx context.Context, ID string) (data []byte, err error) {
	if len(ID) == 0 {
		return nil, fmt.Errorf("StatusGet: ID is empty")
	}

	// The narrow pgxPool interface exposes no QueryRow, so mirror db_core's row loop.
	rows, err := c.pool.Query(ctx, sqlStatusGet, ID)
	if err != nil {
		return nil, fmt.Errorf("StatusGet: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		if err = rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("StatusGet: %w", err)
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("StatusGet: %w", err)
	}

	return data, nil
}

// StatusDelete removes the status entry for a job and reports how many rows were removed.
func (c *PostgresExchangeClient) StatusDelete(ctx context.Context, ID string) (nDeleted int, err error) {
	if len(ID) == 0 {
		return 0, fmt.Errorf("StatusDelete: ID is empty")
	}

	tag, err := c.pool.Exec(ctx, sqlStatusDelete, ID)
	if err != nil {
		return 0, fmt.Errorf("StatusDelete: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// SweepExpiredStatus deletes expired rows from the batch_status table and returns the
// number removed. Correctness does not depend on it (reads filter expiry inline); it is
// a safety-net reclaim invoked by the gc binary. Exported for that caller; gc wiring is
// out of scope here.
func (c *PostgresExchangeClient) SweepExpiredStatus(ctx context.Context) (nDeleted int, err error) {
	tag, err := c.pool.Exec(ctx, sqlStatusSweep)
	if err != nil {
		return 0, fmt.Errorf("SweepExpiredStatus: %w", err)
	}

	nDeleted = int(tag.RowsAffected())
	if nDeleted > 0 {
		c.logger.V(logging.INFO).Info("SweepExpiredStatus: reclaimed expired rows", "nDeleted", nDeleted)
	}
	return nDeleted, nil
}
