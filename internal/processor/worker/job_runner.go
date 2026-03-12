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
		attribute.String(uotel.AttrBatchID, jobItem.ID),
		attribute.String(uotel.AttrTenantID, jobItem.TenantID),
	}
	if jobInfo.BatchJob != nil {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrInputFileID, jobInfo.BatchJob.InputFileID))
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

	// If an SLO deadline is set, create a child context that cancels when the deadline fires.
	// This context is passed to executeJob to bound dispatch and trigger expiration handling.
	sloCtx, sloCancel := ctx, func() {}
	if !task.SLO.IsZero() {
		sloCtx, sloCancel = context.WithDeadline(ctx, task.SLO)
	}
	defer sloCancel()

	// event watcher for cancel event
	eventWatcher, err := p.clients.Event.ECConsumerGetChannel(ctx, jobInfo.JobID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to get event watcher")
		span.RecordError(err)
		span.SetStatus(codes.Error, "event watcher failed")
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

	// ingestion: pre-process job
	if err := p.preProcessJob(ctx, jobInfo, &cancelRequested); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "pre-process failed")
		p.handleJobError(ctx, err, jobItem, updater, task, nil, nil)
		return
	}

	// transition to in_progress before executing requests
	if err := updater.UpdatePersistentStatus(ctx, jobItem, openai.BatchStatusInProgress, nil, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to in_progress")
		span.RecordError(err)
		span.SetStatus(codes.Error, "status transition failed")
		if failErr := p.handleFailed(ctx, updater, jobItem, nil); failErr != nil {
			logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
		}
		return
	}

	// execution: execute inference requests
	requestCounts, err := p.executeJob(ctx, sloCtx, updater, jobInfo, &cancelRequested)
	if err != nil {
		switch {
		case errors.Is(err, ErrExpired):
			if expiredErr := p.handleExpired(ctx, updater, jobItem, jobInfo, requestCounts); expiredErr != nil {
				logger.V(logging.ERROR).Error(expiredErr, "Failed to finalize expired job")
				span.RecordError(expiredErr)
				span.SetStatus(codes.Error, "expired finalization failed")
			}
			metrics.RecordJobProcessingDuration(time.Since(jobStart), jobItem.TenantID, metrics.GetSizeBucket(int(requestCounts.Total)))

		case errors.Is(err, ErrCancelled):
			if cancelErr := p.handleCancelled(ctx, updater, jobItem, jobInfo, requestCounts); cancelErr != nil {
				logger.V(logging.ERROR).Error(cancelErr, "Failed to finalize cancelled job")
				span.RecordError(cancelErr)
				span.SetStatus(codes.Error, "cancelled finalization failed")
			}
			if requestCounts != nil {
				metrics.RecordJobProcessingDuration(time.Since(jobStart), jobItem.TenantID, metrics.GetSizeBucket(int(requestCounts.Total)))
			}

		default:
			span.RecordError(err)
			span.SetStatus(codes.Error, "execution failed")
			p.handleJobError(ctx, err, jobItem, updater, task, requestCounts, jobInfo)
		}
		return
	}

	// finalization: upload output, update status to completed
	if err := p.finalizeJob(ctx, updater, jobItem, jobInfo, requestCounts); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to finalize job")
		span.RecordError(err)
		span.SetStatus(codes.Error, "finalize failed")
		// Upload retries already exhausted inside finalizeJob — don't re-attempt upload.
		// Pass requestCounts so they are recorded in the failed status.
		if failErr := p.handleFailed(ctx, updater, jobItem, requestCounts); failErr != nil {
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

// handleJobError routes an error to the appropriate handler (cancel, re-enqueue, or fail).
// requestCounts and jobInfo are non-nil only when the error originates from execution (executeJob).
func (p *Processor) handleJobError(
	ctx context.Context,
	err error,
	jobItem *db.BatchItem,
	updater *StatusUpdater,
	task *db.BatchJobPriority,
	requestCounts *openai.BatchRequestCounts,
	jobInfo *batch_types.JobInfo,
) {
	logger := klog.FromContext(ctx)

	switch {
	case errors.Is(err, ErrCancelled):
		// Ingestion cancel: no output files exist yet
		if cancelErr := p.handleCancelled(ctx, updater, jobItem, nil, nil); cancelErr != nil {
			logger.V(logging.ERROR).Error(cancelErr, "Failed to handle cancelled event")
		}

	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// Parent context was cancelled or deadline exceeded (e.g. pod shutdown).
		// Re-enqueue so another worker can pick it up.
		// Note: SLO expiry returns ErrExpired, which is handled before this function is called.
		if task != nil {
			bgCtx := klog.NewContext(context.Background(), klog.FromContext(ctx))
			if enqErr := p.poller.enqueueOne(bgCtx, task); enqErr != nil {
				logger.V(logging.ERROR).Error(enqErr, "Failed to re-enqueue the job to the queue")
				if failErr := p.handleFailed(bgCtx, updater, jobItem, nil); failErr != nil {
					logger.V(logging.ERROR).Error(failErr, "Failed to mark job as failed after re-enqueue failure")
				}
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonSystemError)
				logger.V(logging.INFO).Info("Re-enqueued the job to the queue")
			}
		}

	default:
		if requestCounts != nil && jobInfo != nil {
			if failErr := p.handleFailedWithPartial(ctx, updater, jobItem, jobInfo, requestCounts); failErr != nil {
				logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event with partial output")
			}
		} else {
			if failErr := p.handleFailed(ctx, updater, jobItem, nil); failErr != nil {
				logger.V(logging.ERROR).Error(failErr, "Failed to handle failed event")
			}
		}
	}
}

