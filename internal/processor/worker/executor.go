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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	httpclient "github.com/llm-d/llm-d-batch-gateway/pkg/clients/http"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// outputLine represents a single line in the output JSONL file following the OpenAI batch output format.
type outputLine struct {
	ID       string                    `json:"id"`
	CustomID string                    `json:"custom_id"`
	Response *batch_types.ResponseData `json:"response"`
	Error    *outputError              `json:"error"`

	// hadCapacityRetry is true when at least one retry was caused by a
	// capacity-related response (429/5xx). Network-error retries do not
	// set this flag. Used for AIMD signaling.
	hadCapacityRetry bool `json:"-"`
}

type outputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// isSuccess returns true when the output line represents a fully successful request
// (no non-HTTP error and a 200 HTTP status). HTTP error responses (4xx/5xx) are not
// considered successful even though they populate the Response field.
//
// NOTE: because HTTP errors are written to the output file (not the error file),
// request_counts.failed may be greater than the number of lines in the error file.
// This diverges from OpenAI's documented behavior but aligns with the OpenAPI schema
// (see executeOneRequest for rationale).
func (o *outputLine) isSuccess() bool {
	return o.Error == nil && o.Response != nil && o.Response.StatusCode == 200
}

// progressUpdateInterval is the minimum time between Redis progress updates.
// Updates within this window are skipped — the next update after the interval
// will include all accumulated progress. Declared as var so tests can override.
var progressUpdateInterval = time.Second

// executionProgress tracks per-request progress across goroutines
// and pushes throttled updates to the status store.
type executionProgress struct {
	completed  atomic.Int64
	failed     atomic.Int64
	total      int64
	updater    *StatusUpdater
	jobID      string
	lastUpdate atomic.Int64 // unix nanoseconds of last Redis push
}

// record increments the appropriate counter and pushes a throttled progress
// update to Redis. Updates are skipped if less than progressUpdateInterval
// has elapsed since the last push, reducing Redis writes from O(requests)
// to O(job_duration / interval).
func (ep *executionProgress) record(ctx context.Context, success bool) {
	if success {
		ep.completed.Add(1)
	} else {
		ep.failed.Add(1)
	}
	now := time.Now().UnixNano()
	last := ep.lastUpdate.Load()
	if now-last < int64(progressUpdateInterval) {
		return
	}
	// Best-effort CAS: if another goroutine raced us, skip this update.
	if !ep.lastUpdate.CompareAndSwap(last, now) {
		return
	}
	ep.push(ctx)
}

// flush pushes the final progress to Redis unconditionally, ensuring the
// last update reflects the true counts regardless of throttling.
func (ep *executionProgress) flush(ctx context.Context) {
	ep.push(ctx)
}

func (ep *executionProgress) push(ctx context.Context) {
	if err := ep.updater.UpdateProgressCounts(ctx, ep.jobID, &openai.BatchRequestCounts{
		Total:     ep.total,
		Completed: ep.completed.Load(),
		Failed:    ep.failed.Load(),
	}); err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "Failed to update progress counts (best-effort)")
	}
}

func (ep *executionProgress) counts() *openai.BatchRequestCounts {
	return &openai.BatchRequestCounts{
		Total:     ep.total,
		Completed: ep.completed.Load(),
		Failed:    ep.failed.Load(),
	}
}

