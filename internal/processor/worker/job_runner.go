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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

func (p *Processor) runJob(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	task *db.BatchJobPriority,
) {
	// this logger includes job ID in the context
	logger := klog.FromContext(ctx)

	defer p.wg.Done()
	defer p.release()
	defer func() {
		if r := recover(); r != nil {
			recoverErr := fmt.Errorf("%v", r)
			klog.FromContext(ctx).Error(recoverErr, "Panic recovered")
		}
	}()

	metrics.IncActiveWorkers()
	defer metrics.DecActiveWorkers()

	// event watcher for cancel event
	eventWatcher, err := p.clients.event.ECConsumerGetChannel(ctx, jobInfo.JobID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to get event watcher")
		// re-enqueue the job to the queue so this job can be picked up later by another worker
		// best-effort
		if task != nil {
			bgCtx := klog.NewContext(context.Background(), klog.FromContext(ctx))
			if enqErr := p.poller.enqueueOne(bgCtx, task); enqErr != nil {
				logger.V(logging.ERROR).Error(enqErr, "Failed to re-enqueue the job to the queue")
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonSystemError)
			}
		}
		return
	}
	defer eventWatcher.CloseFn()

	// cancel requested flag and cancelling once
	var cancelRequested atomic.Bool
	var cancellingOnce sync.Once

	// watch for cancel event
	go p.watchCancel(ctx, eventWatcher, updater, jobItem, &cancelRequested, &cancellingOnce)

	// phase 1: pre-process job
	if err := p.preProcessJob(ctx, jobInfo, &cancelRequested); err != nil {
		switch {
		case errors.Is(err, ErrCancelled):
			// user initiated cancellation
			if cancelErr := p.handleCancelled(ctx, jobItem, updater); cancelErr != nil {
				logger.V(logging.ERROR).Error(cancelErr, "Failed to handle cancelled event")
			}
			return

		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			// processor shutdown / job context cancelled
			// re-enqueue the job to the queue so this job can be picked up later by another worker
			// use background context to avoid context cancellation error while preserving logger values.
			if task != nil {
				bgCtx := klog.NewContext(context.Background(), klog.FromContext(ctx))
				if enqErr := p.poller.enqueueOne(bgCtx, task); enqErr != nil {
					logger.V(logging.ERROR).Error(enqErr, "Failed to re-enqueue the job to the queue")
				} else {
					logger.V(logging.INFO).Info("Re-enqueued the job to the queue")
				}
			}
			return

		default:
			// treat as failed job
			if failErr := p.handleFailed(ctx, jobItem, updater); failErr != nil {
				logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
			}
			return
		}
	}

	// TODO:phase 2: process plans and execute requests
}

func (p *Processor) handleFailed(
	ctx context.Context,
	jobItem *db.BatchItem,
	updater *StatusUpdater,
) error {
	logger := klog.FromContext(ctx)

	// cleanup local artifacts (best-effort)
	p.cleanupJobArtifacts(ctx, jobItem.ID, jobItem.TenantID)

	// update persistent status -> failed
	if err := updater.UpdatePersistentStatus(ctx, jobItem, openai.BatchStatusFailed, nil, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to failed")
		return err
	}

	metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
	logger.V(logging.INFO).Info("Job failed handled")
	return nil
}
