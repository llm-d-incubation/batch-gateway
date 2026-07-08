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

// PostgresExchangeClient implements the four exchange interfaces over PostgreSQL,
// mirroring the redis ExchangeDBClientRedis. Per-interface methods live in
// exchange_queue.go, exchange_event.go, exchange_status.go, exchange_inflight.go.

package postgresql

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

//go:embed exchange_schema.sql
var exchangeSchemaSql string

const (
	// DefaultPollInterval bounds worst-case latency when a NOTIFY is missed.
	// Exported so integration tests can calibrate NOTIFY-vs-fallback timing.
	DefaultPollInterval = 1 * time.Second

	// statusTTLDefaultSec matches the redis default (ttlSecDefault): 60 days.
	statusTTLDefaultSec = 60 * 60 * 24 * 60

	// The two fixed LISTEN/NOTIFY channels; never interpolated from user input.
	channelQueueWake = "batch_queue_wake"
	channelEvents    = "batch_events"
)

// PostgresExchangeClient implements all four exchange interfaces.
type PostgresExchangeClient struct {
	pool      pgxPool
	listener  *pgListener
	logger    logr.Logger
	closeOnce sync.Once

	eventsMu     sync.Mutex
	eventSubs    map[string]*eventSub // per-job consumer registry, guarded by eventsMu
	eventsCancel context.CancelFunc
	// Closed when runEventDispatcher exits; Close() waits on it so the dispatcher
	// can never touch the pool after Close.
	eventsDone chan struct{}
}

// Compile-time checks: PostgresExchangeClient implements all four interfaces.
var (
	_ db_api.BatchPriorityQueueClient = (*PostgresExchangeClient)(nil)
	_ db_api.BatchEventChannelClient  = (*PostgresExchangeClient)(nil)
	_ db_api.BatchStatusClient        = (*PostgresExchangeClient)(nil)
	_ db_api.InFlightClient           = (*PostgresExchangeClient)(nil)
)

// NewPostgresExchangeClient applies the (idempotent) exchange schema and starts
// the LISTEN and event dispatchers. It opens its own pool rather than sharing the
// db_client's: it permanently parks one connection in LISTEN and must not starve
// the persistent layer. The caller resolves config.Url.
func NewPostgresExchangeClient(ctx context.Context, config *PostgreSQLConfig, logger logr.Logger) (*PostgresExchangeClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if config == nil {
		return nil, fmt.Errorf("postgresql config cannot be nil")
	}

	config.ReserveConnForListen = true
	pool, err := newPool(ctx, config)
	if err != nil {
		return nil, err
	}

	if _, err := pool.Exec(ctx, exchangeSchemaSql); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to apply exchange schema: %w", err)
	}

	c := &PostgresExchangeClient{
		pool:      pool,
		listener:  newPGListener(pool, DefaultPollInterval, logger),
		logger:    logger,
		eventSubs: make(map[string]*eventSub),
	}
	c.startEventDispatcher()

	logger.Info("NewPostgresExchangeClient: client created successfully",
		"maxConns", pool.Config().MaxConns)
	return c, nil
}

// Close is idempotent. Dispatchers are joined before the pool closes so they can
// never query an already-closed pool.
func (c *PostgresExchangeClient) Close() error {
	c.closeOnce.Do(func() {
		c.eventsCancel()
		<-c.eventsDone
		_ = c.listener.close()
		c.pool.Close()
	})
	return nil
}

const sweepExpiredSQL = `WITH q AS (
	DELETE FROM batch_queue WHERE expires_at IS NOT NULL AND expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
), s AS (
	DELETE FROM batch_status WHERE expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
), e AS (
	DELETE FROM batch_events WHERE expires_at <= EXTRACT(EPOCH FROM NOW())::BIGINT
)
SELECT 1`

// SweepExpired reclaims expired rows from all three exchange tables. Correctness
// does not depend on it (reads filter expiry inline); the gc binary runs it
// periodically as a safety net.
func (c *PostgresExchangeClient) SweepExpired(ctx context.Context) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if _, err = c.pool.Exec(ctx, sweepExpiredSQL); err != nil {
		return fmt.Errorf("SweepExpired: %w", err)
	}
	return nil
}
