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
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	db "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	uotel "github.com/llm-d/llm-d-batch-gateway/internal/util/otel"
)

// panicRecoveryTimeout bounds how long handlePanicRecovery waits for DB
// operations before giving up. This prevents an unreachable database from
// blocking wg.Done() and release() indefinitely, which would leak the
// worker token and eventually deadlock Stop().
// Declared as var (not const) so tests can shorten it.
var panicRecoveryTimeout = time.Minute

// defaultHeartbeatInterval is the fallback heartbeat interval used when
// the config value is zero. Matches ProcessorConfig.HeartbeatInterval default.
const defaultHeartbeatInterval = 5 * time.Minute

// JobRunner holds per-job state and orchestrates the lifecycle of a single
// batch job: ingestion, execution, finalization, and cleanup.
//
// Shared resources (file manager, poller, DB clients) are accessed via the
// processor back-reference.
type JobRunner struct {
	processor *Processor
	updater   *StatusUpdater
	jobItem   *db.BatchItem
	jobInfo   *batch_types.JobInfo
	task      *db.BatchJobPriority

	eventWatcher *db.BatchEventsChan
	userCancelFn context.CancelFunc
	abortFn      context.CancelFunc

	requestCounts *openai.BatchRequestCounts
}

// Run orchestrates the full lifecycle of a single batch job:
// ingestion, execution, finalization, and cleanup.
func (jr *JobRunner) Run(ctx context.Context) {
	p := jr.processor

	// Clean up in-flight entry on exit (first defer = last to run via LIFO),
	// ensuring the entry is removed regardless of how Run terminates.
	defer p.deleteInFlight(context.Background(), jr.jobItem.ID)

	// Restore parent trace context propagated from the apiserver via Redis tags
	if len(jr.jobInfo.TraceContext) > 0 {
		propagator := otel.GetTextMapPropagator()
		ctx = propagator.Extract(ctx, propagation.MapCarrier(jr.jobInfo.TraceContext))
	}

	spanAttrs := []attribute.KeyValue{
		attribute.String(uotel.AttrBatchID, jr.jobItem.ID),
		attribute.String(uotel.AttrTenantID, jr.jobItem.TenantID),
	}
	if jr.jobInfo.BatchJob != nil {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrInputFileID, jr.jobInfo.BatchJob.InputFileID))
	}
	ctx, span := uotel.StartSpan(ctx, "process-batch",
		trace.WithAttributes(spanAttrs...),
	)
	defer span.End()

	logger := logr.FromContextOrDiscard(ctx)

	defer p.wg.Done()
	defer p.release()

	// Declared before the deferred recover so the panic handler can inspect
	// how far execution progressed and attempt partial-result preservation.
	var (
		transitionedToInProgress bool
		requestCounts            *openai.BatchRequestCounts
	)
	// Note: recover() only catches panics on this goroutine. Panics in child
	// goroutines (watchCancel, per-model/per-request goroutines in executeJob)
	// are not caught here and will crash the process.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		stackTrace := debug.Stack()
		logr.FromContextOrDiscard(ctx).Error(fmt.Errorf("panic: %v\n%s", r, stackTrace), "Panic recovered")
		span.RecordError(fmt.Errorf("panic: %v", r))
		span.SetStatus(codes.Error, "panic recovered")

		jr.handlePanicRecovery(ctx, transitionedToInProgress, requestCounts)
	}()

	metrics.IncActiveWorkers()
	defer metrics.DecActiveWorkers()

	jobStart := time.Now()

	// If an SLO deadline is set, create a child context that cancels when the deadline fires.
	// This context is passed to executeJob to bound dispatch and trigger expiration handling.
	sloCtx, sloCancel := ctx, func() {}
	if jr.task != nil && !jr.task.SLO.IsZero() {
		sloCtx, sloCancel = context.WithDeadline(ctx, jr.task.SLO)
	}
	defer sloCancel()

	// event watcher for cancel event
	eventWatcher, err := p.event.ECConsumerGetChannel(ctx, jr.jobInfo.JobID)
	if err != nil {
		logger.Error(err, "Failed to get event watcher")
		span.RecordError(err)
		span.SetStatus(codes.Error, "event watcher failed")
		// Re-enqueue best-effort. Use a detached context because ctx may already be
		// cancelled (e.g. pod shutdown) and we don't want the enqueue call to be short-circuited.
		if jr.task != nil {
			bgCtx, bgSpan := uotel.DetachedContext(ctx, "re-enqueue")
			defer bgSpan.End()
			if enqErr := p.poller.enqueueOne(bgCtx, jr.task); enqErr != nil {
				logger.Error(enqErr, "Failed to re-enqueue the job to the queue")
				if failErr := p.handleFailed(bgCtx, jr.updater, jr.jobItem, nil, jr.jobInfo); failErr != nil {
					logger.Error(failErr, "Failed to mark job as failed after re-enqueue failure")
				}
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonSystemError)
			}
		}
		return
	}
	defer eventWatcher.CloseFn()

	// userCancelCtx is a user-cancel-only signal: it is derived from context.Background so that
	// SIGTERM and SLO expiry do NOT propagate into it. userCancelCtx.Err() != nil exclusively means
	// the user requested cancellation via the API.
	userCancelCtx, userCancelFn := context.WithCancel(context.Background())
	jr.userCancelFn = userCancelFn
	defer userCancelFn()

	// requestAbortCtx is derived from sloCtx so SLO expiry and SIGTERM propagate automatically
	// to all dispatch loops and inference calls. User cancel is wired via AfterFunc so that
	// cancelling userCancelCtx automatically cancels requestAbortCtx without manual dispatch.
	// Fatal I/O errors in executor.go still call requestAbortFn directly.
	requestAbortCtx, requestAbortFn := context.WithCancel(sloCtx)
	context.AfterFunc(userCancelCtx, requestAbortFn)
	jr.abortFn = requestAbortFn
	defer requestAbortFn()

	// watch for cancel event
	jr.eventWatcher = eventWatcher
	go jr.watchCancel(ctx)

	// Start heartbeat: periodically refreshes the in-flight entry so the
	// orphan reconciler knows this job is still being actively processed.
	// On each tick it also checks the DB status — if the reconciler acted
	// (terminal status or reverted to validating), it calls requestAbortFn
	// to stop all in-flight requests. The processor's terminal CAS write
	// will then fail with ErrConflict, and the processor yields.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go p.heartbeat(heartbeatCtx, jr.jobItem.ID, requestAbortFn)

	// ingestion: pre-process job (rejects unregistered-model requests early)
	if err := p.preProcessJob(ctx, sloCtx, userCancelCtx, jr.jobInfo); err != nil {
		// errExpired, errCancelled, and errShutdown are expected terminal states, not system errors.
		if !errors.Is(err, errExpired) && !errors.Is(err, errCancelled) && !errors.Is(err, errShutdown) {
			logger.Error(err, "Pre-processing failed")
			span.RecordError(err)
			span.SetStatus(codes.Error, "pre-process failed")
		}
		jr.handleError(ctx, err)
		// No RecordJobProcessingDuration here: preprocessing is ingestion (parsing, plan
		// building), not inference execution. Recording elapsed time would pollute the
		// processing-duration metric with ingestion overhead.
		return
	}

	// transition to in_progress before executing requests
	if err := jr.updater.UpdatePersistentStatus(ctx, jr.jobItem, openai.BatchStatusInProgress, nil, nil); err != nil {
		logger.Error(err, "Failed to update status to in_progress")
		span.RecordError(err)
		span.SetStatus(codes.Error, "status transition failed")
		if failErr := p.handleFailed(ctx, jr.updater, jr.jobItem, nil, jr.jobInfo); failErr != nil {
			logger.Error(failErr, "Failed to handle failed event")
		}
		return
	}
	transitionedToInProgress = true

	// execution: execute inference requests
	var execErr error
	requestCounts, execErr = jr.executeJob(ctx, sloCtx, userCancelCtx, requestAbortCtx)
	jr.requestCounts = requestCounts
	if execErr != nil {
		// errExpired, errCancelled, and errShutdown are expected terminal states, not system errors.
		if !errors.Is(execErr, errExpired) && !errors.Is(execErr, errCancelled) && !errors.Is(execErr, errShutdown) {
			span.RecordError(execErr)
			span.SetStatus(codes.Error, "execution failed")
		}
		jr.handleError(ctx, execErr)
		// Record processing duration for any job that ran (partially or fully).
		// executeJob always returns non-nil counts alongside its sentinel errors
		// (errExpired, errCancelled, errShutdown, system errors) because partial
		// work was done. The nil guard remains defensive for unexpected future paths.
		if requestCounts != nil {
			metrics.RecordJobProcessingDuration(time.Since(jobStart), metrics.GetSizeBucket(int(requestCounts.Total)))
		}
		return
	}

	// finalization: upload output, update status to completed
	if err := p.finalizeJob(ctx, userCancelCtx, jr.updater, jr.jobItem, jr.jobInfo, requestCounts); err != nil {
		if errors.Is(err, errCancelled) {
			// Cancel arrived during finalization — DB already updated to cancelled.
			// Treat as successful cancellation (same as handleCancelled).
			// Use background context: ctx may be cancelled (SIGTERM) and cleanup is local I/O only.
			p.cleanupJobArtifacts(context.Background(), jr.jobItem.ID, jr.jobItem.TenantID)
			metrics.RecordJobProcessingDuration(time.Since(jobStart), metrics.GetSizeBucket(int(requestCounts.Total)))
			recordE2ELatency(jr.jobInfo, metrics.E2EStatusCancelled)
			metrics.RecordCancellation(metrics.CancelPhaseFinalizing)
			metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
			logger.V(logging.INFO).Info("Job cancelled during finalization")
			return
		}
		logger.Error(err, "Failed to finalize job")
		span.RecordError(err)
		span.SetStatus(codes.Error, "finalize failed")
		if errors.Is(err, errFinalizeFailedOver) {
			// finalizeJob already wrote failed status with file IDs preserved.
			// Calling handleFailed would overwrite file IDs with empty strings.
			p.cleanupJobArtifacts(context.Background(), jr.jobItem.ID, jr.jobItem.TenantID)
			metrics.RecordJobProcessingDuration(time.Since(jobStart), metrics.GetSizeBucket(int(requestCounts.Total)))
			recordE2ELatency(jr.jobInfo, metrics.E2EStatusFailed)
			metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		} else {
			// Pre-upload failure (e.g. finalizing status write) — no file IDs exist yet.
			if failErr := p.handleFailed(ctx, jr.updater, jr.jobItem, requestCounts, jr.jobInfo); failErr != nil {
				logger.Error(failErr, "Failed to handle failed event")
			}
		}
		return
	}

	// cleanup local artifacts (best-effort); use background context since ctx may be cancelled.
	p.cleanupJobArtifacts(context.Background(), jr.jobItem.ID, jr.jobItem.TenantID)
	metrics.RecordJobProcessingDuration(time.Since(jobStart), metrics.GetSizeBucket(int(requestCounts.Total)))
	recordE2ELatency(jr.jobInfo, metrics.E2EStatusCompleted)
	metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
	logger.V(logging.INFO).Info("Job completed successfully")
}