// uploadPartialResults uploads whatever output/error files exist locally to shared storage.
// Returns file IDs (empty string if the file was empty or upload failed).
// Errors are logged but not propagated — partial upload is best-effort.
func (p *Processor) uploadPartialResults(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	dbJob *db.BatchItem,
) (outputFileID string, errorFileID string) {
	logger := klog.FromContext(ctx)

	var err error
	outputFileID, err = p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeOutput)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to upload output file (best-effort)")
	}

	errorFileID, err = p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeError)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to upload error file (best-effort)")
	}

	return outputFileID, errorFileID
}

// handleExpired finalizes a job whose SLO deadline fired during execution.
// Partial results are preserved: completed requests remain in the output file,
// and unexecuted requests were already written to the error file as "batch_expired"
// by drainUnprocessedRequests. This function uploads both files and transitions
// the job directly to expired status (in_progress → expired).
func (p *Processor) handleExpired(
	ctx context.Context,
	updater *StatusUpdater,
	dbJob *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Job SLO expired mid-execution, uploading partial results")

	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, dbJob)

	p.cleanupJobArtifacts(ctx, dbJob.ID, dbJob.TenantID)

	if err := updater.UpdateExpiredStatus(ctx, dbJob, requestCounts, outputFileID, errorFileID); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to expired")
		return err
	}

	setRequestCountAttrs(ctx, requestCounts)

	metrics.RecordJobProcessed(metrics.ResultExpired, metrics.ReasonExpiredExecution)
	logger.V(logging.INFO).Info("Job expired handled", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// handleFailedWithPartial finalizes an execution failure by uploading partial results before
// transitioning to failed status. Completed requests are preserved in the output file,
// and unexecuted requests were already drained to the error file as "batch_failed".
func (p *Processor) handleFailedWithPartial(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Job failed mid-execution, uploading partial results")

	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, jobItem)

	p.cleanupJobArtifacts(ctx, jobItem.ID, jobItem.TenantID)

	if err := updater.UpdateFailedStatus(ctx, jobItem, requestCounts, outputFileID, errorFileID); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to failed")
		return err
	}

	setRequestCountAttrs(ctx, requestCounts)

	metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
	logger.V(logging.INFO).Info("Job failed handled with partial output", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// handleFailed finalizes a failed job without partial output upload.
// Used for ingestion failures (no output files), finalization failures (upload retries exhausted),
// and re-enqueue failures (infrastructure-level issue). requestCounts is recorded in DB when non-nil.
func (p *Processor) handleFailed(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	requestCounts *openai.BatchRequestCounts,
) error {
	logger := klog.FromContext(ctx)

	p.cleanupJobArtifacts(ctx, jobItem.ID, jobItem.TenantID)

	if err := updater.UpdateFailedStatus(ctx, jobItem, requestCounts, "", ""); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to failed")
		return err
	}

	setRequestCountAttrs(ctx, requestCounts)

	metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
	logger.V(logging.INFO).Info("Job failed handled")
	return nil
}

func setRequestCountAttrs(ctx context.Context, counts *openai.BatchRequestCounts) {
	if counts == nil {
		return
	}
	uotel.SetAttr(ctx,
		attribute.Int64(uotel.AttrRequestTotal, counts.Total),
		attribute.Int64(uotel.AttrRequestCompleted, counts.Completed),
		attribute.Int64(uotel.AttrRequestFailed, counts.Failed),
	)
}
