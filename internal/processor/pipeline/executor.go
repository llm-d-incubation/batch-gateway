package pipeline

import (
	"context"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

// JobExecutor orchestrates the execution pipeline.
type JobExecutor struct {
	source     RequestSource
	dispatcher RequestDispatcher
	pending    *PendingRequests
	collector  *ResultCollector
	tracker    *ProgressTracker
	logger     logr.Logger
}

type JobExecutorConfig struct {
	Source     RequestSource
	Dispatcher RequestDispatcher
	Pending    *PendingRequests
	Collector  *ResultCollector
	Tracker    *ProgressTracker
	Logger     logr.Logger
}

func NewJobExecutor(cfg JobExecutorConfig) *JobExecutor {
	return &JobExecutor{
		source:     cfg.Source,
		dispatcher: cfg.Dispatcher,
		pending:    cfg.Pending,
		collector:  cfg.Collector,
		tracker:    cfg.Tracker,
		logger:     cfg.Logger,
	}
}

func (je *JobExecutor) Execute(ctx context.Context) (*openai.BatchRequestCounts, error) {
	g, ctx := errgroup.WithContext(ctx)

	requestCh := make(chan RequestItem)
	resultCh := make(chan ResultItem, defaultResultBuffer)

	trackerCtx, trackerCancel := context.WithCancel(ctx)
	trackerDone := make(chan struct{})
	go func() {
		_ = je.tracker.Run(trackerCtx)
		close(trackerDone)
	}()

	g.Go(func() error { return je.dispatcher.Run(ctx, requestCh, resultCh) })

	g.Go(func() error { return je.collector.Drain(resultCh) })

	g.Go(func() error { return je.source.Produce(ctx, requestCh) })

	err := g.Wait()

	trackerCancel()
	<-trackerDone

	return je.tracker.Counts(), err
}
