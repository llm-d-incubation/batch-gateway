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

// BatchEventChannelClient over PostgreSQL. The batch_events table is the source
// of truth (durable-until-consumed, late-attach-safe); NOTIFY is a latency hint
// only. One shared dispatcher goroutine drains rows destructively and fans them
// out to per-job Go channels, reproducing the Redis BLMPop semantics.
//
//	producer ──INSERT+pg_notify──▶ batch_events table ◀──DELETE RETURNING── dispatcher
//	                                     ▲                                      │
//	                        poll tick / NOTIFY wake                     per-job Go chan
//	                                                                           │
//	                                                                       consumer

package postgresql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// Per-consumer buffer depth, matching the Redis implementation's eventChanSize.
const eventChanBufSize = 100

// Logged (never returned) when a consumer's buffer is full at delivery time: the
// event was already deleted from the table, so this is a genuine drop.
var errEventChannelFull = errors.New("event channel full")

// Insert + NOTIFY in one statement, so no transaction is needed (the narrow
// pgxPool interface exposes no Begin).
const ecSendEventSQL = `WITH ins AS (
	INSERT INTO batch_events (job_id, event_type, expires_at)
	VALUES ($1, $2, $3)
	RETURNING job_id
)
SELECT pg_notify('` + channelEvents + `', (SELECT job_id FROM ins))`

// Destructive FIFO pop. FOR UPDATE SKIP LOCKED keeps concurrent drainers from
// handing the same row to two consumers.
const ecDrainEventsSQL = `DELETE FROM batch_events
WHERE id IN (
	SELECT id FROM batch_events
	WHERE job_id = $1 AND expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT
	ORDER BY id
	FOR UPDATE SKIP LOCKED
)
RETURNING event_type`

// eventSub is one consumer's subscription; closeOnce makes CloseFn idempotent.
type eventSub struct {
	ch        chan db_api.BatchEvent
	closeOnce sync.Once
}

// ECProducerSendEvents inserts each event and fires a NOTIFY per insert. Returns
// the IDs that were successfully inserted.
func (c *PostgresExchangeClient) ECProducerSendEvents(ctx context.Context, events []db_api.BatchEvent) (
	sentIDs []string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("empty events")
	}
	for i := range events {
		if err = events[i].IsValid(); err != nil {
			return nil, err
		}
	}

	sentIDs = make([]string, 0, len(events))
	for i := range events {
		event := events[i]
		expiresAt := time.Now().Unix() + int64(event.TTL)
		if _, err = c.pool.Exec(ctx, ecSendEventSQL, event.ID, int(event.Type), expiresAt); err != nil {
			return sentIDs, fmt.Errorf("ECProducerSendEvents: %w", err)
		}
		sentIDs = append(sentIDs, event.ID)
	}

	logr.FromContextOrDiscard(ctx).V(logging.INFO).Info("ECProducerSendEvents: succeeded", "nIDs", len(sentIDs))
	return sentIDs, nil
}

// ECConsumerGetChannel registers a consumer for the job's events, then drains
// events already stored for the job so a late-attaching consumer misses nothing.
func (c *PostgresExchangeClient) ECConsumerGetChannel(ctx context.Context, ID string) (
	batchEventsChan *db_api.BatchEventsChan, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(ID) == 0 {
		return nil, fmt.Errorf("ID is empty")
	}

	sub := &eventSub{
		ch: make(chan db_api.BatchEvent, eventChanBufSize),
	}

	c.eventsMu.Lock()
	c.eventSubs[ID] = sub
	c.eventsMu.Unlock()

	c.deliverJobEvents(ctx, ID)

	closeFn := func() {
		c.eventsMu.Lock()
		if cur, ok := c.eventSubs[ID]; ok && cur == sub {
			delete(c.eventSubs, ID)
		}
		c.eventsMu.Unlock()
		// The delete above (under the same lock a delivering dispatcher must hold)
		// guarantees no send races this close.
		sub.closeOnce.Do(func() { close(sub.ch) })
	}

	logr.FromContextOrDiscard(ctx).V(logging.INFO).Info("ECConsumerGetChannel: succeeded", "ID", ID)
	return &db_api.BatchEventsChan{ID: ID, Events: sub.ch, CloseFn: closeFn}, nil
}