// executeJob performs execution: reads plan files per model, sends inference
// requests concurrently (one goroutine per model, multiple requests per model), and writes results to
// output.jsonl (successes) and error.jsonl (failures). Returns request counts for finalization.
//
// On success, returns (counts, nil). On interruption or error, output and error writers are
// always flushed (buffered data written to the underlying files) before returning, and partial
// counts are returned alongside the sentinel/cause error:
//   - SLO expired:    (counts, errExpired)   — undispatched drained as batch_expired
//   - User cancel:    (counts, errCancelled) — undispatched drained as batch_cancelled
//   - System error:   (counts, firstErr)     — undispatched drained as batch_failed
//   - Pod shutdown:   (counts, errShutdown)  — job left for orphan reconciler; counts reflect
//     work done before SIGTERM
//
// requestAbortCtx controls the dispatch loop and all in-flight inference calls: cancelling it
// stops dispatch and aborts in-flight requests. It is derived from sloCtx in runJob, so SLO
// expiry and SIGTERM propagate automatically. User cancel also triggers requestAbortFn via
// context.AfterFunc(userCancelCtx, requestAbortFn) wired in runJob — watchCancel itself only
// calls userCancelFn.
// userCancelCtx is a user-cancel-only signal derived from context.Background; it does not inherit
// SLO expiry or SIGTERM. Its sole purpose is to let the drain phase distinguish user cancel from
// SLO expiry.
func (p *Processor) executeJob(ctx, sloCtx, userCancelCtx, requestAbortCtx context.Context, params *jobExecutionParams) (*openai.BatchRequestCounts, error) {
	logger := logr.FromContextOrDiscard(ctx)
	logger.V(logging.INFO).Info("Starting execution: executing job")

	jobRootDir, err := p.jobRootDir(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve job root directory: %w", err)
	}

	modelMap, err := readModelMap(jobRootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read model map: %w", err)
	}

	// Early SLO check: if the deadline already fired before execution begins (e.g. SLO expired
	// during ingestion), skip dispatch entirely. No output file is written since no requests
	// were dispatched, but error.jsonl may already contain model_not_found entries from
	// ingestion. handleExpired will upload whatever files exist.
	if sloCtx.Err() == context.DeadlineExceeded {
		logger.V(logging.INFO).Info("SLO already expired at execution start, skipping dispatch",
			"total", modelMap.LineCount)
		return &openai.BatchRequestCounts{Total: modelMap.LineCount, Failed: modelMap.RejectedCount}, errExpired
	}

	inputFilePath, err := p.jobInputFilePath(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}
	defer inputFile.Close()

	outputFilePath, err := p.jobOutputFilePath(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	outputFile, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	errorFilePath, err := p.jobErrorFilePath(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	// Append mode: ingestion may have already written model_not_found errors.
	errorFile, err := os.OpenFile(errorFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create error file: %w", err)
	}
	defer errorFile.Close()

	plansDir, err := p.jobPlansDir(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}

	progress := &executionProgress{
		total:   modelMap.LineCount,
		updater: params.updater,
		jobID:   params.jobInfo.JobID,
	}
	// Seed with requests already rejected during ingestion (model not found).
	progress.failed.Store(modelMap.RejectedCount)

	abortFn := params.requestAbortFn
	// This only happens in tests.
	if abortFn == nil {
		abortFn = func() {}
	}
	collector := newResultCollector(
		bufio.NewWriterSize(outputFile, 1024*1024),
		bufio.NewWriterSize(errorFile, 1024*1024),
		progress,
		logger,
		abortFn,
	)
	collector.start(ctx)

	passThroughHeaders := params.jobInfo.PassThroughHeaders
	if len(passThroughHeaders) > 0 {
		headerNames := make([]string, 0, len(passThroughHeaders))
		for k := range passThroughHeaders {
			headerNames = append(headerNames, k)
		}
		logger.V(logging.DEBUG).Info("pass-through headers attached to job", "headerNames", headerNames)
	}

	tenantID := params.jobInfo.TenantID

	// Error handler: iterates over all model results, aborts siblings on first
	// error, then determines the final outcome by checking context state.
	// SIGTERM is NOT checked: output is already flushed to disk, so the caller
	// should proceed to finalizeJob rather than re-enqueueing a complete job.
	errCh := make(chan error, len(modelMap.SafeToModel))
	resultCh := make(chan error, 1)
	go func() {
		var firstErr error
		for range modelMap.SafeToModel {
			if err := <-errCh; err != nil && firstErr == nil {
				firstErr = err
				abortFn()
			}
		}
		// All model goroutines have finished — safe to signal executeJob to flush.
		if firstErr != nil {
			resultCh <- firstErr
			return
		}
		switch {
		case errors.Is(sloCtx.Err(), context.DeadlineExceeded):
			resultCh <- errExpired
		case userCancelCtx.Err() != nil:
			resultCh <- errCancelled
		default:
			resultCh <- nil
		}
	}()

	// Start processing each model.
	for safeModelID, modelID := range modelMap.SafeToModel {
		go func() {
			var err error
			if p.asyncInference != nil {
				err = p.processModelAsync(
					requestAbortCtx,
					ctx,
					sloCtx,
					userCancelCtx,
					inputFile,
					plansDir, safeModelID, modelID,
					collector,
					passThroughHeaders,
					tenantID,
				)
			} else {
				err = p.processModel(
					requestAbortCtx,
					ctx,
					sloCtx,
					userCancelCtx,
					inputFile,
					plansDir, safeModelID, modelID,
					collector,
					passThroughHeaders,
					tenantID,
				)
			}
			errCh <- err
		}()
	}

	// Wait on the result, flush, collect counts, log and exit.
	resultErr := <-resultCh

	if flushErr := collector.flush(); resultErr == nil {
		resultErr = flushErr
	}
	progress.flush(ctx)
	counts := progress.counts()

	var msg string
	logVals := []any{"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed}
	if resultErr != nil {
		msg = "Execution finished"
		logVals = append([]any{"error", resultErr}, logVals...)
	} else {
		msg = "Execution completed"
	}

	logger.V(logging.INFO).Info(msg, logVals...)
	return counts, resultErr
}

// processModel processes all plan entries for a single model concurrently.
// Concurrency is bounded by both a global semaphore (p.globalSem, shared across
// all models/workers) and a per-endpoint adaptive semaphore controlled by AIMD.
// Models sharing the same inference endpoint share one AIMD controller.
//
// Semaphore acquisition order: endpoint-local before global (shared).
// This prevents starving other endpoints — blocking on global only wastes a local slot.
//
// Error strategy in this function: when a goroutine encounters a fatal error, modelErr is captured
// via errOnce but the context is NOT cancelled within this function. Context cancellation is
// propagated at the executeJob level (requestAbortFn), which stops dispatch across all models.
// Already-dispatched goroutines may finish with errors or cancellation rather than successful
// completion, depending on when requestAbortFn fires.
func (p *Processor) processModel(
	requestAbortCtx context.Context,
	mainCtx context.Context,
	sloCtx context.Context,
	userCancelCtx context.Context,
	inputFile *os.File,
	plansDir, safeModelID, modelID string,
	collector *resultCollector,
	passThroughHeaders map[string]string,
	tenantID string,
) error {
	logger := logr.FromContextOrDiscard(requestAbortCtx).WithValues("model", modelID)
	requestAbortCtx = logr.NewContext(requestAbortCtx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("model setup failed: read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for a model", "numEntries", len(entries))

	// Resolve the per-endpoint adaptive semaphore and AIMD controller for this
	// model. Models sharing the same inference endpoint share the same pair.
	// ClientFor can return nil after gateway config changes between ingestion and
	// execution, or during recovery when model_map/plan files predate the current
	// resolver. In that case, drain all entries as model_not_found.
	client := p.inference.ClientFor(modelID)
	epLimit := p.endpointLimits[client]
	if epLimit == nil {
		logger.V(logging.INFO).Info("No endpoint limit for model (client not in resolver), draining as model_not_found")
		p.drainUnprocessedRequests(requestAbortCtx, inputFile, entries, collector,
			batch_types.BatchErrorCode(inference.ErrCodeModelNotFound))
		return nil
	}
	endpointSem := epLimit.sem

	var (
		wg                sync.WaitGroup
		errOnce           sync.Once
		modelErr          error
		dispatchedCount   int
		shutdownCancelled atomic.Int32
	)

dispatch:
	for i, entry := range entries {
		if requestAbortCtx.Err() != nil {
			// Context cancelled (SLO expiry, SIGTERM, or user cancel via requestAbortFn).
			// Do not set modelErr here; the drain switch determines the correct sentinel
			// by inspecting sloCtx, userCancelCtx, and requestAbortCtx independently.
			break
		}

		// Acquire semaphores in order: endpoint-local before global (shared).
		// This order prevents starving other endpoints — blocking on global only wastes a local slot.
		if err := endpointSem.Acquire(requestAbortCtx); err != nil {
			break dispatch
		}

		if err := p.globalSem.Acquire(requestAbortCtx); err != nil {
			endpointSem.Release()
			break dispatch
		}

		dispatchedCount = i + 1
		wg.Add(1)
		go func(entry planEntry) {
			defer wg.Done()
			defer endpointSem.Release()
			defer p.globalSem.Release()

			result, execErr := p.executeOneRequest(requestAbortCtx, sloCtx, inputFile, entry, modelID, passThroughHeaders, tenantID)

			// AIMD signal: adjust concurrency based on inference endpoint capacity.
			//
			// Signal semantics:
			//   429          → RecordRateLimit (sustained overload after all retries)
			//   5xx          → RecordRateLimit (server overload / unhealthy)
			//   200 with capacity retries → RecordRateLimit (retry absorbed 429/5xx)
			//   200 with network-only retries → RecordSuccess (no capacity signal)
			//   200 clean    → RecordSuccess (genuine available capacity)
			//   4xx (not 429) → RecordSuccess (gateway had capacity, request was malformed)
			//   non-HTTP err → skip (no capacity signal — network, timeout, etc.)
			//   fatal execErr → skip (local I/O, not related to gateway capacity)
			//
			// AIMD only affects future dispatch. It does not abort in-flight
			// requests — those continue until completion or context cancellation.
			if epLimit.aimd != nil && execErr == nil && result != nil && result.Response != nil {
				sc := result.Response.StatusCode
				switch {
				case sc == http.StatusTooManyRequests:
					epLimit.aimd.RecordRateLimit(metrics.AIMDSignal429)
					metrics.RecordAIMDDecrease(epLimit.label, metrics.AIMDSignal429)
				case sc >= http.StatusInternalServerError:
					epLimit.aimd.RecordRateLimit(metrics.AIMDSignal5xx)
					metrics.RecordAIMDDecrease(epLimit.label, metrics.AIMDSignal5xx)
				case result.hadCapacityRetry:
					epLimit.aimd.RecordRateLimit(metrics.AIMDSignalCapacityRetry)
					metrics.RecordAIMDDecrease(epLimit.label, metrics.AIMDSignalCapacityRetry)
				default:
					oldLimit := epLimit.aimd.Limit()
					epLimit.aimd.RecordSuccess()
					if epLimit.aimd.Limit() != oldLimit {
						metrics.RecordAIMDIncrease(epLimit.label)
					}
				}
				metrics.SetAIMDConcurrencyLimit(epLimit.label, float64(epLimit.aimd.Limit()))
			}
			if execErr != nil {
				// Fatal read failure: the input file is unreadable at this offset
				// (e.g. disk corruption). We do not know the CustomID, so we cannot
				// write a batch_failed entry to the error file. This means
				// completed + failed < total for this job, but the job status is
				// set to failed, which already signals that output files are incomplete.
				logger.Error(execErr, "Fatal error executing request", "offset", entry.Offset)
				errOnce.Do(func() { modelErr = execErr })
				return
			}

			// If user-initiated cancel arrived while this request was in-flight,
			// overwrite the result as batch_cancelled and send to the collector.
			// SLO expiry does not overwrite in-flight results — only user cancel does.
			if sloCtx.Err() == nil && userCancelCtx.Err() != nil {
				collector.collect(&ResultItem{
					RequestID: result.ID,
					CustomID:  result.CustomID,
					Error: &OutputError{
						Code:    string(batch_types.ErrCodeBatchCancelled),
						Message: "This request was cancelled while in progress.",
					},
				})
				return
			}

			if result.Error != nil && mainCtx.Err() != nil {
				shutdownCancelled.Add(1)
			}

			collector.collect(outputLineToResultItem(result))
		}(entry)
	}

	wg.Wait()

	return p.drainAndFinalize(requestAbortCtx, mainCtx, sloCtx, userCancelCtx,
		inputFile, entries[dispatchedCount:], collector, modelErr, logger, len(entries),
		shutdownCancelled.Load())
}

// processModelAsync processes all plan entries for a single model using the
// async submit/collect pattern. All requests are submitted to the queue first,
// then results are collected as they arrive on a shared channel.
func (p *Processor) processModelAsync(
	requestAbortCtx context.Context,
	mainCtx context.Context,
	sloCtx context.Context,
	userCancelCtx context.Context,
	inputFile *os.File,
	plansDir, safeModelID, modelID string,
	collector *resultCollector,
	passThroughHeaders map[string]string,
	tenantID string,
) error {
	logger := logr.FromContextOrDiscard(requestAbortCtx).WithValues("model", modelID)
	requestAbortCtx = logr.NewContext(requestAbortCtx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("model setup failed: read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for model (async)", "numEntries", len(entries))

	asyncClient := p.asyncInference.ClientFor(modelID)
	if asyncClient == nil {
		logger.V(logging.INFO).Info("No async client for model, draining as model_not_found")
		p.drainUnprocessedRequests(
			requestAbortCtx, inputFile, entries, collector,
			inference.ErrCodeModelNotFound)
		return nil
	}
	defer func() {
		if err := asyncClient.Close(); err != nil {
			logger.Error(err, "Failed to close async client")
		}
	}()

	// ── Phase 1: Submit ────────────────────────────────────────────────────
	type pendingRequest struct {
		batchReqID string
		customID   string
	}

	pending := make(map[string]*pendingRequest)
	var submitCount int

	for _, entry := range entries {
		if requestAbortCtx.Err() != nil {
			logger.V(logging.INFO).Info("Async submit aborted", "submitted", len(pending), "total", len(entries), "reason", requestAbortCtx.Err())
			break
		}

		req, batchReqID, parseErr, readErr := readRequestLine(inputFile, entry, logger)
		if readErr != nil {
			return readErr
		}
		if parseErr != nil {
			collector.collect(outputLineToResultItem(parseErr))
			submitCount++
			continue
		}

		if errors.Is(sloCtx.Err(), context.DeadlineExceeded) {
			break
		}

		headers := maps.Clone(passThroughHeaders)
		headers = mergeInferenceHeaders(headers, sloCtx, p.cfg.InferenceObjectiveFor(modelID), p.fairnessID(tenantID))

		inferReq := &inference.GenerateRequest{
			RequestID: batchReqID,
			Endpoint:  req.URL,
			Params:    req.Body,
			Headers:   headers,
		}

		if submitErr := asyncClient.Submit(requestAbortCtx, inferReq); submitErr != nil {
			collector.collect(&ResultItem{
				RequestID: batchReqID,
				CustomID:  req.CustomID,
				Error:     &OutputError{Code: string(submitErr.Category), Message: submitErr.Message},
			})
			submitCount++
			continue
		}

		pending[batchReqID] = &pendingRequest{
			batchReqID: batchReqID,
			customID:   req.CustomID,
		}
		submitCount++
	}

	logger.V(logging.INFO).Info("Submit phase complete", "submitted", len(pending), "total", submitCount)

	// ── Phase 2: Collect ───────────────────────────────────────────────────
	var modelErr error

	for len(pending) > 0 {
		resp, err := asyncClient.GetResult(requestAbortCtx)
		if err != nil {
			if requestAbortCtx.Err() == nil {
				logger.Error(err, "Failed to collect async result", "pendingCount", len(pending))
				modelErr = fmt.Errorf("async result collection failed: %w", err)
			}
			break
		}

		pr, ok := pending[resp.RequestID]
		if !ok {
			logger.V(logging.TRACE).Info("Ignoring result for unknown request", "requestID", resp.RequestID)
			continue
		}

		out := buildOutputLine(pr.batchReqID, pr.customID, modelID, resp.RequestID, resp, nil, logger)
		if sloCtx.Err() == nil && userCancelCtx.Err() != nil {
			collector.collect(&ResultItem{
				RequestID: out.ID,
				CustomID:  out.CustomID,
				Error: &OutputError{
					Code:    string(batch_types.ErrCodeBatchCancelled),
					Message: "This request was cancelled while in progress.",
				},
			})
		} else {
			collector.collect(outputLineToResultItem(out))
		}
		delete(pending, resp.RequestID)
	}

	// Drain submitted-but-uncollected requests as errors so that
	// output_lines + error_lines == total_requests.
	for _, pr := range pending {
		collector.collect(&ResultItem{
			RequestID: pr.batchReqID,
			CustomID:  pr.customID,
			Error:     &OutputError{Code: string(batch_types.ErrCodeBatchExpired), Message: "result not collected before deadline"},
		})
	}

	return p.drainAndFinalize(requestAbortCtx, mainCtx, sloCtx, userCancelCtx,
		inputFile, entries[submitCount:], collector, modelErr, logger, len(entries), 0)
}

// drainAndFinalize drains undispatched entries based on termination reason and
// returns the appropriate sentinel error. Shared by processModel and processModelAsync.
func (p *Processor) drainAndFinalize(
	requestAbortCtx context.Context,
	mainCtx context.Context,
	sloCtx context.Context,
	userCancelCtx context.Context,
	inputFile *os.File,
	undispatched []planEntry,
	collector *resultCollector,
	modelErr error,
	logger logr.Logger,
	totalEntries int,
	shutdownCancelledCount int32,
) error {
	var returnErr error
	switch {
	case errors.Is(sloCtx.Err(), context.DeadlineExceeded):
		// SLO deadline fired during dispatch — record remaining requests as expired.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("SLO expired: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, collector,
				batch_types.ErrCodeBatchExpired)
		}
		returnErr = errExpired

	case userCancelCtx.Err() != nil:
		// User-initiated cancel — record remaining requests as cancelled.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Cancelled: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, collector,
				batch_types.ErrCodeBatchCancelled)
		}
		returnErr = errCancelled

	case modelErr != nil:
		// System error in a model goroutine — record remaining requests as failed.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Fatal error: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, collector,
				batch_types.ErrCodeBatchFailed)
		}
		returnErr = modelErr

	default:
		if mainCtx.Err() != nil && (len(undispatched) > 0 || shutdownCancelledCount > 0) {
			// Pod shutdown (SIGTERM): main processor context is cancelled.
			// Do not drain here — the job will be left for the orphan
			// reconciler to transition to a terminal state. The undispatched
			// check catches SIGTERM arriving during dispatch; shutdownCancelled
			// catches SIGTERM cancelling already-dispatched in-flight requests.
			returnErr = errShutdown
		} else if requestAbortCtx.Err() != nil && len(undispatched) > 0 {
			// Sibling model abort: requestAbortCtx was cancelled by another
			// model's error (requestAbortFn), but this is not SLO/cancel/SIGTERM.
			// Drain undispatched entries as batch_failed so that
			// completed + failed == total holds for the job.
			logger.V(logging.INFO).Info("Sibling abort: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, collector,
				batch_types.ErrCodeBatchFailed)
		}
	}

	siblingAbort := returnErr == nil && requestAbortCtx.Err() != nil
	logger.V(logging.INFO).Info("Finished processing model", "numEntries", totalEntries, "hasError", returnErr != nil, "siblingAbort", siblingAbort)
	return returnErr
}

