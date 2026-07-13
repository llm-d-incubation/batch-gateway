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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	asyncapi "github.com/llm-d-incubation/llm-d-async/api"
	"github.com/llm-d-incubation/llm-d-async/producer"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	httpclient "github.com/llm-d/llm-d-batch-gateway/pkg/clients/http"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

var _ AsyncInferenceClient = (*asyncProducerClient)(nil)

// resultDispatcher reads results from the producer's shared result queue and
// routes them to the correct caller by request ID. The processor dispatches
// multiple requests per model concurrently, and results arrive in any order,
// so a single reader must demux them.
type resultDispatcher struct {
	producer    producer.Producer
	logger      logr.Logger
	pollTimeout time.Duration
	waiters     sync.Map // requestID -> chan<- *GenerateResponse
	once        sync.Once
	wg          sync.WaitGroup
	cancel      context.CancelFunc
}

func newResultDispatcher(p producer.Producer, logger logr.Logger, pollTimeout time.Duration) *resultDispatcher {
	return &resultDispatcher{
		producer:    p,
		logger:      logger,
		pollTimeout: pollTimeout,
	}
}

func (d *resultDispatcher) ensureStarted() {
	d.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		d.cancel = cancel
		d.wg.Add(1)
		go d.run(ctx)
	})
}

func (d *resultDispatcher) run(ctx context.Context) {
	defer d.wg.Done()
	for ctx.Err() == nil {
		d.processNext(ctx)
	}
}

// processNext polls for a single result and routes it. A deferred recover
// keeps the dispatcher goroutine alive if a library bug or unexpected nil
// causes a panic — without this, sync.Once would prevent a restart.
func (d *resultDispatcher) processNext(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error(fmt.Errorf("panic: %v", r), "resultDispatcher recovered from panic")
		}
	}()

	pollCtx, pollCancel := context.WithTimeout(ctx, d.pollTimeout)
	result, err := d.producer.GetResult(pollCtx)
	pollCancel()
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			d.logger.Error(err, "Failed to read from result queue")
		}
		return
	}

	val, ok := d.waiters.LoadAndDelete(result.ID)
	if !ok {
		d.logger.Info("Dropped result with no waiter", "resultID", result.ID)
		return
	}
	ch, ok := val.(chan<- *GenerateResponse)
	if !ok {
		d.logger.Error(fmt.Errorf("unexpected type %T in waiters map", val), "Type assertion failed")
		return
	}
	resp := &GenerateResponse{
		RequestID: result.ID,
		Response:  []byte(result.Payload),
	}
	select {
	case ch <- resp:
	default:
		d.logger.Error(fmt.Errorf("result channel full"), "Dropping result", "resultID", result.ID)
	}
}

func (d *resultDispatcher) register(requestID string, ch chan<- *GenerateResponse) {
	d.waiters.Store(requestID, ch)
	d.ensureStarted()
}

func (d *resultDispatcher) unregister(requestID string) {
	d.waiters.Delete(requestID)
}

func (d *resultDispatcher) Close() error {
	if d.cancel != nil {
		d.cancel()
		done := make(chan struct{})
		go func() {
			d.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("result dispatcher did not shut down within 2s")
		}
	}
	return nil
}

const (
	defaultResultBufferSize = 100
	defaultDeadline         = 5 * time.Minute
)

// asyncPool holds the shared resources for one inference pool.
// Multiple per-job clients share the same pool.
type asyncPool struct {
	producer   producer.Producer
	dispatcher *resultDispatcher
	logger     logr.Logger
}

// asyncProducerClient is a per-job client that submits requests and collects
// results via an internal channel. Each job gets its own client (and channel)
// from AsyncGatewayResolver.ClientFor, backed by a shared pool.
type asyncProducerClient struct {
	pool       *asyncPool
	results    chan *GenerateResponse
	pendingIDs sync.Map // tracks submitted request IDs for cleanup
	logger     logr.Logger
}

func newAsyncProducerClient(pool *asyncPool) *asyncProducerClient {
	return &asyncProducerClient{
		pool:    pool,
		results: make(chan *GenerateResponse, defaultResultBufferSize),
		logger:  pool.logger,
	}
}

// Submit enqueues a request for async processing. The result will be routed
// to this client's internal channel by the shared dispatcher.
func (c *asyncProducerClient) Submit(ctx context.Context, req *GenerateRequest) *ClientError {
	now := time.Now()
	deadline := now.Add(defaultDeadline)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}

	metadata := make(map[string]string)
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(metadata))

	reqMsg := &asyncapi.RequestMessage{
		ID:       req.RequestID,
		Created:  now.Unix(),
		Deadline: deadline.Unix(),
		Payload:  req.Params,
		Headers:  req.Headers,
		Endpoint: req.Endpoint,
		Metadata: metadata,
	}

	c.pool.dispatcher.register(req.RequestID, c.results)
	c.pendingIDs.Store(req.RequestID, struct{}{})

	if err := c.pool.producer.SubmitRequest(ctx, reqMsg); err != nil {
		c.pool.dispatcher.unregister(req.RequestID)
		c.pendingIDs.Delete(req.RequestID)
		return &ClientError{
			Category: httpclient.ErrCategoryServer,
			Message:  fmt.Sprintf("submit async request: %v", err),
			RawError: err,
		}
	}

	c.logger.V(logging.TRACE).Info("Submitted async request", "requestID", req.RequestID)
	return nil
}

// GetResult blocks until the next result arrives or the context is cancelled.
func (c *asyncProducerClient) GetResult(ctx context.Context) (*GenerateResponse, error) {
	select {
	case resp := <-c.results:
		c.pendingIDs.Delete(resp.RequestID)
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Cancel marks all still-pending submitted request IDs as cancelled so the
// dispatcher can drop them before dispatch. Uses the producer CancelRequests
// API; already-dispatched/in-flight requests are not aborted.
func (c *asyncProducerClient) Cancel(ctx context.Context) error {
	var ids []string
	c.pendingIDs.Range(func(key, _ any) bool {
		id, ok := key.(string)
		if !ok {
			return true
		}
		ids = append(ids, id)
		return true
	})
	if len(ids) == 0 {
		return nil
	}
	if err := c.pool.producer.CancelRequests(ctx, ids); err != nil {
		return fmt.Errorf("cancel async requests: %w", err)
	}
	c.logger.V(logging.INFO).Info("Cancelled pending async requests", "count", len(ids))
	return nil
}

// Close unregisters all pending waiters from the shared dispatcher.
func (c *asyncProducerClient) Close() error {
	c.pendingIDs.Range(func(key, _ any) bool {
		id, ok := key.(string)
		if !ok {
			return true
		}
		c.pool.dispatcher.unregister(id)
		return true
	})
	return nil
}
