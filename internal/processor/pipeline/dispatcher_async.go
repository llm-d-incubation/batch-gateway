package pipeline

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// AsyncDispatcher submits requests to async queues (fire-and-forget).
// Results arrive via per-model ResultBroadcasters (backed by shared
// clients) that send directly to resultCh.
type AsyncDispatcher struct {
	resolver     *inference.AsyncGatewayResolver
	broadcasters *BroadcasterGroup
	pending      *PendingRequests
	logger       logr.Logger
}

var _ RequestDispatcher = (*AsyncDispatcher)(nil)

func NewAsyncDispatcher(
	resolver *inference.AsyncGatewayResolver,
	broadcasters *BroadcasterGroup,
	pending *PendingRequests,
	logger logr.Logger,
) *AsyncDispatcher {
	return &AsyncDispatcher{
		resolver:     resolver,
		broadcasters: broadcasters,
		pending:      pending,
		logger:       logger,
	}
}

func (d *AsyncDispatcher) Run(ctx context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
	d.broadcasters.Subscribe(resultCh)

	// Submit phase — fast queue writes.
	for msg := range requestCh {
		if ctx.Err() != nil {
			resultCh <- *msg.Canceled()
			break
		}

		client := d.resolver.SharedClientFor(msg.ModelID)
		if client == nil {
			resultCh <- *msg.ModelNotFound()
			continue
		}

		d.pending.Store(msg)

		req := &inference.GenerateRequest{
			RequestID: msg.RequestID,
			Endpoint:  msg.Endpoint,
			Params:    msg.Body,
			Headers:   msg.Headers,
		}

		if submitErr := client.Submit(ctx, req); submitErr != nil {
			resultCh <- *msg.Error(
				string(submitErr.Category),
				submitErr.Message,
			)
			continue
		}
	}

	// Drain remaining requests as cancelled (if loop broke early).
	for msg := range requestCh {
		resultCh <- *msg.Canceled()
	}

	// Wait for all pending results to be resolved by the collector.
	d.pending.Wait(ctx)

	// Unsubscribe blocks until any in-progress broadcaster send completes
	// (MutexMap.Delete waits for Range to release the lock), so closing
	// resultCh after this is safe — no concurrent writes remain.
	d.broadcasters.Unsubscribe(resultCh)
	close(resultCh)

	return nil
}