// drainUnprocessedRequests sends error entries for undispatched plan entries to
// the collector. Called from processModel when dispatch is interrupted (SLO
// expiry, cancellation, or systemic failure). For each entry it reads the
// original request from input.jsonl to extract the custom_id.
func (p *Processor) drainUnprocessedRequests(
	ctx context.Context,
	inputFile *os.File,
	entries []planEntry,
	collector *resultCollector,
	errCode batch_types.BatchErrorCode,
) {
	errMessage := errCode.Message()

	// Allocate a single read buffer sized to the largest entry to avoid per-entry allocations.
	var maxLen uint32
	for _, e := range entries {
		if e.Length > maxLen {
			maxLen = e.Length
		}
	}
	buf := make([]byte, maxLen)

	for _, entry := range entries {
		customID := ""
		if _, err := inputFile.ReadAt(buf[:entry.Length], entry.Offset); err == nil {
			var req batch_types.Request
			if err := json.Unmarshal(bytes.TrimSuffix(buf[:entry.Length], []byte{'\n'}), &req); err == nil {
				customID = req.CustomID
			}
		}

		collector.collect(&ResultItem{
			RequestID: newBatchRequestID(uuid.NewString()),
			CustomID:  customID,
			Error:     &OutputError{Code: string(errCode), Message: errMessage},
		})
	}
}

