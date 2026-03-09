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
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
	"github.com/llm-d-incubation/batch-gateway/internal/util/semaphore"
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
		_, err := w.errors.Write(line)
		w.errorsMu.Unlock()
		return err
	}
	w.outputMu.Lock()
	_, err := w.output.Write(line)
	w.outputMu.Unlock()
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

// executionProgress tracks per-request progress across goroutines
// and pushes lightweight updates to the status store after every request.
type executionProgress struct {
	completed atomic.Int64
	failed    atomic.Int64
	total     int64
	updater   *StatusUpdater
	jobID     string
}

func (ep *executionProgress) record(ctx context.Context, success bool) {
	if success {
		ep.completed.Add(1)
	} else {
		ep.failed.Add(1)
	}
	// best-effort: status store failure should not block request processing
	_ = ep.updater.UpdateProgressCounts(ctx, ep.jobID, &openai.BatchRequestCounts{
		Total:     ep.total,
		Completed: ep.completed.Load(),
		Failed:    ep.failed.Load(),
	})
}

func (ep *executionProgress) counts() *openai.BatchRequestCounts {
	return &openai.BatchRequestCounts{
		Total:     ep.total,
		Completed: ep.completed.Load(),
		Failed:    ep.failed.Load(),
	}
}

// executeJob performs phase 2: reads plan files per model, sends inference
// requests concurrently (one goroutine per model), and writes results to
// output.jsonl (successes) and error.jsonl (failures). Returns request counts for finalization.
func (p *Processor) executeJob(
	ctx context.Context,
	updater *StatusUpdater,
	jobInfo *batch_types.JobInfo,
	cancelRequested *atomic.Bool,
) (*openai.BatchRequestCounts, error) {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Starting Phase 2: executing job")

	jobRootDir, err := p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve job root directory: %w", err)
	}

	modelMap, err := readModelMap(jobRootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read model map: %w", err)
	}

	inputFilePath, err := p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open input file: %w", err)
	}
	defer inputFile.Close()

	outputFilePath, err := p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	outputFile, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	errorFilePath, err := p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, err
	}
	errorFile, err := os.OpenFile(errorFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create error file: %w", err)
	}
	defer errorFile.Close()

	writers := &outputWriters{
		output: bufio.NewWriterSize(outputFile, 1024*1024),
		errors: bufio.NewWriterSize(errorFile, 1024*1024),
	}

	plansDir, err := p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, err
	}

	// one goroutine per model; concurrency within each model is bounded
	// by globalSem (processor-wide concurrency limit) and perModelMaxConcurrency (per-model concurrency limit)
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()

	progress := &executionProgress{
		total:   modelMap.LineCount,
		updater: updater,
		jobID:   jobInfo.JobID,
	}

	errCh := make(chan error, len(modelMap.SafeToModel))

	passThroughHeaders := jobInfo.PassThroughHeaders
	if len(passThroughHeaders) > 0 {
		headerNames := make([]string, 0, len(passThroughHeaders))
		for k := range passThroughHeaders {
			headerNames = append(headerNames, k)
		}
		logger.V(logging.DEBUG).Info("pass-through headers attached to job", "headerNames", headerNames)
	}

	for safeModelID, modelID := range modelMap.SafeToModel {
		go func(safeModelID, modelID string) {
			err := p.processModel(
				execCtx,
				inputFile,
				plansDir, safeModelID, modelID,
				writers,
				cancelRequested,
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

	if firstErr != nil {
		// prefer parent-context / user-cancel errors for correct routing in handleJobError
		if ctx.Err() != nil {
			return nil, ctx.Err() // parent-context error
		}
		if cancelRequested.Load() {
			return nil, ErrCancelled // user-cancel error
		}
		// process error from model goroutines
		return nil, firstErr
	}

	if err := writers.output.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush output file: %w", err)
	}
	if err := writers.errors.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush error file: %w", err)
	}

	counts := progress.counts()
	logger.V(logging.INFO).Info("Phase 2: execution completed",
		"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)

	return counts, nil
}

// processModel processes all plan entries for a single model concurrently.
// Concurrency is bounded by both a global semaphore (p.globalSem, shared across
// all models/workers) and a per-model semaphore (PerModelMaxConcurrency).
//
// Semaphore acquisition order: local (per-model) before global (shared).
// This prevents starving other models — blocking on global only wastes a local slot.
//
// Error strategy in this function: when a goroutine encounters a fatal error, firstErr is captured
// via errOnce but the context is NOT cancelled within this function. Already-dispatched
// goroutines run to completion. Context cancellation is propagated at the executeJob level
// (execCancel), which stops dispatch across all models.
func (p *Processor) processModel(
	ctx context.Context,
	inputFile *os.File,
	plansDir, safeModelID, modelID string,
	writers *outputWriters,
	cancelRequested *atomic.Bool,
	progress *executionProgress,
	passThroughHeaders map[string]string,
) error {
	logger := klog.FromContext(ctx).WithValues("model", modelID)
	ctx = klog.NewContext(ctx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("failed to read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for a model", "numEntries", len(entries))

	modelSem, err := semaphore.New(p.cfg.PerModelMaxConcurrency)
	if err != nil {
		return fmt.Errorf("failed to create model semaphore: %w", err)
	}

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)

dispatch:
	for _, entry := range entries {
		if err := checkCancellation(ctx, cancelRequested); err != nil {
			errOnce.Do(func() { firstErr = err })
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

		wg.Add(1)
		go func(entry planEntry) {
			defer wg.Done()
			defer modelSem.Release()
			defer p.globalSem.Release()

			result, execErr := p.executeOneRequest(ctx, inputFile, entry, modelID, passThroughHeaders)
			if execErr != nil {
				logger.Error(execErr, "Fatal error executing request", "offset", entry.Offset)
				errOnce.Do(func() { firstErr = execErr })
				return
			}

			progress.record(ctx, result.Error == nil)

			lineBytes, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				logger.Error(marshalErr, "Failed to marshal output line", "offset", entry.Offset)
				errOnce.Do(func() { firstErr = fmt.Errorf("failed to marshal output line: %w", marshalErr) })
				return
			}
			lineBytes = append(lineBytes, '\n')

			// Write to error file if the result has an error, otherwise to output file.
			isError := result.Error != nil
			if writeErr := writers.write(lineBytes, isError); writeErr != nil {
				kind := "output"
				if isError {
					kind = "error"
				}
				logger.Error(writeErr, "Failed to write line", "kind", kind, "offset", entry.Offset)
				errOnce.Do(func() { firstErr = fmt.Errorf("failed to write %s line: %w", kind, writeErr) })
			}
		}(entry)
	}

	wg.Wait()

	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}

	logger.V(logging.INFO).Info("Finished processing model", "numEntries", len(entries), "hasError", firstErr != nil)
	return firstErr
}

