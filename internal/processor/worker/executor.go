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
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"github.com/llm-d-incubation/batch-gateway/internal/util/semaphore"
	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
)

// outputWriters holds the buffered writers and their mutexes for the output and error JSONL files.
// A single instance is created per job and shared across model goroutines.
type outputWriters struct {
	output   *bufio.Writer
	outputMu sync.Mutex
	errors   *bufio.Writer
	errorsMu sync.Mutex
}

// write writes line to the error file if isError is true, otherwise to the output file.
func (w *outputWriters) write(line []byte, isError bool) error {
	if isError {
		w.errorsMu.Lock()
		defer w.errorsMu.Unlock()
		_, err := w.errors.Write(line)
		return err
	}
	w.outputMu.Lock()
	defer w.outputMu.Unlock()
	_, err := w.output.Write(line)
	return err
}

// outputLine represents a single line in the output JSONL file following the OpenAI batch output format.
type outputLine struct {
	ID       string                    `json:"id"`
	CustomID string                    `json:"custom_id"`
	Response *batch_types.ResponseData `json:"response"`
	Error    *outputError              `json:"error"`
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
// requests concurrently (one goroutine per model), and writes results to
// output.jsonl (successes) and error.jsonl (failures). Returns request counts for finalization.
//
// On success, returns (counts, nil). On interruption or error, undispatched requests are
// drained to the error file with the appropriate code, writers are flushed, and partial counts
// are returned alongside the sentinel/cause error:
//   - SLO expired:    (counts, ErrExpired)    — drain as batch_expired
//   - User cancel:    (counts, ErrCancelled)  — drain as batch_cancelled
//   - System error:   (counts, firstErr)      — drain as batch_failed
//   - Pod shutdown:   (nil, ctx.Err())        — no flush, caller re-enqueues

// abortCtx is cancelled when the user requests batch cancellation. Cancelling it aborts all
// in-flight inference HTTP requests immediately, freeing downstream resources (GPU slots, EPP
// capacity). abortCtx is derived from sloCtx in the caller so the SLO deadline is also respected.
//
// Dispatch abort relies solely on context cancellation (checkAbortCondition checks ctx.Err()).
// The cancelRequested flag is NOT polled to stop dispatch; it is only consulted in the
// error-handling path to distinguish the cancellation reason (user cancel vs SLO vs pod shutdown)
// and to drain undispatched entries with the correct error code.
func (p *Processor) executeJob(ctx, sloCtx, abortCtx context.Context, params *jobExecutionParams) (*openai.BatchRequestCounts, error) {
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
		return &openai.BatchRequestCounts{Total: modelMap.LineCount, Failed: modelMap.RejectedCount}, ErrExpired
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

	writers := &outputWriters{
		output: bufio.NewWriterSize(outputFile, 1024*1024),
		errors: bufio.NewWriterSize(errorFile, 1024*1024),
	}

	plansDir, err := p.jobPlansDir(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}

	// one goroutine per model; concurrency within each model is bounded
	// by globalSem (processor-wide concurrency limit) and perModelMaxConcurrency (per-model concurrency limit).
	// execCtx is derived from abortCtx (which itself is derived from sloCtx) so both the SLO
	// deadline and user-initiated cancellation propagate to all dispatch loops and inference calls.
	execCtx, execCancel := context.WithCancel(abortCtx)
	defer execCancel()

	progress := &executionProgress{
		total:   modelMap.LineCount,
		updater: params.updater,
		jobID:   params.jobInfo.JobID,
	}
	// Seed with requests already rejected during ingestion (model not found).
	progress.failed.Store(modelMap.RejectedCount)

	errCh := make(chan error, len(modelMap.SafeToModel))

	passThroughHeaders := params.jobInfo.PassThroughHeaders
	if len(passThroughHeaders) > 0 {
		headerNames := make([]string, 0, len(passThroughHeaders))
		for k := range passThroughHeaders {
			headerNames = append(headerNames, k)
		}
		logger.V(logging.DEBUG).Info("pass-through headers attached to job", "headerNames", headerNames)
	}

	for safeModelID, modelID := range modelMap.SafeToModel {
		// Ordering guarantee: processModel returns → execCancel → errCh send.
		// This ensures the first real error reaches errCh before any context.Canceled
		// from other models whose contexts were cancelled by execCancel.
		go func(safeModelID, modelID string) {
			err := p.processModel(
				execCtx,
				sloCtx,
				inputFile,
				plansDir, safeModelID, modelID,
				writers,
				params.cancelRequested,
				progress,
				passThroughHeaders,
			)
			if err != nil {
				execCancel()
			}
			errCh <- err
		}(safeModelID, modelID)
	}

	var firstErr error
	for range modelMap.SafeToModel {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Push final progress to Redis so the last throttled update doesn't
	// leave stale counts visible to polling clients.
	progress.flush(ctx)

	if firstErr != nil {
		// prefer parent-context / user-cancel errors for correct routing in handleJobError
		if ctx.Err() != nil {
			return nil, ctx.Err() // parent-context error (e.g. pod shutdown)
		}
		if params.cancelRequested.Load() {
			_ = writers.output.Flush()
			_ = writers.errors.Flush()
			counts := progress.counts()
			logger.V(logging.INFO).Info("Execution cancelled, returning partial counts",
				"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
			return counts, ErrCancelled
		}
		// SLO deadline exceeded: sloCtx deadline fired during execution.
		// processModel already drained undispatched entries to error file; flush and return partial counts.
		// Use sloCtx.Err() rather than execCtx.Err(): execCtx may have been cancelled by a goroutine
		// via execCancel() before the sloCtx deadline propagated, setting execCtx.Err() = Canceled.
		if sloCtx.Err() == context.DeadlineExceeded {
			// best-effort: flush the output and error files
			_ = writers.output.Flush()
			_ = writers.errors.Flush()
			counts := progress.counts()
			logger.V(logging.INFO).Info("Execution SLO expired, returning partial counts",
				"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
			return counts, ErrExpired
		}
		// System error from model goroutines — flush partial results.
		_ = writers.output.Flush()
		_ = writers.errors.Flush()
		counts := progress.counts()
		logger.V(logging.INFO).Info("Execution system error, returning partial counts",
			"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
		return counts, firstErr
	}

	if err := writers.output.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush output file: %w", err)
	}
	if err := writers.errors.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush error file: %w", err)
	}

	counts := progress.counts()
	logger.V(logging.INFO).Info("Execution completed",
		"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)

	// Cancel may have arrived after all requests were dispatched and completed normally
	// (i.e. context cancellation never interrupted dispatch). Honour the cancellation.
	if params.cancelRequested.Load() {
		return counts, ErrCancelled
	}

	return counts, nil
}

// processModel processes all plan entries for a single model concurrently.
// Concurrency is bounded by both a global semaphore (p.globalSem, shared across
// all models/workers) and a per-model semaphore (PerModelMaxConcurrency).
//
// Semaphore acquisition order: local (per-model) before global (shared).
// This prevents starving other models — blocking on global only wastes a local slot.
//
// Error strategy in this function: when a goroutine encounters a fatal error, modelErr is captured
// via errOnce but the context is NOT cancelled within this function. Context cancellation is
// propagated at the executeJob level (execCancel), which stops dispatch across all models.
// Already-dispatched goroutines may finish with errors or cancellation rather than successful
// completion, depending on when execCancel fires.
func (p *Processor) processModel(
	ctx context.Context,
	sloCtx context.Context,
	inputFile *os.File,
	plansDir, safeModelID, modelID string,
	writers *outputWriters,
	cancelRequested *atomic.Bool,
	progress *executionProgress,
	passThroughHeaders map[string]string,
) error {
	logger := logr.FromContextOrDiscard(ctx).WithValues("model", modelID)
	ctx = logr.NewContext(ctx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("failed to read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for a model", "numEntries", len(entries))

	modelSem, err := semaphore.New(p.cfg.PerModelMaxConcurrency, p.guardCallback)
	if err != nil {
		return fmt.Errorf("failed to create model semaphore: %w", err)
	}

	var (
		wg              sync.WaitGroup
		errOnce         sync.Once
		modelErr        error
		dispatchedCount int
	)

dispatch:
	for i, entry := range entries {
		if err := checkAbortCondition(ctx); err != nil {
			errOnce.Do(func() { modelErr = err })
			break
		}

		// Acquire semaphores in order: local (per-model) before global (shared).
		// This order prevents starving other models — blocking on global only wastes a local slot.
		if err := modelSem.Acquire(ctx); err != nil {
			break dispatch
		}

		if err := p.globalSem.Acquire(ctx); err != nil {
			modelSem.Release()
			break dispatch
		}

		dispatchedCount = i + 1
		wg.Add(1)
		go func(entry planEntry) {
			defer wg.Done()
			defer modelSem.Release()
			defer p.globalSem.Release()

			result, execErr := p.executeOneRequest(ctx, sloCtx, inputFile, entry, modelID, passThroughHeaders)
			if execErr != nil {
				logger.Error(execErr, "Fatal error executing request", "offset", entry.Offset)
				errOnce.Do(func() { modelErr = execErr })
				return
			}

			// If cancel was requested while this request was in-flight,
			// overwrite the result as batch_cancelled and write to the error file
			// so that output lines + error lines == total requests.
			// Note: abortCtx is already cancelled at this point, so the HTTP
			// request was aborted and this goroutine returns almost immediately.
			if cancelRequested.Load() {
				result.Response = nil
				result.Error = &outputError{
					Code:    batch_types.ErrCodeBatchCancelled,
					Message: "This request was cancelled while in progress.",
				}
				progress.record(ctx, false)

				lineBytes, marshalErr := json.Marshal(result)
				if marshalErr != nil {
					logger.Error(marshalErr, "Failed to marshal cancelled output line", "offset", entry.Offset)
					errOnce.Do(func() { modelErr = fmt.Errorf("failed to marshal cancelled line: %w", marshalErr) })
					return
				}
				lineBytes = append(lineBytes, '\n')
				if writeErr := writers.write(lineBytes, true); writeErr != nil {
					logger.Error(writeErr, "Failed to write cancelled line", "offset", entry.Offset)
					errOnce.Do(func() { modelErr = fmt.Errorf("failed to write cancelled line: %w", writeErr) })
				}
				return
			}

			progress.record(ctx, result.isSuccess())

			lineBytes, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				logger.Error(marshalErr, "Failed to marshal output line", "offset", entry.Offset)
				errOnce.Do(func() { modelErr = fmt.Errorf("failed to marshal output line: %w", marshalErr) })
				return
			}
			lineBytes = append(lineBytes, '\n')

			// Write to error file only for non-HTTP errors (error field populated).
			// HTTP error responses (4xx/5xx) go to output file since they carry a valid
			// response object with status_code and body per the OpenAI batch spec.
			isError := result.Error != nil
			if writeErr := writers.write(lineBytes, isError); writeErr != nil {
				kind := "output"
				if isError {
					kind = "error"
				}
				logger.Error(writeErr, "Failed to write line", "kind", kind, "offset", entry.Offset)
				errOnce.Do(func() { modelErr = fmt.Errorf("failed to write %s line: %w", kind, writeErr) })
			}
		}(entry)
	}

	wg.Wait()

	// Drain undispatched entries to the error file based on the termination reason.
	// Priority: SLO expiry > user cancel > system error.
	// Use sloCtx.Err() rather than ctx.Err(): ctx (execCtx) may report Canceled if execCancel()
	// was called by another goroutine before the sloCtx deadline propagated.
	// (Same rationale as the sloCtx check in executeJob.)
	switch {
	case sloCtx.Err() == context.DeadlineExceeded && !cancelRequested.Load():
		// SLO deadline fired during dispatch — record remaining requests as expired.
		undispatched := entries[dispatchedCount:]
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("SLO expired: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(ctx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchExpired,
				"This request could not be executed before the completion window expired.")
		}

	case cancelRequested.Load():
		// User-initiated cancel — record remaining requests as cancelled.
		undispatched := entries[dispatchedCount:]
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Cancelled: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(ctx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchCancelled,
				"This request was not executed because the batch was cancelled.")
		}

	case modelErr != nil:
		// System error in a model goroutine — record remaining requests as failed.
		undispatched := entries[dispatchedCount:]
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Fatal error: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(ctx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchFailed,
				"This request was not executed because the batch encountered a system error.")
		}
	}

	if modelErr == nil && ctx.Err() != nil {
		modelErr = ctx.Err()
	}

	logger.V(logging.INFO).Info("Finished processing model", "numEntries", len(entries), "hasError", modelErr != nil)
	return modelErr
}

// drainUnprocessedRequests records undispatched requests in the error file when a job terminates
// mid-execution (SLO expiry, cancellation, or systemic failure). For each plan entry, it reads
// the original request from input.jsonl to extract the custom_id, then writes an error line with
// the given error code and message.
func (p *Processor) drainUnprocessedRequests(
	ctx context.Context,
	inputFile *os.File,
	entries []planEntry,
	writers *outputWriters,
	progress *executionProgress,
	errCode string,
	errMessage string,
) {
	logger := logr.FromContextOrDiscard(ctx)

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

		requestID := uuid.NewString()

		line := &outputLine{
			ID:       newBatchRequestID(requestID),
			CustomID: customID,
			Error: &outputError{
				Code:    errCode,
				Message: errMessage,
			},
		}

		lineBytes, err := json.Marshal(line)
		if err != nil {
			logger.Error(err, "Failed to marshal drain entry", "errCode", errCode, "offset", entry.Offset)
			continue
		}
		lineBytes = append(lineBytes, '\n')

		if writeErr := writers.write(lineBytes, true); writeErr != nil {
			logger.Error(writeErr, "Failed to write drain entry", "errCode", errCode, "offset", entry.Offset)
		}

		// Context may be cancelled here (e.g. SLO deadline fired), so the Redis progress
		// update inside record() may fail silently. The atomic counter still increments
		// correctly and the final counts are committed by the terminal status update.
		progress.record(ctx, false)
	}
}

