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

// This file implements the shared LISTEN/NOTIFY dispatcher for the PostgreSQL
// exchange client. It owns exactly ONE dedicated pooled connection and LISTENs on
// both fixed exchange channels (channelQueueWake, channelEvents). NOTIFY payloads
// and poll-fallback ticks are fanned out to per-channel subscribers.
//
// The exchange tables are the source of truth; NOTIFY is only a latency hint. A
// subscriber treats every wake (real notification OR poll tick) as "re-drain the
// table", so a dropped/at-most-once NOTIFY, or a pool that cannot hand out a
// dedicated connection (pgxmock's Acquire returns "not implemented"), degrades to
// ~pollInterval latency, never to lost work.
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

// pgListener fans NOTIFY payloads (and poll-fallback ticks) from one dedicated
// connection out to per-channel subscribers. Delivery is non-blocking on
// buffered(1) channels: a pending wake is idempotent, so coalescing wakes is
// harmless (the subscriber always re-drains the underlying table).
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

	// listenErrLogged suppresses repeat ERROR logs when LISTEN keeps failing (e.g.
	// behind a transaction-pooling pgbouncer that rejects LISTEN). Touched only by
	// the single run goroutine, so it needs no lock. Reset once LISTEN succeeds.
	listenErrLogged bool
}

// newPGListener builds a listener over the given pool. It does not start the
// dispatcher goroutine; call start.
func newPGListener(pool *pgxpool.Pool, pollInterval time.Duration, logger logr.Logger) *pgListener {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &pgListener{
		pool:         pool,
		pollInterval: pollInterval,
		logger:       logger,
		subs:         make(map[string]map[int]chan string),
		done:         make(chan struct{}),
	}
}

// start launches the single dispatcher goroutine and stores its cancel func so
// close can stop it.
func (l *pgListener) start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.run(runCtx)
}

// subscribe registers a subscriber for a channel and returns its delivery channel
// plus an unsubscribe func. Multiple subscribers per channel are allowed. The
// delivery channel is never closed by the listener (the dispatcher may still try
// to send under the lock); unsubscribe just detaches it so it is GC'd once the
// caller drops it.
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
		if l.cancel != nil {
			l.cancel()
		}
		<-l.done
	})
	return nil
}

// deliver sends payload non-blockingly to every subscriber of one channel.
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

// deliverAll sends payload non-blockingly to every subscriber of every channel.
// Used for poll-fallback ticks, which carry no specific channel/job.
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

// run is the single dispatcher loop. It keeps one dedicated connection parked in
// WaitForNotification. If it cannot acquire a connection it falls to POLL-ONLY
// mode (emit ticks every pollInterval), so consumers still make progress.
func (l *pgListener) run(ctx context.Context) {
	defer close(l.done)

	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := l.pool.Acquire(ctx)
		if err != nil {
			// POLL-ONLY: no dedicated conn (pgxmock returns "not implemented", or
			// the pool is momentarily exhausted). Tick, then retry.
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
		// listenLoop returned on a connection error; back off then re-acquire,
		// ticking in the meantime so consumers keep draining.
		l.deliverAll("")
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.pollInterval):
		}
	}
}

// logListenErr reports a LISTEN failure once at ERROR, then downgrades repeats to
// V(INFO). A pooler in transaction/statement mode rejects LISTEN on every acquire,
// so without this de-dup the loop would flood ERROR logs every pollInterval. The
// data path still works: run() falls back to poll-only ticks, so consumers re-drain
// the tables and only lose the NOTIFY latency optimization, never work.
func (l *pgListener) logListenErr(err error, channel string) {
	if l.listenErrLogged {
		l.logger.V(logging.INFO).Info("pgListener: LISTEN still failing, poll-only fallback", "channel", channel, "err", err.Error())
		return
	}
	l.listenErrLogged = true
	l.logger.Error(err, "pgListener: LISTEN failed, falling back to poll-only (NOTIFY disabled, e.g. a transaction-pooling pgbouncer)", "channel", channel)
}

// listenLoop LISTENs on both channels then blocks on notifications with a
// pollInterval timeout. It returns on a connection-level error (so run
// re-acquires) or on parent-context cancellation (shutdown).
func (l *pgListener) listenLoop(ctx context.Context, conn *pgxpool.Conn) {
	// Channel names are compile-time constants, not user input, so there is no
	// injection concern; LISTEN cannot be parameterized.
	if _, err := conn.Exec(ctx, "LISTEN "+channelQueueWake); err != nil {
		l.logListenErr(err, channelQueueWake)
		return
	}
	if _, err := conn.Exec(ctx, "LISTEN "+channelEvents); err != nil {
		l.logListenErr(err, channelEvents)
		return
	}
	// LISTEN succeeded: re-arm the one-shot error log so a later transient failure
	// (that then recovers) is reported again.
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
			// Parent context cancelled -> shutdown.
			return
		default:
			// Connection-level error: bail so run re-acquires a fresh connection.
			l.logger.Error(err, "pgListener: WaitForNotification failed")
			return
		}
	}
}
