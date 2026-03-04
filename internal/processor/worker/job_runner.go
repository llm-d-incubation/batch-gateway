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
	"time"

	"k8s.io/klog/v2"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
)

func (p *Processor) runJob(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	task *db.BatchJobPriority,
) {
	// Restore parent trace context propagated from the apiserver via Redis tags
	if len(jobInfo.TraceContext) > 0 {
		propagator := otel.GetTextMapPropagator()
		ctx = propagator.Extract(ctx, propagation.MapCarrier(jobInfo.TraceContext))
	}

	spanAttrs := []attribute.KeyValue{
		attribute.String(uotel.AttrJobID, jobItem.ID),
		attribute.String(uotel.AttrTenantID, jobItem.TenantID),
	}
	if jobInfo.BatchJob != nil {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrFileID, jobInfo.BatchJob.InputFileID))
	}
	ctx, span := uotel.StartSpan(ctx, "process-batch",
		trace.WithAttributes(spanAttrs...),
	)
	defer span.End()

	// this logger includes job ID in the context
	logger := klog.FromContext(ctx)

	defer p.wg.Done()
	defer p.release()
	defer func() {
		if r := recover(); r != nil {
			recoverErr := fmt.Errorf("%v", r)
			klog.FromContext(ctx).Error(recoverErr, "Panic recovered")
			span.RecordError(recoverErr)
			span.SetStatus(codes.Error, "panic recovered")
		}
	}()

	metrics.IncActiveWorkers()
	defer metrics.DecActiveWorkers()

	jobStart := time.Now()

	// event watcher for cancel event
	eventWatcher, err := p.clients.Event.ECConsumerGetChannel(ctx, jobInfo.JobID)
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "pre-process failed")
		p.handleJobError(ctx, err, jobItem, updater, task)
		return
	}

	// transition to in_progress before executing requests
	if err := updater.UpdatePersistentStatus(ctx, jobItem, openai.BatchStatusInProgress, nil, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to in_progress")
		span.RecordError(err)
		span.SetStatus(codes.Error, "status transition failed")
		if failErr := p.handleFailed(ctx, jobItem, updater); failErr != nil {
			logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
		}
		return
	}

	// phase 2: execute inference requests
	requestCounts, err := p.executeJob(ctx, updater, jobInfo, &cancelRequested)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "execution failed")
		p.handleJobError(ctx, err, jobItem, updater, task)
		return
	}

	// phase 3: finalize (upload output, update status to completed)
	if err := p.finalizeJob(ctx, updater, jobItem, jobInfo, requestCounts); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to finalize job")
		span.RecordError(err)
		span.SetStatus(codes.Error, "finalize failed")
		if failErr := p.handleFailed(ctx, jobItem, updater); failErr != nil {
			logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
		}
		return
	}

	// cleanup local artifacts (best-effort)
	p.cleanupJobArtifacts(ctx, jobItem.ID, jobItem.TenantID)
	metrics.RecordJobProcessingDuration(time.Since(jobStart), jobItem.TenantID, metrics.GetSizeBucket(int(requestCounts.Total)))
	metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
	logger.V(logging.INFO).Info("Job completed successfully")
}

// handleJobError routes a phase error to the appropriate handler (cancel, re-enqueue, or fail).
func (p *Processor) handleJobError(
	ctx context.Context,
	err error,
	jobItem *db.BatchItem,
	updater *StatusUpdater,
	task *db.BatchJobPriority,
) {
	logger := klog.FromContext(ctx)

	switch {
	case errors.Is(err, ErrCancelled):
		if cancelErr := p.handleCancelled(ctx, jobItem, updater); cancelErr != nil {
			logger.V(logging.ERROR).Error(cancelErr, "Failed to handle cancelled event")
		}

	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// context canceled or deadline exceeded
		// re-enqueue the job to the queue so this job can be picked up later by another worker
		if task != nil {
			bgCtx := klog.NewContext(context.Background(), klog.FromContext(ctx))
			if enqErr := p.poller.enqueueOne(bgCtx, task); enqErr != nil {
				// re-enqueue failed, handle as failed
				logger.V(logging.ERROR).Error(enqErr, "Failed to re-enqueue the job to the queue")
				if failErr := p.handleFailed(bgCtx, jobItem, updater); failErr != nil {
					// best-effort
					logger.V(logging.ERROR).Error(failErr, "Failed to mark job as failed after re-enqueue failure")
				}
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonSystemError)
				logger.V(logging.INFO).Info("Re-enqueued the job to the queue")
			}
		}
	default:
		if failErr := p.handleFailed(ctx, jobItem, updater); failErr != nil {
			logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
		}
	}
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