// handlePanicRecovery moves a job to a terminal failed state after a panic in Run.
// It tries to preserve partial results when possible, falling back to a plain failure.
// A secondary recover guard prevents a double-panic from crashing the process.
func (jr *JobRunner) handlePanicRecovery(
	ctx context.Context,
	transitionedToInProgress bool,
	requestCounts *openai.BatchRequestCounts,
) {
	defer func() {
		if r := recover(); r != nil {
			logr.FromContextOrDiscard(ctx).Error(fmt.Errorf("panic in handlePanicRecovery: %v\n%s", r, debug.Stack()),
				"Double panic: recovery handler itself panicked")
		}
	}()

	logger := logr.FromContextOrDiscard(ctx)

	if jr.updater == nil || jr.jobItem == nil {
		logger.Error(fmt.Errorf("updater or jobItem is nil"), "Cannot recover job")
		return
	}

	// Use context.Background() because the original ctx may be cancelled (e.g. pod shutdown)
	// and we must ensure the DB update is not short-circuited.
	// Bound the recovery with a timeout so that an unreachable DB cannot block
	// wg.Done() and release() indefinitely, which would leak the worker token
	// and eventually deadlock Stop().
	bgCtx, bgCancel := context.WithTimeout(context.Background(), panicRecoveryTimeout)
	defer bgCancel()
	bgCtx = logr.NewContext(bgCtx, logger)
	if err := jr.processor.handleFailed(bgCtx, jr.updater, jr.jobItem, requestCounts, jr.jobInfo); err != nil {
		logger.Error(err, "Failed to mark job as failed after panic — job will remain in_progress until startup recovery runs",
			"jobID", jr.jobItem.ID, "tenantID", jr.jobItem.TenantID)
	}
}

