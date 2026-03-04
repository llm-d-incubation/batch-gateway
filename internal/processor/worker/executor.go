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
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
)

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
// requests concurrently (one goroutine per model), and writes results to the
// output JSONL file. Returns request counts for finalization.
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

	writer := bufio.NewWriterSize(outputFile, 1024*1024)

	plansDir, err := p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return nil, err
	}

	// run one goroutine per model for per-model processing, collect errors via channel
	var writerMu sync.Mutex
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()

	progress := &executionProgress{
		total:   modelMap.LineCount,
		updater: updater,
		jobID:   jobInfo.JobID,
	}

	errCh := make(chan error, len(modelMap.SafeToModel))

	// TODO: current implementation runs one goroutine per model without scheduling.
	// Design calls for:
	//   - Round-robin model selection with model_request_budget per turn
	//   - GlobalConcurrency (max in-flight requests per job)
	//   - PerModelConcurrency (max in-flight requests per model)
	//   - Starvation prevention: models drained mid-turn are removed from rotation
	// See: docs/design/batch_processor_architecture.md (Phase 2 Scheduling)
	passThroughHeaders := jobInfo.PassThroughHeaders
	if len(passThroughHeaders) > 0 {
		headerNames := make([]string, 0, len(passThroughHeaders))
		for k := range passThroughHeaders {
			headerNames = append(headerNames, k)
		}
		logger.V(logging.DEBUG).Info("pass-through headers attached to job", "headerNames", headerNames)
	}

	// jobInfo.JobID holds the batch ID (e.g. "batch_<uuid>")
	for safeModelID, modelID := range modelMap.SafeToModel {
		go func(safeModelID, modelID string) {
			err := p.processModel(execCtx, inputFile, plansDir, safeModelID, modelID, writer, &writerMu, cancelRequested, progress, passThroughHeaders, jobInfo.JobID)
			if err != nil {
				execCancel()
			}
			errCh <- err
		}(safeModelID, modelID)
	}

	var firstErr error
	// collect errors from all model goroutines
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

	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush output file: %w", err)
	}

	counts := progress.counts()
	logger.V(logging.INFO).Info("Phase 2: execution completed",
		"total", counts.Total, "completed", counts.Completed, "failed", counts.Failed)

	return counts, nil
}

// processModel processes all plan entries for a single model.
// It writes output lines to the shared writer under writerMu and
// updates progress after every request.
func (p *Processor) processModel(
	ctx context.Context,
	inputFile *os.File,
	plansDir, safeModelID, modelID string,
	writer *bufio.Writer,
	writerMu *sync.Mutex,
	cancelRequested *atomic.Bool,
	progress *executionProgress,
	passThroughHeaders map[string]string,
	batchID string,
) error {
	logger := klog.FromContext(ctx).WithValues("model", modelID)
	ctx = klog.NewContext(ctx, logger)

	planPath := filepath.Join(plansDir, safeModelID+".plan")
	entries, err := readPlanEntries(planPath)
	if err != nil {
		return fmt.Errorf("failed to read plan for model %s: %w", modelID, err)
	}

	logger.V(logging.INFO).Info("Processing requests for a model", "entries", len(entries))

	// process each plan entry sequentially
	// TODO: use concurrent processing with bounded concurrency (global concurrency and per-model concurrency)
	for _, entry := range entries {
		if err := checkCancellation(ctx, cancelRequested); err != nil {
			return err
		}

		result, execErr := p.executeOneRequest(ctx, inputFile, entry, modelID, passThroughHeaders, batchID)
		if execErr != nil {
			return execErr
		}

		// update progress after every request
		// TODO: update progress every certain number of requests?
		progress.record(ctx, result.Error == nil)

		lineBytes, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("failed to marshal output line: %w", err)
		}
		lineBytes = append(lineBytes, '\n')

		// shared writer under writerMu
		writerMu.Lock()
		_, writeErr := writer.Write(lineBytes)
		writerMu.Unlock()
		if writeErr != nil {
			return fmt.Errorf("failed to write output line: %w", writeErr)
		}
	}

	return nil
}

// executeOneRequest reads a single input line from the input file at the given plan entry offset,
// sends it to the inference gateway, and returns the formatted output line.
func (p *Processor) executeOneRequest(
	ctx context.Context,
	inputFile *os.File,
	entry planEntry,
	modelID string,
	passThroughHeaders map[string]string,
	batchID string,
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

	// Add batch.id to the current span so the otelhttp transport child span can be correlated
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(uotel.AttrBatchID, batchID))

	inferResp, inferErr := p.clients.Inference.Generate(ctx, inferReq)

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
		logger.V(logging.TRACE).Info("Inference request failed", "error", inferErr.Message)
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

// finalizeJob performs phase 3: uploads the output file to file storage,
// creates a file record in the database, and updates job status to completed.
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

	outputFileID := fmt.Sprintf("file_%s", uuid.NewString())
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(uotel.AttrOutputFileID, outputFileID))
	outputFileName := fmt.Sprintf("batch_output_%s.jsonl", jobInfo.JobID)

	fileSize, err := p.uploadOutputFile(ctx, jobInfo, outputFileName)
	if err != nil {
		return err
	}

	if err := p.storeOutputFileRecord(ctx, outputFileID, outputFileName, jobInfo.TenantID, fileSize, dbJob.Tags); err != nil {
		return err
	}

	// finalizing → completed
	if err := updater.UpdateCompletedStatus(ctx, dbJob, requestCounts, outputFileID); err != nil {
		return fmt.Errorf("failed to update job status to completed: %w", err)
	}

	logger.V(logging.INFO).Info("Phase 3: finalization completed", "outputFileID", outputFileID)
	return nil
}

// uploadOutputFile uploads the local output file to shared storage with retry.
func (p *Processor) uploadOutputFile(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	outputFileName string,
) (int64, error) {
	logger := klog.FromContext(ctx)

	outputFilePath, err := p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return 0, err
	}

	outputFile, err := os.Open(outputFilePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open output file for upload: %w", err)
	}
	defer outputFile.Close()

	folderName, err := ucom.GetFolderNameByTenantID(jobInfo.TenantID)
	if err != nil {
		return 0, fmt.Errorf("failed to get folder name: %w", err)
	}

	retryCfg := p.cfg.UploadRetry
	maxAttempts := retryCfg.MaxRetries + 1

	fileMeta, err := p.clients.File.Store(ctx, outputFileName, folderName, 0, 0, outputFile)
	for attempt := 1; err != nil && attempt < maxAttempts; attempt++ {
		backoff := min(retryCfg.InitialBackoff*(1<<(attempt-1)), retryCfg.MaxBackoff)
		logger.V(logging.WARNING).Info("Retrying output file upload",
			"attempt", attempt+1, "maxAttempts", maxAttempts, "backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("upload retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}

		if _, seekErr := outputFile.Seek(0, io.SeekStart); seekErr != nil {
			return 0, fmt.Errorf("failed to seek output file for retry: %w", seekErr)
		}
		fileMeta, err = p.clients.File.Store(ctx, outputFileName, folderName, 0, 0, outputFile)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to upload output file after %d attempts: %w", maxAttempts, err)
	}

	return fileMeta.Size, nil
}

// storeOutputFileRecord creates a file metadata record in the database.
// If the batch has a user-provided output_expires_after_seconds tag, it takes
// precedence over the config default (DefaultOutputExpirationSeconds).
func (p *Processor) storeOutputFileRecord(
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
		return fmt.Errorf("failed to store output file record: %w", err)
	}
	return nil
}

// resolveOutputExpiration returns the ExpiresAt timestamp for an output file.
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