// outputLineToResultItem converts an outputLine (returned by executeOneRequest) to a
// ResultItem for the collector.
func outputLineToResultItem(ol *outputLine) *ResultItem {
	var outErr *OutputError
	if ol.Error != nil {
		outErr = &OutputError{Code: ol.Error.Code, Message: ol.Error.Message}
	}
	return &ResultItem{
		RequestID:        ol.ID,
		CustomID:         ol.CustomID,
		Response:         ol.Response,
		Error:            outErr,
		HadCapacityRetry: ol.hadCapacityRetry,
	}
}

const (
	sloTTFTMSHeader          = "x-slo-ttft-ms"
	inferenceObjectiveHeader = "x-gateway-inference-objective"
	fairnessIDHeader         = "x-gateway-inference-fairness-id"
)

// mergeInferenceHeaders adds processor-managed headers to the outgoing inference request:
//   - x-slo-ttft-ms: remaining milliseconds until the SLO deadline (>= 0).
//   - x-gateway-inference-objective: name of the InferenceObjective CRD that
//     determines the priority band for this request.
//   - x-gateway-inference-fairness-id: tenant identifier for per-tenant fairness
//     within a priority band.
//
// Headers are only added when the relevant value is available/configured.
// If sloCtx has no deadline, is cancelled, or has an expired deadline, the SLO
// header is not set. If inferenceObjective is empty, the objective header is not set.
// If fairnessID is non-empty, the fairness header is set only when the outgoing
// headers do not already include x-gateway-inference-fairness-id. Unlike SLO and
// objective (which are processor-authoritative and always override), fairness is
// user-overridable: callers can supply a custom flow key (e.g. API key, group ID)
// via pass-through headers, and the processor falls back to tenantID only when no
// override is present.
func mergeInferenceHeaders(headers map[string]string, sloCtx context.Context, inferenceObjective, fairnessID string) map[string]string {
	hasSLO := false
	var sloMs int64
	if sloCtx.Err() == nil {
		if dl, ok := sloCtx.Deadline(); ok {
			ms := time.Until(dl).Milliseconds()
			if ms >= 0 {
				hasSLO = true
				sloMs = ms
			}
		}
	}
	hasObjective := inferenceObjective != ""
	hasFairness := fairnessID != ""
	if hasFairness && headers != nil {
		if _, exists := headers[fairnessIDHeader]; exists {
			hasFairness = false
		}
	}

	if !hasSLO && !hasObjective && !hasFairness {
		return headers
	}
	if headers == nil {
		headers = make(map[string]string)
	}
	if hasSLO {
		headers[sloTTFTMSHeader] = strconv.FormatInt(sloMs, 10)
	}
	if hasObjective {
		headers[inferenceObjectiveHeader] = inferenceObjective
	}
	if hasFairness {
		headers[fairnessIDHeader] = fairnessID
	}
	return headers
}