// handleError routes an error from preProcessJob or executeJob to the appropriate handler.
// It is the single decision point for all job-level error routing.
func (jr *JobRunner) handleError(ctx context.Context, err error) {
	p := jr.processor
	logger := logr.FromContextOrDiscard(ctx)

	switch {
	case errors.Is(err, errCancelled):
		// User-initiated cancel at any phase. requestCounts is nil for ingestion-phase cancels
		// (preProcessJob returns errCancelled before execution begins) and non-nil for
		// execution-phase cancels (executeJob returns partial counts). handleCancelled
		// uses requestCounts == nil as the signal to skip partial-output upload.
		if cancelErr := jr.handleCancelled(ctx); cancelErr != nil {
			logger.Error(cancelErr, "Failed to handle cancelled event")
			if errors.Is(cancelErr, errFinalizeFailedOver) {
				recordE2ELatency(jr.jobInfo, metrics.E2EStatusFailed)
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			}
		}

	case errors.Is(err, errExpired):
		// SLO deadline reached. requestCounts may be nil if SLO expired during preprocessing
		// (before executeJob ran). handleExpired and UpdateExpiredStatus both tolerate nil
		// counts — the DB field is left at zero, which is correct since no requests were processed.
		if expiredErr := p.handleExpired(ctx, jr.updater, jr.jobItem, jr.jobInfo, jr.requestCounts); expiredErr != nil {
			logger.Error(expiredErr, "Failed to finalize expired job")
		}

	case errors.Is(err, errShutdown):
		// SIGTERM received — leave the job in its current state for the orphan
		// reconciler to handle. The reconciler detects non-terminal jobs that
		// are not in the queue and have a stale (or missing) in-flight entry,
		// then transitions them to a terminal state (failed, cancelled, etc.).
		//
		// The in-flight entry is cleaned up by the defer at the top of Run.
		// If the process is killed (SIGKILL) before that defer runs, the entry
		// remains and becomes stale, which the reconciler also handles.
		logger.V(logging.INFO).Info("SIGTERM received, leaving job for orphan reconciler")

	default:
		if failErr := p.handleFailed(ctx, jr.updater, jr.jobItem, jr.requestCounts, jr.jobInfo); failErr != nil {
			logger.Error(failErr, "Failed to handle failed event")
		}
	}
}

