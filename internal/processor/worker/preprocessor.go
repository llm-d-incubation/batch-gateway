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
	"hash/fnv"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
)

// preProcessJob performs the pre-processing steps for the job.
// It downloads the input file, creates per-model plan files, and writes
// error entries for requests targeting unregistered models.
// The rejected count is persisted in model_map.json so executeJob can
// seed BatchRequestCounts.Failed without an extra parameter.
func (p *Processor) preProcessJob(ctx context.Context, jobInfo *batch_types.JobInfo, cancelRequested *atomic.Bool) error {
	logger := logr.FromContextOrDiscard(ctx)
	logger.V(logging.INFO).Info("Pre-processing job") // job id is in the logger already
	planBuildStart := time.Now()
	jobID := jobInfo.JobID
	inputFileID := jobInfo.BatchJob.InputFileID
	if inputFileID == "" {
		err := fmt.Errorf("input file ID is empty")
		logger.Error(err, "Input file ID is empty")
		return err
	}

	jobRootDir, err := p.jobRootDir(jobID, jobInfo.TenantID)
	if err != nil {
		logger.Error(err, "Failed to resolve job root directory")
		return err
	}

	// job directory creation
	if err := os.MkdirAll(jobRootDir, 0o700); err != nil {
		logger.Error(err, "Failed to create job root directory", "jobRootDir", jobRootDir)
		return err
	}

	// input file stream open
	reader, metadata, err := p.openInputFileStream(ctx, inputFileID)
	if err != nil {
		logger.Error(err, "Failed to open input file stream", "inputFileId", inputFileID)
		return err
	}
	defer reader.Close()

	if metadata != nil {
		logger.V(logging.INFO).Info("Input file metadata", "metadata", metadata)
	}

	// create local input file
	localInputFile, localInputFilePath, err := p.createLocalInputFile(jobID, jobInfo.TenantID)
	if err != nil {
		logger.Error(err, "Failed to create local input file", "path", localInputFilePath)
		return err
	}
	defer localInputFile.Close()

	writer := bufio.NewWriterSize(localInputFile, 1024*1024)

	acc := newPlanAccumulator(jobRootDir)

	// model intern tables
	used := make(map[string]int)           // to prevent duplicate model IDs
	modelToSafe := make(map[string]string) // to map the model ID to a safe file name

	seenCustomIDs := make(map[string]struct{})

	// streaming loop
	// In per-model mode, check each model against the resolver and reject
	// unregistered models early. In global mode, all models are routed to
	// the same endpoint so no check is needed.
	// p.inference != nil: NewProcessor does not validate clients; unit tests often
	// call preProcessJob without Run() and omit Inference (treat as non-per-model).
	// Production paths hit Processor.validate() in prepare() before work runs.
	// The guard also avoids panicking if a future caller wires a nil resolver.
	isPerModelGateway := p.inference != nil && !p.inference.IsGlobal()
	registeredModels := make(map[string]bool) // modelID -> registered (per-model only)

	// Always truncate error.jsonl at the start of ingestion so that re-enqueued
	// jobs don't carry stale error entries from a previous attempt.
	// Execution opens the same file in append mode.
	// In global mode this creates an empty file that finalization omits (size 0).
	errorFilePath, err := p.jobErrorFilePath(jobID, jobInfo.TenantID)
	if err != nil {
		return err
	}
	errorFile, err := os.OpenFile(errorFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create error file: %w", err)
	}
	errorWriter := bufio.NewWriter(errorFile)
	defer func() {
		_ = errorWriter.Flush()
		errorFile.Close()
	}()

	var offset int64
	var lineCount int64 // to count the number of lines in the input file for logging
	var rejectedCount int64
	inputFileReader := bufio.NewReaderSize(reader, 1024*1024)

	for {
		// Ingestion uses the parent ctx (not abortCtx), so user-cancel signals do not
		// propagate through the context tree. Check cancelRequested explicitly.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cancelRequested.Load() {
			logger.V(logging.INFO).Info("preProcess: cancel requested")
			return ErrCancelled
		}

		// read a line from the input file
		line, done, err := readNormalizedLine(inputFileReader)
		if err != nil {
			// if error occurs, fail the pre-processing and the job
			logger.Error(err, "Failed to read line from input file")
			return err
		}
		if done {
			break
		}

		lineCount++

		// write the line to the input file.
		if _, err := writer.Write(line); err != nil {
			logger.Error(err, "Failed to write line to input file", "path", localInputFilePath, "lineCount", lineCount)
			return err
		}

		requestMeta, err := extractAndValidateLine(line)
		if err != nil {
			logger.Error(err, "Failed to validate request line", "lineCount", lineCount)
			return err
		}

		if _, exists := seenCustomIDs[requestMeta.CustomID]; exists {
			err := fmt.Errorf("line %d: duplicate custom_id %q", lineCount, requestMeta.CustomID)
			logger.Error(err, "Duplicate custom_id in batch input")
			return err
		}
		seenCustomIDs[requestMeta.CustomID] = struct{}{}

		if isPerModelGateway {
			registered, checked := registeredModels[requestMeta.ModelID]
			if !checked {
				registered = p.inference.ClientFor(requestMeta.ModelID) != nil
				registeredModels[requestMeta.ModelID] = registered
			}
			if !registered {
				// No plan entry exists yet, so generate a UUID for the batch request ID.
				// newBatchRequestID adds the "batch_req_" prefix for format consistency.
				errLine := &outputLine{
					ID:       newBatchRequestID(uuid.NewString()),
					CustomID: requestMeta.CustomID,
					Error: &outputError{
						Code:    inference.ErrCodeModelNotFound,
						Message: fmt.Sprintf("model %q is not configured in any gateway", requestMeta.ModelID),
					},
				}
				lineBytes, marshalErr := json.Marshal(errLine)
				if marshalErr != nil {
					return fmt.Errorf("failed to marshal model_not_found error: %w", marshalErr)
				}
				lineBytes = append(lineBytes, '\n')
				if _, writeErr := errorWriter.Write(lineBytes); writeErr != nil {
					return fmt.Errorf("failed to write model_not_found error: %w", writeErr)
				}
				rejectedCount++
				metrics.RecordRequestError(requestMeta.ModelID)
				logger.V(logging.DEBUG).Info("Rejected request for unregistered model",
					"customId", requestMeta.CustomID, "model", requestMeta.ModelID)
				offset += int64(len(line))
				continue
			}
		}

		nextOffset := accumulatePlanEntry(
			acc, requestMeta.ModelID, modelToSafe, used, offset, uint32(len(line)), requestMeta.PrefixHash,
		)
		offset = nextOffset
	}

	// flush input.jsonl file
	if err := writer.Flush(); err != nil {
		logger.Error(err, "Failed to flush input file", "path", localInputFilePath)
		return err
	}

	if err := finalizePlanFiles(acc, modelToSafe); err != nil {
		logger.Error(err, "Failed to finalize plan files")
		return err
	}

	// model map file writing
	if err := writeModelMappings(jobRootDir, modelToSafe, lineCount, rejectedCount); err != nil {
		logger.Error(err, "Failed to write model map file")
		return err
	}

	metrics.RecordPlanBuildDuration(time.Since(planBuildStart), metrics.GetSizeBucket(int(lineCount)))
	modelCounts := make(map[string]int, len(modelToSafe))
	for model, safe := range modelToSafe {
		modelCounts[model] = len(acc.entries[safe])
	}
	logger.V(logging.INFO).Info("Processor Pre-processing job completed",
		"inputFilePath", localInputFilePath, "planFilePath", acc.plansDir(),
		"lineCount", lineCount, "rejected", rejectedCount, "models", modelCounts)

	return nil
}