const sloTTFTMSHeader = "x-slo-ttft-ms"

// mergeSLOTTFTIntoHeaders sets sloTTFTMSHeader to the remaining time until sloCtx's deadline
// in whole milliseconds, clamped to >= 0. If sloCtx has no deadline, is cancelled, or has an
// expired deadline, the headers map is returned unchanged.
func mergeSLOTTFTIntoHeaders(headers map[string]string, sloCtx context.Context) map[string]string {
	if sloCtx.Err() != nil {
		return headers
	}
	dl, ok := sloCtx.Deadline()
	if !ok {
		return headers
	}
	sloMs := time.Until(dl).Milliseconds()
	if sloMs < 0 {
		// this check is needed in case the context deadline was exceeded between the sloCtx.Err() check
		// and the time.Until call above. We return the headers unchanged to avoid sending a negative value
		// to the inference gateway.
		return headers
	}
	if headers == nil {
		headers = make(map[string]string)
	}
	headers[sloTTFTMSHeader] = strconv.FormatInt(sloMs, 10)
	return headers
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
) (*outputLine, error) {
	// read the request line from input.jsonl at the given offset and length
	buf := make([]byte, entry.Length)
	if _, err := inputFile.ReadAt(buf, entry.Offset); err != nil {
		return nil, fmt.Errorf("failed to read plan entry input at offset %d: %w", entry.Offset, err)
	}

	// trim the newline character from the request line
	trimmed := bytes.TrimSuffix(buf, []byte{'\n'})

	// generate a new request ID
	requestID := uuid.NewString()

	// parse the request line into a batch_types.Request object
	var req batch_types.Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "failed to parse request line, recording as error")
		return &outputLine{
			ID: newBatchRequestID(requestID),
			Error: &outputError{
				Code:    string(httpclient.ErrCategoryParse),
				Message: fmt.Sprintf("failed to parse request line: %v", err),
			},
		}, nil
	}

	// model id, job id and tenant id are already set in the context
	logger := logr.FromContextOrDiscard(ctx).WithValues("customId", req.CustomID, "requestId", requestID)

	// Per-model mode rejects unregistered models at ingestion (fast path). ClientFor can
	// still return nil after gateway config changes between ingestion and execution, or
	// during recovery when model_map/plan files predate the current resolver — treat as
	// a request-level error so the rest of the batch can complete.
	inferClient := p.inference.ClientFor(modelID)
	if inferClient == nil {
		logger.V(logging.INFO).Info("ClientFor returned nil during execution (expected rejection at ingestion)",
			"model", modelID)
		result := &outputLine{
			ID:       newBatchRequestID(requestID),
			CustomID: req.CustomID,
			Error: &outputError{
				Code:    inference.ErrCodeModelNotFound,
				Message: fmt.Sprintf("model %q is not configured in any gateway", modelID),
			},
		}
		metrics.RecordRequestError(modelID)
		return result, nil
	}

	headers := maps.Clone(passThroughHeaders)
	headers = mergeSLOTTFTIntoHeaders(headers, sloCtx)

	inferReq := &inference.GenerateRequest{
		RequestID: newBatchRequestID(requestID),
		Endpoint:  req.URL,
		Params:    req.Body,
		Headers:   headers,
	}

	if sloCtx.Err() == context.DeadlineExceeded {
		logger.V(logging.INFO).Info("SLO expired during execution, skipping request", "error", sloCtx.Err())
		result := &outputLine{
			ID:       newBatchRequestID(requestID),
			CustomID: req.CustomID,
			Error: &outputError{
				Code:    batch_types.ErrCodeBatchExpired,
				Message: "This request could not be executed before the completion window expired.",
			},
		}
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

	result := &outputLine{
		ID:       newBatchRequestID(requestID),
		CustomID: req.CustomID,
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
				RequestID:  inferReq.RequestID,
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
	return result, nil
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
