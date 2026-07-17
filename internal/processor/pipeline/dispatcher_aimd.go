package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/semaphore"
)

// EndpointAIMD pairs an adaptive semaphore with its AIMD controller
// for a single inference endpoint.
type EndpointAIMD struct {
	Sem   *semaphore.AdaptiveSemaphore
	AIMD  *semaphore.AIMDController
	Label string
}

// AIMDDispatcher wraps another dispatcher with per-endpoint adaptive
// semaphores and a global semaphore. Acquires endpoint then global
// (same order as the original processModel) to prevent starvation.
type AIMDDispatcher struct {
	next      RequestDispatcher
	models    map[string]*EndpointAIMD
	globalSem semaphore.Semaphore
	logger    logr.Logger
}

var _ RequestDispatcher = (*AIMDDispatcher)(nil)

func NewAIMDDispatcher(
	next RequestDispatcher,
	models map[string]*EndpointAIMD,
	globalLimit int,
	logger logr.Logger,
) (*AIMDDispatcher, error) {
	globalSem, err := semaphore.New(globalLimit, nil)
	if err != nil {
		return nil, fmt.Errorf("global concurrency semaphore: %w", err)
	}
	return &AIMDDispatcher{
		next:      next,
		models:    models,
		globalSem: globalSem,
		logger:    logger,
	}, nil
}

func (d *AIMDDispatcher) Run(ctx context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
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
			ep := d.models[result.ModelID]
			if ep != nil {
				ep.Sem.Release()
				recordAIMDSignal(ep, result)
			}
			d.globalSem.Release()
			resultCh <- result
		}
	}()

	// feed delegateRequestCh from requestCh with semaphore gating
	for msg := range requestCh {
		if err := d.acquireSlot(ctx, msg.ModelID); err != nil {
			resultCh <- *msg.Error(cancelCode(ctx))
			break
		}
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

func (d *AIMDDispatcher) acquireSlot(ctx context.Context, modelID string) error {
	ep := d.models[modelID]
	if ep != nil {
		if err := ep.Sem.Acquire(ctx); err != nil {
			return err
		}
	}
	if err := d.globalSem.Acquire(ctx); err != nil {
		if ep != nil {
			ep.Sem.Release()
		}
		return err
	}
	return nil
}

func recordAIMDSignal(ep *EndpointAIMD, result ResultItem) {
	if ep.AIMD == nil || result.Response == nil {
		return
	}

	sc := result.Response.StatusCode

	switch {
	case sc == http.StatusTooManyRequests:
		ep.AIMD.RecordRateLimit(metrics.AIMDSignal429)
		metrics.RecordAIMDDecrease(ep.Label, metrics.AIMDSignal429)
	case sc >= http.StatusInternalServerError:
		ep.AIMD.RecordRateLimit(metrics.AIMDSignal5xx)
		metrics.RecordAIMDDecrease(ep.Label, metrics.AIMDSignal5xx)
	case result.HadCapacityRetry:
		ep.AIMD.RecordRateLimit(metrics.AIMDSignalCapacityRetry)
		metrics.RecordAIMDDecrease(ep.Label, metrics.AIMDSignalCapacityRetry)
	default:
		oldLimit := ep.AIMD.Limit()
		ep.AIMD.RecordSuccess()
		if ep.AIMD.Limit() != oldLimit {
			metrics.RecordAIMDIncrease(ep.Label)
		}
	}

	metrics.SetAIMDConcurrencyLimit(ep.Label, float64(ep.AIMD.Limit()))
}