// readRequestLine reads a single plan entry from the input file, parses it, and
// generates a batch request ID. Returns the parsed request and batch request ID
// on success, an outputLine on parse error, or a fatal error on I/O failure.
func readRequestLine(inputFile *os.File, entry planEntry, logger logr.Logger) (*batch_types.Request, string, *outputLine, error) {
	buf := make([]byte, entry.Length)
	if _, err := inputFile.ReadAt(buf, entry.Offset); err != nil {
		return nil, "", nil, fmt.Errorf("%w at offset %d: %w", errRequestInputRead, entry.Offset, err)
	}
	trimmed := bytes.TrimSuffix(buf, []byte{'\n'})
	batchReqID := newBatchRequestID(uuid.NewString())

	var req batch_types.Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		logger.Error(err, "failed to parse request line, recording as error")
		return nil, batchReqID, newErrorOutputLine(batchReqID, "", string(httpclient.ErrCategoryParse),
			fmt.Sprintf("failed to parse request line: %v", err)), nil
	}

	return &req, batchReqID, nil, nil
}

func (p *Processor) fairnessID(tenantID string) string {
	if p.cfg.SendFairnessHeader {
		return tenantID
	}
	return ""
}

// executeOneRequest reads a single input line from the input file at the given plan entry offset,
// sends it to the inference gateway, and returns the formatted output line.
func (p *Processor) executeOneRequest(
	ctx context.Context,
	sloCtx context.Context,
	inputFile *os.File,
	entry planEntry,
	modelID string,
	passThroughHeaders map[string]string,
	tenantID string,
) (*outputLine, error) {
	logger := logr.FromContextOrDiscard(ctx)
	req, batchReqID, parseErr, readErr := readRequestLine(inputFile, entry, logger)
	if readErr != nil {
		return nil, readErr
	}
	if parseErr != nil {
		return parseErr, nil
	}

	logger = logger.WithValues("customId", req.CustomID, "requestId", batchReqID)

	inferClient := p.inference.ClientFor(modelID)
	if inferClient == nil {
		logger.V(logging.INFO).Info("ClientFor returned nil during execution (expected rejection at ingestion)",
			"model", modelID)
		metrics.RecordRequestError(modelID)
		return newErrorOutputLine(batchReqID, req.CustomID, inference.ErrCodeModelNotFound,
			fmt.Sprintf("model %q is not configured in any gateway", modelID)), nil
	}

	headers := maps.Clone(passThroughHeaders)
	headers = mergeInferenceHeaders(headers, sloCtx, p.cfg.InferenceObjectiveFor(modelID), p.fairnessID(tenantID))

	inferReq := &inference.GenerateRequest{
		RequestID: batchReqID,
		Endpoint:  req.URL,
		Params:    req.Body,
		Headers:   headers,
	}

	if sloCtx.Err() == context.DeadlineExceeded {
		logger.V(logging.INFO).Info("SLO expired during execution, skipping request", "error", sloCtx.Err())
		result := newErrorOutputLine(batchReqID, req.CustomID,
			string(batch_types.ErrCodeBatchExpired), batch_types.ErrCodeBatchExpired.Message())
		metrics.RecordRequestError(modelID)
		return result, nil
	}

	start := time.Now()
	metrics.IncProcessorInflightRequests()
	metrics.IncModelInflightRequests(modelID)
	logger.V(logging.TRACE).Info("Dispatching inference request")

	inferResp, inferErr := inferClient.Generate(ctx, inferReq)

	metrics.DecModelInflightRequests(modelID)
	metrics.DecProcessorInflightRequests()
	metrics.RecordModelRequestExecutionDuration(time.Since(start), modelID)

	result := buildOutputLine(batchReqID, req.CustomID, modelID, inferReq.RequestID, inferResp, inferErr, logger)
	return result, nil
}

