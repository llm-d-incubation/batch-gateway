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

// BatchStatusClient over the UNLOGGED batch_status table. Reads filter expired
// rows inline; DB-side NOW() avoids app/db clock skew.

package postgresql

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

const (
	statusSetSQL = `INSERT INTO batch_status (job_id, data, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (job_id) DO UPDATE SET data = EXCLUDED.data, expires_at = EXCLUDED.expires_at`

	// An expired row reads as a miss, matching the redis TTL contract.
	statusGetSQL = `SELECT data FROM batch_status WHERE job_id = $1 AND expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT`

	statusDeleteSQL = `DELETE FROM batch_status WHERE job_id = $1`
)

// StatusSet stores or updates the status payload for a job. A non-positive TTL falls
// back to statusTTLDefaultSec to match the redis default.
func (c *PostgresExchangeClient) StatusSet(ctx context.Context, ID string, TTL int, data []byte) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	if len(data) == 0 {
		return fmt.Errorf("data is empty for ID %s", ID)
	}

	if TTL <= 0 {
		TTL = statusTTLDefaultSec
	}
	expiresAt := time.Now().Unix() + int64(TTL)

	if _, err = c.pool.Exec(ctx, statusSetSQL, ID, data, expiresAt); err != nil {
		return fmt.Errorf("StatusSet: %w", err)
	}

	logr.FromContextOrDiscard(ctx).V(logging.INFO).Info("StatusSet: succeeded", "id", ID)
	return nil
}

// StatusGet returns the status payload for a job, or (nil, nil) when the job has no
// entry or its entry has expired.
func (c *PostgresExchangeClient) StatusGet(ctx context.Context, ID string) (data []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(ID) == 0 {
		return nil, fmt.Errorf("ID is empty")
	}

	// The narrow pgxPool interface exposes no QueryRow, so mirror db_core's row loop.
	rows, err := c.pool.Query(ctx, statusGetSQL, ID)
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
	if ctx == nil {
		ctx = context.Background()
	}
	if len(ID) == 0 {
		return 0, fmt.Errorf("ID is empty")
	}

	tag, err := c.pool.Exec(ctx, statusDeleteSQL, ID)
	if err != nil {
		return 0, fmt.Errorf("StatusDelete: %w", err)
	}

	return int(tag.RowsAffected()), nil
}
