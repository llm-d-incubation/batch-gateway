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

// Shared LISTEN/NOTIFY dispatcher: one dedicated pooled connection LISTENing on
// both exchange channels, fanning wakes out to per-channel subscribers.
//
// The exchange tables are the source of truth; NOTIFY is only a latency hint.
// Subscribers treat every wake (real notification OR poll tick) as "re-drain the
// table", so a dropped NOTIFY or an unavailable dedicated connection degrades to
// ~pollInterval latency, never lost work.
//
//	pool ──Acquire(1 conn)──▶ LISTEN both channels ──WaitForNotification──▶ deliver
//	                                                        │
//	                                          poll timeout ─┴─▶ "" tick to all subs

package postgresql

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// Delivery is non-blocking on buffered(1) channels: a pending wake is idempotent,
// so coalescing wakes is harmless (the subscriber always re-drains the table).
type pgListener struct {
	pool         *pgxpool.Pool
	pollInterval time.Duration
	logger       logr.Logger

	mu     sync.Mutex
	subs   map[string]map[int]chan string // channel -> subID -> delivery chan
	nextID int

	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}

	// Suppresses repeat ERROR logs when LISTEN keeps failing (e.g. a
	// transaction-pooling pgbouncer rejects it every acquire). Touched only by the
	// run goroutine, so no lock.
	listenErrLogged bool
}

// newPGListener launches the dispatcher goroutine. Its lifetime is deliberately
// not tied to a caller context (which may carry a startup deadline); it ends only
// when close() cancels it.
func newPGListener(pool *pgxpool.Pool, pollInterval time.Duration, logger logr.Logger) *pgListener {
	l := &pgListener{
		pool:         pool,
		pollInterval: pollInterval,
		logger:       logger,
		subs:         make(map[string]map[int]chan string),
		done:         make(chan struct{}),
	}
	runCtx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go l.run(runCtx)
	return l
}

// subscribe returns a delivery channel plus an unsubscribe func. The delivery
// channel is never closed (the dispatcher may still try to send under the lock);
// unsubscribe just detaches it.
func (l *pgListener) subscribe(channel string) (<-chan string, func()) {
	ch := make(chan string, 1)

	l.mu.Lock()
	id := l.nextID
	l.nextID++
	if l.subs[channel] == nil {
		l.subs[channel] = make(map[int]chan string)
	}
	l.subs[channel][id] = ch
	l.mu.Unlock()

	unsubscribe := func() {
		l.mu.Lock()
		if m, ok := l.subs[channel]; ok {
			delete(m, id)
		}
		l.mu.Unlock()
	}
	return ch, unsubscribe
}

// close stops the dispatcher goroutine and waits for it to exit. Idempotent.
func (l *pgListener) close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		<-l.done
	})
	return nil
}

func (l *pgListener) deliver(channel, payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ch := range l.subs[channel] {
		select {
		case ch <- payload:
		default:
		}
	}
}

// deliverAll is used for poll-fallback ticks, which carry no specific channel.
func (l *pgListener) deliverAll(payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.subs {
		for _, ch := range m {
			select {
			case ch <- payload:
			default:
			}
		}
	}
}

// run parks one dedicated connection in WaitForNotification. If it cannot acquire
// a connection it degrades to poll-only ticks, so consumers still make progress.
func (l *pgListener) run(ctx context.Context) {
	defer close(l.done)

	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := l.pool.Acquire(ctx)
		if err != nil {
			l.logger.V(logging.INFO).Info("pgListener: acquire failed, poll-only tick", "err", err.Error())
			l.deliverAll("")
			select {
			case <-ctx.Done():
				return
			case <-time.After(l.pollInterval):
			}
			continue
		}

		l.listenLoop(ctx, conn)
		conn.Release()
		if ctx.Err() != nil {
			return
		}
		// Connection error: tick then re-acquire so consumers keep draining.
		l.deliverAll("")
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.pollInterval):
		}
	}
}

// logListenErr reports a LISTEN failure once at ERROR, then downgrades repeats to
// V(INFO): a pooler in transaction mode rejects LISTEN on every acquire, which
// would otherwise flood ERROR logs every pollInterval.
func (l *pgListener) logListenErr(err error, channel string) {
	if l.listenErrLogged {
		l.logger.V(logging.INFO).Info("pgListener: LISTEN still failing, poll-only fallback", "channel", channel, "err", err.Error())
		return
	}
	l.listenErrLogged = true
	l.logger.Error(err, "pgListener: LISTEN failed, falling back to poll-only (NOTIFY disabled, e.g. a transaction-pooling pgbouncer)", "channel", channel)
}

// listenLoop returns on a connection-level error (so run re-acquires) or on
// parent-context cancellation.
func (l *pgListener) listenLoop(ctx context.Context, conn *pgxpool.Conn) {
	// LISTEN cannot be parameterized; channel names are compile-time constants.
	if _, err := conn.Exec(ctx, "LISTEN "+channelQueueWake); err != nil {
		l.logListenErr(err, channelQueueWake)
		return
	}
	if _, err := conn.Exec(ctx, "LISTEN "+channelEvents); err != nil {
		l.logListenErr(err, channelEvents)
		return
	}
	// Re-arm the one-shot error log so a later transient failure is reported.
	l.listenErrLogged = false
	l.logger.V(logging.INFO).Info("pgListener: listening", "channels", []string{channelQueueWake, channelEvents})

	for {
		if ctx.Err() != nil {
			return
		}

		wctx, cancel := context.WithTimeout(ctx, l.pollInterval)
		n, err := conn.Conn().WaitForNotification(wctx)
		cancel()

		switch {
		case err == nil:
			l.deliver(n.Channel, n.Payload)
		case errors.Is(err, context.DeadlineExceeded):
			// Poll-fallback tick: everyone re-drains their table.
			l.deliverAll("")
		case ctx.Err() != nil:
			return
		default:
			// Connection-level error: bail so run re-acquires.
			l.logger.Error(err, "pgListener: WaitForNotification failed")
			return
		}
	}
}