func newErrorOutputLine(batchReqID, customID, code, message string) *outputLine {
	return &outputLine{
		ID:       batchReqID,
		CustomID: customID,
		Error:    &outputError{Code: code, Message: message},
	}
}

// buildOutputLine converts an inference response and/or error into an outputLine.
// Used by both executeOneRequest (sync path) and processModelAsync (async path).
func buildOutputLine(
	batchReqID, customID, modelID, serverRequestID string,
	inferResp *inference.GenerateResponse,
	inferErr *inference.ClientError,
	logger logr.Logger,
) *outputLine {
	result := &outputLine{
		ID:       batchReqID,
		CustomID: customID,
	}

	// Response handling by case.
	//
	// Design note: HTTP errors (4xx/5xx) are written to the output file with their
	// status code and body, rather than the error file. The OpenAI Batch API guides
	// describe output_file_id as containing "successfully executed requests", but
	// the OpenAPI schema defines the error field as "for requests that failed with a
	// non-HTTP error", implying HTTP errors belong in the response. We follow the
	// schema interpretation here, as it preserves the HTTP status code and body for
	// callers to inspect.
	if inferErr != nil {
		logger.V(logging.DEBUG).Info("Inference request failed", "error", inferErr.Message)
		if inferErr.StatusCode > 0 {
			if inferErr.DroppedReason == httpclient.DroppedReasonTTLExpired {
				result.Error = &outputError{
					Code:    string(batch_types.ErrCodeBatchExpired),
					Message: batch_types.ErrCodeBatchExpired.Message(),
				}
				metrics.RecordRequestError(modelID)
				return result
			}
			// HTTP error (4xx/5xx) — populate response with status code and original body
			// per OpenAI spec, error field is only for non-HTTP errors
			// Ensure body is always a non-nil object to satisfy the OpenAI schema (type: object).
			body := make(map[string]interface{})
			if len(inferErr.ResponseBody) > 0 {
				if err := json.Unmarshal(inferErr.ResponseBody, &body); err != nil {
					// Non-JSON response body cannot be placed directly into a JSON object field,
					// so we wrap it in a synthetic error structure to preserve the content.
					body = map[string]interface{}{
						"error": map[string]interface{}{
							"message": string(inferErr.ResponseBody),
							"type":    inferErr.OpenAIErrorType(),
						},
					}
				}
			}
			result.Response = &batch_types.ResponseData{
				StatusCode: inferErr.StatusCode,
				RequestID:  serverRequestID,
				Body:       body,
			}
		} else {
			// Non-HTTP error (network, timeout, etc.)
			result.Error = &outputError{
				Code:    string(inferErr.Category),
				Message: inferErr.Message,
			}
		}
	} else if inferResp == nil {
		// ok status without error but no response
		err := fmt.Errorf("inference returned no error but response is nil")
		logger.Error(err, "Inference request failed")
		result.Error = &outputError{
			Code:    string(httpclient.ErrCategoryServer),
			Message: err.Error(),
		}
	} else {
		result.hadCapacityRetry = inferResp.HadCapacityRetry
		// success — unmarshal the response body
		var body map[string]interface{}
		if len(inferResp.Response) > 0 {
			if err := json.Unmarshal(inferResp.Response, &body); err != nil {
				// failed to unmarshal the response body
				logger.Error(err, "failed to unmarshal inference response body")
				result.Error = &outputError{
					Code:    string(httpclient.ErrCategoryParse),
					Message: fmt.Sprintf("inference succeeded but response body could not be parsed: %v", err),
				}
			}
		}
		if result.Error == nil {
			logger.V(logging.TRACE).Info("Inference request completed", "serverRequestId", inferResp.RequestID)
			result.Response = &batch_types.ResponseData{
				StatusCode: 200,
				RequestID:  inferResp.RequestID,
				Body:       body,
			}
			recordTokenUsageFromBody(body, modelID, logger)
		}
	}

	if !result.isSuccess() {
		metrics.RecordRequestError(modelID)
	}
	return result
}

