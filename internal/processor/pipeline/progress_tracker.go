package pipeline

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

var progressUpdateInterval = time.Second

// ProgressUpdater pushes progress counts to a status store.
type ProgressUpdater interface {
	UpdateProgressCounts(ctx context.Context, jobID string, counts *openai.BatchRequestCounts) error
}

// ProgressTracker tracks request completion counts and pushes throttled
// updates to the status store.
type ProgressTracker struct {
	total     int64
	completed atomic.Int64
	failed    atomic.Int64
	dirty     atomic.Bool
	updater   ProgressUpdater
	jobID     string
	interval  time.Duration
	logger    logr.Logger
}

func NewProgressTracker(total int64, updater ProgressUpdater, jobID string, logger logr.Logger) *ProgressTracker {
	return &ProgressTracker{
		total:   total,
		updater: updater,
		jobID:   jobID,
		logger:  logger,
	}
}

// RecordSuccess records a successful result. Non-blocking.
func (pt *ProgressTracker) RecordSuccess(msg ResultItem) {
	pt.completed.Add(1)
	pt.dirty.Store(true)
}

// RecordFailure records a failed request. Non-blocking.
func (pt *ProgressTracker) RecordFailure(err error) {
	pt.failed.Add(1)
	pt.dirty.Store(true)
}

// Run starts the ticker that pushes throttled updates to the status store.
// Returns when ctx is cancelled, after pushing final counts.
func (pt *ProgressTracker) Run(ctx context.Context) error {
	interval := pt.interval
	if interval <= 0 {
		interval = progressUpdateInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Use a detached context for the final push so that a cancelled
			// ctx doesn't prevent the last progress update from reaching Redis.
			pt.push(context.Background())
			return nil
		case <-ticker.C:
			if pt.dirty.CompareAndSwap(true, false) {
				pt.push(ctx)
			}
		}
	}
}

// Counts returns the current request counts. Safe to call after Run returns.
func (pt *ProgressTracker) Counts() *openai.BatchRequestCounts {
	return &openai.BatchRequestCounts{
		Total:     pt.total,
		Completed: pt.completed.Load(),
		Failed:    pt.failed.Load(),
	}
}

// AddFailed adds to the failed counter. Called after Run returns for
// undispatched request draining.
func (pt *ProgressTracker) AddFailed(n int64) {
	pt.failed.Add(n)
}

func (pt *ProgressTracker) push(ctx context.Context) {
	if pt.updater == nil {
		return
	}
	if err := pt.updater.UpdateProgressCounts(ctx, pt.jobID, pt.Counts()); err != nil {
		pt.logger.Error(err, "Failed to update progress counts (best-effort)")
	}
}
