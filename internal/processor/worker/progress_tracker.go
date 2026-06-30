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

package worker

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

// ProgressTracker tracks per-request completion counts and pushes throttled
// updates to Redis. It runs as a single goroutine — no atomics or locks needed.
type ProgressTracker struct {
	total     int64
	completed int64
	failed    int64
	updater   *StatusUpdater
	jobID     string
	interval  time.Duration

	ch   chan ProgressEvent
	done chan struct{}
}

// NewProgressTracker creates a ProgressTracker with the given initial counts.
func NewProgressTracker(updater *StatusUpdater, jobID string, total, initialFailed int64) *ProgressTracker {
	return &ProgressTracker{
		total:    total,
		failed:   initialFailed,
		updater:  updater,
		jobID:    jobID,
		interval: progressUpdateInterval,
		ch:       make(chan ProgressEvent, 1024),
		done:     make(chan struct{}),
	}
}

// Start launches the tracker goroutine. Returns the channel to send events to.
// Call Stop to shut down.
func (pt *ProgressTracker) Start(ctx context.Context) chan<- ProgressEvent {
	go func() {
		pt.run(ctx)
		close(pt.done)
	}()
	return pt.ch
}

// Stop closes the event channel and waits for the tracker goroutine to finish
// its final push.
func (pt *ProgressTracker) Stop() {
	close(pt.ch)
	<-pt.done
}

// run reads progress events from the internal channel and pushes throttled
// updates to Redis on each ticker tick. Performs a final push when the channel
// is closed or ctx is cancelled.
func (pt *ProgressTracker) run(ctx context.Context) {
	ticker := time.NewTicker(pt.interval)
	defer ticker.Stop()
	dirty := false

	for {
		select {
		case ev, ok := <-pt.ch:
			if !ok {
				pt.push(ctx)
				return
			}
			if ev.Success {
				pt.completed++
			} else {
				pt.failed++
			}
			dirty = true

		case <-ticker.C:
			if dirty {
				pt.push(ctx)
				dirty = false
			}

		case <-ctx.Done():
			// Drain any buffered events before the final push so counts
			// are accurate even when the context is cancelled concurrently
			// with the last batch of sends.
			for {
				select {
				case ev, ok := <-pt.ch:
					if !ok {
						pt.push(context.Background())
						return
					}
					if ev.Success {
						pt.completed++
					} else {
						pt.failed++
					}
				default:
					pt.push(context.Background())
					return
				}
			}
		}
	}
}

// Counts returns the current request counts. Safe to call after Run returns.
func (pt *ProgressTracker) Counts() *openai.BatchRequestCounts {
	return &openai.BatchRequestCounts{
		Total:     pt.total,
		Completed: pt.completed,
		Failed:    pt.failed,
	}
}

// AddFailed adds n to the failed counter. Used after the pipeline shuts down
// to account for undispatched requests drained outside the channel pipeline.
func (pt *ProgressTracker) AddFailed(n int64) {
	pt.failed += n
}

func (pt *ProgressTracker) push(ctx context.Context) {
	if err := pt.updater.UpdateProgressCounts(ctx, pt.jobID, &openai.BatchRequestCounts{
		Total:     pt.total,
		Completed: pt.completed,
		Failed:    pt.failed,
	}); err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "Failed to update progress counts (best-effort)")
	}
}
