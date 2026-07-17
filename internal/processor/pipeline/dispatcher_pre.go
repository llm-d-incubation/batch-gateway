package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
)

// PreDispatcher performs shared pre-dispatch logic before forwarding
// requests to the next dispatcher in the chain. It handles:
//   - context cancellation and draining remaining requests
//   - filtering requests with ParseError
//   - stamping SubmittedAt
//   - incrementing inflight metrics
type PreDispatcher struct {
	next RequestDispatcher
}

var _ RequestDispatcher = (*PreDispatcher)(nil)

func NewPreDispatcher(next RequestDispatcher) *PreDispatcher {
	return &PreDispatcher{next: next}
}

func (d *PreDispatcher) Run(ctx context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
	delegateRequestCh := make(chan RequestItem)
	delegateResultCh := make(chan ResultItem)

	var innerWg sync.WaitGroup
	var innerErr error
	innerWg.Add(1)
	go func() {
		defer innerWg.Done()
		innerErr = d.next.Run(ctx, delegateRequestCh, delegateResultCh)
	}()

	var resultWg sync.WaitGroup
	resultWg.Add(1)
	go func() {
		defer resultWg.Done()
		for result := range delegateResultCh {
			resultCh <- result
		}
	}()

	for msg := range requestCh {
		if ctx.Err() != nil {
			resultCh <- *msg.Error(cancelCode(ctx))
			break
		}

		if msg.ParseError != nil {
			resultCh <- *msg.Error(msg.ParseError.Code, msg.ParseError.Message)
			continue
		}

		msg.SubmittedAt = time.Now()
		metrics.IncProcessorInflightRequests()
		metrics.IncModelInflightRequests(msg.ModelID)

		delegateRequestCh <- msg
	}

	for msg := range requestCh {
		resultCh <- *msg.Error(cancelCode(ctx))
	}

	close(delegateRequestCh)

	innerWg.Wait()
	resultWg.Wait()
	close(resultCh)
	return innerErr
}