// recordTokenUsageFromBody extracts prompt and completion token counts from the
// inference response body and records them as metrics. Skips if the usage object
// is absent, if neither prompt_tokens nor completion_tokens is a valid numeric value,
// or if either one is negative.
func recordTokenUsageFromBody(body map[string]interface{}, model string, logger logr.Logger) {
	usage, ok := body["usage"].(map[string]interface{})
	if !ok {
		logger.V(logging.DEBUG).Info("Inference response missing usage data, skipping token metrics")
		return
	}
	prompt, promptOK := jsonNumericToFloat64(usage["prompt_tokens"])
	completion, completionOK := jsonNumericToFloat64(usage["completion_tokens"])
	if !promptOK && !completionOK {
		logger.V(logging.DEBUG).Info("Inference response usage has no numeric token fields, skipping token metrics")
		return
	}
	// Prometheus Counter.Add() panics on negative values. Guard against non-conforming
	// inference backends that might return negative token counts.
	if prompt < 0 || completion < 0 {
		logger.V(logging.DEBUG).Info("Inference response usage has negative token values, skipping token metrics",
			"prompt_tokens", prompt, "completion_tokens", completion)
		return
	}
	metrics.RecordTokenUsage(prompt, completion, model)
}

func jsonNumericToFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// newBatchRequestID formats requestID into the "batch_req_<uuid>" form required by the
// OpenAI Batch API for output/error line IDs. When used in executeOneRequest, the same
// requestID is also passed to the inference client so the two can be correlated in logs.
func newBatchRequestID(requestID string) string {
	return fmt.Sprintf("batch_req_%s", requestID)
}