// startEventDispatcher runs once from the constructor. The dispatcher lives on a
// background-derived context (the construction ctx may carry a deadline); Close()
// cancels it via eventsCancel.
func (c *PostgresExchangeClient) startEventDispatcher() {
	dispCtx, cancel := context.WithCancel(context.Background())
	c.eventsCancel = cancel
	c.eventsDone = make(chan struct{})

	wake, unsubscribe := c.listener.subscribe(channelEvents)
	go c.runEventDispatcher(dispCtx, wake, unsubscribe)
}

// runEventDispatcher drains one job per job_id wake, or every subscribed job on
// an empty poll-fallback tick, so a dropped NOTIFY is a latency hit, never lost work.
func (c *PostgresExchangeClient) runEventDispatcher(ctx context.Context, wake <-chan string, unsubscribe func()) {
	defer close(c.eventsDone)
	defer unsubscribe()
	c.logger.V(logging.INFO).Info("event dispatcher: start")

	for {
		select {
		case <-ctx.Done():
			c.logger.V(logging.INFO).Info("event dispatcher: stop")
			return
		case payload, ok := <-wake:
			if !ok {
				return
			}
			// Select is random: a buffered wake may win even after cancellation. Bail
			// before draining so we never query a pool Close is about to shut down.
			if ctx.Err() != nil {
				c.logger.V(logging.INFO).Info("event dispatcher: stop")
				return
			}
			if payload == "" {
				c.deliverAllJobEvents(ctx)
			} else {
				c.deliverJobEvents(ctx, payload)
			}
		}
	}
}

func (c *PostgresExchangeClient) deliverAllJobEvents(ctx context.Context) {
	c.eventsMu.Lock()
	jobIDs := make([]string, 0, len(c.eventSubs))
	for id := range c.eventSubs {
		jobIDs = append(jobIDs, id)
	}
	c.eventsMu.Unlock()

	for _, id := range jobIDs {
		c.deliverJobEvents(ctx, id)
	}
}

// deliverJobEvents drains one job's events and fans them out. The fan-out runs
// under eventsMu with a same-subscription re-check, so it can never send on a
// channel CloseFn already closed; events for a mid-drain-closed consumer are
// dropped (CloseFn means the job no longer cares).
func (c *PostgresExchangeClient) deliverJobEvents(ctx context.Context, jobID string) {
	c.eventsMu.Lock()
	sub, ok := c.eventSubs[jobID]
	c.eventsMu.Unlock()
	if !ok {
		return
	}

	events, err := c.drainJobEvents(ctx, jobID)
	if err != nil {
		c.logger.Error(err, "event dispatcher: drain failed", "ID", jobID)
		return
	}
	if len(events) == 0 {
		return
	}

	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if cur, present := c.eventSubs[jobID]; !present || cur != sub {
		return
	}
	for _, event := range events {
		select {
		case sub.ch <- event:
		default:
			c.logger.Error(errEventChannelFull, "event dispatcher: dropping event", "ID", jobID, "type", event.Type)
		}
	}
}

func (c *PostgresExchangeClient) drainJobEvents(ctx context.Context, jobID string) ([]db_api.BatchEvent, error) {
	rows, err := c.pool.Query(ctx, ecDrainEventsSQL, jobID)
	if err != nil {
		return nil, fmt.Errorf("drain events: %w", err)
	}
	defer rows.Close()

	var events []db_api.BatchEvent
	for rows.Next() {
		var eventType int
		if err := rows.Scan(&eventType); err != nil {
			return nil, fmt.Errorf("drain events scan: %w", err)
		}
		events = append(events, db_api.BatchEvent{ID: jobID, Type: db_api.BatchEventType(eventType)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drain events rows: %w", err)
	}

	return events, nil
}
