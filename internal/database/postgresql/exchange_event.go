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

// This file implements the PostgreSQL BatchEventChannelClient methods on
// PostgresExchangeClient. The batch_events table is the source of truth
// (durable-until-consumed, late-attach-safe); the batch_events NOTIFY channel is
// a latency hint only. A single shared dispatcher goroutine (started lazily on
// the first consumer) drains rows destructively via
// DELETE ... RETURNING ... ORDER BY id FOR UPDATE SKIP LOCKED and fans them out
// to per-job Go channels, reproducing the Redis BLMPop semantics.
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

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// eventChanBufSize is the per-consumer buffer depth, matching the Redis
// implementation's eventChanSize. Control events (cancel/pause/resume) are
// low-frequency, so this buffer effectively never fills.
const eventChanBufSize = 100

// errEventChannelFull is logged (never returned) when a consumer's buffer is
// full at delivery time. The event has already been removed from the table, so
// this is a genuine drop; the deep buffer makes it practically unreachable.
var errEventChannelFull = errors.New("event channel full")

// ecSendEventSQL inserts one event and fires the batch_events NOTIFY carrying the
// job_id, all in a single statement so no transaction is needed (the narrow
// pgxPool interface exposes no Begin). The NOTIFY is a latency hint only; the row
// itself is the durable source of truth.
const ecSendEventSQL = `WITH ins AS (
	INSERT INTO batch_events (job_id, event_type, expires_at)
	VALUES ($1, $2, $3)
	RETURNING job_id
)
SELECT pg_notify('` + channelEvents + `', (SELECT job_id FROM ins))`

// ecDrainEventsSQL destructively pops all live events for a job in FIFO (id)
// order. FOR UPDATE SKIP LOCKED keeps concurrent drainers from handing the same
// row to two consumers. Expired rows are filtered inline so correctness never
// depends on the safety-net GC.
const ecDrainEventsSQL = `DELETE FROM batch_events
WHERE id IN (
	SELECT id FROM batch_events
	WHERE job_id = $1 AND expires_at > EXTRACT(EPOCH FROM NOW())::BIGINT
	ORDER BY id
	FOR UPDATE SKIP LOCKED
)
RETURNING event_type`

// eventSub is a single consumer's subscription. The eventSubs map on
// PostgresExchangeClient (declared in exchange_db.go) is keyed by job ID and
// holds one of these per active consumer. closeOnce guards the channel close so
// CloseFn is idempotent.
type eventSub struct {
	id        string
	ch        chan db_api.BatchEvent
	closeOnce sync.Once
}

// ECProducerSendEvents inserts each event and fires a NOTIFY per insert. Rows are
// durable, so a consumer that attaches after this returns still receives the
// events. Returns the IDs that were successfully inserted.
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

	c.logger.V(logging.INFO).Info("ECProducerSendEvents: succeeded", "nIDs", len(sentIDs))
	return sentIDs, nil
}

// ECConsumerGetChannel registers a consumer for the job's events and returns a
// channel plus a CloseFn. It lazily starts the single shared dispatcher goroutine
// on the first call, then immediately drains any events already stored for the
// job so a late-attaching consumer never misses an event that was produced
// before it subscribed.
func (c *PostgresExchangeClient) ECConsumerGetChannel(ctx context.Context, ID string) (
	batchEventsChan *db_api.BatchEventsChan, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(ID) == 0 {
		return nil, fmt.Errorf("ID is empty")
	}

	sub := &eventSub{
		id: ID,
		ch: make(chan db_api.BatchEvent, eventChanBufSize),
	}

	c.eventsMu.Lock()
	if !c.eventsStarted {
		c.startEventDispatcherLocked()
	}
	c.eventSubs[ID] = sub
	c.eventsMu.Unlock()

	// Drain events already durably stored for this job (late-attach safety).
	c.deliverJobEvents(ctx, ID)

	closeFn := func() {
		c.eventsMu.Lock()
		if cur, ok := c.eventSubs[ID]; ok && cur == sub {
			delete(c.eventSubs, ID)
		}
		c.eventsMu.Unlock()
		// Closed exactly once; the delete above (under the same lock a delivering
		// dispatcher must also hold) guarantees no send races this close.
		sub.closeOnce.Do(func() { close(sub.ch) })
	}

	c.logger.V(logging.INFO).Info("ECConsumerGetChannel: succeeded", "ID", ID)
	return &db_api.BatchEventsChan{ID: ID, Events: sub.ch, CloseFn: closeFn}, nil
}

// startEventDispatcherLocked launches the one shared dispatcher goroutine. Caller
// must hold c.eventsMu. The dispatcher lives on a background-derived context so it
// is independent of any single caller's ctx; Close() cancels it via eventsCancel.
func (c *PostgresExchangeClient) startEventDispatcherLocked() {
	dispCtx, cancel := context.WithCancel(context.Background())
	c.eventsCancel = cancel
	c.eventsDone = make(chan struct{})
	c.eventsStarted = true

	wake, unsubscribe := c.listener.subscribe(channelEvents)
	go c.runEventDispatcher(dispCtx, wake, unsubscribe)
}

// runEventDispatcher is the single shared consumer loop. A wake carrying a job_id
// drains that job; an empty poll-fallback tick drains every subscribed job. Both
// the table and the re-drain-on-every-wake behavior make a dropped NOTIFY at most
// a latency hit, never lost work.
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
			// A buffered wake may be picked even after cancellation (Go select is
			// random). Bail before draining so we never query a pool that Close is
			// about to shut down.
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

// deliverAllJobEvents drains every currently subscribed job. Used on poll-fallback
// ticks, where the wake carries no specific job_id.
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

// deliverJobEvents destructively drains one job's events and fans them out to its
// subscriber. The DB drain runs without the lock (SKIP LOCKED never blocks); the
// non-blocking fan-out runs under eventsMu with a re-check that the same
// subscription is still registered, so it can never send on a channel CloseFn has
// already closed. If the consumer closed mid-drain the events are dropped, which
// is fine: CloseFn means the job finished and no longer cares.
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

// drainJobEvents runs the destructive FIFO pop for a single job and rebuilds the
// consumer-facing events (which carry no TTL).
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
