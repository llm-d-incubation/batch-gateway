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

package inference

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d-incubation/llm-d-async/producer"
	"github.com/redis/go-redis/v9"
)

const asyncQueuePrefix = "llm-d-async:"

// AsyncClientConfig holds the resolved configuration for async dispatch.
type AsyncClientConfig struct {
	RedisURL          string
	Models            map[string]string // model name -> pool name
	ResultPollTimeout time.Duration     // per-poll timeout in the result dispatcher loop
}

// AsyncGatewayResolver routes models to per-job AsyncInferenceClient instances.
// Each call to ClientFor creates a fresh client with its own result channel,
// backed by a shared producer and dispatcher per pool.
// Immutable after construction — safe for concurrent reads.
type AsyncGatewayResolver struct {
	pools           map[string]*asyncPool // model → pool
	closers         []io.Closer
	clientFactories map[string]func() AsyncInferenceClient // test-only override
}

// ClientFor creates a fresh per-job async client for the given model.
// Returns nil if no matching pool exists.
func (r *AsyncGatewayResolver) ClientFor(modelID string) AsyncInferenceClient {
	if r.clientFactories != nil {
		if factory, ok := r.clientFactories[modelID]; ok {
			return factory()
		}
		return nil
	}
	pool, ok := r.pools[modelID]
	if !ok {
		return nil
	}
	return newAsyncProducerClient(pool)
}

// NewTestAsyncResolver creates a resolver backed by factory functions instead of
// real Redis connections. Each call to ClientFor invokes the corresponding factory.
func NewTestAsyncResolver(factories map[string]func() AsyncInferenceClient) *AsyncGatewayResolver {
	return &AsyncGatewayResolver{clientFactories: factories}
}

// Close releases resources held by the resolver (dispatchers, producers, Redis).
func (r *AsyncGatewayResolver) Close() error {
	var errs []error
	for _, c := range r.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NewAsyncResolver creates an AsyncGatewayResolver with one shared pool
// (producer + dispatcher) per model/pool pair.
func NewAsyncResolver(config AsyncClientConfig, logger logr.Logger) (*AsyncGatewayResolver, error) {
	opts, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse async inference Redis URL: %w", err)
	}
	rdb := redis.NewClient(opts)

	poolToModel := make(map[string]string, len(config.Models))
	for model, poolName := range config.Models {
		if existing, ok := poolToModel[poolName]; ok {
			_ = rdb.Close()
			return nil, fmt.Errorf("models %q and %q both map to pool %q: each pool must have a single consumer", existing, model, poolName)
		}
		poolToModel[poolName] = model
	}

	if config.ResultPollTimeout <= 0 {
		_ = rdb.Close()
		return nil, fmt.Errorf("resultPollTimeout must be > 0")
	}

	pools := make(map[string]*asyncPool, len(config.Models))
	var closers []io.Closer

	for model, poolName := range config.Models {
		p, err := producer.NewRedisSortedSetProducer(
			producer.RedisSortedSetConfig{
				RequestQueueName: asyncQueuePrefix + "requests:" + poolName,
				ResultQueueName:  asyncQueuePrefix + "results:" + poolName,
			},
			producer.WithRedisClient(rdb),
		)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			_ = rdb.Close()
			return nil, fmt.Errorf("failed to create producer for model %q (pool %s): %w", model, poolName, err)
		}

		poolLogger := logger.WithName("async-inference").WithValues("pool", poolName)
		d := newResultDispatcher(p, poolLogger, config.ResultPollTimeout)
		pool := &asyncPool{
			producer:   p,
			dispatcher: d,
			logger:     poolLogger,
		}

		pools[model] = pool
		closers = append(closers, d, p)
	}

	closers = append(closers, rdb)

	return &AsyncGatewayResolver{pools: pools, closers: closers}, nil
}
