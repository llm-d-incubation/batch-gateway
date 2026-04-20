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
// requests concurrently (one goroutine per model, multiple requests per model), and writes results to
// output.jsonl (successes) and error.jsonl (failures). Returns request counts for finalization.
//
// On success, returns (counts, nil). On interruption or error, output and error writers are
// always flushed (buffered data written to the underlying files) before returning, and partial
// counts are returned alongside the sentinel/cause error:
//   - SLO expired:    (counts, errExpired)   — undispatched drained as batch_expired
//   - User cancel:    (counts, errCancelled) — undispatched drained as batch_cancelled
//   - System error:   (counts, firstErr)     — undispatched drained as batch_failed
//   - Pod shutdown:   (counts, errShutdown)  — caller re-enqueues; counts reflect work done
//     before SIGTERM, flush preserves partial output for startup recovery
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

	writers := &outputWriters{
		output: bufio.NewWriterSize(outputFile, 1024*1024),
		errors: bufio.NewWriterSize(errorFile, 1024*1024),
	}

	plansDir, err := p.jobPlansDir(params.jobInfo.JobID, params.jobInfo.TenantID)
	if err != nil {
		return nil, err
	}

	// requestAbortCtx and requestAbortFn are set in runJob before watchCancel starts,
	// eliminating the race window where a cancel event could arrive before the fn is assigned.

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

	// User-initiated cancellation: watchCancel calls userCancelFn() only.
	// context.AfterFunc(userCancelCtx, requestAbortFn) — wired in runJob — then
	// cancels requestAbortCtx to stop the dispatch loop. userCancelCtx is isolated
	// from sloCtx (derived from context.Background), so SLO expiry and SIGTERM do
	// not set it. processModel's drain phase checks sloCtx.Err() vs
	// userCancelCtx.Err() to choose the right error code (errExpired vs errCancelled).
	for safeModelID, modelID := range modelMap.SafeToModel {
		// Ordering guarantee: processModel returns → requestAbortFn → errCh send.
		// This ensures the first real error reaches errCh before any context.Canceled
		// from other models whose contexts were cancelled by requestAbortFn.
		go func(safeModelID, modelID string) {
			err := p.processModel(
				requestAbortCtx,
				ctx,
				sloCtx,
				userCancelCtx,
				inputFile,
				plansDir, safeModelID, modelID,
				writers,
				progress,
				passThroughHeaders,
			)
			// Abort all sibling models when any model hits a fatal I/O error
			// (e.g. output file write failure). modelErr is only set for local
			// I/O failures — not inference errors, which are recorded normally
			// in the error file. Since all models share the same output writers,
			// a write failure in one model means the shared file is unusable
			// and continuing other models would produce corrupt output.
			// Guard against nil requestAbortFn for direct-call test paths.
			if err != nil {
				if fn := params.requestAbortFn; fn != nil {
					fn()
				}
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
		// Flush partial results to disk before routing the error.
		// Required for all non-nil firstErr paths: errExpired, errCancelled, system errors
		// (callers upload from disk), and SIGTERM (startup recovery reads from disk on restart).
		if err := writers.output.Flush(); err != nil {
			logger.Error(err, "Failed to flush output file on error path (partial results may be truncated)")
		}
		if err := writers.errors.Flush(); err != nil {
			logger.Error(err, "Failed to flush error file on error path (partial results may be truncated)")
		}
		// processModel already drained undispatched entries and returned a sentinel (errExpired or
		// errCancelled) or the underlying system error. All terminal handlers now use detached
		// contexts, so we preserve processModel's decision even when SIGTERM is concurrent.
		counts := progress.counts()
		switch {
		case errors.Is(firstErr, errExpired):
			logger.V(logging.INFO).Info("Execution SLO expired, returning partial counts",
				"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
		case errors.Is(firstErr, errCancelled):
			logger.V(logging.INFO).Info("Execution cancelled, returning partial counts",
				"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
		default:
			logger.V(logging.INFO).Info("Execution system error, returning partial counts",
				"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)
		}
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

	// A terminal signal may have arrived after all requests completed normally.
	// SLO expiry and user cancel affect the job's terminal status even when all
	// requests finished — e.g. "completed but past SLO" vs "completed".
	// SIGTERM is NOT checked here: all output is already flushed to disk and counts
	// are final, so the caller should proceed to finalizeJob (which uses a detached
	// context) rather than re-enqueueing a fully-complete job.
	switch {
	case errors.Is(sloCtx.Err(), context.DeadlineExceeded):
		return counts, errExpired
	case userCancelCtx.Err() != nil:
		return counts, errCancelled
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
	writers *outputWriters,
	progress *executionProgress,
	passThroughHeaders map[string]string,
) error {
	logger := logr.FromContextOrDiscard(requestAbortCtx).WithValues("model", modelID)
	requestAbortCtx = logr.NewContext(requestAbortCtx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("model setup failed: read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for a model", "numEntries", len(entries))

	// PerModelMaxConcurrency > 0 is enforced by config validation; this cannot fail.
	modelSem, _ := semaphore.New(p.cfg.PerModelMaxConcurrency, p.guardCallback)

	var (
		wg              sync.WaitGroup
		errOnce         sync.Once
		modelErr        error
		dispatchedCount int
	)

dispatch:
	for i, entry := range entries {
		if requestAbortCtx.Err() != nil {
			// Context cancelled (SLO expiry, SIGTERM, or user cancel via requestAbortFn).
			// Do not set modelErr here; the drain switch determines the correct sentinel
			// by inspecting sloCtx, userCancelCtx, and requestAbortCtx independently.
			break
		}

		// Acquire semaphores in order: local (per-model) before global (shared).
		// This order prevents starving other models — blocking on global only wastes a local slot.
		if err := modelSem.Acquire(requestAbortCtx); err != nil {
			break dispatch
		}

		if err := p.globalSem.Acquire(requestAbortCtx); err != nil {
			modelSem.Release()
			break dispatch
		}

		dispatchedCount = i + 1
		wg.Add(1)
		go func(entry planEntry) {
			defer wg.Done()
			defer modelSem.Release()
			defer p.globalSem.Release()

			result, execErr := p.executeOneRequest(requestAbortCtx, sloCtx, inputFile, entry, modelID, passThroughHeaders)
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
			// overwrite the result as batch_cancelled and write to the error file
			// so that output lines + error lines == total requests.
			// SLO expiry does not overwrite in-flight results — only user cancel does.
			if sloCtx.Err() == nil && userCancelCtx.Err() != nil {
				result.Response = nil
				result.Error = &outputError{
					Code:    string(batch_types.ErrCodeBatchCancelled),
					Message: "This request was cancelled while in progress.",
				}
				progress.record(requestAbortCtx, false)

				lineBytes, marshalErr := json.Marshal(result)
				if marshalErr != nil {
					errOnce.Do(func() {
						modelErr = fmt.Errorf("marshal cancelled output line at offset %d: %w", entry.Offset, marshalErr)
					})
					return
				}
				lineBytes = append(lineBytes, '\n')
				if writeErr := writers.write(lineBytes, true); writeErr != nil {
					errOnce.Do(func() { modelErr = fmt.Errorf("write cancelled output line at offset %d: %w", entry.Offset, writeErr) })
				}
				return
			}

			progress.record(requestAbortCtx, result.isSuccess())

			lineBytes, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				errOnce.Do(func() { modelErr = fmt.Errorf("marshal output line at offset %d: %w", entry.Offset, marshalErr) })
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
				errOnce.Do(func() { modelErr = fmt.Errorf("write %s line at offset %d: %w", kind, entry.Offset, writeErr) })
			}
		}(entry)
	}

	wg.Wait()

	// Drain undispatched entries to the error file based on the termination reason, and return the
	// appropriate sentinel so executeJob can route without re-examining context state.
	// Priority: SLO expiry > user cancel > system error > pod shutdown.
	// Use sloCtx.Err() rather than requestAbortCtx.Err(): requestAbortCtx may report Canceled if
	// requestAbortFn() was called by another goroutine before the sloCtx deadline propagated.
	undispatched := entries[dispatchedCount:]
	var returnErr error
	switch {
	case errors.Is(sloCtx.Err(), context.DeadlineExceeded):
		// SLO deadline fired during dispatch — record remaining requests as expired.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("SLO expired: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchExpired)
		}
		returnErr = errExpired

	case userCancelCtx.Err() != nil:
		// User-initiated cancel — record remaining requests as cancelled.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Cancelled: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchCancelled)
		}
		returnErr = errCancelled

	case modelErr != nil:
		// System error in a model goroutine — record remaining requests as failed.
		if len(undispatched) > 0 {
			logger.V(logging.INFO).Info("Fatal error: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchFailed)
		}
		returnErr = modelErr

	default:
		if mainCtx.Err() != nil && len(undispatched) > 0 {
			// Pod shutdown (SIGTERM): main processor context is cancelled.
			// Do not drain here — startup recovery or the re-enqueued worker
			// will process these entries from scratch.
			returnErr = errShutdown
		} else if requestAbortCtx.Err() != nil && len(undispatched) > 0 {
			// Sibling model abort: requestAbortCtx was cancelled by another
			// model's error (requestAbortFn), but this is not SLO/cancel/SIGTERM.
			// Drain undispatched entries as batch_failed so that
			// completed + failed == total holds for the job.
			logger.V(logging.INFO).Info("Sibling abort: draining undispatched entries", "count", len(undispatched))
			p.drainUnprocessedRequests(requestAbortCtx, inputFile, undispatched, writers, progress,
				batch_types.ErrCodeBatchFailed)
		}
	}

	siblingAbort := returnErr == nil && requestAbortCtx.Err() != nil
	logger.V(logging.INFO).Info("Finished processing model", "numEntries", len(entries), "hasError", returnErr != nil, "siblingAbort", siblingAbort)
	return returnErr
}

// drainUnprocessedRequests records undispatched requests in the error file when a job terminates
// mid-execution (SLO expiry, cancellation, or systemic failure). For each plan entry, it reads
// the original request from input.jsonl to extract the custom_id, then writes an error line with
// the given error code and its canonical message.
func (p *Processor) drainUnprocessedRequests(
	ctx context.Context,
	inputFile *os.File,
	entries []planEntry,
	writers *outputWriters,
	progress *executionProgress,
	errCode batch_types.BatchErrorCode,
) {
	errMessage := errCode.Message()
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
				Code:    string(errCode),
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

const (
	sloTTFTMSHeader          = "x-slo-ttft-ms"
	inferenceObjectiveHeader = "x-gateway-inference-objective"
)

// mergeInferenceHeaders adds processor-managed headers to the outgoing inference request:
//   - x-slo-ttft-ms: remaining milliseconds until the SLO deadline (>= 0).
//   - x-gateway-inference-objective: name of the InferenceObjective CRD that
//     determines the priority band for this request.
//
// Headers are only added when the relevant value is available/configured.
// If sloCtx has no deadline, is cancelled, or has an expired deadline, the SLO
// header is not set. If inferenceObjective is empty, the objective header is not set.
func mergeInferenceHeaders(headers map[string]string, sloCtx context.Context, inferenceObjective string) map[string]string {
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

	if !hasSLO && !hasObjective {
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
		return nil, fmt.Errorf("%w at offset %d: %w", errRequestInputRead, entry.Offset, err)
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
	headers = mergeInferenceHeaders(headers, sloCtx, p.cfg.InferenceObjective)

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
				Code:    string(batch_types.ErrCodeBatchExpired),
				Message: batch_types.ErrCodeBatchExpired.Message(),
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
