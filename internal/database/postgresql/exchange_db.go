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

// This file defines PostgresExchangeClient, the single struct that implements all
// four exchange interfaces (BatchPriorityQueueClient, BatchEventChannelClient,
// BatchStatusClient, InFlightClient) over PostgreSQL, mirroring the redis
// ExchangeDBClientRedis. It owns the connection pool, the shared LISTEN/NOTIFY
// dispatcher, and the lazily-started events dispatcher state. The per-interface
// methods live in exchange_queue.go, exchange_event.go, exchange_status.go, and
// exchange_inflight.go.

package postgresql

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	ucom "github.com/llm-d/llm-d-batch-gateway/internal/util/com"
)

//go:embed exchange_schema.sql
var exchangeSchemaSql string

const (
	// pollIntervalDefault is the dequeue/event poll fallback interval. Every
	// consumer re-drains on each poll tick, so this bounds worst-case latency when
	// a NOTIFY is missed. Tests inject a much smaller value.
	pollIntervalDefault = 1 * time.Second

	// statusTTLDefaultSec matches the redis default (ttlSecDefault): 60 days.
	statusTTLDefaultSec = 60 * 60 * 24 * 60

	// channelQueueWake and channelEvents are the two fixed LISTEN/NOTIFY channels.
	// They are compile-time constants (never interpolated from user input).
	channelQueueWake = "batch_queue_wake"
	channelEvents    = "batch_events"
)

// PostgresExchangeClient is the single struct implementing all four exchange
// interfaces. All CRUD goes through pool (the narrow pgxPool interface: a
// *pgxpool.Pool in prod, pgxmock in unit tests). listener is the shared
// LISTEN/NOTIFY dispatcher; it is nil in pure-CRUD unit tests.
type PostgresExchangeClient struct {
	pool         pgxPool
	listener     *pgListener
	logger       logr.Logger
	pollInterval time.Duration
	closeOnce    sync.Once

	// events dispatcher state, guarded by eventsMu (see exchange_event.go).
	eventsMu      sync.Mutex
	eventSubs     map[string]*eventSub
	eventsStarted bool
	eventsCancel  context.CancelFunc
	// eventsDone is closed by runEventDispatcher when it exits. Close() waits on it
	// (like pgListener.done) so the dispatcher can never touch the pool after Close.
	eventsDone chan struct{}
}

// Compile-time checks: PostgresExchangeClient implements all four interfaces.
var (
	_ db_api.BatchPriorityQueueClient = (*PostgresExchangeClient)(nil)
	_ db_api.BatchEventChannelClient  = (*PostgresExchangeClient)(nil)
	_ db_api.BatchStatusClient        = (*PostgresExchangeClient)(nil)
	_ db_api.InFlightClient           = (*PostgresExchangeClient)(nil)
)

// NewPostgresExchangeClient creates the exchange client, applies the (idempotent)
// exchange schema once, and starts the shared LISTEN dispatcher. It opens its own
// pool from the shared PostgreSQL URL: the pool holds one long-lived LISTEN
// connection, so a second small pool is cheap and keeps this constructor parallel
// to NewPostgresBatchDBClient without touching the shared db_core files.
func NewPostgresExchangeClient(ctx context.Context, config *PostgreSQLConfig, logger logr.Logger) (*PostgresExchangeClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if config == nil {
		return nil, fmt.Errorf("postgresql config cannot be nil")
	}

	// Resolve the connection URL from the mounted secret when not set in config,
	// mirroring NewPostgreSQLDBClients. The exchange builds before the db client in
	// NewClientset, so the shared config's URL may still be empty here.
	if config.Url == "" {
		postgreSQLURL, err := ucom.ReadSecretFile(ucom.SecretKeyPostgreSQLURL)
		if err != nil {
			return nil, err
		}
		config.Url = postgreSQLURL
	}

	// Reserve pool headroom for the permanently parked LISTEN connection so a low
	// pool_max_conns on the shared URL cannot starve exchange CRUD.
	config.ReserveConnForListen = true
	pool, err := newPool(ctx, config)
	if err != nil {
		return nil, err
	}

	// newPool always returns a *pgxpool.Pool in production; the assertion only
	// fails in tests that bypass this constructor (they build the struct directly).
	pgPool, ok := pool.(*pgxpool.Pool)
	if !ok {
		pool.Close()
		return nil, fmt.Errorf("expected *pgxpool.Pool from newPool, got %T", pool)
	}

	if _, err := pgPool.Exec(ctx, exchangeSchemaSql); err != nil {
		pgPool.Close()
		return nil, fmt.Errorf("failed to apply exchange schema: %w", err)
	}

	listener := newPGListener(pgPool, pollIntervalDefault, logger)
	// The listener runs for the client's whole lifetime; close() stops it.
	listener.start(context.Background())

	c := &PostgresExchangeClient{
		pool:         pgPool,
		listener:     listener,
		logger:       logger,
		pollInterval: pollIntervalDefault,
		eventSubs:    make(map[string]*eventSub),
	}

	logger.Info("NewPostgresExchangeClient: client created successfully",
		"maxConns", pgPool.Config().MaxConns)
	return c, nil
}

// Close cancels the events dispatcher, waits for it to exit, stops the LISTEN
// dispatcher, and closes the pool. Joining the dispatcher before closing the pool
// guarantees it can never run a drain query against an already-closed pool.
// Idempotent via closeOnce.
func (c *PostgresExchangeClient) Close() error {
	c.closeOnce.Do(func() {
		c.eventsMu.Lock()
		cancel := c.eventsCancel
		done := c.eventsDone
		c.eventsMu.Unlock()

		if cancel != nil {
			cancel()
		}
		// Wait for the dispatcher goroutine to fully exit before touching the pool.
		if done != nil {
			<-done
		}

		if c.listener != nil {
			_ = c.listener.close()
		}
		if c.pool != nil {
			c.pool.Close()
		}
	})
	return nil
}

// SweepExpired deletes expired rows from batch_queue, batch_status, and
// batch_events. Correctness does not depend on it (every read filters expired rows
// inline); it is a safety-net reclaim for the gc binary. It is additive and not
// wired into gc here.
func (c *PostgresExchangeClient) SweepExpired(ctx context.Context) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	const sweepSQL = `WITH q AS (
	DELETE FROM batch_queue WHERE expires_at IS NOT NULL AND expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
), s AS (
	DELETE FROM batch_status WHERE expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
), e AS (
	DELETE FROM batch_events WHERE expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
)
SELECT 1`

	if _, err = c.pool.Exec(ctx, sweepSQL); err != nil {
		return fmt.Errorf("SweepExpired: %w", err)
	}
	return nil
}
