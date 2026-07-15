package pipeline

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
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

		if msg.ParseError != nil {
			resultCh <- *msg.Error(msg.ParseError.Code, msg.ParseError.Message)
			continue
		}

		client := d.resolver.SharedClientFor(msg.ModelID)
		if client == nil {
			resultCh <- *msg.ModelNotFound()
			continue
		}

		msg.SubmittedAt = time.Now()
		d.pending.Store(msg)
		metrics.IncProcessorInflightRequests()
		metrics.IncModelInflightRequests(msg.ModelID)

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

	// Drain submitted-but-uncollected requests as errors so that
	// output_lines + error_lines == total_requests.
	d.pending.DrainUnresolved(func(msg RequestItem) {
		resultCh <- *msg.Error("batch_expired", "result not collected before deadline")
	})

	// Unsubscribe removes resultCh from the broadcast list. A concurrent
	// send from the broadcaster may still race with close(resultCh);
	// safeChannelSend recovers from the resulting panic.
	d.broadcasters.Unsubscribe(resultCh)
	close(resultCh)

	return nil
}
