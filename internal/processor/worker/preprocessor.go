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
	"sync/atomic"

	"k8s.io/klog/v2"

	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

// preProcessJob performs the pre-processing steps for the job
// it downloads the input file from the files store in job work folder : <tenantID>/jobs/<jobid>/input.jsonl,
// creates the plan per model, while saving the input file in the work folder.
// temp plan file is saved in the work folder's subfolder while creating the plan (<tenantID>/jobs/<jobid>/plans/<modelid>.plan.tmp)
// then the temp plan file is renamed to the final plan file (<tenantID>/jobs/<jobid>/plans/<modelid>.plan)
func (p *Processor) preProcessJob(ctx context.Context, jobInfo *batch_types.JobInfo, cancelRequested *atomic.Bool) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Pre-processing job") // job id is in the logger already
	jobID := jobInfo.JobID
	inputFileID := jobInfo.BatchJob.BatchSpec.InputFileID
	if inputFileID == "" {
		err := fmt.Errorf("input file ID is empty")
		logger.V(logging.ERROR).Error(err, "Input file ID is empty")
		return err
	}

	jobRootDir, err := p.jobRootDir(jobID, jobInfo.TenantID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to resolve job root directory")
		return err
	}

	// job directory creation
	if err := os.MkdirAll(jobRootDir, 0o700); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to create job root directory", "jobRootDir", jobRootDir)
		return err
	}

	// input file stream open
	reader, metadata, err := p.openInputFileStream(ctx, inputFileID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to open input file stream", "inputFileId", inputFileID)
		return err
	}
	defer reader.Close()

	if metadata != nil {
		logger.V(logging.INFO).Info("Input file metadata", "metadata", metadata)
	}

	// create local input file
	localInputFile, localInputFilePath, err := p.createLocalInputFile(jobID, jobInfo.TenantID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to create local input file", "path", localInputFilePath)
		return err
	}
	defer localInputFile.Close()

	// copy input file stream to local input file
	writer := bufio.NewWriterSize(localInputFile, 1024*1024)

	// plan writer creation
	planWriter := newPlanWriter(jobRootDir, p.cfg.MaxOpenFiles)
	defer func() {
		// best-effort
		_ = planWriter.CloseAll()
	}()

	// model intern tables
	used := make(map[string]int)           // to prevent duplicate model IDs
	modelToSafe := make(map[string]string) // to map the model ID to a safe file name

	// streaming loop
	var offset int64
	var lineCount int64 // to count the number of lines in the input file for logging
	inputFileReader := bufio.NewReaderSize(reader, 1024*1024)

	for {
		// cancellation checks
		if err := checkPreProcessCancellation(ctx, cancelRequested); err != nil {
			if err == ErrCancelled {
				logger.V(logging.INFO).Info("preProcess: cancel requested")
			}
			return err
		}

		// read a line from the input file
		line, done, err := readNormalizedLine(inputFileReader)
		if err != nil {
			// if error occurs, fail the pre-processing and the job
			// TODO: we might want to handle partial failure and continue to the next line in the future
			//       with line writing error / plan entry append error below
			logger.V(logging.ERROR).Error(err, "Failed to read line from input file")
			return err
		}
		if done {
			break
		}

		lineCount++

		// write the line to the input file.
		if _, err := writer.Write(line); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to write line to input file", "path", localInputFilePath, "lineCount", lineCount)
			return err
		}

		// parse line and extract model id
		modelID, err := extractModelID(line)
		if err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to unmarshal request line", "lineCount", lineCount)
			return err
		}

		// plan entry append
		nextOffset, safeModelID, length, err := appendPlanEntryForModel(
			planWriter, modelID, modelToSafe, used, offset, line,
		)
		if err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to append plan entry", "modelID", modelID, "safeModelID", safeModelID, "offset", offset, "length", length, "lineCount", lineCount)
			return err
		}
		offset = nextOffset
	}

	// flush input.jsonl file
	if err := writer.Flush(); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to flush input file", "path", localInputFilePath)
		return err
	}

	// finalize the plan files
	if err := finalizePlanFiles(planWriter, modelToSafe); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to finalize plan files")
		return err
	}

	// model map file writing
	if err := writeModelMappings(jobRootDir, modelToSafe, lineCount); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to write model map file")
		return err
	}

	// log info
	logger.V(logging.INFO).Info("Processor Pre-processing job completed", "inputFilePath", localInputFilePath, "planFilePath", planWriter.plansDir(), "lineCount", lineCount)

	return nil
}

func checkPreProcessCancellation(ctx context.Context, cancelRequested *atomic.Bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if cancelRequested.Load() {
		return ErrCancelled
	}
	return nil
}

// readNormalizedLine reads a line from the input file and returns it normalized (terminated with '\n')
// if error occurs, it returns false for loop continuation decision (means not EOF) and the error
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

func extractModelID(line []byte) (string, error) {
	var req planRequestLine
	trimmedLine := bytes.TrimSuffix(line, []byte{'\n'})
	if err := json.Unmarshal(trimmedLine, &req); err != nil {
		return "", err
	}
	modelID := req.Body.Model
	if modelID == "" {
		return "", fmt.Errorf("model id is empty")
	}
	return modelID, nil
}

func writeModelMappings(jobRootDir string, modelToSafe map[string]string, lineCount int64) error {
	safeToModel := make(map[string]string, len(modelToSafe))
	for modelID, safeID := range modelToSafe {
		safeToModel[safeID] = modelID
	}

	modelMap := modelMapFile{
		ModelToSafe: modelToSafe,
		SafeToModel: safeToModel,
		LineCount:   lineCount,
	}
	return writeModelMapFile(jobRootDir, modelMap)
}
