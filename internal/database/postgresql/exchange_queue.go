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

// This file implements the PostgreSQL BatchPriorityQueueClient methods on
// PostgresExchangeClient. The batch_queue table is the source of truth; the
// batch_queue_wake NOTIFY channel is a latency hint only. Dequeue is an atomic
// destructive DELETE ... RETURNING (FOR UPDATE SKIP LOCKED) so a job is handed
// to exactly one caller, honoring the interface's exclusive-dequeue contract.

package postgresql

import (
	"context"
	"fmt"
	"time"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// pqEnqueueSQL inserts a job idempotently (ON CONFLICT DO NOTHING mirrors Redis
// ZAddNX) and fires the wake NOTIFY only when a row was actually inserted, so a
// duplicate enqueue is a silent no-op that does not wake dequeuers needlessly.
const pqEnqueueSQL = `WITH ins AS (
	INSERT INTO batch_queue (job_id, slo_score, data, expires_at)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (job_id) DO NOTHING
	RETURNING job_id
)
SELECT pg_notify('` + channelQueueWake + `', '') FROM ins`

// pqDrainSQL atomically removes up to $1 highest-priority (lowest slo_score)
// non-expired jobs and returns them. FOR UPDATE SKIP LOCKED lets concurrent
// processors drain disjoint heads without blocking each other. The outer
// SELECT re-sorts because DELETE ... RETURNING emits rows in arbitrary
// physical order, not the inner subquery's ORDER BY.
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
	logger := c.logger
	if item == nil {
		return fmt.Errorf("empty item")
	}
	if err = item.IsValid(); err != nil {
		return err
	}
	logger = logger.WithValues("ID", item.ID)

	// expires_at is unix seconds; nil means no TTL (the per-row equivalent of
	// Redis' whole-key Expire), leaving cleanup to the safety-net GC.
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
	logger := c.logger
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
	logger := c.logger

	// First drain is always attempted. For the non-blocking contract (timeout<=0)
	// this is the whole operation, matching Redis ZMPop.
	jobPriorities, err = c.drainQueue(ctx, maxItems)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || len(jobPriorities) > 0 {
		return jobPriorities, nil
	}

	// Blocking path: wait for a wake (real NOTIFY or ~pollInterval fallback tick)
	// and re-drain until we get items or the timeout elapses. The GUARD above
	// guarantees the listener is only touched when timeout>0 AND the first drain
	// was empty, so nil-listener CRUD tests never reach here.
	wake, unsubscribe := c.listener.subscribe(channelQueueWake)
	defer unsubscribe()

	// One timer for the whole wait. Re-creating time.After each iteration would leak
	// a live runtime timer per wake until the deadline (a busy queue fires many).
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Shutdown/cancellation is not an error for the caller; mirror Redis,
			// which folds context cancellation into a no-items result.
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

// drainQueue runs the atomic destructive dequeue once and reconstructs the
// dequeued jobs. slo_score is the SLO as unix microseconds; the Data column is
// preserved on the way out (harmless, since Data is optional).
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
