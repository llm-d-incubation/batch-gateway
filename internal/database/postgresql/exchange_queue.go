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

// BatchPriorityQueueClient over PostgreSQL. The batch_queue table is the source
// of truth; NOTIFY is a latency hint only. Dequeue is destructive and atomic so a
// job is handed to exactly one caller (the interface's exclusive-dequeue contract).

package postgresql

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// Idempotent insert (mirrors Redis ZAddNX); the wake NOTIFY fires only when a row
// was actually inserted.
const pqEnqueueSQL = `WITH ins AS (
	INSERT INTO batch_queue (job_id, slo_score, data, expires_at)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (job_id) DO NOTHING
	RETURNING job_id
)
SELECT pg_notify('` + channelQueueWake + `', '') FROM ins`

// FOR UPDATE SKIP LOCKED lets concurrent processors drain disjoint heads without
// blocking. The outer SELECT re-sorts because DELETE ... RETURNING emits rows in
// arbitrary physical order, not the inner subquery's ORDER BY.
const pqDrainSQL = `WITH drained AS (
	DELETE FROM batch_queue
	WHERE job_id IN (
		SELECT job_id FROM batch_queue
		WHERE (expires_at IS NULL OR expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT)
		ORDER BY slo_score, job_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	)
	RETURNING job_id, slo_score, data
)
SELECT job_id, slo_score, data FROM drained ORDER BY slo_score, job_id`

const pqDeleteSQL = `DELETE FROM batch_queue WHERE job_id = $1 AND slo_score = $2`

const pqGetIDsSQL = `SELECT job_id FROM batch_queue
WHERE expires_at IS NULL OR expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT`

func (c *PostgresExchangeClient) PQEnqueue(ctx context.Context, item *db_api.BatchJobPriority) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if item == nil {
		return fmt.Errorf("empty item")
	}
	if err = item.IsValid(); err != nil {
		return err
	}
	logger = logger.WithValues("ID", item.ID)

	// nil expires_at means no TTL, the per-row equivalent of Redis' whole-key Expire.
	var expiresAt *int64
	if item.TTL > 0 {
		exp := time.Now().Unix() + int64(item.TTL)
		expiresAt = &exp
	}

	if _, err = c.pool.Exec(ctx, pqEnqueueSQL,
		item.ID, item.SLO.UnixMicro(), item.Data, expiresAt); err != nil {
		return fmt.Errorf("PQEnqueue: %w", err)
	}

	logger.V(logging.INFO).Info("PQEnqueue: succeeded")
	return nil
}

func (c *PostgresExchangeClient) PQDelete(ctx context.Context, item *db_api.BatchJobPriority) (nDeleted int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if item == nil {
		return 0, fmt.Errorf("empty item")
	}
	if err = item.IsValid(); err != nil {
		return 0, err
	}
	logger = logger.WithValues("ID", item.ID)

	tag, err := c.pool.Exec(ctx, pqDeleteSQL, item.ID, item.SLO.UnixMicro())
	if err != nil {
		return 0, fmt.Errorf("PQDelete: %w", err)
	}
	nDeleted = int(tag.RowsAffected())

	logger.V(logging.INFO).Info("PQDelete: succeeded", "nDeleted", nDeleted)
	return nDeleted, nil
}

func (c *PostgresExchangeClient) PQDequeue(ctx context.Context, timeout time.Duration, maxItems int) (
	jobPriorities []*db_api.BatchJobPriority, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)

	jobPriorities, err = c.drainQueue(ctx, maxItems)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || len(jobPriorities) > 0 {
		return jobPriorities, nil
	}

	// Blocking path: wait for a wake and re-drain until items arrive or the
	// timeout elapses.
	wake, unsubscribe := c.listener.subscribe(channelQueueWake)
	defer unsubscribe()

	// One timer for the whole wait; time.After per iteration would leak a live
	// runtime timer per wake until the deadline.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Mirror Redis: context cancellation folds into a no-items result.
			return nil, nil
		case <-timer.C:
			return nil, nil
		case <-wake:
			jobPriorities, err = c.drainQueue(ctx, maxItems)
			if err != nil {
				return nil, err
			}
			if len(jobPriorities) > 0 {
				logger.V(logging.INFO).Info("PQDequeue: succeeded", "nItems", len(jobPriorities))
				return jobPriorities, nil
			}
		}
	}
}

// drainQueue runs the atomic destructive dequeue once. slo_score is the SLO as
// unix microseconds.
func (c *PostgresExchangeClient) drainQueue(ctx context.Context, maxItems int) ([]*db_api.BatchJobPriority, error) {
	rows, err := c.pool.Query(ctx, pqDrainSQL, maxItems)
	if err != nil {
		return nil, fmt.Errorf("PQDequeue: %w", err)
	}
	defer rows.Close()

	var jobPriorities []*db_api.BatchJobPriority
	for rows.Next() {
		var (
			jobID    string
			sloScore int64
			data     []byte
		)
		if err := rows.Scan(&jobID, &sloScore, &data); err != nil {
			return nil, fmt.Errorf("PQDequeue: scan: %w", err)
		}
		jobPriorities = append(jobPriorities, &db_api.BatchJobPriority{
			ID:   jobID,
			SLO:  time.UnixMicro(sloScore).UTC(),
			Data: data,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PQDequeue: rows: %w", err)
	}

	return jobPriorities, nil
}

func (c *PostgresExchangeClient) PQGetIDs(ctx context.Context) (map[string]bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	rows, err := c.pool.Query(ctx, pqGetIDsSQL)
	if err != nil {
		return nil, fmt.Errorf("PQGetIDs: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("PQGetIDs: scan: %w", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PQGetIDs: rows: %w", err)
	}

	return ids, nil
}