// checkAbortCondition returns a non-nil error if dispatch should stop because the context
// is done (cancelled, deadline exceeded, or SLO deadline). It does NOT check the
// cancelRequested flag; that flag is only consulted in the error-handling path to
// distinguish the cancellation reason (user cancel vs SLO vs pod shutdown).
func checkAbortCondition(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// readNormalizedLine reads the next line from the reader, ensuring it ends with '\n'.
// Returns (line, eof, err): line is the normalized bytes, eof is true when input is exhausted.
func readNormalizedLine(r *bufio.Reader) ([]byte, bool, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	if len(line) == 0 && err == io.EOF {
		return nil, true, nil
	}
	// if last line is not terminated with '\n', append '\n' to the line
	if line[len(line)-1] != '\n' {
		line = append(line, '\n')
	}
	return line, false, nil
}

type requestMeta struct {
	CustomID   string
	ModelID    string
	PrefixHash uint32
}

// extractAndValidateLine parses and validates a request line and returns the
// metadata needed during ingestion.
func extractAndValidateLine(line []byte) (requestMeta, error) {
	var req planRequestLine
	trimmedLine := bytes.TrimSuffix(line, []byte{'\n'})
	if err := json.Unmarshal(trimmedLine, &req); err != nil {
		return requestMeta{}, err
	}
	if req.CustomID == "" {
		return requestMeta{}, fmt.Errorf("custom_id is required")
	}
	if req.Body.Model == "" {
		return requestMeta{}, fmt.Errorf("model id is empty")
	}
	if req.Body.Stream != nil && *req.Body.Stream {
		return requestMeta{}, fmt.Errorf("streaming is not supported in batch requests (model: %s)", req.Body.Model)
	}

	prefixHash := NoPrefixHash
	for _, msg := range req.Body.Messages {
		if msg.Role == "system" && msg.Content != "" {
			h := fnv.New32a()
			h.Write([]byte(msg.Content))
			prefixHash = h.Sum32()
			break
		}
	}

	return requestMeta{
		CustomID:   req.CustomID,
		ModelID:    req.Body.Model,
		PrefixHash: prefixHash,
	}, nil
}

func writeModelMappings(jobRootDir string, modelToSafe map[string]string, lineCount, rejectedCount int64) error {
	safeToModel := make(map[string]string, len(modelToSafe))
	for modelID, safeID := range modelToSafe {
		safeToModel[safeID] = modelID
	}

	modelMap := modelMapFile{
		ModelToSafe:   modelToSafe,
		SafeToModel:   safeToModel,
		LineCount:     lineCount,
		RejectedCount: rejectedCount,
	}
	return writeModelMapFile(jobRootDir, modelMap)
}