// --- Methods that remain on Processor (used from both Processor and JobRunner) ---

// uploadPartialResults uploads whatever output/error files exist locally to shared storage.
// Returns file IDs (empty string if the file was empty or upload failed).
// Errors are logged but not propagated — partial upload is best-effort.
// The two uploads are independent and run concurrently.
func (p *Processor) uploadPartialResults(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	dbJob *db.BatchItem,
) (outputFileID string, errorFileID string) {
	logger := logr.FromContextOrDiscard(ctx)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		var err error
		outputFileID, err = p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeOutput)
		if err != nil {
			logger.Error(err, "Failed to upload output file (best-effort)")
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		errorFileID, err = p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeError)
		if err != nil {
			logger.Error(err, "Failed to upload error file (best-effort)")
		}
	}()

	wg.Wait()
	return outputFileID, errorFileID
}

// handleExpired finalizes a job whose SLO deadline fired.
// Three cases reach here:
// (1) deadline expired during ingestion (preProcessJob) — no output files exist yet;
//
//	uploadPartialResults uploads nothing, requestCounts is nil, DB counts remain zero.
//
// (2) deadline expired before dispatch began — executeJob skips dispatch:
//
//	no completions are written to the output file, but error.jsonl may already contain
//	model_not_found lines from ingestion. uploadPartialResults uploads whatever exists.
//
// (3) deadline expired during execution — completed requests remain in the output file
//
//	and undispatched entries were drained as "batch_expired" by drainUnprocessedRequests.
//
// In all cases, this function uploads whatever files exist and transitions the job to expired status.
// Uses a detached context so that a concurrent SIGTERM cannot abort the upload or DB write.
func (p *Processor) handleExpired(
	ctx context.Context,
	updater *StatusUpdater,
	dbJob *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
) error {
	ioCtx, ioSpan := uotel.DetachedContext(ctx, "handle-expired")
	ioCtx, ioCancel := context.WithTimeout(ioCtx, finalizationTimeout)
	defer ioCancel()
	defer ioSpan.End()

	logger := logr.FromContextOrDiscard(ctx)
	logger.V(logging.INFO).Info("Job SLO expired, finalizing as expired")

	outputFileID, errorFileID := p.uploadPartialResults(ioCtx, jobInfo, dbJob)

	if err := updater.UpdateExpiredStatus(ioCtx, dbJob, requestCounts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Failed to update status to expired")
		return err
	}

	// Cleanup after terminal status write: if the write failed above, the local
	// files survive for startup recovery to re-upload.
	p.cleanupJobArtifacts(ioCtx, dbJob.ID, dbJob.TenantID)

	setRequestCountAttrs(ctx, requestCounts)

	recordE2ELatency(jobInfo, metrics.E2EStatusExpired)
	metrics.RecordJobProcessed(metrics.ResultExpired, metrics.ReasonExpiredExecution)
	logger.V(logging.INFO).Info("Job expired handled", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// handleFailed finalizes a failed job by optionally uploading partial results before
// transitioning to failed status. When jobInfo is non-nil and partial output exists on
// disk, the output and error files are uploaded so the user can retrieve whatever
// completed before the failure. When jobInfo is nil (e.g. ingestion failure, malformed
// job), only the DB status transition is performed.
//
// Records E2E latency as failed when jobInfo is available (nil-safe).
// Uses a detached context so that a concurrent SIGTERM cannot abort the upload or DB write.
// If the caller already has a tighter deadline (e.g. panic recovery), that deadline is respected.
func (p *Processor) handleFailed(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	requestCounts *openai.BatchRequestCounts,
	jobInfo *batch_types.JobInfo,
) error {
	timeout := finalizationTimeout
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	ioCtx, ioSpan := uotel.DetachedContext(ctx, "handle-failed")
	ioCtx, ioCancel := context.WithTimeout(ioCtx, timeout)
	defer ioCancel()
	defer ioSpan.End()

	logger := logr.FromContextOrDiscard(ctx)

	var outputFileID, errorFileID string
	if jobInfo != nil {
		outputFileID, errorFileID = p.uploadPartialResults(ioCtx, jobInfo, jobItem)
	}

	if err := updater.UpdateFailedStatus(ioCtx, jobItem, requestCounts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Failed to update status to failed")
		return err
	}

	p.cleanupJobArtifacts(ioCtx, jobItem.ID, jobItem.TenantID)

	setRequestCountAttrs(ctx, requestCounts)

	recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
	metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
	logger.V(logging.INFO).Info("Job failed handled", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// recordE2ELatency records the full lifecycle duration from batch submission to terminal state.
// No-op if jobInfo, BatchJob, or CreatedAt is missing (e.g. DB conversion failure).
func recordE2ELatency(jobInfo *batch_types.JobInfo, status string) {
	if jobInfo == nil || jobInfo.BatchJob == nil || jobInfo.BatchJob.CreatedAt == 0 {
		return
	}
	createdAt := time.Unix(jobInfo.BatchJob.CreatedAt, 0)
	metrics.RecordJobE2ELatency(time.Since(createdAt), status)
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