// executeOneRequest reads a single input line from the input file at the given plan entry offset,
// sends it to the inference gateway, and returns the formatted output line.
func (p *Processor) executeOneRequest(
	ctx context.Context,
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

	// parse the request line into a batch_types.Request object
	var req batch_types.Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		klog.FromContext(ctx).Error(err, "failed to parse request line, recording as error")
		return &outputLine{
			ID: fmt.Sprintf("batch_req_%s", uuid.NewString()),
			Error: &outputError{
				Code:    string(inference.ErrCategoryParse),
				Message: fmt.Sprintf("failed to parse request line: %v", err),
			},
		}, nil
	}

	requestID := uuid.NewString()
	// model id, job id and tenant id are already set in the context
	logger := klog.FromContext(ctx).WithValues("customId", req.CustomID, "requestId", requestID)

	inferReq := &inference.GenerateRequest{
		RequestID: requestID,
		Endpoint:  req.URL,
		Params:    req.Body,
		Headers:   passThroughHeaders,
	}

	start := time.Now()
	metrics.IncProcessorInflightRequests()
	metrics.IncModelInflightRequests(modelID)
	logger.V(logging.TRACE).Info("Dispatching inference request")

	inferClient := p.clients.Inference.ClientFor(modelID)
	inferResp, inferErr := inferClient.Generate(ctx, inferReq)

	metrics.DecModelInflightRequests(modelID)
	metrics.DecProcessorInflightRequests()
	metrics.RecordModelRequestExecutionDuration(time.Since(start), modelID)

	result := &outputLine{
		ID:       fmt.Sprintf("batch_req_%s", uuid.NewString()),
		CustomID: req.CustomID,
	}

	// response handling by case
	if inferErr != nil {
		// error is returned by the inference client
		logger.V(logging.DEBUG).Info("Inference request failed", "error", inferErr.Message)
		result.Error = &outputError{
			Code:    string(inferErr.Category),
			Message: inferErr.Message,
		}
	} else if inferResp == nil {
		// ok status without error but no response
		logger.Error(nil, "inference returned no error but response is nil")
		result.Error = &outputError{
			Code:    string(inference.ErrCategoryServer),
			Message: "inference returned no error but response is nil",
		}
	} else {
		// success — unmarshal the response body
		var body map[string]interface{}
		if len(inferResp.Response) > 0 {
			if err := json.Unmarshal(inferResp.Response, &body); err != nil {
				// failed to unmarshal the response body
				logger.Error(err, "failed to unmarshal inference response body")
				result.Error = &outputError{
					Code:    string(inference.ErrCategoryParse),
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
		}
	}

	if result.Error != nil {
		metrics.RecordRequestError(modelID)
	}
	return result, nil
}

// finalizeJob performs phase 3: uploads output and error files to shared storage,
// creates file records in the database, and updates job status to completed.
func (p *Processor) finalizeJob(
	ctx context.Context,
	updater *StatusUpdater,
	dbJob *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Starting Phase 3: finalizing job")

	// in_progress → finalizing
	if err := updater.UpdatePersistentStatus(ctx, dbJob, openai.BatchStatusFinalizing, requestCounts, nil); err != nil {
		return fmt.Errorf("failed to update job status to finalizing: %w", err)
	}

	// Per the OpenAI batch spec, output_file_id and error_file_id are both optional:
	// output_file_id is omitted when all requests failed; error_file_id is omitted when no
	// requests failed. We skip uploading and recording empty files accordingly.
	var outputFileID string
	outputFileName := jobOutputStorageName(jobInfo.JobID)
	outputFileSize, err := p.uploadOutputFile(ctx, jobInfo, outputFileName)
	if err != nil {
		return err
	}
	if outputFileSize > 0 {
		outputFileID = ucom.NewFileID()
		uotel.SetAttr(ctx, attribute.String(uotel.AttrOutputFileID, outputFileID))
		if err := p.storeFileRecord(ctx, outputFileID, outputFileName, jobInfo.TenantID, outputFileSize, dbJob.Tags); err != nil {
			return err
		}
	}

	var errorFileID string
	errorFileName := jobErrorStorageName(jobInfo.JobID)
	errorFileSize, err := p.uploadErrorFile(ctx, jobInfo, errorFileName)
	if err != nil {
		return err
	}
	if errorFileSize > 0 {
		errorFileID = ucom.NewFileID()
		uotel.SetAttr(ctx, attribute.String(uotel.AttrErrorFileID, errorFileID))
		if err := p.storeFileRecord(ctx, errorFileID, errorFileName, jobInfo.TenantID, errorFileSize, dbJob.Tags); err != nil {
			return err
		}
	}

	// finalizing → completed
	if err := updater.UpdateCompletedStatus(ctx, dbJob, requestCounts, outputFileID, errorFileID); err != nil {
		return fmt.Errorf("failed to update job status to completed: %w", err)
	}

	uotel.SetAttr(ctx,
		attribute.Int64(uotel.AttrRequestTotal, requestCounts.Total),
		attribute.Int64(uotel.AttrRequestCompleted, requestCounts.Completed),
		attribute.Int64(uotel.AttrRequestFailed, requestCounts.Failed),
	)

	logger.V(logging.INFO).Info("Phase 3: finalization completed", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// uploadOutputFile uploads the local output file to shared storage with retry.
// Returns the file size; returns 0 without error if the file is empty (all requests failed).
// Per the OpenAI batch spec, output_file_id is optional and may be omitted when there are no
// successful requests.
func (p *Processor) uploadOutputFile(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	fileName string,
) (int64, error) {
	filePath, err := p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return 0, err
	}
	return p.uploadJobFile(ctx, filePath, fileName, jobInfo.TenantID, metrics.FileTypeOutput)
}

// uploadErrorFile uploads the local error file to shared storage with retry.
// Returns the file size; returns 0 without error if the file is empty (no errors occurred).
// Per the OpenAI batch spec, error_file_id is optional and may be omitted when no requests failed.
func (p *Processor) uploadErrorFile(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	fileName string,
) (int64, error) {
	filePath, err := p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return 0, err
	}
	return p.uploadJobFile(ctx, filePath, fileName, jobInfo.TenantID, metrics.FileTypeError)
}

// uploadJobFile uploads a local file to shared storage with retry.
// Returns the file size; returns 0 without error if the file does not exist or is empty.
func (p *Processor) uploadJobFile(
	ctx context.Context,
	filePath, fileName, tenantID string,
	fileType metrics.FileType,
) (int64, error) {
	logger := klog.FromContext(ctx)

	stat, err := os.Stat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to stat file %s: %w", filePath, err)
	}
	if stat.Size() == 0 {
		return 0, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open file %s for upload: %w", filePath, err)
	}
	defer f.Close()

	folderName, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		return 0, fmt.Errorf("failed to get folder name: %w", err)
	}

	retryCfg := p.cfg.UploadRetry
	maxAttempts := retryCfg.MaxRetries + 1

	// TODO: distinguish retryable (network/storage transient) vs non-retryable (auth, permission)
	// errors and skip retries for the latter. Deferred until we have more storage backends
	// or see real non-transient failures in production.
	fileMeta, err := p.clients.File.Store(ctx, fileName, folderName, 0, 0, f)
	for attempt := 1; err != nil && attempt < maxAttempts; attempt++ {
		metrics.RecordFileUploadRetry(fileType)
		backoff := min(retryCfg.InitialBackoff*(1<<(attempt-1)), retryCfg.MaxBackoff)
		logger.V(logging.WARNING).Info("Retrying file upload",
			"file", fileName, "attempt", attempt+1, "maxAttempts", maxAttempts, "backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("upload retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}

		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return 0, fmt.Errorf("failed to seek file %s for retry: %w", fileName, seekErr)
		}
		fileMeta, err = p.clients.File.Store(ctx, fileName, folderName, 0, 0, f)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to upload file %s after %d attempts: %w", fileName, maxAttempts, err)
	}

	return fileMeta.Size, nil
}

// storeFileRecord creates a file metadata record in the database.
// Used for both output and error files.
// If the batch has a user-provided output_expires_after_seconds tag, it takes
// precedence over the config default (DefaultOutputExpirationSeconds).
func (p *Processor) storeFileRecord(
	ctx context.Context,
	fileID, fileName, tenantID string,
	size int64,
	batchTags db.Tags,
) error {
	now := time.Now().Unix()

	expiresAt := p.resolveOutputExpiration(now, batchTags)

	fileObj := &openai.FileObject{
		ID:        fileID,
		Bytes:     size,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		Filename:  fileName,
		Object:    "file",
		Purpose:   openai.FileObjectPurposeBatchOutput,
		Status:    openai.FileObjectStatusProcessed,
	}
	fileItem, err := converter.FileToDBItem(fileObj, tenantID, db.Tags{})
	if err != nil {
		return fmt.Errorf("failed to convert file to db item: %w", err)
	}

	if err := p.clients.FileDB.DBStore(ctx, fileItem); err != nil {
		return fmt.Errorf("failed to store file record: %w", err)
	}
	return nil
}

// resolveOutputExpiration returns the ExpiresAt timestamp for an output or error file.
// Priority: user-provided output_expires_after_seconds tag > config default.
// Returns 0 (no expiration) if neither is set.
func (p *Processor) resolveOutputExpiration(now int64, batchTags db.Tags) int64 {
	if s, ok := batchTags[batch_types.TagOutputExpiresAfterSeconds]; ok {
		if ttl, err := strconv.ParseInt(s, 10, 64); err == nil && ttl > 0 {
			return now + ttl
		}
	}
	if ttl := p.cfg.DefaultOutputExpirationSeconds; ttl > 0 {
		return now + ttl
	}
	return 0
}
